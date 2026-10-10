package telemetry

// The metric catalog. Labels stay low-cardinality: service, phase, probe
// type, outcome — never target names or deployment IDs.
var (
	ProbeSamples  = NewCounter("vigilante_probe_samples_total", "Samples collected by central probes, by probe type.", "type")
	ProbeRestarts = NewCounter("vigilante_probe_restarts_total", "Probes that stopped with an error and were restarted.", "type")
	AgentSamples  = NewCounter("vigilante_agent_samples_total", "Samples pushed by on-host agents.")

	Evaluation = NewHistogram("vigilante_evaluation_seconds", "Time to evaluate every rule of a phase once.",
		[]float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1})
	Verdicts = NewCounter("vigilante_verdicts_total", "Phase verdicts.", "service", "phase", "verdict")

	RollbackTrigger = NewHistogram("vigilante_rollback_trigger_seconds",
		"From the failing evaluation to the recorded rollback start (gates, lock, state write).", DurationBuckets)
	Rollbacks        = NewCounter("vigilante_rollbacks_total", "Rollbacks by result: rolled_back, failed, await_approval, blocked, handed_over.", "service", "result")
	Approvals        = NewCounter("vigilante_rollback_approvals_total", "Rollbacks in approve mode by decision: requested, approved, rejected, expired.", "service", "decision")
	RollbackDuration = NewHistogram("vigilante_rollback_duration_seconds", "Rollback plan execution time.", DurationBuckets, "result")

	StoreAppend = NewHistogram("vigilante_store_append_seconds", "State store write latency.",
		[]float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}, "backend")
	StoreErrors = NewCounter("vigilante_store_errors_total", "Failed state store writes (fenced = leadership lost).", "reason")

	SSHConnections = NewGauge("vigilante_ssh_connections", "Pooled SSH connections.")
	SSHSessions    = NewGauge("vigilante_ssh_sessions", "Open SSH sessions (commands and streams).")
	SSHDials       = NewCounter("vigilante_ssh_dials_total", "SSH connection attempts.", "result")

	APIRequests = NewCounter("vigilante_api_requests_total", "API requests by route and status code.", "method", "route", "code")
	APILatency  = NewHistogram("vigilante_api_request_seconds", "API request latency (excluding long-poll waits is up to the client).",
		[]float64{.001, .005, .01, .05, .1, .5, 1, 5, 30, 120, 600}, "route")

	ITSMCalls = NewCounter("vigilante_itsm_calls_total", "ServiceNow calls (incidents, work notes) by result.", "kind", "result")

	AuditExported = NewCounter("vigilante_audit_exported_total", "Audit records sent to the SIEM.")
	AuditDropped  = NewCounter("vigilante_audit_export_dropped_total", "Audit records not sent to the SIEM (queue full or send failure).")
)
