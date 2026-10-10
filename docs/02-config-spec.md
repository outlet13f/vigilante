# 02. 설정 파일 명세 (`vigilante.yaml`)

완전한 예시: [`examples/config/vigilante.yaml`](../examples/config/vigilante.yaml) — 베어메탈(Nginx+F5), vSphere VM 위 Docker(HAProxy), EC2(ALB), OpenStack VM(Octavia) 4개 서비스를 모두 담고 있습니다.

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
audit:       {...}   # SIEM 전송(syslog)과 보존 기간
secrets:     {...}   # *_ref 비밀값 출처(HashiCorp Vault)와 캐시
api:         {...}   # 오픈 API 호출 한도와 OAuth 토큰 수명
change_freeze: [...] # 변경 동결 기간 (새 배포 거부)
itsm:        {...}   # ServiceNow 변경 티켓 게이트·인시던트·작업 노트
console:     {...}   # 웹 운영 콘솔 로그인(OIDC)과 세션
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
| `state.dsn_ref` / `state.dsn_env` / `state.dsn` | — | PostgreSQL 접속 문자열. 비밀번호가 들어가므로 `dsn_ref`(Vault) 또는 `dsn_env` 권장. 스키마는 시작 시 자동 마이그레이션 |
| `state.auto_migrate` | `true` | 시작할 때 스키마 마이그레이션 적용. `false`면 적용 대기 마이그레이션이 있을 때 시작을 거부하므로 `vigilante store migrate`로 직접 적용(docs/08-upgrade.md) |
| `ha.enabled` | `false` | 여러 `vigilante server` 노드 중 하나만 리더로 동작. `postgres` 필수 |
| `ha.advertise_url` | — | 다른 노드가 이 노드 API에 접근할 주소. 팔로워는 모든 API 요청을 리더의 이 주소로 전달. 환경변수 `VIGILANTE_HA_ADVERTISE_URL`이 있으면 그 값을 씀(여러 노드가 설정 파일 하나를 공유할 때, 예: Helm 차트) |
| `ha.node_id` | 호스트명 | 리스 기록에 남는 노드 이름. 환경변수 `VIGILANTE_HA_NODE_ID`가 우선 |
| `ha.lease_ttl` | `15s` | 리더 리스 유효시간(최소 3s). TTL/3마다 갱신. 리더가 죽으면 대략 TTL 안에 다른 노드가 이어받음 |
| `metrics_public` | `false` | `/metrics`를 인증 없이 제공. 기본은 전체 범위 viewer 토큰(`viewer@*`) 필요 |
| `tls.cert_file` / `tls.key_file` | — | 있으면 HTTPS로 직접 서비스(TLS 1.2 이상). 파일이 바뀌면 재시작 없이 새 인증서를 씀(cert-manager·갱신 작업). 없으면 HTTP이므로 앞에 TLS 프록시·인그레스를 둠 |
| `tls.client_ca_file` / `tls.client_auth` | —, `none` | 클라이언트 인증서 검증. `optional`: 제시된 인증서만 검증(에이전트는 인증서, 브라우저·CI는 토큰), `require`: 모든 클라이언트에 인증서 요구. 어느 경우든 API 토큰 인증은 그대로 |
| `tls.min_version` | `1.2` | `1.3`으로 올릴 수 있음 |

```yaml
server:
  listen: ":8088"
  state: {backend: postgres, dsn_env: VIGILANTE_PG_DSN}
  ha: {enabled: true, advertise_url: "https://vigilante-1.internal:8088", lease_ttl: 15s}
```

리더만 판정·롤백을 실행하고 상태를 기록합니다. 리더 자리를 잃은 노드의 기록은 DB에서 거부되므로(펜싱) 두 노드가 동시에 결정을 남기지 않습니다. 새 리더는 공유 상태를 다시 읽고, 진행 중이던 롤백을 완료된 단계부터 이어서 끝냅니다.

### 자체 관측성

세 엔드포인트는 각 노드가 직접 답하며 리더로 전달하지 않습니다.

| 엔드포인트 | 용도 | 응답 |
|---|---|---|
| `GET /healthz` | 생존(liveness). 프로세스가 응답하면 200 | 역할, 리더, 서킷, 저장소 |
| `GET /readyz` | 준비(readiness). 로드밸런서·Kubernetes가 트래픽을 보낼지 판단 | 저장소 Ping 실패 또는 HA에서 리더를 모르면 503. `{"ready":..,"checks":{"store":..,"leader":..}}` |
| `GET /metrics` | Prometheus 텍스트 형식(0.0.4). 외부 수집기가 가져가기만 함 | 아래 지표 |

