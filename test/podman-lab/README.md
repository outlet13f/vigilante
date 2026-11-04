# 로컬 Podman 랩 테스트 계획

실장비 없이 로컬 Podman에서 **실제 소프트웨어**(Nginx, HAProxy, Envoy, OpenSSH, Podman API, PostgreSQL, etcd)를 상대로 Vigilante를 시험합니다. 로드맵 M8-1 랩 중 상용 장비가 필요 없는 부분을 앞당기는 것이 목적입니다. 결과는 `vigilante lab run` 기록으로 남기고, 3회 연속 통과한 조합은 호환성 매트릭스(docs/09) 갱신 근거로 씁니다.

## 1. 범위

| 구분 | 이 랩에서 시험 | 지금 상태 (docs/09) | 시험 대상 |
|---|---|---|---|
| 트래픽 | `nginx` | 실험적 | Nginx 1.22 (Debian 12), upstream include 파일 + `nginx -t`·reload |
| 트래픽 | `haproxy` | 실험적 | HAProxy 2.6 (Debian 12), runtime API TCP·unix 소켓(SSH 터널) |
| 트래픽 | `envoy` | 실험적 | Envoy 1.32, 파일 기반 EDS(`watched_directory`) |
| 실행기 | `symlink` | 실험적 | 릴리스 디렉토리 + `current` 링크, `init: none` + `restart_cmd` |
| 실행기 | `container` | 실험적 | Podman 6.1 Docker 호환 API (이미지 태그 교체) |
| 실행기 | `exec` | 실험적 | SSH로 실행하는 사용자 명령 |
| 프로브 | `host` | 실험적 | 컨테이너의 `/proc`(SSH) |
| 프로브 | `docker` | 실험적 | Podman API (재시작·OOM·health) |
| 프로브 | `db` | 실험적 | PostgreSQL 16 (선택: MySQL 8.4) |
| 프로브 | `grpc` | 실험적 | etcd 3.5의 `grpc.health.v1` |
| 프로브 | `tcp` | 실험적 | 실제 리스너 |
| 플랫폼 | PostgreSQL 상태 저장소, HA 2노드, `sudo_scope: changes`, SSH 세션 예산 | CI·단위 테스트 | 실제 sshd·sudo·PostgreSQL |

**제외:** F5, vSphere, Nutanix, AWS ALB, OpenStack·Octavia(장비·계정 필요), KVM(중첩 가상화 없음), `webhook`·`http`·`log`·`access_log`(이미 검증됨, 회귀로만 확인).

**한계:** Podman 머신이 arm64라 amd64는 시험하지 않습니다. 컨테이너에는 systemd가 없어 `init: systemd` 경로는 시험하지 않습니다. `container` 실행기의 결과는 Docker Engine이 아니라 **Podman의 Docker 호환 API** 기준으로 기록합니다.

## 2. 환경

```
vgl-lab 10.89.50.0/24 (rootless Podman, podman-machine-default)

  vgl-ctl   .2   컨트롤러: vigilante(linux), ssh, curl, Podman 소켓 → /var/run/docker.sock, 이 폴더 → /lab
  vgl-lb    .10  Nginx :8080 · HAProxy :8081 (API :9999, /run/haproxy/admin.sock) · Envoy :8082 (admin :9901) · sshd
  vgl-app1  .11  sshd + fakeapp :18080, /opt/app/releases/{v1,v1.1,v2}, current → v1
  vgl-app2  .12  (app1과 같음)
  vgl-app3  .13  fakeapp 컨테이너 (localhost/vgl-app:v1, v1.1, v2)
  vgl-pg    .20  PostgreSQL 16 (max_connections=30)
  vgl-etcd  .22  etcd 3.5 (gRPC health :2379)
```

- fakeapp는 버전 `v2`를 불량으로 동작시킵니다: 지연 증가, 5xx 약 10%, 로그에 OOM·예외.
- 대상 호스트의 "배포"는 `/usr/local/bin/deploy.sh <버전>`(링크 교체 + 재시작)이고, 컨테이너의 "배포"는 `inject-container.sh <버전>`(같은 IP로 이미지 교체)입니다.
- SSH 사용자는 `deploy`이며, 기본은 `NOPASSWD: ALL`입니다. S1 시나리오에서 `vigilante sudoers`가 만든 규칙으로 좁힙니다.

### 준비

```bash
cd test/podman-lab
./lab.sh build     # Linux 바이너리, SSH 키(.run/), 이미지 4종, PostgreSQL·etcd 이미지
./lab.sh up        # 네트워크와 컨테이너 7개
./lab.sh status    # 헬스·SSH·Podman API 확인
./lab.sh vig validate -c lab.yaml
./lab.sh vig doctor -c lab.yaml
```

