package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/model"
)

// newID returns an opaque identifier; clients must not parse it.
func newID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// StartOperation records a running operation for the API.
func (e *Engine) StartOperation(kind, service string, d *model.Deployment, phase model.Phase, by string) *model.Operation {
	op := &model.Operation{ID: newID("op_"), Kind: kind, Status: model.OpRunning, Service: service,
		Phase: phase, CreatedBy: by, CreatedAt: time.Now().UTC()}
	if d != nil {
		op.DeploymentID = d.ID
	}
	e.mu.Lock()
	e.operations[op.ID] = op
	if e.liveOps == nil {
		e.liveOps = map[string]bool{}
	}
	e.liveOps[op.ID] = true
	cp := *op
	e.mu.Unlock()
	e.record(journal.Entry{Kind: journal.KindOperation, Operation: &cp, Service: service, DeployID: op.DeploymentID})
	return &cp
}

// FinishOperation records how an operation ended.
func (e *Engine) FinishOperation(id, status string, res *model.OperationResult, errMsg string) {
	e.mu.Lock()
	op, ok := e.operations[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	op.Status, op.Result, op.Error, op.FinishedAt = status, res, errMsg, &now
	delete(e.liveOps, id)
	cp := *op
	e.mu.Unlock()
	e.record(journal.Entry{Kind: journal.KindOperation, Operation: &cp, Service: cp.Service, DeployID: cp.DeploymentID})
}

// Operation returns a copy of an operation. One that was running on a
// previous leader is judged from its deployment: once the deployment has
// left OBSERVING / ROLLING_BACK after the operation began, it is complete.
func (e *Engine) Operation(id string) (*model.Operation, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	op, ok := e.operations[id]
	if !ok {
		return nil, false
	}
	cp := *op
	if cp.Status == model.OpRunning && !e.liveOps[id] && cp.DeploymentID != "" {
		if d := e.deployments[cp.DeploymentID]; d != nil && d.UpdatedAt.After(cp.CreatedAt) &&
			d.State != model.StateObserving && d.State != model.StateRollingBack && d.State != model.StatePending {
			cp.Status, cp.Result = model.OpCompleted, model.ResultOf(d)
			t := d.UpdatedAt
			cp.FinishedAt = &t
		}
	}
	return &cp, true
}

// Operations lists every known operation, newest first.
func (e *Engine) Operations() []*model.Operation {
	e.mu.Lock()
	ids := make([]string, 0, len(e.operations))
	for id := range e.operations {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	out := make([]*model.Operation, 0, len(ids))
	for _, id := range ids {
		if op, ok := e.Operation(id); ok {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Idempotent returns the remembered response for an idempotency key.
func (e *Engine) Idempotent(key string) (*model.IdemRecord, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.idem[key]
	if !ok || time.Since(r.CreatedAt) > model.IdemTTL {
		return nil, false
	}
	return r, true
}

// RememberIdempotent stores the response for an idempotency key durably,
// before it is sent, so a leader change cannot replay the action.
func (e *Engine) RememberIdempotent(r *model.IdemRecord) {
	e.mu.Lock()
	e.idem[r.Key] = r
	for k, old := range e.idem {
		if time.Since(old.CreatedAt) > model.IdemTTL {
			delete(e.idem, k)
		}
	}
	e.mu.Unlock()
	e.record(journal.Entry{Kind: journal.KindIdempotency, Idem: r})
}