| 지표 | 종류 | 레이블 | 의미 |
|---|---|---|---|
| `vigilante_rollback_trigger_seconds` | histogram | — | 실패 판정부터 롤백 시작 기록까지(게이트·락·저장 포함) |
| `vigilante_rollbacks_total` | counter | service, result | `rolled_back`, `failed`, `await_approval`, `blocked`, `handed_over` |
| `vigilante_rollback_duration_seconds` | histogram | result | 롤백 플랜 실행 시간 |
| `vigilante_verdicts_total` | counter | service, phase, verdict | 단계 판정 결과 |
| `vigilante_evaluation_seconds` | histogram | — | 한 번의 규칙 평가 시간 |
| `vigilante_probe_samples_total` / `vigilante_probe_restarts_total` | counter | type | 수집 샘플 수 / 오류로 재시작한 프로브 수 |
| `vigilante_agent_samples_total` | counter | — | 에이전트가 보낸 샘플 수 |
| `vigilante_circuit_state` | gauge | state | 현재 서킷 상태가 1 |
| `vigilante_leader` / `vigilante_engine_active` | gauge | — | 리더 여부 / 판정·롤백 가능 여부(펜싱되면 0) |
| `vigilante_deployments` | gauge | state | 상태별 배포 수 |
| `vigilante_agents_connected` | gauge | — | 30초 안에 하트비트를 보낸 에이전트 수 |
| `vigilante_store_append_seconds` / `vigilante_store_errors_total` | histogram / counter | backend / reason | 상태 저장 지연 / 실패(`fenced`, `error`) |
| `vigilante_ssh_connections` / `vigilante_ssh_sessions` / `vigilante_ssh_dials_total` | gauge / gauge / counter | — / — / result | SSH 풀 연결 수, 열린 세션 수, 접속 시도 |
| `vigilante_api_requests_total` / `vigilante_api_request_seconds` | counter / histogram | method, route, code / route | API 요청 수와 지연 |
| `vigilante_audit_exported_total` / `vigilante_audit_export_dropped_total` | counter | — | SIEM 전송 / 유실 |
| `vigilante_build_info`, `process_start_time_seconds`, `go_goroutines`, `go_memstats_heap_alloc_bytes` | gauge | version, go_version | 빌드·프로세스 정보 |

레이블에는 대상 이름이나 배포 ID를 넣지 않습니다. 대상 수천 대에서도 시계열 수가 서비스 수에 비례하게 유지됩니다.

**로그:** `VIGILANTE_LOG_FORMAT=json`이면 한 줄에 JSON 객체 하나로 출력합니다(기본 text). `VIGILANTE_LOG=debug|warn`으로 수준을 바꿉니다. 배포 관련 로그에는 `deployment`, API 요청 로그에는 `request_id`가 붙습니다. `request_id`는 요청의 `X-Request-ID`를 쓰고, 없으면 W3C `traceparent`의 trace id, 그것도 없으면 새로 만들어 응답 헤더로 돌려줍니다. 팔로워가 리더로 전달할 때 같은 값을 넘기므로 두 노드의 로그를 하나의 요청으로 묶을 수 있습니다. 변경 요청과 실패한 요청은 info, 성공한 조회는 debug로 남습니다.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: vigilante
    authorization: {credentials_file: /etc/prometheus/vigilante-token}   # viewer@* 서비스 계정
    static_configs: [{targets: ["vigilante-1:8088", "vigilante-2:8088"]}]
```

## `agent`

| 키 | 기본값 | 설명 |
|---|---|---|
| `push_interval` | `1s` | 샘플 일괄 전송 주기 (실패 시 최대 50,000개 버퍼 후 재전송) |
| `heartbeat_interval` | `5s` | 오케스트레이터 생존 확인 + 활성 배포 정보 수신 |
| `failsafe_after` | `30s` | 하트비트 단절 후 자율 판정 시작까지 |
| `failsafe` | `hold` | `hold`(로그·알림만) \| `rollback`(자기 호스트만, 트래픽 단계 제외 플랜으로 롤백) |
| `tls.ca_file` | 시스템 신뢰 저장소 | 서버 인증서를 발급한 사설 CA |
| `tls.cert_file` / `tls.key_file` | — | 서버가 `client_auth: optional`·`require`일 때 제시할 에이전트 인증서. 파일이 바뀌면 다시 읽음 |

## `credentials.<name>`

| `type` | 사용 키 |
|---|---|
| `ssh` | `user`, `ssh_ca`(권장), `private_key_ref` 또는 `private_key_file`, `passphrase_ref`/`passphrase_env`, `password_ref`/`password_env`, `use_ssh_agent`, `known_hosts_file`(기본 `~/.ssh/known_hosts`), `insecure_ignore_host_key` |
| `basic` | `username_ref`/`username_env` (또는 `user`), `password_ref`/`password_env` — F5, vCenter, Prism, 웹훅 |
| `token` | `token_ref`/`token_env` — 웹훅 Bearer |
| `aws` | `region`, `profile` — 나머지는 AWS 기본 자격증명 체인(IAM Role 권장) |

`*_ref`는 비밀값 참조입니다. `*_env`보다 우선하며 형식은 세 가지입니다.

| 참조 | 예 | 값의 출처 |
|---|---|---|
| `vault:<mount>/<path>#<key>` | `vault:secret/prod/f5#password` | Vault KV v2 (`secrets.vault` 필요) |
| `env:NAME` | `env:F5_PASSWORD` | 환경변수 |
| `file:/path` | `file:/run/secrets/f5-password` | 파일 내용(Kubernetes Secret 마운트 등). 끝 줄바꿈 제거 |

### `ssh_ca` — 단기 SSH 인증서

장기 개인키를 대상 서버마다 배포하는 대신, Vault SSH CA가 접속할 때마다 단기 인증서를 발급합니다.

```yaml
credentials:
  ssh-deploy:
    type: ssh
    user: deploy
    ssh_ca: {mount: ssh-client-signer, role: vigilante, ttl: 30m}   # principals 기본값: [user]
```

