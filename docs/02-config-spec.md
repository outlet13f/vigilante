# 02. 설정 파일 명세 (`vigilante.yaml`)

완전한 예시: [`examples/config/vigilante.yaml`](../examples/config/vigilante.yaml) — 베어메탈(Nginx+F5), vSphere VM 위 Docker(HAProxy), EC2(ALB) 3개 서비스를 모두 담고 있습니다.

검증: `vigilante validate -c vigilante.yaml` — 알 수 없는 키(오타)는 즉시 거부되고, 모든 교차 참조(target→credential, service→probe/executor/traffic, rule→metric)를 검사합니다. CI의 첫 단계로 넣으십시오.

## 공통 규칙

- **비밀값은 YAML에 쓰지 않습니다.** `*_env`(환경변수 이름) 또는 키 파일 경로만 허용됩니다.
- 기간 값은 Go duration 문자열: `500ms`, `30s`, `5m`, `1h` (`0` 대신 `0s`).
- 문자열 값 다수는 Go 템플릿을 지원합니다. 사용 가능한 필드:

| 필드 | 의미 |
|---|---|
| `{{.Name}}` `{{.Address}}` `{{.Kind}}` | 대상 이름 / 주소 / 종류 |
| `{{.Labels.KEY}}` | 대상 라벨 (없는 키는 **오류** — 빈 문자열로 조용히 넘어가지 않음) |
| `{{.Service}}` `{{.Version}}` `{{.PreviousVersion}}` `{{.DeploymentID}}` `{{.Phase}}` | 배포 컨텍스트 |
| `{{.Checkpoint.KEY}}` | `prepare`가 수집한 체크포인트 |
| `{{env "NAME"}}` | 환경변수 |

## 최상위 구조

```yaml
version: v1
server:      {...}   # API/저널
agent:       {...}   # 에이전트 동작
credentials: {...}   # 이름 → 자격증명 (값은 env/file 참조)
targets:     [...]   # 관측·제어 대상 (호스트, LB, 하이퍼바이저 호스트 등)
traffic:     {...}   # 이름 → 트래픽 제어기 (전략 C)
executors:   {...}   # 이름 → 롤백 전략 (A/B/D/범용)
services:    [...]   # 서비스 = 대상 + 프로브 + 규칙 + 단계 + 롤백 플랜
safety:      {...}   # 서킷브레이커·blast radius·플래핑·관측 쿼럼
notify:      [...]   # 알림
auth:        {...}   # API 인증(OIDC·서비스 계정)과 역할·범위
```

## `server`

| 키 | 기본값 | 설명 |
|---|---|---|
| `listen` | `:8088` | REST API 주소 |
| `auth_token_env` | — | Bearer 토큰 환경변수. 비우면 인증 비활성(개발용, 경고 로그) |
| `webhook_secret_env` | — | GitHub HMAC 서명 / GitLab `X-Gitlab-Token` 검증 비밀 |
| `journal_path` | `vigilante-journal.jsonl` | `state.backend: file`일 때의 WAL 저널. **CI 단발 실행 간 서킷/플래핑 이력 공유를 위해 공유 경로 권장**. 같은 디렉토리에 리스(락) 파일 생성 |
| `dry_run` | `false` | 모든 변경 액션을 로그로만 출력 (읽기 전용 명령·체크포인트는 실행) |
| `state.backend` | `file` | `file`(단일 노드·CI) \| `postgres`(여러 노드가 공유, HA 전제) |
| `state.dsn_env` / `state.dsn` | — | PostgreSQL 접속 문자열. 비밀번호가 들어가므로 `dsn_env` 권장. 스키마는 시작 시 자동 마이그레이션 |
| `ha.enabled` | `false` | 여러 `vigilante server` 노드 중 하나만 리더로 동작. `postgres` 필수 |
| `ha.advertise_url` | — | 다른 노드가 이 노드 API에 접근할 주소. 팔로워는 모든 API 요청을 리더의 이 주소로 전달 |
| `ha.node_id` | 호스트명 | 리스 기록에 남는 노드 이름 |
| `ha.lease_ttl` | `15s` | 리더 리스 유효시간(최소 3s). TTL/3마다 갱신. 리더가 죽으면 대략 TTL 안에 다른 노드가 이어받음 |

