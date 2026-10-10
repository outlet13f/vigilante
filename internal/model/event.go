package model

import (
	"encoding/json"
	"time"
)

// CloudEvent is a CloudEvents 1.0 record (structured JSON mode) of something that
// happened to a deployment, the circuit breaker or an agent. Sequence orders
// all events of a cluster; it is what SSE clients send back as Last-Event-ID.
type CloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject,omitempty"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Sequence        int64           `json:"sequence"`
	Service         string          `json:"service,omitempty"`
	Team            string          `json:"team,omitempty"`
	Data            json.RawMessage `json:"data"`
}

// Event types.
const (
	EvDeploymentCreated    = "vigilante.deployment.created"
	EvDeploymentMarkedGood = "vigilante.deployment.marked_good"
	EvObservationStarted   = "vigilante.observation.started"
	EvObservationPassed    = "vigilante.observation.passed"
	EvObservationFailed    = "vigilante.observation.failed"
	EvObservationHeld      = "vigilante.observation.held"
	EvObservationAborted   = "vigilante.observation.aborted"
	EvRollbackStarted      = "vigilante.rollback.started"
	EvRollbackCompleted    = "vigilante.rollback.completed"
	EvRollbackFailed       = "vigilante.rollback.failed"
	EvApprovalRequested    = "vigilante.approval.requested"
	EvApprovalDecided      = "vigilante.approval.decided"
	EvCircuitOpened        = "vigilante.circuit.opened"
	EvCircuitHalfOpened    = "vigilante.circuit.half_opened"
	EvCircuitClosed        = "vigilante.circuit.closed"
	EvAgentLost            = "vigilante.agent.lost"
	EvWebhookDisabled      = "vigilante.webhook.disabled"
	EvPing                 = "vigilante.ping"
)

// EventTypes is the catalog with a one-line meaning each.
var EventTypes = []struct{ Type, Description string }{
	{EvDeploymentCreated, "A deployment was registered."},
	{EvDeploymentMarkedGood, "A version was registered as known-good for a service."},
	{EvObservationStarted, "Observation of a phase started."},
	{EvObservationPassed, "A phase passed; the deployment may be promoted."},
	{EvObservationFailed, "A phase failed its rules; a rollback follows unless blocked."},
	{EvObservationHeld, "A phase ended without a clear verdict; a human must decide."},
	{EvObservationAborted, "Observation was stopped before a verdict."},
	{EvRollbackStarted, "A rollback started (automatic or manual)."},
	{EvRollbackCompleted, "Every target is back on the previous version."},
	{EvRollbackFailed, "The rollback failed or was blocked; targets may be isolated. Human required."},
	{EvApprovalRequested, "An escalation step (e.g. VM snapshot restore) waits for approval."},
	{EvApprovalDecided, "A rollback or escalation waiting for approval was approved or rejected."},
	{EvCircuitOpened, "The circuit breaker opened: automatic actions are frozen."},
	{EvCircuitHalfOpened, "The circuit breaker allows one trial action."},
	{EvCircuitClosed, "The circuit breaker closed: automation resumed."},
	{EvAgentLost, "An on-host agent stopped sending heartbeats."},
	{EvWebhookDisabled, "A webhook subscription was disabled after repeated delivery failures."},
	{EvPing, "Test event sent on request."},
}

// Webhook is an outbound event subscription.
type Webhook struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	Types       []string `json:"types"`    // empty = every type
	Services    []string `json:"services"` // empty with no teams = every service
	Teams       []string `json:"teams"`
	Active      bool     `json:"active"`
	// DisabledReason says why delivery stopped (automatic disable).
	DisabledReason string       `json:"disabled_reason,omitempty"`
	KeyVersion     int          `json:"key_version"` // signing secret generation
	Cursor         int64        `json:"cursor"`      // last event sequence handled
	Failures       int          `json:"consecutive_failures"`
	DeadLetters    []DeadLetter `json:"dead_letters,omitempty"`
	CreatedBy      string       `json:"created_by,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	// Deleted marks the record of a removed subscription.
	Deleted bool `json:"deleted,omitempty"`
}

// DeadLetter is an event a webhook could not deliver after every retry.
type DeadLetter struct {
	Sequence int64     `json:"sequence"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Attempts int       `json:"attempts"`
	Error    string    `json:"error"`
}

// MaxDeadLetters bounds the dead-letter list kept per webhook.
const MaxDeadLetters = 100