- vigilante는 메모리에서 일회용 ed25519 키를 만들고 공개키만 Vault `POST <mount>/sign/<role>`로 보내 서명받습니다. 개인키는 디스크에 남지 않습니다.
- 인증서는 수명의 80%가 지나면 새 키로 다시 발급합니다. 그 전까지 새 연결은 같은 인증서를 씁니다.
- 대상 서버 sshd는 CA 공개키만 신뢰하면 됩니다(`TrustedUserCAKeys`). 인증서의 principal이 로그인 사용자와 같아야 합니다.
- `ssh_ca`와 개인키를 함께 지정하면 인증서를 먼저 시도합니다.

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
    sudo: true               # 변경 명령에 sudo 사용
    sudo_scope: changes      # changes: 변경 명령만 하나씩 sudo -n (권장) | all(기본): 모든 명령을 sudo -n sh -c 로 감쌈
    timeout: 10s
    max_sessions: 8          # 이 대상에 동시에 여는 SSH 세션 상한 (sshd MaxSessions 기본 10보다 작게)
    reserved_sessions: 2     # 그중 롤백·트래픽 변경만 쓰는 몫
```

- **`sudo_scope`:** `all`(기존 동작, 기본값)은 모든 명령을 `sudo -n sh -c '...'`로 실행하므로 sudoers에 무제한 권한이 필요하고, `validate`가 경고합니다. `changes`는 읽기(`readlink`, `cat`, `tail`, `systemctl is-active`, `virsh domstate` 등)를 sudo 없이 실행하고, 바꾸는 명령(`ln`, `mv`, `systemctl restart`, `nginx -s reload`, `virsh snapshot-revert` 등)에만 하나씩 `sudo -n`을 붙입니다. 필요한 sudoers 규칙은 `vigilante sudoers`가 대상별로 만들어 주고, `vigilante doctor`가 규칙마다 `sudo -n -l`로 허용 여부를 확인합니다(실행하지 않음). 직접 쓴 명령(`exec` 실행기, `restart_cmd`, 바꾼 `test_cmd`·`reload_cmd`)은 쓴 그대로 실행되므로 필요하면 명령 안에 `sudo -n`을 넣고 규칙을 직접 추가합니다.
- **SSH 세션 예산:** 로그 스트림은 실행되는 동안 세션을 하나씩 쥡니다. 세션이 모자라 롤백 명령이 막히지 않도록, 수집은 `max_sessions - reserved_sessions`까지만 쓰고 기다리며, 롤백 단계와 트래픽 드레인·복귀만 예약분을 씁니다. 대기는 `vigilante_ssh_session_waits_total{priority}`로 보이고, `doctor`가 로그 스트림 수와 수집 몫을 비교합니다.

## `services[].probes[]` — 수집 플러그인

공통: `id`(점 금지, 메트릭 접두어), `type`, `interval`(기본 2s), `timeout`(기본 1s). 메트릭 이름은 `<id>.<metric>`.

| type | 설정 | 생성 메트릭 | 비고 |
|---|---|---|---|
| `http` | `url`, `method`, `headers`, `expect_status[]`, `body_regex`, `json_path`, `json_expect`, `tls_skip_verify` | `up`, `latency_ms`, `status`, `consecutive_failures`, `consecutive_timeouts`, `timeout` | Spring actuator: `json_path: components.db.status`, `json_expect: UP` |
| `grpc` | `address`, `service`, `tls` | `up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts` | 표준 `grpc.health.v1` |
| `tcp` | `address` | `up`, `latency_ms`(connect), `consecutive_*` | |
| `host` | `devices[]`(생략 시 sd*/vd*/xvd*/nvme*/dm-*) | `load1`, `load_per_cpu`, `cpu_busy_pct`, `mem_available_pct`, `mem_available_mb`, `disk_util_pct`(최대 장치), `cpu_count`, `up` | `/proc` 1회 왕복. Linux 전용 |
| `docker` | `container`, `socket`(기본 `/var/run/docker.sock`) 또는 `host` | `running`, `restart_count`, `restarts`(관측 시작 후 증가분), `oom_killed`, `health_ok`, 이벤트: `oom_events`, `die_events`, `restart_events` | SSH 터널로 원격 소켓 접근. Podman 호환 소켓 지원 |
| `log` | `path`, `patterns{name: regex}`, `remote_grep`(선택, `grep -E` 식) | 초당: `lines`, `match.<name>` | 원격 `tail -n0 -F`, 로컬은 로테이션(inode/truncate) 감지. `remote_grep`이 있으면 대상에서 `grep --line-buffered -E`로 먼저 걸러 맞는 줄만 SSH로 보냄(대용량 로그). 이때 `lines`는 없으며 이를 쓰는 규칙은 `validate`가 거부. 대상 grep이 식과 `--line-buffered`를 받는지 `doctor`가 확인 |
| `access_log` | `path`, `format`(combined\|json), `status_field`, `latency_field`, `latency_unit`(s\|ms) | 초당: `requests`, `count_5xx`, `count_4xx`, `error_rate_5xx`(%), 요청별 `latency_ms`(초당 256개 샘플링), `unparsed` | combined 뒤의 `$request_time` 자동 인식 |
| `db` | `driver`(postgres\|mysql), `dsn` 또는 `dsn_env`, `pool_size`(기본 3), `query`(기본 `SELECT 1`), `pool_check_interval`(기본 `1m`) | `up`, `query_ms`, `pool_acquired`·`pool_acquire_ms`(전체 점검 때), `consecutive_*` | 매 주기는 열어 둔 커넥션 1개로 쿼리. `pool_check_interval`마다 `pool_size`개 새 커넥션을 **동시에** 확보 → 풀 고갈/`max_connections` 문제 검출. `0s`면 매 주기 전체 점검(이전 동작) |

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
  mode: approve                    # auto | approve. 생략하면 auto(기존 동작)이고 validate가 경고
  approval: {timeout: 30m, on_timeout: hold, drain_first: true}
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

### 롤백 모드 (`mode`)

| 모드 | 단계가 FAIL이면 |
|---|---|
| `auto` | 즉시 롤백 플랜을 실행합니다. 생략하면 이 모드이며, `vigilante validate`가 명시하라고 경고합니다 |
| `approve` | 롤백 대상·이유·만료 시각을 담은 계획(`pending_rollback`)을 만들고 `AWAITING_APPROVAL`(CI 종료 코드 3)로 멈춥니다. 콘솔, API(`POST /v2/deployments/{id}/approvals`), CLI(`vigilante rollback --id ID --approve` 또는 `--reject`)로 결정합니다 |

`approve` 모드의 세부 동작:

- **승인:** 준비한 계획대로 롤백합니다. 사람의 결정이므로 서킷브레이커와 플래핑 제한을 거치지 않습니다(수동 롤백과 같음). 승인자는 `approved_by`에 남습니다.
- **거절:** 새 버전을 유지하고, `drain_first`로 빼 둔 대상을 다시 트래픽에 넣은 뒤 `HELD`로 둡니다. 오탐을 판정에서 걸러 내는 경로입니다.
- **`drain_first: true`:** 결정을 기다리는 동안 위반한 대상을 트래픽에서 뺍니다(blast radius 적용). `rollback.traffic`이 필요합니다.
- **`timeout`(기본 30m)과 `on_timeout`:** `hold`(기본)면 계속 기다리며 운영자에게 한 번 더 상위 호출하고, 그 뒤에도 승인할 수 있습니다. `rollback`이면 자동 롤백으로 넘어가며 이때는 서킷브레이커·플래핑 제한이 적용됩니다. 만료 처리는 서버(리더)가 15초마다 합니다. CI 단발 실행만 쓰는 경우에는 만료 처리가 없습니다.
- **4-eyes(`auth.four_eyes`):** 배포를 만든 사람이나 롤백을 요청한 사람은 승인·거절할 수 없습니다.
- **에이전트 failsafe:** `approve` 모드 서비스에서는 `agent.failsafe: rollback`이어도 에이전트가 혼자 롤백하지 않고 보류합니다.
- 이벤트: `vigilante.approval.requested`(계획 생성), `vigilante.approval.decided`(`decision: approved|rejected`). 지표: `vigilante_rollback_approvals_total{decision}`.

## `executors.<name>` — 롤백 전략

| type | 전략 | 주요 키 | prepare 체크포인트 |
|---|---|---|---|
| `symlink` | A. 디렉토리 전환 | `link`, `releases_dir`, `release`(기본 `{{.PreviousVersion}}`), `init`(systemd\|sysv\|none), `unit`, `restart_cmd`, `atomic`(기본 true: `ln -sfn tmp && mv -Tf`; AIX/Solaris는 false) | `symlink.previous` (배포 전 실제 링크 대상) |
| `container` | B. 컨테이너 전환 | `name`, `socket`/`host`, `image_repo`(기본: 현재 이미지 repo), `tag`(기본 `{{.PreviousVersion}}`), `pull`, `stop_timeout_sec` | `container.previous_image` |
| `vsphere` | D. VM 스냅샷 | `url`, `credential`, `vm`, `snapshot`, `power_on`, `tls_skip_verify` | 스냅샷 생성 (`vsphere.snapshot`) |
| `nutanix` | D. VM 스냅샷 | `url`, `credential`, `vm_uuid`, `snapshot` | 스냅샷 생성 (`nutanix.snapshot_uuid`) |
| `kvm` | D. VM 스냅샷 | `hypervisor`(target), `domain`, `snapshot` | `virsh snapshot-create-as --atomic` |
| `openstack` **(실험적: M8 실장비 검증 전)** | D. 인스턴스 스냅샷 | `credential`(type `openstack`), `server_id`(기본 `{{.Labels.openstack_server_id}}`), `mode`(auto\|volume\|image), `snapshot`(이름), `revert_timeout`(기본 15m), `power_on`(기본 true), `keep_snapshots`(기본 3) | 볼륨 부팅: Cinder 볼륨 스냅샷(`openstack.volume_snapshot_id`) / 이미지 부팅: Nova 서버 스냅샷(`openstack.image_id`) |
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
| `octavia` **(실험적: M8 실장비 검증 전)** | Octavia v2 풀 멤버 `admin_state_up=false`(드레인) → LB `provisioning_status` ACTIVE 대기 / `admin_state_up=true` → 멤버 `operating_status` ONLINE 대기. `PENDING_*`·409는 재시도 | `credential`(type `openstack`), `pool_id`, `member_address`(기본 `{{.Address}}`), `member_port`, `wait_timeout` |

공통: `drain_wait`(드레인 후 대기).

### OpenStack

```yaml
credentials:
  openstack-prod:
    type: openstack
    auth_url: https://keystone.example.internal:5000/v3
    region: RegionOne
    interface: internal                 # 카탈로그에서 쓸 엔드포인트: public(기본) | internal | admin
    application_credential_id: 4f6c...  # 권장: 프로젝트 범위 고정, 사용자 비밀번호 불필요
    application_credential_secret_ref: "vault:secret/prod/openstack#app_cred_secret"
    cacert: /etc/vigilante/openstack-ca.pem
    # 대안: user + password_ref + project_name(또는 project_id) [+ user_domain_name, project_domain_name, 기본 Default]