```yaml
server:
  listen: ":8088"
  state: {backend: postgres, dsn_env: VIGILANTE_PG_DSN}
  ha: {enabled: true, advertise_url: "https://vigilante-1.internal:8088", lease_ttl: 15s}
```

리더만 판정·롤백을 실행하고 상태를 기록합니다. 리더 자리를 잃은 노드의 기록은 DB에서 거부되므로(펜싱) 두 노드가 동시에 결정을 남기지 않습니다. 새 리더는 공유 상태를 다시 읽고, 진행 중이던 롤백을 완료된 단계부터 이어서 끝냅니다.

## `agent`

| 키 | 기본값 | 설명 |
|---|---|---|
| `push_interval` | `1s` | 샘플 일괄 전송 주기 (실패 시 최대 50,000개 버퍼 후 재전송) |
| `heartbeat_interval` | `5s` | 오케스트레이터 생존 확인 + 활성 배포 정보 수신 |
| `failsafe_after` | `30s` | 하트비트 단절 후 자율 판정 시작까지 |
| `failsafe` | `hold` | `hold`(로그·알림만) \| `rollback`(자기 호스트만, 트래픽 단계 제외 플랜으로 롤백) |

## `credentials.<name>`

| `type` | 사용 키 |
|---|---|
| `ssh` | `user`, `private_key_file`, `passphrase_env`, `password_env`, `use_ssh_agent`, `known_hosts_file`(기본 `~/.ssh/known_hosts`), `insecure_ignore_host_key` |
| `basic` | `username_env` (또는 `user`), `password_env` — F5, vCenter, Prism, 웹훅 |
| `token` | `token_env` — 웹훅 Bearer |
| `aws` | `region`, `profile` — 나머지는 AWS 기본 자격증명 체인(IAM Role 권장) |

## `targets[]`

```yaml
- name: order-bm-01          # 고유 이름 (규칙/단계/API에서 참조)
  kind: baremetal            # baremetal | vm | cloud_vm | container_host (정보성)
  address: 10.10.1.11
  labels: {instance_id: i-..., vcenter_vm: ...}   # 템플릿에서 사용
  connection:
    type: ssh                # ssh | local | none(API 전용 대상)
    credential: ssh-deploy
    port: 22
    bastion: bastion-dc1     # 다른 target을 점프 호스트로 (다단 가능)
    sudo: true               # 모든 명령을 sudo -n sh -c 로 감쌈
    timeout: 10s
```

## `services[].probes[]` — 수집 플러그인

공통: `id`(점 금지, 메트릭 접두어), `type`, `interval`(기본 2s), `timeout`(기본 1s). 메트릭 이름은 `<id>.<metric>`.

| type | 설정 | 생성 메트릭 | 비고 |
|---|---|---|---|
| `http` | `url`, `method`, `headers`, `expect_status[]`, `body_regex`, `json_path`, `json_expect`, `tls_skip_verify` | `up`, `latency_ms`, `status`, `consecutive_failures`, `consecutive_timeouts`, `timeout` | Spring actuator: `json_path: components.db.status`, `json_expect: UP` |
| `grpc` | `address`, `service`, `tls` | `up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts` | 표준 `grpc.health.v1` |
| `tcp` | `address` | `up`, `latency_ms`(connect), `consecutive_*` | |
| `host` | `devices[]`(생략 시 sd*/vd*/xvd*/nvme*/dm-*) | `load1`, `load_per_cpu`, `cpu_busy_pct`, `mem_available_pct`, `mem_available_mb`, `disk_util_pct`(최대 장치), `cpu_count`, `up` | `/proc` 1회 왕복. Linux 전용 |
| `docker` | `container`, `socket`(기본 `/var/run/docker.sock`) 또는 `host` | `running`, `restart_count`, `restarts`(관측 시작 후 증가분), `oom_killed`, `health_ok`, 이벤트: `oom_events`, `die_events`, `restart_events` | SSH 터널로 원격 소켓 접근. Podman 호환 소켓 지원 |
| `log` | `path`, `patterns{name: regex}` | 초당: `lines`, `match.<name>` | 원격 `tail -n0 -F`, 로컬은 로테이션(inode/truncate) 감지 |
| `access_log` | `path`, `format`(combined\|json), `status_field`, `latency_field`, `latency_unit`(s\|ms) | 초당: `requests`, `count_5xx`, `count_4xx`, `error_rate_5xx`(%), 요청별 `latency_ms`(초당 256개 샘플링), `unparsed` | combined 뒤의 `$request_time` 자동 인식 |
| `db` | `driver`(postgres\|mysql), `dsn` 또는 `dsn_env`, `pool_size`(기본 3), `query`(기본 `SELECT 1`) | `up`, `pool_acquired`, `pool_acquire_ms`, `query_ms`, `consecutive_*` | `pool_size`개 커넥션을 **동시에** 확보 → 풀 고갈/`max_connections` 문제 검출 |