자원: Podman 머신 4 CPU·10 GiB에서 메모리 약 1.5 GiB, 디스크 약 1.5 GiB(이미지). `./lab.sh down`은 컨테이너만, `./lab.sh clean`은 랩 이미지와 `.run/`까지 지웁니다.

## 3. 시나리오

모든 L 시나리오는 아래 형식으로 3회 반복합니다. 결과는 `.run/lab-results.jsonl`에 쌓입니다.

```bash
./lab.sh vig lab run -c lab.yaml --service <서비스> --label "<시험 대상>" \
  --version v2 --previous v1 --baseline 10s --repeat 3 --out .run/lab-results.jsonl \
  --inject "<불량 배포 명령>" --reset "<정상 복구 명령>"
./lab.sh vig lab summary .run/lab-results.jsonl
```

### 3.1 롤백 조합 (L)

| ID | 서비스 | 조합 | `--inject` / `--reset` | 통과 기준 |
|---|---|---|---|---|
| L1 | `app-nginx` | symlink + nginx + java-web 프리셋 | `ssh 10.89.50.11 deploy.sh v2` / `ssh 10.89.50.11 deploy.sh v1` | 탐지 → upstream에 `down` → 링크 v1 → 헬스 3회 → `down` 해제. 끝난 뒤 `nginx -T`에 `down` 없음, :8080 정상 |
| L2 | `app-haproxy` | symlink + haproxy(TCP API) | L1과 같음 | `show servers state`에서 app1이 drain → ready로 돌아옴 |
| L3 | `app-haproxy-ssh` | symlink + haproxy(unix 소켓, SSH 터널, `drain_to_maint`) | L1과 같음 | L2와 같음, maint 경유 |
| L4 | `app-envoy` | symlink + envoy(EDS) | L1과 같음 | EDS의 `health_status: DRAINING` 설정·해제, Envoy admin `/clusters`에서 app1 복귀 |
| L5 | `app-container` | container + docker 프로브 | `/lab/inject-container.sh v2` / `/lab/inject-container.sh v1` | 컨테이너가 `localhost/vgl-app:v1`로 다시 만들어지고 **IP 10.89.50.13이 유지**됨, 헬스 정상 |

### 3.2 실패 경로 (F)

| ID | 상황 | 만드는 방법 | 기대 결과 |
|---|---|---|---|
| F1 | `nginx -t` 실패 | 드레인 직전 lb의 lab.conf를 깨뜨림 | 백업 복원, reload 안 함, 제자리 롤백 계속 (docs/04 S7) |
| F2 | 풀이 비게 되는 드레인 | app2를 먼저 `down`으로 둔 채 L1 | 드레인 거부, 제자리 롤백, `blast-radius` 이벤트 (S8) |
| F3 | 이전 릴리스 없음 | app1의 `/opt/app/releases/v1` 삭제 | 재시도 소진 → `ROLLBACK_FAILED`(exit 3), app1은 풀에서 빠진 채 격리 (S4) |
| F4 | 롤백 실패 누적 | F3을 연속 실행 | 서킷 OPEN, 다음 `watch`는 exit 3, `circuit reset` 후 정상 |
| F5 | HAProxy API 단절 | 롤백 중 `podman pause vgl-lb` 후 5초 뒤 재개 | 단계 재시도로 완료, 트래픽 복귀 |
| F6 | 컨테이너 기동 실패 | 없는 태그(`--previous v9`)로 롤백 | 보상 처리: 원래 컨테이너 복구, "이전보다 나빠지지 않음" (S6) |

### 3.3 프로브 (P)

`lab.yaml`에 프로브 전용 서비스를 추가해 `watch --dry-run`으로 지표와 위반을 확인합니다.

| ID | 프로브 | 대상 | 확인 |
|---|---|---|---|
| P1 | `db` (postgres) | vgl-pg | `up`, `query_ms`. 다른 세션으로 커넥션 30개를 잡으면 `pool_acquired` 감소 → 규칙 위반 |
| P2 | `db` (mysql, 선택) | mysql:8.4 컨테이너 추가 | P1과 같음 |
| P3 | `grpc` | vgl-etcd:2379 | 정상 `up=1`. `podman stop vgl-etcd` 후 `consecutive_failures` 증가 |
| P4 | `tcp` | vgl-lb:8081 | 연결 지연, 중지 시 `up=0` |
| P5 | `host` | app1 | `cpu_count`, `mem_available_pct`가 `podman exec vgl-app1 cat /proc/meminfo`와 일치 |
| P6 | `docker` | vgl-app3 | `podman restart vgl-app3` → `restarts` 증가, `--memory 32m`으로 OOM → `oom_killed` |