targets:
  - {name: order-os-01, address: 10.20.0.11, labels: {openstack_server_id: 6a1b...}, connection: {type: ssh, credential: ssh-deploy}}
executors:
  os-snap: {type: openstack, openstack: {credential: openstack-prod}}
traffic:
  order-lb: {type: octavia, octavia: {credential: openstack-prod, pool_id: 9c2e..., member_port: 8080}}
```

| 부팅 방식 | prepare | rollback | verify |
|---|---|---|---|
| 볼륨 부팅 (`image`가 비어 있음) | 루트 볼륨의 Cinder 스냅샷(`force`, 실행 중에도) | 서버 정지 → `revert_to_snapshot`(볼륨 API 3.40) → 볼륨에 `vigilante.reverted_to` 기록 → 서버 기동 | 서버 ACTIVE·실행 중, 볼륨 표시가 체크포인트 스냅샷 |
| 이미지 부팅 | Nova `createImage`(Glance 이미지, `active`까지 대기) | 체크포인트 이미지로 `rebuild`. IP·포트·메타데이터 유지 | 서버 ACTIVE, 서버 이미지가 체크포인트 이미지 |

- `mode: auto`(기본)는 서버의 부팅 방식을 보고 고릅니다. 다른 방식을 강제하면 prepare에서 거부합니다.
- **재실행 안전:** 이미 체크포인트로 복원된 서버·볼륨은 다시 손대지 않습니다(크래시 후 재개, 리더 교체).
- **정리:** prepare가 끝나면 같은 서버(볼륨)의 vigilante 스냅샷·이미지 중 최신 `keep_snapshots`개만 남기고 지웁니다(`vigilante.managed` 메타데이터가 있는 것만). Cinder revert는 가장 최근 스냅샷으로만 되돌릴 수 있으므로, 배포 사이에 다른 도구가 같은 볼륨의 스냅샷을 만들면 revert가 거부됩니다.
- **revert가 거부되면** 서버는 정지 상태로 남고, 원인(백엔드 미지원, 최신 스냅샷 아님, 사용 중 볼륨 거부)과 함께 실패를 보고합니다. 롤백 실패이므로 에스컬레이션·서킷 규칙이 그대로 적용됩니다. 백엔드·릴리스별 동작은 M8 실장비 랩에서 확정합니다.
- **한계:** 스냅샷은 디스크 상태만 되돌립니다. 메모리와 외부 DB는 되돌리지 않고, 분리된 데이터 볼륨은 대상이 아닙니다.
- **`octavia`:** 같은 로드밸런서의 변경은 한 번에 하나만 받으므로, 변경마다 `provisioning_status: ACTIVE`를 기다리고 409는 재시도하며 대상을 하나씩 바꿉니다. 이미 원하는 상태인 멤버는 건너뜁니다. 다시 켠 멤버는 `operating_status`가 ONLINE(또는 NO_MONITOR)이 될 때까지 기다립니다. neutron-lbaas(레거시)는 지원하지 않습니다.
- **`doctor`:** Keystone 로그인, 서버 조회와 부팅 방식, 루트 볼륨, Cinder 최대 마이크로버전(3.40 이상), 스냅샷 쿼터 여유, Octavia 풀·로드밸런서 상태를 점검합니다. 멤버 수정 권한처럼 변경 없이는 확인할 수 없는 항목은 경고로 남깁니다.

## `safety`

```yaml
safety:
  observer_quorum: true
  circuit_breaker: {failure_threshold: 2, window: 1h, open_duration: 0s}
  blast_radius:    {min_healthy: 1, min_healthy_percent: 50}
  flapping:        {max_rollbacks_per_hour: 3, cooldown: 5m}
  rollback_lease:  {wait: 10s, on_unavailable: proceed}
  observer_guard:  {max_lag: 1s, loopback_timeout: 1s, timeout_share: 0.5, min_services: 3, grace: 1m}
