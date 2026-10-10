package model

import (
	"encoding/json"
	"time"
)

// Operation is a long-running request accepted by the API (observe a phase,
// roll back, approve an escalation, capture a baseline). Clients poll it or
// wait for its event instead of holding a connection open.
type Operation struct {
	ID           string           `json:"id"`
	Kind         string           `json:"kind"`   // observation | rollback | approval | baseline
	Status       string           `json:"status"` // running | completed | failed
	Service      string           `json:"service"`
	DeploymentID string           `json:"deployment_id,omitempty"`
	Phase        Phase            `json:"phase,omitempty"`
	CreatedBy    string           `json:"created_by,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	FinishedAt   *time.Time       `json:"finished_at,omitempty"`
	Result       *OperationResult `json:"result,omitempty"`
	Error        string           `json:"error,omitempty"`
}

const (
	OpObservation = "observation"
	OpRollback    = "rollback"
	OpApproval    = "approval"
	OpBaseline    = "baseline"

	OpRunning   = "running"
	OpCompleted = "completed" // the work ran; see Result for what it found
	OpFailed    = "failed"    // the work could not run (blocked, engine error)
)

// OperationResult is where the deployment stood when the operation ended.
type OperationResult struct {
	State           State   `json:"state,omitempty"`
	Verdict         Verdict `json:"verdict,omitempty"`
	ExitCode        *int    `json:"exit_code,omitempty"`
	Reason          string  `json:"reason,omitempty"`
	BaselineSamples int     `json:"baseline_samples,omitempty"`
}

// ResultOf summarises a deployment for an operation result.
func ResultOf(d *Deployment) *OperationResult {
	code := ExitCode(d)
	return &OperationResult{State: d.State, Verdict: d.Verdict, ExitCode: &code, Reason: d.Reason}
}

// IdemRecord remembers the response to a request sent with an
// Idempotency-Key, so a retried request gets the same answer instead of
// starting a second rollback.
type IdemRecord struct {
	Key         string          `json:"key"`         // SHA-256 of principal + key
	Fingerprint string          `json:"fingerprint"` // SHA-256 of method, path and body
	Status      int             `json:"status"`
	Location    string          `json:"location,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// IdemTTL is how long idempotency records are kept.
const IdemTTL = 24 * time.Hour
