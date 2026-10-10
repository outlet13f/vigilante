package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/model"
)

var (
	ErrWebhookNotFound = errors.New("webhook not found")
	ErrNoSigningKey    = errors.New("webhooks need api.webhook_signing_key_ref")
	ErrEventGone       = errors.New("event is no longer kept")
)

// SecretPrefix marks webhook signing secrets (Standard Webhooks format).
const SecretPrefix = "whsec_"

// signingKey derives a subscription's HMAC key from the master key, so no
// secret is stored anywhere; rotating bumps KeyVersion.
func (b *Bus) signingKey(ctx context.Context, w *model.Webhook) ([]byte, error) {
	if b.d.SigningKey == nil {
		return nil, ErrNoSigningKey
	}
	master, err := b.d.SigningKey(ctx)
	if err != nil {
		return nil, err
	}
	if len(master) == 0 {
		return nil, ErrNoSigningKey
	}
	m := hmac.New(sha256.New, master)
	fmt.Fprintf(m, "vigilante-webhook\x00%s\x00%d", w.ID, w.KeyVersion)
	return m.Sum(nil), nil
}

// Secret returns the subscription's signing secret (shown to its owner once).
func (b *Bus) Secret(ctx context.Context, w *model.Webhook) (string, error) {
	k, err := b.signingKey(ctx, w)
	if err != nil {
		return "", err
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(k), nil
}

// Sign computes the Standard Webhooks signature header value.
func Sign(key []byte, id, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + timestamp + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a delivery against a whsec_ secret (receivers and tests).
func Verify(secret, id, timestamp, signature string, body []byte) bool {
	raw, err := base64.StdEncoding.DecodeString(secret[len(SecretPrefix):])
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(Sign(raw, id, timestamp, body)), []byte(signature))
}

// ---------------------------------------------------------------- subscriptions

func (b *Bus) saveLocked(w *model.Webhook) {
	w.UpdatedAt = time.Now().UTC()
	cp := *w
	cp.DeadLetters = append([]model.DeadLetter(nil), w.DeadLetters...)
	b.d.Record(journal.Entry{Kind: journal.KindWebhook, Time: cp.UpdatedAt, Webhook: &cp})
}

// CreateWebhook registers a subscription starting after the latest event.
func (b *Bus) CreateWebhook(ctx context.Context, w *model.Webhook) (string, error) {
	var id [8]byte
	_, _ = rand.Read(id[:])
	w.ID = "wh_" + hex.EncodeToString(id[:])
	w.KeyVersion, w.Active = 1, true
	w.CreatedAt = time.Now().UTC()
	secret, err := b.Secret(ctx, w)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Cursor = b.seq
	b.hooks[w.ID] = w
	b.saveLocked(w)
	b.syncWorkersLocked()
	return secret, nil
}

// Webhooks lists subscriptions (copies).
func (b *Bus) Webhooks() []*model.Webhook {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*model.Webhook, 0, len(b.hooks))
	for _, w := range b.hooks {
		cp := *w
		out = append(out, &cp)
	}
	slices.SortFunc(out, func(x, y *model.Webhook) int {
		if c := x.CreatedAt.Compare(y.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare([]byte(x.ID), []byte(y.ID))
	})
	return out
}

// Webhook returns one subscription.
func (b *Bus) Webhook(id string) (*model.Webhook, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.hooks[id]
	if !ok {
		return nil, false
	}
	cp := *w
	return &cp, true
}

// UpdateWebhook changes a subscription. Re-activating clears the failure count.
func (b *Bus) UpdateWebhook(id string, fn func(*model.Webhook) error) (*model.Webhook, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.hooks[id]
	if !ok {
		return nil, ErrWebhookNotFound
	}
	cp := *w
	wasActive := cp.Active
	if err := fn(&cp); err != nil {
		return nil, err
	}
	if cp.Active && !wasActive {
		cp.Failures, cp.DisabledReason = 0, ""
	}
	b.hooks[id] = &cp
	b.saveLocked(&cp)
	b.signal()
	b.syncWorkersLocked()
	out := cp
	return &out, nil
}

// RotateSecret issues a new signing secret; the old one stops at once.
func (b *Bus) RotateSecret(ctx context.Context, id string) (string, *model.Webhook, error) {
	w, err := b.UpdateWebhook(id, func(w *model.Webhook) error { w.KeyVersion++; return nil })
	if err != nil {
		return "", nil, err
	}
	s, err := b.Secret(ctx, w)
	return s, w, err
}

// DeleteWebhook removes a subscription.
func (b *Bus) DeleteWebhook(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.hooks[id]
	if !ok {
		return ErrWebhookNotFound
	}
	delete(b.hooks, id)
	delete(b.queue, id)
	delete(b.history, id)
	b.d.Record(journal.Entry{Kind: journal.KindWebhook, Time: time.Now().UTC(), Webhook: &model.Webhook{ID: w.ID, Deleted: true}})
	b.syncWorkersLocked()
	return nil
}

// Deliveries returns recent attempts, newest first.
func (b *Bus) Deliveries(id string) ([]Delivery, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.hooks[id]; !ok {
		return nil, false
	}
	h := b.history[id]
	out := make([]Delivery, len(h))
	for i := range h {
		out[i] = h[len(h)-1-i]
	}
	return out, true
}

// Redeliver queues one stored event for the subscription again (for
// example from its dead-letter list) and removes it from that list.
func (b *Bus) Redeliver(id string, seq int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.hooks[id]
	if !ok {
		return ErrWebhookNotFound
	}
	if b.eventLocked(seq) == nil {
		return ErrEventGone
	}
	b.queue[id] = append(b.queue[id], seq)
	if i := slices.IndexFunc(w.DeadLetters, func(d model.DeadLetter) bool { return d.Sequence == seq }); i >= 0 {
		w.DeadLetters = slices.Delete(w.DeadLetters, i, i+1)
		b.saveLocked(w)
	}
	b.signal()
	return nil
}

// Ping publishes a test event that reaches only this subscription's filter
// (type vigilante.ping is delivered regardless of the type filter).
func (b *Bus) Ping(id, by string) (*model.CloudEvent, error) {
	if _, ok := b.Webhook(id); !ok {
		return nil, ErrWebhookNotFound
	}
	ev := b.Emit(model.EvPing, id, "", map[string]any{"webhook_id": id, "requested_by": by})
	if ev == nil {
		return nil, errors.New("this node is not the active leader")
	}
	return ev, nil
}

func (b *Bus) eventLocked(seq int64) *model.CloudEvent {
	i, ok := slices.BinarySearchFunc(b.events, seq, func(e *model.CloudEvent, s int64) int {
		switch {
		case e.Sequence < s:
			return -1
		case e.Sequence > s:
			return 1
		}
		return 0
	})
	if !ok {
		return nil
	}
	return b.events[i]
}

// Matches reports whether a subscription wants an event. Events of no
// service (circuit breaker, agents) go to every subscription.
func Matches(w *model.Webhook, ev *model.CloudEvent) bool {
	if ev.Type == model.EvPing {
		return ev.Subject == w.ID
	}
	if len(w.Types) > 0 && !slices.Contains(w.Types, ev.Type) {
		return false
	}
	if ev.Service == "" || (len(w.Services) == 0 && len(w.Teams) == 0) {
		return true
	}
	return slices.Contains(w.Services, ev.Service) || (ev.Team != "" && slices.Contains(w.Teams, ev.Team))
}

// ---------------------------------------------------------------- delivery workers

// syncWorkersLocked runs one worker per subscription.
func (b *Bus) syncWorkersLocked() {
	if b.ctx == nil {
		return
	}
	for id, cancel := range b.workers {
		if _, ok := b.hooks[id]; !ok {
			cancel()
			delete(b.workers, id)
		}
	}
	for id := range b.hooks {
		if _, ok := b.workers[id]; !ok {
			ctx, cancel := context.WithCancel(b.ctx)
			b.workers[id] = cancel
			b.workersWG.Add(1)
			go func() {
				defer b.workersWG.Done()
				b.work(ctx, id)
			}()
		}
	}
}

// next picks what a worker should send now: a queued redelivery, else the
// first matching event after the cursor. wait is closed on the next change.
func (b *Bus) next(id string) (w *model.Webhook, ev *model.CloudEvent, redelivery bool, wait <-chan struct{}, gone bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wait = b.changed
	hw, ok := b.hooks[id]
	if !ok {
		return nil, nil, false, wait, true
	}
	cp := *hw
	if !cp.Active || (b.d.Active != nil && !b.d.Active()) {
		return &cp, nil, false, wait, false
	}
	for len(b.queue[id]) > 0 {
		seq := b.queue[id][0]
		if e := b.eventLocked(seq); e != nil {
			return &cp, e, true, wait, false
		}
		b.queue[id] = b.queue[id][1:]
	}
	for _, e := range b.events {
		if e.Sequence > cp.Cursor && Matches(&cp, e) {
			return &cp, e, false, wait, false
		}
	}
	// Nothing matching: move the cursor past what was skipped.
	if n := len(b.events); n > 0 && b.events[n-1].Sequence > hw.Cursor {
		hw.Cursor = b.events[n-1].Sequence
		b.d.Record(journal.Entry{Kind: journal.KindWebhookCursor, Message: id, Seq: hw.Cursor})
	}
	return &cp, nil, false, wait, false
}

func (b *Bus) work(ctx context.Context, id string) {
	for ctx.Err() == nil {
		w, ev, redelivery, wait, gone := b.next(id)
		if gone {
			return
		}
		if ev == nil {
			select {
			case <-ctx.Done():
				return
			case <-wait:
			case <-time.After(time.Second): // leadership may have changed
			}
			continue
		}
		b.deliver(ctx, w, ev, redelivery)
	}
}

// deliver sends one event with retries, then records the outcome.
func (b *Bus) deliver(ctx context.Context, w *model.Webhook, ev *model.CloudEvent, redelivery bool) {
	body, _ := json.Marshal(ev)
	var lastErr string
	attempts := 0
	for i := 0; i <= len(b.Backoff); i++ {
		attempts++
		start := time.Now()
		code, err := b.post(ctx, w, ev, body)
		d := Delivery{Sequence: ev.Sequence, Type: ev.Type, Attempt: attempts, At: start.UTC(), StatusCode: code,
			DurationMs: time.Since(start).Milliseconds(), Outcome: "delivered"}
		if err == nil {
			b.finish(w.ID, ev, redelivery, d, nil)
			return
		}
		lastErr = err.Error()
		d.Error, d.Outcome = lastErr, "retrying"
		if i == len(b.Backoff) {
			d.Outcome = "dead"
		}
		b.addHistory(w.ID, d)
		if i == len(b.Backoff) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.Backoff[i]):
		}
		// Stop retrying if the subscription went away or was paused.
		if cur, ok := b.Webhook(w.ID); !ok || !cur.Active {
			return
		}
	}
	b.finish(w.ID, ev, redelivery, Delivery{}, &model.DeadLetter{Sequence: ev.Sequence, Type: ev.Type, At: time.Now().UTC(), Attempts: attempts, Error: lastErr})
}