```

| 키 | 기본값 | 설명 |
|---|---|---|
| `rollback_lease.wait` | `10s` | 롤백 시작 시 서비스 잠금(상태 저장소 lease)을 얻으려고 저장소를 다시 시도하는 시간 |
| `rollback_lease.on_unavailable` | `proceed` | 그래도 저장소에 닿지 않을 때. `proceed`는 이 프로세스 안의 잠금만으로 롤백하고 배포 이벤트(`safety`), 감사(`lease.unavailable`), 경고 알림을 남깁니다. 롤백 도중 저장소가 돌아오면 lease를 다시 잡고, 다른 프로세스가 이미 잡고 있으면 `lease.conflict`로 알립니다. `fail`은 롤백을 시작하지 않습니다(ROLLBACK_FAILED) |
| `observer_guard.disabled` | `false` | 관측 장치 자체가 불안정할 때 프로브 실패 기반 위반을 롤백 대신 HOLD하는 기능을 끕니다 |
| `observer_guard.max_lag` | `1s` | 내부 250ms 타이머가 이보다 늦게 깨면 관측 장치 과부하(CPU 부족, GC, VM 정지) |
| `observer_guard.loopback_timeout` | `1s` | 프로세스 안 TCP 에코 왕복이 이보다 길면 과부하(소켓·네트워크 스택 고갈) |
| `observer_guard.timeout_share` / `min_services` | `0.5` / `3` | 관측 중인 대상의 이 비율 이상에서, 이 개수 이상의 서비스에 걸쳐 프로브가 시간 초과면 관측 쪽 문제로 봅니다. 서비스 하나의 불량 릴리스로는 걸리지 않습니다 |
| `observer_guard.grace` | `1m` | 회복 후에도 이 시간 동안은 HOLD를 유지합니다. 과부하 중 쌓인 연속 실패 수와 윈도우 값이 빠질 시간입니다 |

관측 장치 판별은 프로브 실패에서 나온 지표(`up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts`, `timeout`, `probe_error`)를 쓰는 규칙에만 적용합니다. 로그·액세스 로그·호스트·컨테이너 지표는 대상이 직접 보고한 값이라 그대로 판정합니다. 지표: `vigilante_observer_degraded`, `vigilante_observer_degradations_total{signal}`, `vigilante_observer_holds_total`.

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

## `audit` — 감사 기록

모든 판정·조치는 상태 저장소(파일 저널 또는 PostgreSQL)에 기록되고, 이 기록이 곧 감사 기록입니다.

- **작업자:** 기록마다 `actor`(예: `user:alice`, `sa:ci-order`, `cli:bob@host`, 자동 조치는 `system`), `source`(api·cli·webhook·system), `action`, 대상 서비스·배포, `reason`이 남습니다.
- **변경 티켓:** API는 `X-Change-Ticket` 헤더, CLI는 `--ticket`으로 받은 값을 `ticket`에 남깁니다.
- **권한 거부:** 거부(403)된 요청도 `action: denied`로 남습니다. 반복되는 거부는 권한 탐색 시도의 신호입니다.
- **해시 체인(변조 검출):** 기록마다 직전 기록의 해시(`prev`)와 자신의 해시(`hash`)를 포함합니다. 한 건이라도 고치거나 지우면 그 지점부터 체인이 끊어지고, `vigilante audit verify`가 위치와 원인(수정·삭제)을 보고합니다. DB 관리자가 SQL로 직접 바꿔도 검출됩니다.

```yaml
audit:
  syslog:
    address: tcp://siem.example.internal:6514   # 또는 udp://...
    format: rfc5424                              # rfc5424(JSON 본문, 기본) | cef
  retention: 8760h                               # audit prune의 기본 보존 기간. 자동 삭제는 하지 않음