모든 프로브는 프로세스가 죽거나 시작 실패 시 `<id>.probe_error`를 남기고 지수 백오프로 재시작됩니다.

## `services[].preset` — 규칙 프리셋

프리셋 이름 하나와 몇 개의 값만으로 프로브·롤백 규칙·기준선·단계 시간을 채웁니다. 규칙을 직접 쓰지 않아도 검증된 기준으로 시작할 수 있습니다.

```yaml
services:
  - name: order-api
    targets: [order-bm-01, order-bm-02]
    preset: java-web@1          # 버전 고정 권장: 프리셋이 새 버전으로 바뀌어도 이 서비스의 판정 기준은 그대로
    overrides:
      health_url: "http://{{.Address}}:8080/actuator/health"
      access_log: /var/log/order/access.log
      app_log: /var/log/order/application.log
      error_rate_pct: 3         # 바꾸고 싶은 값만
    rollback: {executor: order-symlink, traffic: nginx-edge}
```

| 내장 프리셋 | 대상 | 필수 값 |
|---|---|---|
| `java-web` | Java/Spring 웹(베어메탈·VM): 헬스, 5xx 비율, OOM·예외·커넥션 풀, 서버 자원 | `health_url`, `access_log`, `app_log` |
| `container-api` | VM 위 단독 컨테이너: 재시작·OOM·중지, 헬스, 선택적 5xx | `container`, `health_url` |
| `static-web` | 정적 웹·프록시: 헬스, 5xx 비율, 지연 | `health_url`, `access_log` |
| `worker` | 배치·큐 워커: 앱 로그 OOM·오류율, 메모리, 선택적 컨테이너 | `app_log` |

- 전체 파라미터와 기본값: `vigilante presets`. 실제로 펼쳐지는 내용: `vigilante presets show java-web --set error_rate_pct=3`.
- 서비스에 직접 쓴 프로브·규칙은 같은 id·이름의 프리셋 항목을 대체하고, 나머지는 프리셋 것을 씁니다. 단계(`phases`)는 항목별로 합쳐져서, `canary: {targets: [...]}`만 써도 관측 시간과 warmup은 프리셋 값을 유지합니다.
- 알 수 없는 override 키, 빠진 필수 값은 `validate`에서 오류입니다.
- **조직 프리셋:** 최상위 `preset_dirs: [presets]`(설정 파일 기준 상대경로)에 같은 형식의 파일을 두면 `preset: corp/java-web@2`처럼 씁니다. 파일 형식은 `internal/presets/builtin/*.yaml`을 참고하십시오: YAML 헤더(name, version, description, params) + `---` + `[[ ]]` 템플릿 본문입니다. `[[ ]]`를 쓰기 때문에 `{{.Address}}` 같은 실행 시 템플릿은 그대로 남습니다.

## `services[].rules[]` — 복합 롤백 규칙

서비스마다 `action: rollback` 규칙이 **1개 이상 필수**입니다. 없으면 어떤 배포도 실패할 수 없어 모두 PASS가 되므로 `validate`가 거부합니다. 단계의 `rules`로 규칙을 골라 쓸 때도 그중 하나는 rollback 규칙이어야 합니다.

