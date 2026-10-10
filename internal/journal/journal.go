// Package journal is an append-only JSONL write-ahead log of everything the
// orchestrator decides and does. It doubles as the audit trail and as the
// recovery source: after a crash, Replay rebuilds deployment state, the
// circuit breaker and rollback history, and reports rollbacks that were in
// flight so they can be resumed (rollback steps are idempotent).
package journal

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"vigilante/internal/model"
	"vigilante/internal/safety"
)

const (
	KindDeployment    = "deployment"      // full deployment snapshot
	KindCircuit       = "circuit"         // circuit breaker state
	KindRollbackStart = "rollback.start"  // service rollback begins (flapping history)
	KindStepDone      = "rollback.step"   // target finished plan step N
	KindAudit         = "audit"           // who did what (API/CLI actions, denials)
	KindAnchor        = "anchor"          // chain start after pruning: Hash = last pruned entry's hash
	KindOperation     = "operation"       // API long-running operation snapshot
	KindIdempotency   = "idempotency"     // response remembered for an Idempotency-Key
	KindAPIClient     = "api-client"      // API client snapshot (secret stored as SHA-256)
	KindAccessToken   = "access-token"    // OAuth access token issued (stored as SHA-256)
	KindClientUsed    = "api-client.used" // Message = client ID; Time = last use (at most hourly)
	KindEvent         = "event"           // CloudEvent published to subscribers
	KindWebhook       = "webhook"         // webhook subscription snapshot
	KindWebhookCursor = "webhook.cursor"  // Message = webhook ID; Seq = last event handled
	KindFreeze        = "freeze"          // change freeze declared or ended through the API
)

// MaxEvents is how many recent events replay keeps for SSE resume and
// webhook catch-up.
const MaxEvents = 10000

// Bookkeeping reports kinds that only rebuild state; their meaning is
// carried by audit entries, so audit queries and SIEM export skip them.
func Bookkeeping(kind string) bool {
	switch kind {
	case KindDeployment, KindStepDone, KindOperation, KindIdempotency, KindAPIClient, KindAccessToken, KindClientUsed,
		KindEvent, KindWebhook, KindWebhookCursor, KindFreeze:
		return true
	}
	return false
}

type Entry struct {
	Time       time.Time            `json:"time"`
	Kind       string               `json:"kind"`
	Service    string               `json:"service,omitempty"`
	Deployment *model.Deployment    `json:"deployment,omitempty"`
	Circuit    *safety.CircuitState `json:"circuit,omitempty"`
	Operation  *model.Operation     `json:"operation,omitempty"`
	Idem       *model.IdemRecord    `json:"idempotency,omitempty"`
	Client     *model.APIClient     `json:"api_client,omitempty"`
	Token      *model.AccessToken   `json:"access_token,omitempty"`
	Event      *model.CloudEvent    `json:"event,omitempty"`
	Webhook    *model.Webhook       `json:"webhook,omitempty"`
	Seq        int64                `json:"seq,omitempty"`
	Freeze     *model.Freeze        `json:"freeze,omitempty"`
	DeployID   string               `json:"deployment_id,omitempty"`
	Target     string               `json:"target,omitempty"`
	Step       int                  `json:"step,omitempty"`
	Message    string               `json:"message,omitempty"`

	// Audit fields.
	Actor  string `json:"actor,omitempty"`  // auth principal (user:alice, sa:ci, cli:bob@host) or "system"
	Source string `json:"source,omitempty"` // api | cli | agent | system
	Action string `json:"action,omitempty"` // e.g. deployment.create, rollback.manual, circuit.reset, denied
	Reason string `json:"reason,omitempty"`
	Ticket string `json:"ticket,omitempty"` // change / incident ticket reference

	// Hash chain: Hash = SHA-256(Prev + newline + entry JSON without Hash).
	// Editing or deleting any entry breaks every hash after it.
	Prev string `json:"prev,omitempty"`
	Hash string `json:"hash,omitempty"`
	// MAC = HMAC-SHA256(audit.chain_key_ref, Hash), set while a chain key is
	// configured. It is not part of Hash; without the key nobody can rewrite
	// entries and recompute a chain that still verifies.
	MAC string `json:"mac,omitempty"`

	// Compacted (anchor entries only): the state the pruned entries had
	// built, replayed in place of them.
	Compacted []Entry `json:"compacted,omitempty"`
}

