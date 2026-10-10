package events

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/safety"
)

// store is an in-memory journal: entries in order, replayable.
type store struct {
	mu      sync.Mutex
	entries []journal.Entry
}

func (s *store) record(e journal.Entry) {
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.mu.Unlock()
}

func (s *store) state() *journal.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := journal.NewState()
	for _, e := range s.entries {
		st.Apply(e)
	}
	return st
}

func newBus(st *store) *Bus {
	b := New(Deps{Record: st.record, Active: func() bool { return true },
		TeamOf:     func(svc string) string { return map[string]string{"order": "payments"}[svc] },
		SigningKey: func(context.Context) ([]byte, error) { return []byte("master-key"), nil }})
	b.Reload(st.state())
	return b
}

func dep(state model.State, verdict model.Verdict) *model.Deployment {
	return &model.Deployment{ID: "d1", Service: "order", Version: "v2", PreviousVersion: "v1", State: state, Verdict: verdict}
}

func types(b *Bus) []string {
	evs, _ := b.Since(0, Filter{}, 0)
	var out []string
	for _, e := range evs {
		out = append(out, strings.TrimPrefix(e.Type, "vigilante."))
	}
	return out
}

func TestEventsFollowDeploymentLifecycle(t *testing.T) {
	st := &store{}
	b := newBus(st)
	for _, d := range []*model.Deployment{
		dep(model.StatePending, model.VerdictPending),
		dep(model.StateObserving, model.VerdictPending),
		dep(model.StateObserving, model.VerdictPending), // re-persisted, no change: no event
		dep(model.StateRollingBack, model.VerdictFail),
		dep(model.StateAwaitApproval, model.VerdictFail),
		dep(model.StateRollingBack, model.VerdictFail),
		dep(model.StateRolledBack, model.VerdictFail),
	} {
		b.Observe(journal.Entry{Kind: journal.KindDeployment, Deployment: d})
	}
	b.Observe(journal.Entry{Kind: journal.KindAudit, Action: "escalation.approve", Actor: "user:bob", DeployID: "d1", Service: "order"})
	b.Observe(journal.Entry{Kind: journal.KindCircuit, Circuit: &safety.CircuitState{State: safety.Open, Reason: "2 failures"}})
	b.Observe(journal.Entry{Kind: journal.KindCircuit, Circuit: &safety.CircuitState{State: safety.Open, Reason: "still"}})
	b.Observe(journal.Entry{Kind: journal.KindCircuit, Circuit: &safety.CircuitState{State: safety.Closed}})
	b.Observe(journal.Entry{Kind: journal.KindStepDone}) // ignored

	want := "deployment.created observation.started observation.failed rollback.started approval.requested rollback.started rollback.completed approval.decided circuit.opened circuit.closed"
	if got := strings.Join(types(b), " "); got != want {
		t.Fatalf("events:\n got  %s\n want %s", got, want)
	}
	evs, _ := b.Since(0, Filter{}, 0)
	if evs[0].Team != "payments" || evs[0].Sequence != 1 || evs[0].ID != "1" || evs[0].SpecVersion != "1.0" || !strings.Contains(string(evs[0].Data), `"exit_code":1`) {
		t.Fatalf("event shape: %+v %s", evs[0], evs[0].Data)
	}

	// Paths that do not go through OBSERVING.
	b2 := newBus(&store{})
	for _, d := range []*model.Deployment{
		{ID: "g", Service: "order", State: model.StatePending},
		{ID: "g", Service: "order", State: model.StateSucceeded},
		{ID: "h", Service: "order", State: model.StatePending},
		{ID: "h", Service: "order", State: model.StateHeld},
		{ID: "o", Service: "order", State: model.StateObserving},
		{ID: "o", Service: "order", State: model.StateAborted, Verdict: model.VerdictHold},
		{ID: "p", Service: "order", State: model.StateObserving},
		{ID: "p", Service: "order", State: model.StatePromoted, Verdict: model.VerdictPass},
		{ID: "q", Service: "order", State: model.StateObserving},
		{ID: "q", Service: "order", State: model.StateRollbackFailed, Verdict: model.VerdictFail},
	} {
		b2.Observe(journal.Entry{Kind: journal.KindDeployment, Deployment: d})
	}
	want = "deployment.created deployment.marked_good deployment.created observation.held observation.started observation.aborted observation.started observation.passed observation.started observation.failed rollback.failed"
	if got := strings.Join(types(b2), " "); got != want {
		t.Fatalf("events:\n got  %s\n want %s", got, want)
	}
}

