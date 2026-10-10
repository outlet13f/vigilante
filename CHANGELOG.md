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
- Load harness (`test/load`, weekly workflow) and chaos tests for store and
  load-balancer outages.

### Fixed (store)
- State writes that failed while the store was unreachable were dropped;
  they are now queued in order and written when it returns.
- A rollback that started while the store was unreachable ended
  ROLLBACK_FAILED because the service lease could not be taken. It now
  retries for `safety.rollback_lease.wait` (10s) and then rolls back under
  the in-process lock, recording `lease.unavailable` (and `lease.conflict`
  if another process holds the lease when the store returns).
  `on_unavailable: fail` keeps the old behaviour.

### Fixed (security)
- The local CLI (no `--server`) acts on the state store directly, so it
  skipped role checks and four-eyes. With authentication configured
  (`auth.local_cli: auto`, the default), deciding approvals, approving
  escalations, `circuit reset|trip` and `--freeze-override` now need
  `--break-glass REASON`, which is audited (`breakglass.<action>`) and sent
  as a critical alert; local approval decisions also apply `four_eyes`.
- Helm chart: refuses to render when the config has no authentication
  (service accounts, OIDC, or `server.auth_token_env` supplied via
  env/envFrom); set `auth.allowAnonymous=true` for development installs.
  Previously every in-cluster caller was an anonymous admin.

### Fixed (deployment)
- Helm chart with server TLS: the HA advertise URL, probes, port names,
  Ingress backend port and ServiceMonitor scheme use https (the advertise
  URL was always `http://`, which broke forwarding); new `tls.enabled`,
  `tls.secretName` and `serviceMonitor.tlsConfig` values.
  `server.tls.client_auth: require` is refused (probes carry no certificate).
- `server.ha.tls` (`ca_file`, `server_name`, client cert): followers verify
  the leader by a name in its certificate rather than the pod IP they
  forward to.
- Helm chart memory defaults raised to 512Mi request / 2Gi limit (512Mi was
  below the 1.1 GiB peak heap measured at 20,000 probes), with `GOMEMLIMIT`
  at 90% of the limit (`goMemLimit` to override or turn off).

### Fixed (decisions)
- An overloaded orchestrator read its own probe timeouts as target failures
  and rolled back healthy releases. `safety.observer_guard` (on by default)
  watches scheduling lag, a loopback round trip and timeouts spread across
  services, and holds breaches built on probe failures while the observer
  is degraded and for `grace` (1m) after.

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
