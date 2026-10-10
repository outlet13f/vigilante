# Changelog

All notable changes are listed here. Versions follow [SemVer](https://semver.org/);
the compatibility promise and upgrade procedures are in
[docs/08-upgrade.md](docs/08-upgrade.md). Every release lists, when there are
any, **Upgrade notes** (steps beyond installing the new package) first.

## [Unreleased]

First release candidate content (1.0.0). Nothing to upgrade from yet.

### Added
- Rollback engine: probes (HTTP, gRPC, TCP, host, Docker, logs, access logs,
  databases), composite rules with baselines, phased observation, rollback
  plans with traffic drain, executors for symlink releases, containers,
  vSphere, Nutanix, KVM, OpenStack and generic exec/webhook, traffic
  controllers for Nginx, HAProxy, Envoy, F5, AWS ALB and Octavia.
- Safety: circuit breaker, blast-radius guard, flapping limit, crash resume.
- State store (file or PostgreSQL) with HA leader election; OIDC, service
  accounts and role bindings; Vault secrets; tamper-evident audit trail.
- Open API v2 (OpenAPI 3.1): OAuth2 client credentials, API keys, scopes,
  rate limits, idempotency keys, events over SSE and signed webhooks.
- Approval mode, change freezes, ServiceNow change gate and incidents,
  Teams / email / PagerDuty alerts, web operations console.
- Decision quality: verdict feedback (`PUT /v2/deployments/{id}/feedback`,
  `vigilante feedback`, console) and `vigilante pilot report` against the
  release gate.
- `vigilante lab run|summary`: the real-equipment scenario (checkpoint, bad
  deployment, detection, drain, restore, back in traffic) with results for
  the compatibility matrix.
- SSH session budget per target (`connection.max_sessions`,
  `reserved_sessions`): rollback always finds a free session.
- `connection.sudo_scope: changes` (sudo only for changing commands),
  `vigilante sudoers` and doctor checks of each rule.
- `log.remote_grep`: filter busy logs on the target before they cross SSH.

### Changed
- DB probe: every interval runs the query on one kept connection; the
  full pool check (pool_size new connections at once) runs every
  `pool_check_interval` (default 1m), so `pool_acquired` samples arrive once
  a minute. `pool_check_interval: 0s` restores the previous behaviour.

### Fixed
- Windows: local commands with quoted arguments reached the program with
  literal backslashes (cmd.exe does not parse Go's argument escaping).
- Release: minimal build, rpm/deb, container image, Helm chart, SBOMs,
  signed air-gapped bundles; `vigilante store status|migrate` with a
  downgrade guard.