```yaml
rules:
  - name: fatal-errors
    action: rollback              # rollback | notify(경고만) | hold(사람 판단)
    when:                         # 트리: any / all / not / leaf
      any:
        - {metric: access.count_5xx, ratio_of: access.requests, window: 30s, op: ">", value: 2, for: 2}
        - {metric: applog.match.exception, agg: rate, window: 10s, op: ">", value: 10}
        - {metric: health.consecutive_timeouts, op: ">=", value: 3}
  - name: latency-regression
    when: {metric: health.latency_ms, agg: p99, window: 1m, op: ">", baseline: {increase_pct: 200}, min_value: 300, for: 3, reset_after: 2}
```

Leaf 조건 필드

| 키 | 기본값 | 설명 |
|---|---|---|
| `metric` | (필수) | `<probe-id>.<metric>` |
| `agg` | `last` (ratio_of 사용 시 `sum`) | `last avg min max sum count rate p50 p90 p95 p99` — `rate` = 합계 ÷ 윈도우 초 |
| `window` | `30s` | 집계 구간 |
| `ratio_of` | — | 값 = `sum(metric) / sum(ratio_of) × 100` (**가중** 비율. 트래픽 0이면 unknown) |
| `op` | `>` | `> >= < <= == !=` |
| `value` | — | 절대 임계치 (`value`/`baseline` 중 하나) |
| `baseline.increase_pct` | — | `>`/`>=`: 값 > 기준×(1+pct/100). `<`/`<=`: 값 < 기준×(1-pct/100) (처리량 급감 탐지) |
| `min_value` | — | baseline 비교 시 절대 하한. 2ms→6ms 같은 "300% 증가" 오탐 방지 |
| `for` | `1` | **연속 N회** 위반해야 참 (일시적 튐 방지) |
| `reset_after` | `1` | 연속 N회 정상이어야 카운터 초기화 (히스테리시스: 위반-정상-위반 패턴도 누적) |
| `scope` | `target` | `target`(대상별) \| `service`(배포된 대상 전체 합산) |
| `absent` | `unknown` | 데이터 없음 처리: `unknown`(증거 부족) \| `breach`(없으면 위반) \| `ok` |

평가는 3값 논리입니다: `any` = 하나라도 참이면 참, 아니면 unknown이 있으면 unknown. `all` = 하나라도 거짓이면 거짓. 카운터 일관성을 위해 **단락 평가를 하지 않습니다.**

## `services[].phases` — 단계별 관측

```yaml
phases:
  canary:  {targets: [order-bm-01], observation_window: 10m, warmup: 60s, eval_interval: 5s, min_samples: 200, on_inconclusive: hold}
  rolling: {percent: 50, observation_window: 15m}
  full:    {observation_window: 20m, rules: [fatal-errors, latency-regression]}
```

| 키 | 기본값 | 설명 |
|---|---|---|
| `targets` / `percent` | canary=첫 대상, rolling=50%, full=전체 | 이 단계에 새 버전이 깔린 대상. **누적**(rolling은 canary 포함) |
| `observation_window` | `5m` | 이 시간 동안 위반이 없으면 PASS. 위반 시 즉시 FAIL |
| `warmup` | `0s` | 판정 보류 구간(JVM 워밍업, 캐시·풀 채우기). 수집은 계속 |
| `eval_interval` | `5s` | 판정 주기 (`for` 카운트 단위) |
| `rules` | 전체 | 이 단계에 적용할 규칙 이름 부분집합 |
| `min_samples` | `0` | 윈도우 동안 최소 샘플 수. 미달 → INCONCLUSIVE |
| `on_inconclusive` | `hold` | `hold`(exit 4) \| `pass` \| `rollback` |

`control_targets: [auto]` (서비스 레벨)는 "아직 배포되지 않은 대상"을 **라이브 대조군**으로 씁니다. 대조군에서도 같은 규칙이 터지면 환경 문제(공유 DB 장애 등)로 보고 HOLD 합니다. 대조군은 baseline 조건의 실시간 기준값으로도 쓰입니다(사전 baseline 파일보다 우선).

## `services[].rollback` — 롤백 플랜