```

| 명령 | 하는 일 |
|---|---|
| `vigilante audit verify -c FILE` | 저장소 전체 체인 검증. 끊어지면 exit 1 |
| `vigilante audit verify --file ARCHIVE.jsonl` | 아카이브 파일만 따로 검증 |
| `vigilante audit query -c FILE [--actor A] [--action denied] [--service S] [--since 2026-10-01]` | 감사 기록 조회 |
| `vigilante audit export -c FILE --out F.jsonl` | 전체 기록을 체인 그대로 내보내기 |
| `vigilante audit prune -c FILE --out ARCHIVE.jsonl [--before 2025-10-01 \| --older-than 8760h]` | 보존 기간이 지난 기록을 아카이브로 옮기고 삭제 |

- **조회 API:** `GET /v1/audit?since=&until=&actor=&service=&action=&kind=&limit=&format=csv`. 모든 서비스에 걸친 정보라 `viewer@*`(전체 범위) 권한이 필요합니다.
- **SIEM 전송:** 저장된 뒤 비동기로 보냅니다. SIEM이 느리거나 끊겨도 롤백을 막지 않으며, 큐가 가득 차면 버리고 개수를 셉니다. 빠진 구간은 `audit export`로 채울 수 있습니다. 배포 상태는 상태가 바뀔 때만 보냅니다.
- **보존 정리(prune):** 지울 구간을 먼저 아카이브에 쓰고(아카이브는 따로 검증 가능), 그 구간이 만든 상태 중 아직 필요한 것을 하나의 앵커 기록에 담아 대체합니다. 필요한 상태는 진행 중인 배포와 롤백 단계, 서비스별 마지막 성공 버전, 서킷 상태, 플래핑 계산용 최근 롤백입니다. 남은 체인은 앵커에서 이어집니다. 파일 백엔드는 서버가 그 파일을 쓰지 않을 때 실행하십시오.

## `change_freeze` — 변경 동결

```yaml
change_freeze:
  - name: weekend
    weekly: {from: "fri 18:00", to: "mon 09:00", timezone: Asia/Seoul}   # 매주 반복
  - name: year-end-closing
    reason: 결산 기간
    start: "2026-12-28T00:00:00+09:00"    # 기간 지정 (RFC 3339)
    end: "2027-01-02T00:00:00+09:00"
    teams: [payments]                     # services/teams 생략 시 전 서비스
    allow_rollback: true                  # 기본 true
```

- **막는 것:** 동결 중인 서비스의 새 배포 등록과 단계 관측 시작. API는 `409`(코드 `change_frozen`), CLI(`watch`, `prepare`)는 종료 코드 3입니다. 동결 전에 등록한 배포도 새 단계를 시작할 수 없습니다.
- **막지 않는 것:** 자동 롤백은 기본으로 허용합니다. 장애 복구는 변경이 아니기 때문입니다. `allow_rollback: false`인 기간에는 자동 롤백 대신 실패한 대상을 격리하고 사람에게 넘깁니다. 수동 롤백은 항상 가능합니다.
- **예외(긴급 배포):** API는 admin이 `freeze_override`에 이유를 넣어 배포를 등록하고, CLI는 `--freeze-override "이유"`를 씁니다. 배포의 `freeze_override`와 감사 기록(`freeze.override`)에 누가 왜 했는지 남습니다.
- **실행 중 선언:** 장애 대응처럼 설정 파일 없이 동결해야 하면 admin이 `POST /v2/freezes`로 선언하고 `DELETE /v2/freezes/{id}`로 일찍 끝냅니다. 상태 저장소에 남아 리더가 바뀌어도 유지됩니다. `GET /v2/freezes`는 설정 창과 선언된 동결을 함께 보여 줍니다.
- 주간 창의 시각은 `timezone`(생략 시 서버 지역 시간) 기준이며, 바이너리에 시간대 데이터가 들어 있어 호스트 설정과 무관하게 동작합니다.

## `api` — 오픈 API

```yaml
api:
  rate_limit: {rate: 20, burst: 40}            # 호출자별 v2 호출 한도 (daily: 일일 상한, 선택)
  emergency_rate_limit: {rate: 1, burst: 10}   # 롤백·승인·중단·서킷 전용 버킷
  token_ttl: 1h                                # OAuth 액세스 토큰 수명 (최대 24h)
  webhook_signing_key_ref: "vault:secret/vigilante/webhooks#key"   # 웹훅 서명 마스터 키. 없으면 웹훅 비활성
  webhook_allowed_hosts: [".example.internal"]                     # 웹훅 URL 허용 호스트(접미사). 비우면 제한 없음