// ChainHash computes the entry's hash given the previous entry's hash.
// It hashes the canonical JSON of the entry (struct field order, sorted map
// keys) with Hash cleared, so it can be recomputed from any stored copy.
func (e Entry) ChainHash(prev string) string {
	e.Prev, e.Hash, e.MAC = prev, "", ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(append([]byte(prev+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

// Chain seals an entry onto the chain after prev.
func (e *Entry) Chain(prev string) {
	e.Prev = prev
	e.Hash = e.ChainHash(prev)
}

// Seal chains the entry after prev and, with a chain key, adds its MAC.
func (e *Entry) Seal(prev string, key []byte) {
	e.Chain(prev)
	e.MAC = ""
	if len(key) > 0 {
		e.MAC = ChainMAC(key, e.Hash)
	}
}

// ChainMAC is HMAC-SHA256(key, hash) in hex. The hash already covers the
// entry and, through Prev, everything before it.
func ChainMAC(key []byte, hash string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(hash))
	return hex.EncodeToString(m.Sum(nil))
}

// Verifier checks a sequence of entries in order.
type Verifier struct {
	// Key, when set, also checks MACs: chained entries before the first one
	// that carries a MAC count as Unkeyed (written before the key was
	// configured); from that entry on, every entry needs a valid MAC.
	Key []byte

	prev      string
	started   bool
	Checked   int   // chained entries verified
	Legacy    int   // entries written before the chain existed (no hash)
	Keyed     int   // entries whose MAC matched (Key set)
	Unkeyed   int   // chained entries before KeyedFrom (Key set)
	KeyedFrom int64 // position of the first entry with a valid MAC (0 = none)
}

// ErrChainBroken describes the first entry whose hash does not match.
type ErrChainBroken struct {
	Pos    int64 // sequence number or line
	Reason string
}

func (e *ErrChainBroken) Error() string {
	return fmt.Sprintf("audit chain broken at entry %d: %s", e.Pos, e.Reason)
}

// Next verifies one entry at position pos.
func (v *Verifier) Next(pos int64, e Entry) error {
	if e.Kind == KindAnchor {
		if v.started {
			return &ErrChainBroken{pos, "anchor in the middle of the chain"}
		}
		v.prev, v.started = e.Hash, true
		if e.MAC != "" { // the last pruned entry's MAC
			return v.checkMAC(pos, e)
		}
		return nil
	}
	if e.Hash == "" {
		if v.started {
			return &ErrChainBroken{pos, "entry has no hash (inserted without the chain)"}
		}
		v.Legacy++
		return nil
	}
	if !v.started {
		v.started, v.prev = true, e.Prev // first chained entry: trust its link
	}
	if e.Prev != v.prev {
		return &ErrChainBroken{pos, "previous-hash link does not match (an entry before it was removed or reordered)"}
	}
	if got := e.ChainHash(e.Prev); got != e.Hash {
		return &ErrChainBroken{pos, "content does not match its hash (the entry was modified)"}
	}
	if err := v.checkMAC(pos, e); err != nil {
		return err
	}
	v.prev = e.Hash
	v.Checked++
	return nil
}

// checkMAC checks a chained entry's MAC against Key (a no-op without one).
func (v *Verifier) checkMAC(pos int64, e Entry) error {
	switch {
	case len(v.Key) == 0:
		return nil
	case e.MAC == "" && v.KeyedFrom == 0:
		v.Unkeyed++ // written before the chain key was configured
		return nil
	case e.MAC == "":
		return &ErrChainBroken{pos, fmt.Sprintf("entry has no MAC although the chain key protects the chain from entry %d on "+
			"(written or rewritten without the key)", v.KeyedFrom)}
	case !hmac.Equal([]byte(e.MAC), []byte(ChainMAC(v.Key, e.Hash))):
		if v.KeyedFrom == 0 {
			return &ErrChainBroken{pos, "MAC does not match (wrong chain key, or the entry was rewritten without it)"}
		}
		return &ErrChainBroken{pos, "MAC does not match (the entry was rewritten without the chain key)"}
	}
	if v.KeyedFrom == 0 {
		v.KeyedFrom = pos
	}
	v.Keyed++
	return nil
}

// Head is the hash of the last verified entry.
func (v *Verifier) Head() string { return v.prev }

type Journal struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func Open(path string) (*Journal, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Journal{f: f, path: path}, nil
}

func (j *Journal) Path() string { return j.path }

// Append writes and fsyncs one entry; decisions are durable before actions run.
func (j *Journal) Append(e Entry) error {
	if j == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	return j.f.Close()
}

// State is the result of replaying a journal.
type State struct {
	Deployments map[string]*model.Deployment
	Circuit     *safety.CircuitState
	Rollbacks   map[string][]time.Time    // service -> rollback start times
	StepsDone   map[string]map[string]int // deployment -> target -> highest completed step index + 1
	Operations  map[string]*model.Operation
	Idempotency map[string]*model.IdemRecord // by IdemRecord.Key
	Clients     map[string]*model.APIClient
	Tokens      map[string]*model.AccessToken // by SHA256
	Events      []*model.CloudEvent           // the most recent MaxEvents, oldest first
	Webhooks    map[string]*model.Webhook
	Freezes     map[string]*model.Freeze
	Corrupt     int // unparsable lines skipped (e.g. torn final write)
}

// InFlight returns deployments whose rollback had started but not finished.
func (s *State) InFlight() []*model.Deployment {
	var out []*model.Deployment
	for _, d := range s.Deployments {
		if d.State == model.StateRollingBack {
			out = append(out, d)
		}
	}
	return out
}

// Compact returns the entries that rebuild what a pruned prefix contributed
// to the state: deployments still in progress (with their rollback steps),
// each service's last successful deployment (the default --previous), the
// circuit breaker, and rollback starts still inside the flapping window.
func Compact(pruned []Entry, cutoff time.Time) []Entry {
	st := NewState()
	for _, e := range pruned {
		st.Apply(e)
	}
	var out []Entry
	if st.Circuit != nil {
		out = append(out, Entry{Kind: KindCircuit, Circuit: st.Circuit})
	}
	lastGood := map[string]*model.Deployment{}
	for _, d := range st.Deployments {
		if d.State == model.StateSucceeded {
			if cur := lastGood[d.Service]; cur == nil || d.UpdatedAt.After(cur.UpdatedAt) {
				lastGood[d.Service] = d
			}
		}
	}
	ids := make([]string, 0, len(st.Deployments))
	for id := range st.Deployments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := st.Deployments[id]
		if d.State.Terminal() && lastGood[d.Service] != d {
			continue
		}
		out = append(out, Entry{Kind: KindDeployment, Time: d.UpdatedAt, Deployment: d})
		if !d.State.Terminal() {
			for target, n := range st.StepsDone[id] {
				out = append(out, Entry{Kind: KindStepDone, DeployID: id, Target: target, Step: n - 1})
			}
		}
	}
	for svc, times := range st.Rollbacks {
		for _, t := range times {
			if cutoff.Sub(t) < time.Hour {
				out = append(out, Entry{Kind: KindRollbackStart, Service: svc, Time: t})
			}
		}
	}
	// Operations still running, and idempotency records young enough that a
	// client may still retry.
	opIDs := make([]string, 0, len(st.Operations))
	for id := range st.Operations {
		opIDs = append(opIDs, id)
	}
	sort.Strings(opIDs)
	for _, id := range opIDs {
		if op := st.Operations[id]; op.Status == model.OpRunning {
			out = append(out, Entry{Kind: KindOperation, Time: op.CreatedAt, Operation: op})
		}
	}
	// Every API client (revoked ones too, for the record) and unexpired tokens.
	clientIDs := make([]string, 0, len(st.Clients))
	for id := range st.Clients {
		clientIDs = append(clientIDs, id)
	}
	sort.Strings(clientIDs)
	for _, id := range clientIDs {
		out = append(out, Entry{Kind: KindAPIClient, Time: st.Clients[id].CreatedAt, Client: st.Clients[id]})
	}
	toks := make([]string, 0, len(st.Tokens))
	for h := range st.Tokens {
		toks = append(toks, h)
	}
	sort.Strings(toks)
	for _, h := range toks {
		if t := st.Tokens[h]; t.ExpiresAt.After(cutoff) {
			out = append(out, Entry{Kind: KindAccessToken, Time: cutoff, Token: t})
		}
	}
	// Webhooks (with their cursors), and the last events so sequence numbers
	// keep increasing and subscribers can still catch up.
	hookIDs := make([]string, 0, len(st.Webhooks))
	for id := range st.Webhooks {
		hookIDs = append(hookIDs, id)
	}
	sort.Strings(hookIDs)
	for _, id := range hookIDs {
		out = append(out, Entry{Kind: KindWebhook, Time: st.Webhooks[id].UpdatedAt, Webhook: st.Webhooks[id]})
	}
	evs := st.Events
	if len(evs) > 1000 {
		evs = evs[len(evs)-1000:]
	}
	for _, ev := range evs {
		out = append(out, Entry{Kind: KindEvent, Time: ev.Time, Event: ev})
	}
	freezeIDs := make([]string, 0, len(st.Freezes))
	for id := range st.Freezes {
		freezeIDs = append(freezeIDs, id)
	}
	sort.Strings(freezeIDs)
	for _, id := range freezeIDs {
		if f := st.Freezes[id]; f.EndedAt == nil && f.EndsAt.After(cutoff) {
			out = append(out, Entry{Kind: KindFreeze, Time: f.CreatedAt, Freeze: f})
		}
	}
	keys := make([]string, 0, len(st.Idempotency))
	for k := range st.Idempotency {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if r := st.Idempotency[k]; cutoff.Sub(r.CreatedAt) < model.IdemTTL {
			out = append(out, Entry{Kind: KindIdempotency, Time: r.CreatedAt, Idem: r})
		}
	}
	return out
}

// NewState returns an empty replay state.
func NewState() *State {
	return &State{
		Deployments: map[string]*model.Deployment{},
		Rollbacks:   map[string][]time.Time{},
		StepsDone:   map[string]map[string]int{},
		Operations:  map[string]*model.Operation{},
		Idempotency: map[string]*model.IdemRecord{},
		Clients:     map[string]*model.APIClient{},
		Tokens:      map[string]*model.AccessToken{},
		Webhooks:    map[string]*model.Webhook{},
		Freezes:     map[string]*model.Freeze{},
	}
}

// Apply folds one entry into the state. Every store backend replays through
// this, so file and database journals mean exactly the same thing.
func (st *State) Apply(e Entry) {
	switch e.Kind {
	case KindDeployment:
		if e.Deployment != nil {
			st.Deployments[e.Deployment.ID] = e.Deployment
		}
	case KindCircuit:
		st.Circuit = e.Circuit
	case KindOperation:
		if e.Operation != nil {
			st.Operations[e.Operation.ID] = e.Operation
		}
	case KindIdempotency:
		if e.Idem != nil {
			st.Idempotency[e.Idem.Key] = e.Idem
		}
	case KindAPIClient:
		if e.Client != nil {
			st.Clients[e.Client.ID] = e.Client
		}
	case KindAccessToken:
		if e.Token != nil {
			st.Tokens[e.Token.SHA256] = e.Token
		}
	case KindEvent:
		if e.Event != nil {
			st.Events = append(st.Events, e.Event)
			if len(st.Events) > 2*MaxEvents {
				st.Events = append([]*model.CloudEvent(nil), st.Events[len(st.Events)-MaxEvents:]...)
			}
		}
	case KindWebhook:
		switch {
		case e.Webhook == nil:
		case e.Webhook.Deleted:
			delete(st.Webhooks, e.Webhook.ID)
		default:
			st.Webhooks[e.Webhook.ID] = e.Webhook
		}
	case KindFreeze:
		if e.Freeze != nil {
			st.Freezes[e.Freeze.ID] = e.Freeze
		}
	case KindWebhookCursor:
		if w := st.Webhooks[e.Message]; w != nil && e.Seq > w.Cursor {
			w.Cursor = e.Seq
		}
	case KindClientUsed:
		if c := st.Clients[e.Message]; c != nil {
			t := e.Time
			c.LastUsedAt = &t
		}
	case KindRollbackStart:
		st.Rollbacks[e.Service] = append(st.Rollbacks[e.Service], e.Time)
	case KindAnchor:
		for _, c := range e.Compacted {
			st.Apply(c)
		}
	case KindStepDone:
		m := st.StepsDone[e.DeployID]
		if m == nil {
			m = map[string]int{}
			st.StepsDone[e.DeployID] = m
		}
		if e.Step+1 > m[e.Target] {
			m[e.Target] = e.Step + 1
		}
	}
}

// Replay rebuilds state from a JSONL journal file (a missing file is empty).
func Replay(path string) (*State, error) {
	st := NewState()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			st.Corrupt++
			continue
		}
		st.Apply(e)
	}
	if err := sc.Err(); err != nil {
		return st, fmt.Errorf("replay %s: %w", path, err)
	}
	return st, nil
}