### 3.4 플랫폼·보안 (S, H)

| ID | 항목 | 방법 | 통과 기준 |
|---|---|---|---|
| S1 | `sudo_scope: changes` | `vigilante sudoers --target lb|app1`로 만든 규칙만 남기고 `90-lab` 제거, `doctor` 후 L1·L4 | doctor의 sudo 점검 통과, 롤백 성공, 규칙 밖 명령은 sudo 거부 |
| S2 | SSH 세션 예산 | `max_sessions: 3`, `reserved_sessions: 1`, sshd `MaxSessions 3` | 로그 스트림이 몫을 다 써도 롤백 단계가 예약 세션으로 실행, `vigilante_ssh_session_waits_total` 증가 |
| S3 | 원격 로그 필터 | `log.remote_grep` 설정 | doctor가 grep 호환 확인, 맞는 줄만 전송 |
| H1 | PostgreSQL 상태 저장소 | `store: postgres`(vgl-pg), `store migrate`·`status` | 스키마 적용, L1이 같은 결과 |
| H2 | HA 2노드 | `vigilante server` 컨테이너 2개 + vgl-pg, 롤백 중 리더 `podman kill` | 새 리더가 30초 안에 이어받아 `ROLLED_BACK` (M0 검증 기준) |
| H3 | 저장소 장애 중 롤백 | 롤백 중 `podman pause vgl-pg` | 롤백 계속, 쓰기 대기열, 복구 후 기록 유실 없음, 해시 체인 정상 |
| H4 | 서버 이미지 | `packaging/container/Dockerfile`로 이미지 빌드, server 실행 | `/healthz`·`/readyz`·`/metrics`, 콘솔 토큰 로그인 |

## 4. 실행 순서

| 순서 | 내용 | 예상 시간 |
|---|---|---|
| 1 | 준비: build, up, status, validate, doctor | 15분 |
| 2 | L1~L5 (각 3회) | 1시간 |
| 3 | F1~F6 | 1시간 |
| 4 | P1~P6 | 1시간 |
| 5 | S1~S3 | 1시간 |
| 6 | H1~H4 (H2·H4는 서버 컨테이너 설정 추가 필요) | 2시간 |
| 7 | 결과 정리: `lab summary`, 발견한 결함, docs/09 갱신안 | 30분 |

## 5. 판정과 기록

- **통과:** 조합마다 `lab run`이 3회 연속 통과(탐지, 드레인, 복원, 확인, 트래픽 복귀, 풀 상태 전후 일치).
- **기록:** `.run/lab-results.jsonl`과 `lab summary` 표, 실패하면 `./lab.sh vig status -c lab.yaml --id <ID>` 출력과 컨테이너 로그(`podman logs vgl-lb`).
- **호환성 매트릭스:** 통과한 플러그인은 `internal/compat/compat.go`와 docs/09를 함께 고치는 PR을 냅니다. 검증 대상에는 "로컬 Podman 랩"과 소프트웨어 버전을 적어, 고객 환경 실장비 검증과 구분합니다.
- **결함:** 시나리오 ID, 재현 명령, 기대·실제 결과, 로그를 남깁니다.

## 6. 위험

| 위험 | 대응 |
|---|---|
| 맥 디스크 여유가 작음(약 25 GiB) | slim 이미지만 쓰고, 끝나면 `./lab.sh clean` |
| Podman 호환 API와 Docker Engine 동작 차이 | 결과에 Podman 버전을 명시, Docker Engine은 별도 시험 |
| 컨테이너의 `/proc`가 VM 값을 보여 줌 | P5는 값의 일치만 확인, 절대값 판단은 하지 않음 |
| 이미지 빌드마다 SSH 호스트 키가 바뀜 | 랩 자격증명에만 `insecure_ignore_host_key: true` |

## 7. 진행 기록

### 2026-11-04 준비 완료

- `./lab.sh build`·`up`: 이미지 6개(vgl-target·lb·ctl, vgl-app v1·v1.1·v2), 컨테이너 7개 기동. `status`에서 헬스 6곳(app1~3, Nginx·HAProxy·Envoy 경유), SSH, Podman API 모두 정상.
- `doctor -c lab.yaml`: 89건 중 통과 85, 경고 2, 실패 2. 실패 2건은 아래 결함 D1.
- **L2 1회 (스모크):** `app-haproxy` PASS, `ROLLED_BACK`, 탐지 4초, 롤백 2초. app1 링크가 v1로 돌아오고 HAProxy에서 drain → ready 복귀.

### 발견한 결함

