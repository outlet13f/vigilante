// Package model holds the runtime types shared by every layer of the engine:
// samples flowing out of probes, deployment state and verdicts.
package model

import "time"

// Sample is a single measurement emitted by a probe for one target.
// Metric names are "<probe-id>.<metric>", e.g. "health.latency_ms".
type Sample struct {
	Target string    `json:"target"`
	Metric string    `json:"metric"`
	Value  float64   `json:"value"`
	Time   time.Time `json:"time"`
	// Source identifies the vantage point: "central" for the orchestrator,
	// "agent:<name>" for samples pushed by a lightweight agent.
	Source string `json:"source,omitempty"`
}

const SourceCentral = "central"

// Phase is a deployment stage with its own observation window.
type Phase string

const (
	PhaseBaseline Phase = "baseline"
	PhaseCanary   Phase = "canary"
	PhaseRolling  Phase = "rolling"
	PhaseFull     Phase = "full"
)

// PhaseOrder is the canonical promotion order.
var PhaseOrder = []Phase{PhaseCanary, PhaseRolling, PhaseFull}

// Verdict is the outcome of a phase evaluation.
type Verdict string

const (
	VerdictPending      Verdict = "PENDING"
	VerdictPass         Verdict = "PASS"
	VerdictFail         Verdict = "FAIL"
	VerdictHold         Verdict = "HOLD"
	VerdictInconclusive Verdict = "INCONCLUSIVE"
)

// State is the lifecycle state of a deployment inside the orchestrator.
type State string

const (
	StatePending        State = "PENDING"
	StateBaseline       State = "BASELINE"
	StateObserving      State = "OBSERVING"
	StatePromoted       State = "PROMOTED"  // phase passed, waiting for the next phase
	StateSucceeded      State = "SUCCEEDED" // full phase passed
	StateHeld           State = "HELD"      // needs a human decision
	StateRollingBack    State = "ROLLING_BACK"
	StateRolledBack     State = "ROLLED_BACK"
	StateRollbackFailed State = "ROLLBACK_FAILED"
	StateAwaitApproval  State = "AWAITING_APPROVAL"
	StateAborted        State = "ABORTED"
)

// Terminal reports whether no further automatic transitions happen.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateRolledBack, StateRollbackFailed, StateAborted:
		return true
	}
	return false
}

// Breach describes one rule that evaluated true.
type Breach struct {
	Rule          string  `json:"rule"`
	Target        string  `json:"target"`
	Detail        string  `json:"detail"`
	Action        string  `json:"action"`
	Environmental bool    `json:"environmental,omitempty"`
	Value         float64 `json:"value,omitempty"`
}

// Event is an entry in a deployment's timeline.
type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// Deployment is the unit the orchestrator watches and, if needed, rolls back.
type Deployment struct {
	ID              string                       `json:"id"`
	Service         string                       `json:"service"`
	Version         string                       `json:"version"`
	PreviousVersion string                       `json:"previous_version"`
	Phase           Phase                        `json:"phase"`
	State           State                        `json:"state"`
	Verdict         Verdict                      `json:"verdict"`
	Reason          string                       `json:"reason,omitempty"`
	Targets         []string                     `json:"targets"`               // targets running the new version
	Checkpoints     map[string]map[string]string `json:"checkpoints,omitempty"` // target -> executor checkpoint
	Breaches        []Breach                     `json:"breaches,omitempty"`
	// Who acted (auth principal IDs such as user:alice or sa:ci-order).
	CreatedBy           string    `json:"created_by,omitempty"`
	RollbackRequestedBy string    `json:"rollback_requested_by,omitempty"`
	ApprovedBy          string    `json:"approved_by,omitempty"`
	Events              []Event   `json:"events,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// AddEvent appends a timeline entry.
func (d *Deployment) AddEvent(kind, msg string) {
	now := time.Now()
	d.Events = append(d.Events, Event{Time: now, Kind: kind, Message: msg})
	d.UpdatedAt = now
}

// ExitCode maps a deployment outcome to the CLI contract used by CI systems.
//
//	0 = phase passed, 1 = engine error, 2 = failed and rolled back,
//	3 = rollback failed / circuit open (human required), 4 = held / inconclusive.
func ExitCode(d *Deployment) int {
	switch d.State {
	case StatePromoted, StateSucceeded:
		return 0
	case StateRolledBack:
		return 2
	case StateRollbackFailed, StateAwaitApproval:
		return 3
	case StateHeld:
		return 4
	}
	return 1
}