```

`rate: 0`이고 `daily`가 없으면 한도가 없습니다. API 클라이언트별 한도와 클라이언트 등록은 설정 파일이 아니라 API(`/v2/api-clients`)로 관리하며, 상태 저장소에 남습니다. 자세한 내용은 docs/06-api.md.

웹훅 서명 비밀은 마스터 키와 구독 ID로 계산하므로 상태 저장소에는 비밀이 남지 않습니다. 마스터 키를 바꾸면 모든 구독의 비밀이 바뀌므로, 키 교체 후에는 각 구독에 `POST /v2/webhooks/{id}/secret`로 새 비밀을 받아 수신 측에 전달하십시오.

## `console` — 웹 운영 콘솔

`vigilante server`는 `/console/`에서 운영 콘솔을 제공합니다. 화면은 바이너리에 내장되어 있고 외부 CDN을 쓰지 않으므로 폐쇄망에서도 그대로 동작합니다. 콘솔은 공개 API(v2)만 호출하므로 사용자가 할 수 있는 일은 그 사용자의 역할·범위와 같습니다.

```yaml
console:
  redirect_url: https://vigilante.example.internal/console/auth/callback   # IdP에 등록한 콜백. 있으면 SSO 로그인
  session_key_ref: "vault:secret/prod/vigilante#console_session_key"     # 세션 쿠키 암호화 키(32자 이상). HA 노드가 같은 값을 써야 함
  client_id: vigilante-console     # 생략 시 auth.oidc.audience
  client_secret_ref: "vault:secret/prod/vigilante#console_client_secret" # 기밀 클라이언트일 때만. PKCE는 항상 사용
  scopes: [openid, profile, email] # 기본값. 그룹 클레임이 별도 스코프면 추가
  # disabled: true                 # 콘솔을 끔
```

- **SSO 로그인:** `auth.oidc`와 `redirect_url`이 있으면 OIDC authorization code + PKCE로 로그인합니다. 콘솔이 받은 ID 토큰을 API와 같은 방식(`auth.oidc`, `auth.role_bindings`)으로 검증하므로, IdP의 `aud`가 `auth.oidc.audience`와 같아야 합니다. 역할 바인딩이 하나도 없는 사용자는 로그인을 거부하고 감사 기록에 남깁니다.
- **세션:** ID 토큰을 AES-GCM으로 암호화한 HttpOnly 쿠키(`SameSite=Lax`, https면 `Secure`)에 담습니다. 서버에는 세션 상태가 없어 HA의 어느 노드든 받을 수 있습니다. 세션은 ID 토큰 만료 시각(최대 12시간)에 끝납니다. `session_key_ref`가 없으면 재시작마다 키가 바뀌어 다시 로그인해야 합니다(`validate`가 경고).
- **CSRF:** 쿠키로 인증한 변경 요청은 `X-CSRF-Token` 헤더에 CSRF 쿠키 값을 담아야 합니다(double-submit). 없으면 `403 forbidden`입니다. `Authorization` 헤더가 있는 요청은 쿠키를 보지 않으므로 API 클라이언트에는 영향이 없습니다.
- **토큰 로그인:** SSO가 없으면 콘솔이 서비스 계정 토큰이나 API 키를 묻습니다. 토큰은 그 브라우저 탭(sessionStorage)에만 남습니다.
- **화면:** 현황(서킷·조치 필요·진행 중·최근 배포·실시간 이벤트), 배포 목록·상세(승인·거절, 롤백, 관측 중단, 규칙 위반, 작업, 타임라인), 서비스, 변경 동결(선언·종료), 감사 기록. 모든 조작은 사유를 받아 감사 기록에 남기며(출처 `ui`), 버튼은 역할에 맞는 것만 보입니다. 실시간 갱신은 `GET /v2/events`(SSE)를 씁니다.
- 보안 헤더: `Content-Security-Policy`(자기 출처만, 인라인 스크립트 없음), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. API 데이터는 모두 텍스트로만 화면에 넣습니다.

## `secrets` — 비밀값 출처

```yaml
secrets:
  cache_ttl: 5m                       # 해석한 값을 메모리에 두는 시간 (기본 5m)
  vault:
    address: https://vault.example.internal:8200
    namespace: ops                    # Vault Enterprise 네임스페이스 (선택)
    ca_file: /etc/vigilante/vault-ca.pem
    auth: approle                     # token | approle | kubernetes
    role_id_env: VAULT_ROLE_ID        # approle
    secret_id_env: VAULT_SECRET_ID    # approle
    # token_env: VAULT_TOKEN          # token (기본 VAULT_TOKEN)
    # k8s_role: vigilante             # kubernetes; JWT는 서비스 계정 토큰 파일
    # auth_mount: approle             # 로그인 경로가 기본값과 다를 때