| ID | 내용 | 영향 | 근거 |
|---|---|---|---|
| D1 (수정함) | `sudo_scope: changes`에서 nginx doctor 점검이 sudo 없이 `test -w <upstream_file>`와 `nginx -t`를 실행. 실제 드레인은 `sudo tee`·`sudo nginx -t`를 씀 | 권장 구성(root 소유 upstream 파일 + `sudo_scope: changes`)에서 doctor가 거짓 실패 2건을 내고, doctor 통과가 필요한 `lab run`이 시작되지 않음. L1(nginx)이 막힘 | `internal/executor/diagnose.go` nginx `Diagnose`, `internal/executor/traffic_sw.go` `set` |
| D2 (수정함) | `lab run --previous v1`인데 시작 전 doctor가 "이전 릴리스 — 이전 버전 미지정 (--previous)"로 생략 | 이전 릴리스 존재 점검이 랩 실행 전에 빠짐 | L2 실행 출력, `cmd/vigilante/labcmd.go`의 doctor 호출 |

### 2026-11-04 D1·D2 수정 후

- **D1 수정:** nginx `Diagnose`가 `set()`과 같은 방식으로 점검합니다. sudo 접두어가 있으면 로그인 사용자의 `test -w` 대신 파일 존재(`test -f`)를 확인하고(쓰기 권한은 sudo 규칙 점검이 맡음), 기본 `nginx -t`는 `sudo -n nginx -t`로 실행합니다. 단위 테스트 `TestNginxDiagnoseSudoChanges` 추가.
- **D2 수정:** `lab run`의 사전 doctor에 `--version`·`--previous`를 넘깁니다. 이제 "이전 릴리스 /opt/app/releases/v1 — 있음"으로 점검됩니다.
- `doctor -c lab.yaml`: 실패 0, 경고 2(SSH 세션 예산, 의도된 값), exit 0.
- **L1 3회:** `app-nginx` 3/3 PASS, `ROLLED_BACK`, 탐지 중앙값 3초, 롤백 2초. 끝난 뒤 `nginx -T`의 upstream에 `down` 없음, LB :8080 200, app1 링크 v1. 확인용 1회 추가 실행도 PASS.


### 2026-11-04 재시험 (커밋 e03416f, PR #17)

- 단위 테스트: gofmt·vet 통과, `go test ./...` 2회 연속 전부 통과.
- `doctor -c lab.yaml`: 실패 0, 경고 2(SSH 세션 예산), exit 0.
- **L1~L5 각 3회, 15/15 PASS** (`.run/retest-results.jsonl`):

| ID | 서비스 | 결과 | 탐지 중앙값 | 롤백 중앙값 |
|---|---|---|---|---|
| L1 | app-nginx | 3/3 | 4초 | 2초 |
| L2 | app-haproxy | 3/3 | 4초 | 2초 |
| L3 | app-haproxy-ssh | 3/3 | 4초 | 4초 |
| L4 | app-envoy | 3/3 | 3초 | 2초 |
| L5 | app-container | 3/3 | 5초 | 0초 |

- 끝난 뒤 상태: Nginx upstream에 `down` 없음, HAProxy app1·app2 admin 0(ready)·op 2(running), Envoy EDS에 `DRAINING` 없음·`/clusters` 둘 다 healthy, app1 링크 v1, vgl-app3는 `localhost/vgl-app:v1`·IP 10.89.50.13 유지.

**시험 중 관찰**

- **랩 절차:** 서비스를 연달아 돌리면, 앞 서비스의 마지막 `--reset`이 fakeapp를 재시작하는 동안 다음 `lab run`의 사전 doctor가 app1 헬스 `connection refused`로 실패해 시작하지 않습니다(L2·L4 첫 시도). 제품 결함이 아니라 절차 문제이므로, 서비스 사이에 app1 헬스가 200이 될 때까지 기다린 뒤 실행합니다.
- **Podman VM 정지:** 재실행 도중 podman-machine-default가 `stopped`가 되어 컨테이너 7개가 함께 멈췄습니다(원인 미상, 디스크 여유 27 GiB·VM 메모리 여유 충분). `podman machine start` 후 `podman start vgl-*`로 복구했고, 이후 실행은 정상.
- **O1 (문서화 필요):** container 실행기는 롤백 때 불량 컨테이너를 지우지 않고 `<이름>-failed-<시각>`으로 바꿔 남깁니다(`internal/executor/container.go:107`, 사후 분석용으로 보임). L5 3회 뒤 `vgl-app3-failed-*` 3개가 쌓였습니다. 운영자 매뉴얼에 이 동작과 정리 방법이 없습니다.