// A new leader replays the journal and continues the sequence; it does not
// re-announce states it already knows.
func TestSequenceContinuesAfterReload(t *testing.T) {
	st := &store{}
	b := newBus(st)
	b.Observe(journal.Entry{Kind: journal.KindDeployment, Deployment: dep(model.StatePending, "")})
	st.record(journal.Entry{Kind: journal.KindDeployment, Deployment: dep(model.StatePending, "")})
	b.Emit(model.EvAgentLost, "app-1", "", map[string]string{"target": "app-1"})

	b2 := newBus(st)
	if b2.Seq() != 2 {
		t.Fatalf("sequence after reload: %d", b2.Seq())
	}
	b2.Observe(journal.Entry{Kind: journal.KindDeployment, Deployment: dep(model.StatePending, "")})
	b2.Observe(journal.Entry{Kind: journal.KindDeployment, Deployment: dep(model.StateObserving, "")})
	evs, _ := b2.Since(2, Filter{}, 0)
	if len(evs) != 1 || evs[0].Sequence != 3 || evs[0].Type != model.EvObservationStarted {
		t.Fatalf("after reload: %+v", evs)
	}
	if _, gap := b2.Since(0, Filter{}, 0); gap {
		t.Fatal("no gap expected")
	}
}

func TestSubscribeHasNoHoles(t *testing.T) {
	b := newBus(&store{})
	b.Emit(model.EvPing, "x", "", nil)
	backlog, sub := b.Subscribe(0, Filter{Types: []string{model.EvPing}})
	b.Emit(model.EvPing, "y", "", nil)
	b.Emit(model.EvAgentLost, "z", "", nil) // filtered out
	if len(backlog) != 1 || backlog[0].Subject != "x" {
		t.Fatalf("backlog: %+v", backlog)
	}
	select {
	case ev := <-sub.C():
		if ev.Subject != "y" {
			t.Fatalf("live: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no live event")
	}
	b.Unsubscribe(sub)
	if _, ok := <-sub.C(); ok {
		t.Fatal("channel must close")
	}
}

func TestMatchesAndSignature(t *testing.T) {
	w := &model.Webhook{ID: "wh_1", Types: []string{model.EvRollbackFailed}, Teams: []string{"payments"}}
	cases := []struct {
		ev   model.CloudEvent
		want bool
	}{
		{model.CloudEvent{Type: model.EvRollbackFailed, Service: "order", Team: "payments"}, true},
		{model.CloudEvent{Type: model.EvRollbackFailed, Service: "search", Team: "search"}, false},
		{model.CloudEvent{Type: model.EvRollbackStarted, Service: "order", Team: "payments"}, false},
		{model.CloudEvent{Type: model.EvPing, Subject: "wh_1"}, true},
		{model.CloudEvent{Type: model.EvPing, Subject: "wh_2"}, false},
	}
	for _, c := range cases {
		if got := Matches(w, &c.ev); got != c.want {
			t.Errorf("%s/%s: %v", c.ev.Type, c.ev.Service, got)
		}
	}
	// Global events reach type-matching subscriptions whatever their scope.
	if !Matches(&model.Webhook{Teams: []string{"payments"}}, &model.CloudEvent{Type: model.EvCircuitOpened}) {
		t.Error("circuit events are for everyone")
	}

	b := newBus(&store{})
	hook := &model.Webhook{ID: "wh_1", KeyVersion: 1}
	secret, err := b.Secret(context.Background(), hook)
	if err != nil || !strings.HasPrefix(secret, SecretPrefix) {
		t.Fatal(secret, err)
	}
	key, _ := b.signingKey(context.Background(), hook)
	sig := Sign(key, "7", "1700000000", []byte(`{"a":1}`))
	if !Verify(secret, "7", "1700000000", sig, []byte(`{"a":1}`)) || Verify(secret, "7", "1700000001", sig, []byte(`{"a":1}`)) {
		t.Fatal("signature verification")
	}
	hook.KeyVersion = 2
	if s2, _ := b.Secret(context.Background(), hook); s2 == secret {
		t.Fatal("rotation must change the secret")
	}
}

func TestWebhooksSurviveReload(t *testing.T) {
	st := &store{}
	b := newBus(st)
	ctx := context.Background()
	keep := &model.Webhook{URL: "https://a.example/hook", Types: []string{model.EvRollbackFailed}}
	drop := &model.Webhook{URL: "https://b.example/hook"}
	if _, err := b.CreateWebhook(ctx, keep); err != nil {
		t.Fatal(err)
	}
	b.CreateWebhook(ctx, drop)
	b.Emit(model.EvAgentLost, "x", "", nil)
	b.mu.Lock()
	b.hooks[keep.ID].Cursor = 1
	b.d.Record(journal.Entry{Kind: journal.KindWebhookCursor, Message: keep.ID, Seq: 1})
	b.mu.Unlock()
	if _, err := b.UpdateWebhook(keep.ID, func(w *model.Webhook) error { w.Description = "pager"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteWebhook(drop.ID); err != nil {
		t.Fatal(err)
	}

	b2 := newBus(st)
	hs := b2.Webhooks()
	if len(hs) != 1 || hs[0].ID != keep.ID || hs[0].Cursor != 1 || hs[0].Description != "pager" || !hs[0].Active {
		t.Fatalf("after reload: %+v", hs)
	}
	s1, _ := b.Secret(ctx, keep)
	s2, _ := b2.Secret(ctx, hs[0])
	if s1 != s2 {
		t.Fatal("the derived secret must not change across nodes")
	}
	// Nothing secret was written to the store.
	for _, e := range st.entries {
		if e.Webhook != nil && strings.Contains(e.Webhook.URL+e.Webhook.Description, "whsec_") {
			t.Fatal("secret persisted")
		}
	}
}