```yaml
rollback:
  executor: order-symlink          # 1차 전략
  traffic: nginx-edge              # 선택: 트래픽 제어기
  scope: deployed                  # deployed(이번 단계까지 배포된 전 대상) | failed(위반 대상만)
  parallelism: 2                   # blast-radius 허용치와 min() 으로 배치 크기 결정
  step_timeout: 2m
  retry: {attempts: 3, backoff: 2s, max_backoff: 30s}
  plan:                            # 생략 시: [drain]→rollback→verify→[enable]
    - action: traffic.drain
    - action: app.rollback
    - action: app.verify
    - {action: probe.verify, probe: health, successes: 3, timeout: 90s}
    - action: traffic.enable
  escalation:                      # app.* 단계 최종 실패 시 순서대로 시도
    - {executor: legacy-kvm-snapshot, require_approval: true}
```

| action | 동작 |
|---|---|
| `traffic.drain` | 대상을 풀에서 제외 후 `drain_wait` 대기. 실패해도 롤백은 진행(제자리 롤백) |
| `app.rollback` | 실행기 `Rollback()` (멱등) |
| `app.verify` | 실행기 `Verify()` — 링크 대상/이미지 태그/VM 전원 상태 등 **버전 사실 확인** |
| `probe.verify` | 지정 프로브의 `Check()`가 연속 `successes`회 성공해야 통과 (**헬스 확인**) |
| `traffic.enable` | 풀 복귀. 앞 단계가 실패하면 실행되지 않음 → 대상은 **격리 상태 유지** |
| `wait` | `duration` 대기 |

## `executors.<name>` — 롤백 전략

| type | 전략 | 주요 키 | prepare 체크포인트 |
|---|---|---|---|
| `symlink` | A. 디렉토리 전환 | `link`, `releases_dir`, `release`(기본 `{{.PreviousVersion}}`), `init`(systemd\|sysv\|none), `unit`, `restart_cmd`, `atomic`(기본 true: `ln -sfn tmp && mv -Tf`; AIX/Solaris는 false) | `symlink.previous` (배포 전 실제 링크 대상) |
| `container` | B. 컨테이너 전환 | `name`, `socket`/`host`, `image_repo`(기본: 현재 이미지 repo), `tag`(기본 `{{.PreviousVersion}}`), `pull`, `stop_timeout_sec` | `container.previous_image` |
| `vsphere` | D. VM 스냅샷 | `url`, `credential`, `vm`, `snapshot`, `power_on`, `tls_skip_verify` | 스냅샷 생성 (`vsphere.snapshot`) |
| `nutanix` | D. VM 스냅샷 | `url`, `credential`, `vm_uuid`, `snapshot` | 스냅샷 생성 (`nutanix.snapshot_uuid`) |
| `kvm` | D. VM 스냅샷 | `hypervisor`(target), `domain`, `snapshot` | `virsh snapshot-create-as --atomic` |
| `exec` | 범용 | `prepare`, `rollback`, `verify`, `on`(target\|local\|다른 target) | stdout |
| `webhook` | 범용(사내 배포 콘솔) | `url`, `method`, `headers`, `body`, `verify_url`, `credential` | — |

## `traffic.<name>` — 트래픽 제어 (전략 C)

| type | 방식 | 주요 키 |
|---|---|---|
| `nginx` | upstream include 파일의 `server ... down;` 토글 → `nginx -t` 실패 시 백업 복원 → reload. HA 쌍 모두 적용 | `hosts[]`, `upstream_file`, `member_format`, `test_cmd`, `reload_cmd` |
| `haproxy` | Runtime API `set server B/S state drain\|maint\|ready` (소켓은 SSH 터널 또는 TCP) | `hosts[]`, `socket` 또는 `address`, `backend`, `server_name`, `drain_to_maint` |
| `envoy` | 파일 기반 EDS의 `health_status: DRAINING` + 원자적 `mv` | `hosts[]`, `eds_file`, `member_format` |
| `f5` | iControl REST `PATCH .../pool/~P~pool/members/~P~ip:port` (`session: user-disabled`[, `state: user-down`]) | `url`, `credential`, `pool`, `member_format`, `force_offline`, `token_auth` |
| `aws_alb` | `DeregisterTargets` → draining 완료 대기 / `RegisterTargets` → healthy 대기 | `credential`, `target_group_arn`, `target_id`(기본 `{{.Labels.instance_id}}`), `port`, `wait_timeout` |

공통: `drain_wait`(드레인 후 대기).

## `safety`