func (b *Bus) post(ctx context.Context, w *model.Webhook, ev *model.CloudEvent, body []byte) (int, error) {
	if b.d.Active != nil && !b.d.Active() {
		return 0, errors.New("node is no longer the leader")
	}
	key, err := b.signingKey(ctx, w)
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("User-Agent", "Vigilante-Webhook/2")
	req.Header.Set("webhook-id", ev.ID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", Sign(key, ev.ID, ts, body))
	resp, err := b.d.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("receiver answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (b *Bus) addHistory(id string, d Delivery) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := append(b.history[id], d)
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
	}
	b.history[id] = h
}

// finish records a delivered event (dead == nil) or a dead letter, advances
// the cursor, and disables a subscription that keeps failing.
func (b *Bus) finish(id string, ev *model.CloudEvent, redelivery bool, d Delivery, dead *model.DeadLetter) {
	if dead == nil {
		b.addHistory(id, d)
	}
	b.mu.Lock()
	w, ok := b.hooks[id]
	if !ok {
		b.mu.Unlock()
		return
	}
	if redelivery {
		if q := b.queue[id]; len(q) > 0 && q[0] == ev.Sequence {
			b.queue[id] = q[1:]
		}
	} else if ev.Sequence > w.Cursor {
		w.Cursor = ev.Sequence
		b.d.Record(journal.Entry{Kind: journal.KindWebhookCursor, Message: id, Seq: w.Cursor})
	}
	var disabled bool
	switch {
	case dead == nil && w.Failures > 0:
		w.Failures = 0
		b.saveLocked(w)
	case dead != nil:
		w.Failures++
		w.DeadLetters = append(w.DeadLetters, *dead)
		if len(w.DeadLetters) > model.MaxDeadLetters {
			w.DeadLetters = w.DeadLetters[len(w.DeadLetters)-model.MaxDeadLetters:]
		}
		if b.DisableAfter > 0 && w.Failures >= b.DisableAfter && w.Active {
			w.Active = false
			w.DisabledReason = fmt.Sprintf("%d events in a row could not be delivered; last error: %s", w.Failures, dead.Error)
			disabled = true
		}
		b.saveLocked(w)
	}
	url, reason := w.URL, w.DisabledReason
	if disabled {
		b.publishLocked(model.EvWebhookDisabled, id, "", map[string]any{"webhook_id": id, "url": url, "reason": reason})
	}
	b.mu.Unlock()
	if disabled {
		b.d.Log.Error("webhook disabled", "webhook", id, "url", url, "reason", reason)
		if b.d.Audit != nil {
			b.d.Audit("webhook.disabled", id+" "+url+": "+reason)
		}
		if b.d.Alert != nil {
			b.d.Alert("Webhook subscription disabled: "+url, reason)
		}
	}
}
