-- Append-only event log: every orchestrator decision, rollback step and
-- circuit-breaker change. Replayed in seq order to rebuild state.
CREATE TABLE vigilante_events (
    seq           bigserial   PRIMARY KEY,
    at            timestamptz NOT NULL,
    kind          text        NOT NULL,
    service       text        NOT NULL DEFAULT '',
    deployment_id text        NOT NULL DEFAULT '',
    body          jsonb       NOT NULL
);
CREATE INDEX vigilante_events_deployment ON vigilante_events (deployment_id);
CREATE INDEX vigilante_events_kind_at   ON vigilante_events (kind, at);

-- Leases back per-service rollback locks and HA leader election.
CREATE TABLE vigilante_leases (
    key        text        PRIMARY KEY,
    owner      text        NOT NULL,
    expires_at timestamptz NOT NULL
);