```yaml
safety:
  observer_quorum: true
  circuit_breaker: {failure_threshold: 2, window: 1h, open_duration: 0s}
  blast_radius:    {min_healthy: 1, min_healthy_percent: 50}
  flapping:        {max_rollbacks_per_hour: 3, cooldown: 5m}
```

상세 동작은 [04-safety-circuit-breaker.md](04-safety-circuit-breaker.md).

## `auth` — 인증과 권한

API 호출자는 Bearer 토큰을 보냅니다. 세 종류를 받습니다.

| 토큰 | 용도 | 신원 |
|---|---|---|
| 서비스 계정 토큰 (`vgl_…`) | CI 잡, 에이전트 | `sa:<name>`. 설정에는 SHA-256 해시만 저장. `vigilante token create`로 발급 |
| OIDC JWT | 사람(사내 SSO: Keycloak, Azure AD, Okta 등) | `user:<preferred_username>`. 그룹을 역할에 매핑 |
| `server.auth_token_env`의 토큰 | 비상용(break-glass) | `token:legacy`, admin. 평소에는 비워 두기를 권장 |

아무것도 설정하지 않으면 인증이 꺼집니다(개발용, 시작 시 경고, 모든 작업이 `anonymous`로 기록).

**역할** (아래로 갈수록 상위 권한 포함)

| 역할 | 할 수 있는 일 |
|---|---|
| `viewer` | 배포·서킷·지표 조회 |
| `deployer` | 배포 생성, 단계 관측 시작, 중단, 기준선 측정, `mark-good` |
| `operator` | 수동 롤백, 승인 대기 에스컬레이션 승인 |
| `admin` | 서킷 리셋·차단, 그 외 전부 |
| `agent` | 에이전트 전용: 샘플 전송, 하트비트만 (다른 역할과 별개) |

**범위(scope)**: `*`(전체, 기본) · `team=<팀>`(서비스의 `team` 값과 일치) · `service=<이름>`. 서킷 리셋처럼 특정 서비스에 속하지 않는 작업은 `*` 범위가 필요합니다. 목록 조회는 권한 있는 서비스만 돌려줍니다.

```yaml
services:
  - name: order-api
    team: payments            # team= 범위가 이 값과 맞춰짐
    ...
auth:
  oidc:
    issuer: https://sso.example.internal/realms/ops
    audience: vigilante       # 토큰의 aud(client ID)
    groups_claim: groups      # 기본 groups
    username_claim: preferred_username
  role_bindings:
    - {group: sre-oncall, role: operator}
    - {group: platform-admins, role: admin}
    - {group: payments-dev, role: deployer, scope: team=payments}
    - {user: alice, role: viewer}
  service_accounts:
    - name: ci-order-api
      token_sha256: fb4afd06a1bdd94f9f3febfaa8b22a2d3fd2db9d10bf0c51ab3a75ba8e9fd8a9
      expires: 2027-06-30     # 이 날짜까지 유효
      roles: [{role: deployer, scope: "service=order-api"}]
    - name: agent-fleet
      token_sha256: 0f1e...   # (64자 hex)
      roles: [{role: agent}]
  four_eyes: true             # 배포 생성자·롤백 요청자는 그 승인 요청을 직접 승인할 수 없음
```

- 토큰 폐기: 서비스 계정 항목을 지우고 설정을 다시 읽히면 즉시 무효가 됩니다. 만료일을 두는 것을 권장합니다.
- 확인: `vigilante whoami --server URL` (환경변수 `VIGILANTE_TOKEN`의 신원과 권한 출력).
- 모든 생성·롤백·승인 기록에 작업자(`created_by`, `rollback_requested_by`, `approved_by`)가 남습니다. 로컬 CLI 실행은 `cli:<OS 사용자>@<호스트>`로 기록됩니다.
- OIDC를 설정하면 서버 시작 시 발급자(issuer)의 discovery 문서를 가져오므로 서버에서 SSO에 접근할 수 있어야 합니다.

## `notify[]`

| 키 | 설명 |
|---|---|
| `type` | `slack`(incoming webhook 텍스트) \| `webhook`(배포 JSON 전체) |
| `url` / `url_env` | 대상 URL |
| `min_level` | `info` \| `warning` \| `critical` |