```

- **로그인:** `approle`과 `kubernetes`는 처음 쓸 때 로그인하고, 토큰 만료 30초 전에 다시 로그인합니다. 토큰이 먼저 폐기돼 403이 오면 한 번 다시 로그인해 재시도합니다.
- **보관:** 비밀값은 메모리 캐시에만 둡니다. 설정 파일, 저널, 감사 기록에는 참조 문자열만 남습니다.
- **로그 가림:** 한 번이라도 해석한 값(6자 이상)은 로그 메시지·속성·오류 문자열에서 `[REDACTED]`로 바뀝니다. `*_env`로 읽은 값도 같습니다.
- **Vault 정책 예:** 필요한 권한은 KV 읽기와 SSH 서명뿐입니다.

```hcl
path "secret/data/prod/*"            { capabilities = ["read"] }
path "ssh-client-signer/sign/vigilante" { capabilities = ["update"] }
```

- **사전 점검:** `vigilante doctor`가 사용하는 모든 `*_ref`를 실제로 해석하고, `ssh_ca`는 일회용 키로 서명을 받아 봅니다. 정책이 막혀 있으면 롤백 전에 드러납니다.
- **장애 시:** Vault가 응답하지 않으면 그 자격증명이 필요한 프로브·실행기만 실패합니다. 캐시에 남은 값은 `cache_ttl` 동안 계속 쓰입니다. 롤백 경로가 Vault에 의존하지 않게 하려면 캐시 시간을 관측 창보다 길게 두십시오.

## `notify[]`

| 키 | 설명 |
|---|---|
| `type` | `slack`(incoming webhook 텍스트) \| `teams`(Teams Workflows 웹훅, Adaptive Card) \| `email`(SMTP) \| `pagerduty`(Events v2) \| `webhook`(배포 JSON 전체) |
| `url` / `url_env` / `url_ref` | slack·teams·webhook 대상 URL. URL에 비밀이 들어가므로 `url_ref`(Vault) 권장 |
| `min_level` | `info` \| `warning` \| `critical` |
| `services` / `teams` | 이 채널로 보낼 서비스·팀. 생략하면 전부. 서비스가 없는 알림(서킷 등)은 모든 채널로 갑니다 |
| `smtp` | email: `host`, `port`(기본 587 STARTTLS, 465는 TLS), `from`, `to[]`, `username`, `password_ref`, `implicit_tls`, `no_starttls`(신뢰 망 릴레이만) |
| `routing_key_ref` / `routing_key_env` | pagerduty: 서비스 integration key. 경고·치명 알림을 `dedup_key`(배포·제목)로 묶어 트리거 |

같은 배포의 같은 알림은 채널마다 10분에 한 번만 보냅니다. 알림 실패는 기록만 하고 롤백을 막지 않습니다.

```yaml
notify:
  - {type: teams, url_ref: "vault:secret/prod/teams#payments_webhook", teams: [payments], min_level: warning}
  - type: email
    min_level: critical
    smtp: {host: smtp.example.internal, from: vigilante@example.internal, to: [sre@example.internal], username: vigilante, password_ref: "vault:secret/prod/smtp#password"}
  - {type: pagerduty, routing_key_ref: "vault:secret/prod/pagerduty#sre", min_level: critical}
```

## `itsm` — ServiceNow

```yaml
itsm:
  servicenow:
    url: https://company.service-now.com
    credential: snow-integration           # type basic(통합 사용자) 또는 token(OAuth bearer)
    change_gate:
      enabled: true
      teams: [payments]                     # services/teams 생략 시 전 서비스
      allowed_states: ["-2", "-1"]          # Scheduled, Implement (기본)
      check_window: true                    # 계획된 시작·종료 시각 안이어야 함 (기본)
      on_error: closed                      # ServiceNow 장애 시 closed(거부, 기본) | open(진행, 미검증 표시)
    incidents:
      enabled: true
      on: [rollback_failed, circuit_opened] # 기본 둘 다
      assignment_group: SRE
      urgency: 1
      impact: 2
    work_notes: true                        # 변경 티켓에 진행 결과 기록 (기본 true)
```

- **변경 티켓 게이트:** 게이트가 적용되는 서비스의 새 배포는 변경 번호가 있어야 합니다. API는 `X-Change-Ticket` 헤더나 v2 `change_ticket`, CLI는 `--ticket`입니다. 티켓은 승인(`approval: approved`)되어 있고, 허용 상태이며, 지금이 계획된 작업 시간 안이어야 합니다. 등록할 때 확인하고 단계를 시작할 때마다 다시 확인합니다(작업 시간이 끝났을 수 있으므로). 거부되면 API `409 change_ticket_invalid`, CLI 종료 코드 3입니다.
- **ServiceNow 장애:** `on_error: closed`면 `503 itsm_unavailable`(Retry-After)로 새 배포를 받지 않고, `open`이면 진행하되 배포의 `change_ticket.unverified: true`와 이벤트에 남깁니다. **롤백은 어느 경우에도 ServiceNow를 기다리지 않습니다.**
- **인시던트:** 롤백 실패와 서킷 열림 때 인시던트를 엽니다. `correlation_id`로 같은 사건을 한 번만 만들고(재시도·리더 교체에도 중복 없음), 감사 기록(`itsm.incident`)과 배포 타임라인에 번호를 남깁니다.
- **작업 노트:** 검증된 티켓이 있는 배포는 관측 시작·판정·롤백 시작·완료·실패·승인 요청·결정을 변경 티켓의 work notes에 남깁니다.
- 인시던트와 작업 노트는 서버(리더)가 이벤트를 따라가며 처리합니다. CI 단발 실행(`vigilante watch`)만 쓰는 구성에서는 게이트만 동작합니다.
- 필요한 ServiceNow 권한: `change_request` 읽기·쓰기(work notes), `incident` 읽기·생성. 지표: `vigilante_itsm_calls_total{kind,result}`.
