---
title: 아키텍처 설계서
doc_id: VGL-SI-02
version: 1.2
date: 2026-10-11
author: Vigilante 개발팀
status: 검토본
history:
  - 1.0 | 2026-10-11 | 최초 작성
  - 1.1 | 2026-10-11 | 코드 대조 검증 반영
  - 1.2 | 2026-10-11 | 결함 수정(PR #13) 반영, PR #12 병합 반영, PR #14 반영
---

# 1. 개요

## 1.1 목적

이 문서는 Vigilante의 시스템 경계, 논리 구성, 배포 토폴로지, 고가용성·장애 복구, 보안 구조, 기술 스택, 품질 속성 달성 방안, 주요 아키텍처 결정을 기술한다. 요구사항은 VGL-SI-01(요구사항 정의서), 패키지·알고리즘 수준 설계는 VGL-SI-03(상세 설계서)을 따른다.

## 1.2 기준

- 코드 기준: master 커밋 `537870c`. PR #12(M5-4: 저장소 장애 중 기록 큐, 카오스 시나리오, 부하 하네스, 병합 `dba3dbe`)와 PR #13(리뷰 결함 수정: 관측 장치 과부하 가드, 저장소 장애 중 롤백 lease, 로컬 CLI 권한 제한, 키 감사 체인, SIEM TLS, HA 전달 TLS, 콘솔 HSTS, Helm 차트, `watch --server` 게이트 플래그, ServiceNow 재시도)와 PR #14(관측 장치 가드가 직접 재는 프로브의 지표만 보류, 병합 `537870c`)까지 병합된 상태이다.
- 원천 문서: docs/01-architecture.md, docs/03-engine-design.md, docs/04-safety-circuit-breaker.md, docs/05-roadmap.md, docs/07-install.md, docs/10-security.md, go.mod.
- 제품은 아직 출시되지 않았다. 실장비(F5·vCenter·Nutanix·AWS·OpenStack) 연동은 시뮬레이터·모의 서버로만 검증했으며 실장비 검증은 M8에서 한다.

## 1.3 아키텍처 원칙

docs/05 "설계 원칙"과 docs/04 서두의 원칙을 아키텍처 원칙으로 채택한다.

| 번호 | 원칙 | 아키텍처상 의미 |
|---|---|---|
| P1 | 롤백 경로는 의존성이 가장 적어야 한다 | 콘솔·ITSM·SSO·Vault·SIEM·알림 장애가 판정·롤백을 막지 않는다. 예외는 명시적 승인 게이트뿐이다 |
| P2 | 플러그형 백엔드 | 저장소·인증·비밀·ITSM·알림은 인터페이스로 두고 개발용 기본 구현(file, 토큰, env)을 유지한다 |
| P3 | 선언형 설정 | 설정 원본은 YAML 파일이다(GitOps 연동은 2차) |
| P4 | 하위 호환 | CLI 종료 코드, REST v1, 설정 v1을 유지하고 변경은 v2로 신설한다 |
| P5 | 실장비 미검증 플러그인은 "실험적" | `internal/compat` 매트릭스와 `validate` 경고로 표시한다 |
| P6 | 초기 운영은 "자동 판정 + 사람 승인" | `rollback.mode: approve`. 하위 호환을 위해 미지정은 `auto`로 해석한다 |
| P7 | Isolate, don't destroy | 확신이 없으면 파괴적 조치 대신 트래픽 격리까지만 한다 |
| P8 | 자동화가 실패하면 자동화를 멈춘다 | 롤백 실패 누적 시 서킷 OPEN |
| P9 | 모든 결정은 먼저 기록된다 | 상태 저장소에 기록한 뒤 실행하고 재시작 시 이어서 수행한다 |

---

# 2. 시스템 컨텍스트

## 2.1 시스템 경계

Vigilante는 하나의 Go 바이너리이며, 실행 모드(`watch` 단발, `server`, `agent`)에 따라 역할이 달라진다. 경계 밖에는 배포 시스템(CI), 운영자, 대상 인프라, 사내 공통 시스템이 있다.

```
                 ┌──────────── 배포 시스템 ────────────┐      ┌──── 사람 ────┐
                 │ Jenkins · GitLab CI · GitHub Actions │      │ 운영자·승인자 │
                 │ 사내 배포 콘솔                        │      │ (브라우저)    │
                 └──┬──────────┬───────────┬────────────┘      └──────┬───────┘
          CLI 종료 코드   REST v1/v2   수신 웹훅(HMAC/토큰)       /console/ (OIDC)
                    │          │           │                          │
   ┌────────────────▼──────────▼───────────▼──────────────────────────▼─────────────┐
   │                       VIGILANTE (단일 Go 바이너리, CGO 없음)                       │
   │  watch(단발) · server(REST·콘솔·이벤트·HA) · agent(대상 호스트 상주, 선택)          │
   └──┬──────────┬──────────┬──────────┬──────────┬──────────┬──────────┬───────────┘
      │SSH 22    │HTTPS 443 │5432      │8200      │443       │443/587   │syslog
      ▼          ▼          ▼          ▼          ▼          ▼          ▼
   대상 호스트  LB·하이퍼  PostgreSQL  Vault     IdP(OIDC)  ServiceNow   SIEM
   (Linux,     바이저·    (HA 상태)   (비밀·    ServiceNow Teams·Slack
   Docker)     클라우드 API          SSH CA)              이메일·PagerDuty
                                                          웹훅 구독자
```

## 2.2 외부 인터페이스

| 구분 | 상대 | 방향 | 프로토콜·형식 | 비고 |
|---|---|---|---|---|
| CI 게이트 | CI 러너 | 입력 | CLI 실행, 종료 코드 0·1·2·3·4 | 0 PASS, 2 롤백 완료, 3 롤백 실패·서킷 OPEN·승인 대기·동결·변경 티켓 거부(`watch --server` 포함), 4 HOLD, 1 오류 |
| REST v1 | CI, 기존 연동 | 입력 | HTTP JSON, Bearer | 동결. 예외로 PR #13에서 `POST /v1/deployments`에 v2와 같은 게이트 필드(`change_ticket`, `freeze_override`)와 게이트 거부 `code`를 더했다 |
| REST v2 | CI, 콘솔, 사내 시스템 | 입력 | OpenAPI 3.1(`api/openapi.yaml`), problem+json | 경로 54개(v1 16, v2 35, 관측 3) |
| 수신 웹훅 | GitHub·GitLab·Jenkins | 입력 | `POST /v1/webhooks/{provider}`, HMAC·토큰 | |
| 에이전트 push | `vigilante agent` | 입력 | `POST /v1/samples`, 하트비트 | 토큰 + 선택적 클라이언트 인증서 |
| 콘솔 | 브라우저 | 입력 | `/console/` 정적 UI, v2 API, SSE | OIDC code + PKCE |
| 관측성 | Prometheus, LB 헬스 체크 | 입력 | `/metrics`, `/healthz`, `/readyz` | 노드가 직접 응답(리더로 전달하지 않음) |
| 대상 호스트 | Linux 서버, Docker·Podman | 출력 | SSH(bastion 경유 가능), stream-local 소켓 터널 | docker.sock, HAProxy 소켓 터널링 |
| 트래픽 제어 | Nginx·HAProxy·Envoy(SSH), F5 iControl REST, AWS ELBv2, Octavia v2 | 출력 | SSH, HTTPS | |
| 가상화·클라우드 | vCenter(govmomi), Nutanix Prism v2, KVM(virsh over SSH), OpenStack(Keystone v3·Nova·Cinder·Glance) | 출력 | HTTPS, SSH | |
| 상태 저장소 | PostgreSQL | 출력 | pgx | HA 시 필수 |
| 비밀 | HashiCorp Vault | 출력 | KV v2, SSH CA 서명 | |
| 인증 | 사내 IdP | 출력 | OIDC discovery·JWKS | |
| ITSM | ServiceNow | 출력 | Table API | 변경 티켓 게이트, 인시던트, 작업 노트 |
| 알림 | Slack·Teams·SMTP·PagerDuty·일반 웹훅 | 출력 | HTTPS, SMTP | |
| 이벤트 구독 | 사내 시스템 | 출력 | CloudEvents 1.0, Standard Webhooks 서명 | |
| SIEM | syslog 수집기 | 출력 | RFC 5424(JSON 본문) 또는 CEF, TCP·UDP·TLS(RFC 5425, 기본 6514) | TLS는 PR #13 |

---

# 3. 논리 아키텍처

## 3.1 구성 개요

```
┌──────────────────────────── VIGILANTE ORCHESTRATOR ────────────────────────────┐
│ 인터페이스 계층: CLI(cmd/vigilante) · REST v1/v2(api) · 콘솔(console) · 이벤트(events) │
├────────────────────────────────────────────────────────────────────────────────┤
│ 오케스트레이션: orchestrator.Engine                                              │
│   Watch(단계 관측) · Rollback(플랜 실행) · 승인 · 동결 · 변경 게이트 · Resume      │
├───────────────┬───────────────┬────────────────┬──────────────────────────────┤
│ 수집          │ 판정           │ 실행            │ 안전장치                       │
│ probe         │ rules         │ executor       │ safety                       │
│ (Collector)   │ decision      │ (Executor,     │ (Breaker, Guard,             │
│ metrics(TSDB) │ observer      │  Traffic)      │  DrainBatch)                 │
├───────────────┴───────────────┴────────────────┴──────────────────────────────┤
│ 상태: store(file/postgres) · journal(기록·재생·해시 체인) · ha(리더 선출)          │
├────────────────────────────────────────────────────────────────────────────────┤
│ 공통: transport(SSH 풀·세션 예산·sudo·dry-run) · auth · secrets · tlsconf ·       │
│       config · presets · telemetry · notify · itsm · audit                      │
└────────────────────────────────────────────────────────────────────────────────┘
```

## 3.2 구성요소

| 구성요소 | 패키지 | 책임 |
|---|---|---|
| CLI·진입점 | cmd/vigilante | 하위 명령(validate, prepare, baseline, watch, rollback, server, agent, doctor, audit, store 등) 실행, 빌드 변형(full·minimal) |
| 설정 | internal/config, internal/presets, internal/tmpl | YAML 스키마, 기본값, 엄격 디코딩, 교차 참조 검증, 프리셋 전개, 템플릿 |
| 입력 자동화 | internal/cienv | CI 환경변수에서 배포 ID·버전 결정 |
| 수집 | internal/probe | 프로브 플러그인(http, grpc, tcp, host, docker, log, access_log, db)과 Collector |
| 시계열 | internal/metrics | (대상, 메트릭)별 보존 30분 저장소와 윈도 집계 |
| 규칙·판정 | internal/rules, internal/decision | 3값 규칙 평가, 베이스라인, 단계 판정(warmup·창·환경 요인·쿼럼·증거·관측 장치 저하) |
| 관측 장치 가드 | internal/observer | 오케스트레이터 자신을 측정 장비로 감시(스케줄링 지연, 루프백 왕복, 여러 서비스에 걸친 프로브 시간 초과). 저하 구간을 기록하고 판정 엔진이 그 구간의 프로브 실패 기반 위반(직접 재는 프로브만)을 HOLD하게 한다(5.7, PR #13, PR #14) |
| 실행기·트래픽 | internal/executor, internal/dockerapi | 롤백 전략(A symlink, B container, D 스냅샷, 범용)과 트래픽 제어(C) |
| 오케스트레이션 | internal/orchestrator | 배포 수명주기, 롤백 플랜, 승인, 동결, 변경 게이트, 작업(Operation), 멱등 기록, API 클라이언트 |
| 안전장치 | internal/safety | 서킷 브레이커, 서비스 락, 플래핑 제한, blast radius |
| 상태 저장 | internal/store, internal/journal | 저장소 인터페이스, 파일·PostgreSQL 백엔드, 기록 종류, 해시 체인, 재생 |
| HA | internal/ha | lease 기반 리더 선출 |
| 감사 | internal/audit | 체인 검증, 조회, 내보내기, SIEM 전송 |
| API | internal/api | REST v1/v2, OAuth 토큰, 호출 한도, 팔로워 전달, 관측 엔드포인트 |
| 이벤트 | internal/events | CloudEvents 생성, SSE, 웹훅 전달 |
| 콘솔 | internal/console | 내장 정적 UI, OIDC 로그인, 세션 쿠키, CSRF |
| 보안 | internal/auth, internal/secrets, internal/tlsconf, internal/sudoers | 인증·인가, 비밀 해석·가림, TLS 설정, sudoers 규칙 |
| 연동 | internal/notify, internal/itsm | 알림 채널, ServiceNow |
| 에이전트 | internal/agent | 로컬 수집·push·failsafe |
| 운영 도구 | internal/doctor, internal/lab, internal/pilot, internal/support, internal/compat | 사전 점검, 실장비 시나리오, 판정 품질 보고서, 지원 번들, 호환성 매트릭스 |
| 관측성 | internal/telemetry | Prometheus 텍스트 형식 지표 |

## 3.3 데이터 흐름 — 한 번의 카나리 판정

```
 CI: vigilante prepare   ─▶ Executor.Prepare(): 링크 대상·이미지 태그·스냅샷 → 상태 저장소
 CI: vigilante baseline  ─▶ 구버전 측정 → baseline.json
 CI: (자체 도구로 canary 배포)
 CI: vigilante watch --phase canary
   ├─ 게이트: 서킷 OPEN? 변경 동결? 변경 티켓? ── 거부 시 종료 코드 3
   ├─ Collector: (배포 대상 + 대조군) × 프로브 고루틴 → Sample → metrics.Store
   ├─ decision.Engine: eval_interval마다(warmup 이후)
   │     배포 대상만 위반 & for N회 연속 → FAIL(즉시)
   │     대조군도 위반 → HOLD(환경 요인) / 관측점 불일치 → HOLD
   │     관측 장치 저하 중 직접 재는 프로브의 실패 기반 위반 → HOLD(observer_guard)
   │     창 만료: 증거 부족 → on_inconclusive / 위반 없음 → PASS
   ├─ FAIL & mode=auto    → 동결 정책 → 플래핑 → 서킷 Allow → 서비스 락(저장소 장애 시 lease 대기 후 진행)
   │        → 플랜: drain → rollback → verify → probe.verify → enable (대상별, 배치 병렬)
   │        → 실패 시 에스컬레이션, 전부 실패 시 격리 유지 + Breaker.Failure()
   ├─ FAIL & mode=approve → 롤백 계획 + AWAITING_APPROVAL (선택: drain_first)
   └─ 결과: 상태 저장소 기록 → 이벤트·알림·ITSM → 종료 코드
```

## 3.4 상태·이벤트 흐름

엔진의 모든 결정은 `journal.Entry`로 상태 저장소에 기록되고, 기록이 성공한 뒤 등록된 후크(이벤트 버스, SIEM 전송)가 실행된다. 이벤트 버스는 저장된 기록에서 CloudEvents를 파생하므로 엔진 코드가 이벤트 발행을 따로 기억할 필요가 없다.

```
 Engine.record(Entry) ──▶ Store.Append ──성공──▶ 후크: events.Bus.Observe ──▶ SSE 구독자
                              │                       │                   └─▶ 웹훅 전달(리더)
                              │                       ├─ audit Exporter ──▶ SIEM(비동기)
                              │                       └─ itsm 작업자 ─────▶ ServiceNow(리더)
                              └─실패(펜싱 아님)──▶ 메모리 큐(순서 보존) ──▶ flush(백오프 최대 5초)
```

---

# 4. 배포 토폴로지

## 4.1 운영 형태

| 형태 | 명령 | 적합한 상황 | 상태 저장 |
|---|---|---|---|
| CI 게이트(단발) | `vigilante watch ...` | 러너가 대상망에 접근 가능, 서버 운영 불필요 | 공유 경로의 파일 저널 |
| 중앙 서버(단일 노드) | `vigilante server` | 여러 파이프라인·콘솔 공유, 크래시 재개 | 파일 저널 |
| 중앙 서버(HA) | `vigilante server` × 2~3 | 운영 환경 권장 | PostgreSQL |
| 서버 + CLI 위임 | `vigilante watch --server URL` | 러너는 대상망 접근 불가(DMZ·폐쇄망 분리) | 서버 측 |
| 에이전트 | `vigilante agent --server URL --target NAME` | SSH 금지 구간, 고빈도 로그, 오케스트레이터 장애 대비 | 서버 측(failsafe는 로컬) |

## 4.2 단일 노드

- rpm·deb 패키지가 `/usr/bin/vigilante`, systemd 유닛 2개(`vigilante-server`, `vigilante-agent`), `/etc/vigilante/`, 서비스 계정 `vigilante`를 설치한다.
- 기본 설정은 `127.0.0.1:8088`에서만 받는다. 인증 설정 전에는 모든 호출이 익명 admin이기 때문이다.
- 상태는 `/var/lib/vigilante`에 남고 패키지 삭제 시에도 지우지 않는다.

## 4.3 HA(여러 서버 + PostgreSQL)

```
   [CI Runner] ──HTTPS──▶ [노드 A: 리더] ──SSH/API──▶ 대상
        │                   │  ▲ 리더 lease 갱신(TTL/3), 기록은 리더만(펜싱)
        │                   ▼  │
        └──HTTPS──▶ [노드 B: 팔로워] ──요청 전달──▶ 노드 A
                            │
                   [PostgreSQL: vigilante_events · vigilante_leases]
   [에이전트] ── push ──▶ 아무 노드(리더로 전달)
   [앞단 LB] ── 헬스 체크 GET /readyz
```

- 노드는 같은 설정을 쓰고, 노드별 값은 `advertise_url`(또는 `VIGILANTE_HA_ADVERTISE_URL`)뿐이다.
- 리더만 판정·롤백·기록을 하고, 팔로워는 모든 API 요청을 리더로 전달한다.
- 서버가 직접 TLS를 제공하면 advertise URL은 https이다. 팔로워는 `server.ha.tls`(`ca_file`, `server_name`, 클라이언트 인증서 `cert_file`·`key_file`)로 리더 인증서를 검증한다. 파드 IP는 인증서에 거의 없으므로 `server_name`에 Service DNS 이름을 주어 그 이름으로 검증한다(PR #13). 설정이 잘못되면 서버가 시작하지 않는다.
- 스키마는 시작 시 자동 마이그레이션(advisory lock으로 한 번만)하며, DBA 통제가 필요하면 `auto_migrate: false`와 `vigilante store migrate`를 쓴다.

## 4.4 Kubernetes(Helm)

| 구성 | 값 | 비고 |
|---|---|---|
| 단일 노드 | `replicaCount: 1`, 영속 볼륨에 저널 | 배포 전략 `Recreate`(저널 쓰기는 한 프로세스만) |
| HA | `replicaCount: 3`, `persistence.enabled: false`, PostgreSQL DSN Secret | PodDisruptionBudget, 롤링 업데이트 |
| 노드 식별 | 차트가 파드 IP를 `VIGILANTE_HA_ADVERTISE_URL`, 파드 이름을 노드 ID로 주입 | 서버 TLS면 `https://$(POD_IP):8088` |
| 안전장치 | 파일 저장소로 리플리카 2 이상이면 렌더링 거부. `config`에 인증(서비스 계정, OIDC, `server.auth_token_env`)이 없으면 렌더링 거부(개발용은 `auth.allowAnonymous: true`) | 인증 요구는 PR #13. 이전에는 클러스터 안 모든 호출자가 익명 admin이었다 |
| TLS | `config`의 `server.tls.cert_file`로 감지(또는 `tls.enabled`), `tls.secretName`의 Secret을 `/etc/vigilante-tls`에 마운트. advertise URL·프로브·포트 이름·Ingress 백엔드 포트·ServiceMonitor scheme을 https로, `serviceMonitor.tlsConfig` | `server.tls.client_auth: require`는 거부(kubelet 프로브·HA 전달에 클라이언트 인증서 없음). PR #13 |
| 메모리 | 요청 512Mi, 한도 2Gi, `GOMEMLIMIT`은 한도의 90%(`goMemLimit`로 변경·끄기) | 프로브 2만 개 최대 힙 약 1.1 GiB 측정치 기준(PR #13 이전 기본 128Mi/512Mi) |
| 이미지 | distroless static, nonroot, 멀티 아키텍처 | 폐쇄망은 `image.repository` 변경 |

차트 템플릿: deploy/helm/vigilante/templates(configmap, deployment, ingress, pdb, pvc, service, serviceaccount, servicemonitor).

## 4.5 폐쇄망

- 아키텍처별 번들 `vigilante_X.Y.Z_airgap_linux_<arch>.tar.gz`에 바이너리·패키지·이미지 아카이브·차트·SBOM·문서·설치 스크립트를 담는다.
- `install.sh --verify`가 번들 안 모든 파일의 체크섬을 확인하고, 서명 확인은 openssl만으로 된다(공개 투명성 로그 미사용).
- 콘솔은 외부 CDN 없이 바이너리에 내장되어 폐쇄망에서 그대로 동작한다.

## 4.6 에이전트 모드

```
 [대상 호스트]
   vigilante agent ── 로컬 프로브(파일 tail, /proc, docker.sock)
        │ push_interval(1s) 샘플 일괄 전송 (실패 시 최대 50,000개 버퍼)
        │ heartbeat_interval(5s) 생존 확인 + 활성 배포 정보 수신
        ▼
 [vigilante server] ── 중앙(SSH) 샘플과 에이전트 샘플을 source로 구분 저장 → 관측 쿼럼
        ╳ 하트비트가 failsafe_after(30s) 동안 끊기면
 [agent] 로컬 규칙 평가 → failsafe: hold(기록·알림) / rollback(자기 호스트만, 트래픽 단계 제외)
```

에이전트는 서버와 같은 설정 파일을 읽고 같은 프로브·규칙·실행기 코드를 쓴다. 차이는 `transport.Runner`가 SSH가 아닌 Local이라는 점뿐이다. 승인 모드 서비스에서는 failsafe가 rollback이어도 보류한다.

---

# 5. 고가용성 및 장애 복구 설계

## 5.1 리더 선출

| 항목 | 설계 |
|---|---|
| 방식 | PostgreSQL `vigilante_leases` 행의 lease. 키 `leader` |
| TTL | 기본 15s(최소 3s), TTL/3마다 갱신 |
| 시계 | lease 만료는 DB 시계 기준이라 노드 간 시계 차이의 영향을 받지 않는다(docs/01 5) |
| 소유자 | lease 소유자 문자열에 노드 ID와 advertise URL을 함께 담아 팔로워가 리더 주소를 안다 |
| 인계 | 리더가 정상 종료하면 lease를 즉시 놓는다. 비정상 종료 시 TTL 만료 후 인계 |
| 새 리더 | 공유 상태를 다시 읽고(Reload) 중단된 롤백을 `Resume()`으로 이어서 끝낸다 |
| 실측 | TTL 3초 설정에서 리더 강제 종료 후 4.1초 만에 전환(docs/01 5). 기본 TTL 15s의 RTO 실측은 미측정 |

## 5.2 펜싱

리더가 아닌 노드의 기록은 저장소가 `ErrFenced`로 거부한다. 펜싱된 노드는 즉시 활성 상태를 끄고(`SetActive(false)`) 새 판정·롤백 단계를 멈춘다. 리더 교체 순간 이미 대상에 보낸 명령(진행 중이던 한 단계)은 되돌리지 못하므로 새 리더가 그 단계를 한 번 더 실행할 수 있다. 따라서 모든 롤백 단계는 멱등이어야 한다.

## 5.3 크래시 재개

| 상황 | 동작 |
|---|---|
| 단일 노드 재시작 | 저널 재생으로 상태 복원, 진행 중이던 롤백을 `Resume()`으로 재개 |
| HA 리더 장애 | lease 만료 후 새 리더가 Reload 후 Resume |
| 단계 재실행 | 완료 단계(`rollback.step` 기록)는 건너뛴다. `traffic.enable`은 항상 실행(멱등) |
| 승인 대기 | `PendingRollback`이 배포 기록에 남아 재시작 후에도 유지 |
| 장기 작업 | Operation과 멱등 키 기록이 저장소에 남아 리더가 바뀌어도 유지 |

## 5.4 상태 저장소 장애 (M5-4 PR #12, PR #13)

기록 대기열은 커밋 `9f9470e`(PR #12), 롤백 lease 처리는 커밋 `f696bc9`(PR #13)에서 구현했다.

| 항목 | 설계 |
|---|---|
| 동작 | 펜싱이 아닌 기록 실패는 버리지 않고 메모리 큐에 순서대로 보관한다. 큐가 비어 있지 않으면 새 기록도 큐 뒤에 선다(해시 체인 순서 유지) |
| 복구 | 플러셔가 200ms부터 2배씩, 최대 5초 간격으로 재시도해 순서대로 기록한다 |
| 롤백 영향 | 저장소는 롤백 경로가 아니므로 판정·롤백은 계속된다 |
| 한도 | 최대 100,000건. 초과분은 버리고 `vigilante_store_errors_total{reason="dropped"}`로 센다 |
| 종료 | `Close()`는 큐가 빌 때까지 최대 10초 기다린다 |
| 한계 | 큐는 메모리에만 있다. 장애 중 프로세스가 죽으면 큐 내용은 유실된다(복구 후 기록되는 배포 스냅샷이 상태를 다시 담는다). HA에서 장애가 `lease_ttl`보다 길면 리더가 lease를 갱신하지 못해 물러나 판정이 멈추고, 다른 노드가 리더가 되면 큐는 펜싱되어 버려진다(`vigilante_store_errors_total{reason="fenced"}`) |
| 관측 | `vigilante_store_pending_writes` |
| 롤백 lease | 롤백은 서비스 lease(`service:<이름>`)를 잡는다. 저장소에 닿지 않으면 `safety.rollback_lease.wait`(기본 10s) 동안 250ms부터 2배씩(상한 2초) 재시도한다. PR #13 이전에는 lease를 못 잡아 ROLLBACK_FAILED로 끝났다 |
| lease 없이 진행 | `on_unavailable: proceed`(기본): 프로세스 내 락만으로 롤백을 진행하고 배포 이벤트(`safety`), 감사 `lease.unavailable`, 경고 알림을 남긴다. 불량 릴리스가 저장소 장애 동안 계속 사용자를 해치는 것보다 낫고 롤백 단계는 멱등이라는 판단이다. 진행 중에도 5초(또는 TTL/3 중 짧은 값)마다 lease를 다시 시도해 저장소가 돌아오면 lease를 잡는다 |
| 동시 롤백 의심 | 저장소가 돌아왔을 때 다른 프로세스가 lease를 갖고 있으면 감사 `lease.conflict`와 경고 알림("Concurrent rollback suspected")을 한 번 남긴다. 다른 프로세스의 lease는 해제하지 않는다 |
| 엄격 모드 | `on_unavailable: fail`: 재시도 후 롤백을 거부하고 ROLLBACK_FAILED(`state store unreachable`, PR #13 이전 동작) |
| 검증 | `TestChaosStoreOutageDuringRollback`(internal/orchestrator/chaos_test.go): 롤백 중 저장소가 끊겨도 lease 없이 롤백이 계속되고("rolling back under this process's lock only" 이벤트), 복구 후 기록 유실 없이 해시 체인이 유지되며, 재기동 시 재개할 롤백이 남지 않는다. `TestChaosStoreOutageLeaseFailMode`: `fail`이면 ROLLBACK_FAILED. `TestGuardStoreUnreachable`(internal/safety): 재시도 시간, 로컬 배타성, 복구 후 충돌 보고, 복구 후 lease 획득. CI(PR #13 실행 38091832392) 통과 |

## 5.5 외부 의존·자원 장애 격리

| 의존 대상 | 장애 시 동작 | 근거 |
|---|---|---|
| ServiceNow | 게이트는 `on_error: closed`(거부, 503) 또는 `open`(진행, 미검증 표시). 롤백은 ServiceNow를 기다리지 않는다. 인시던트·작업 노트는 연결 오류·429·5xx만 1s·2s·4s 후 재시도하고, 인시던트는 백그라운드로 만들어 느린 ServiceNow가 뒤 작업 노트를 지연시키지 않는다(PR #13) | docs/02 itsm |
| 알림 채널 | 실패는 기록만 하고 롤백을 막지 않는다 | docs/02 notify |
| SIEM | 저장 후 비동기 전송, 큐가 차면 버리고 센다. `audit export`로 보충. TLS 수집기 연결 실패도 같은 처리 | docs/02 audit |
| 상태 저장소 | 기록은 대기열, 롤백 lease는 대기 후 lease 없이 진행(5.4) | docs/04 S12a |
| 오케스트레이터 자신(CPU·소켓·네트워크 과부하) | 관측 장치 가드가 저하를 감지하면 직접 재는 프로브의 실패 기반 위반을 HOLD(5.7) | docs/04 S14a |
| Vault | 그 자격증명이 필요한 프로브·실행기만 실패, 캐시 값은 `cache_ttl` 동안 유지 | docs/02 secrets |
| IdP | OIDC discovery는 서버 시작 시 필요 | docs/02 auth |
| LB 풀 조회 실패 | 보수적으로 배치 크기 1로 롤백 | docs/04 S9 |
| SSH 세션 고갈 | 대상별 `max_sessions`(기본 8) 중 `reserved_sessions`(기본 2)는 롤백·트래픽 변경 전용. 수집은 기다린다 | docs/02 targets |
| 변경 동결 | 자동 롤백은 기본 허용(`allow_rollback`), 금지 기간이면 격리 후 사람에게 | docs/02 change_freeze |

## 5.6 업그레이드

- HA는 팔로워를 먼저, 리더를 마지막에 올린다. MINOR 마이그레이션은 추가만 하므로 이전 버전 리더가 계속 동작한다.
- 호환되지 않는 스키마 변경은 MAJOR에서만 하고 `-- vigilante:breaking` 표시를 붙인다. 이전 바이너리는 시작을 거부한다(다운그레이드 가드).
- 현재 마이그레이션은 `001_init.sql` 한 개이며, 첫 마이그레이션은 감사 기록 보호를 위해 되돌리지 않는다. PR #13의 키 체인 MAC은 기록 JSON 본문의 필드라 마이그레이션이 없다.

## 5.7 관측 장치 과부하 가드 (PR #13, PR #14)

M5-4 부하 시험 중 과부하된 Windows 개발 PC에서 관측 쪽 프로브가 시간 초과되자 이를 대상 장애로 보아 정상 배포 90건을 롤백했다(docs/05 M5-4). 오케스트레이터가 CPU·소켓·네트워크 부족으로 측정을 제대로 못 하면 모든 대상이 한꺼번에 나빠 보이므로, 그 증거로 롤백하지 않도록 `internal/observer`를 두었다(커밋 `f696bc9`).

```
 [observer.Guard] ── 관측 중인 배포가 하나라도 있을 때만 실행(참조 계수)
   ├─ 250ms 타이머가 max_lag(1s)보다 늦게 깨어남        → 스케줄링 지연(CPU 기아, GC, VM 일시정지)
   ├─ 프로세스 내 TCP 에코 왕복 > loopback_timeout(1s)  → 소켓·네트워크 스택 고갈
   └─ 최근 30초 중앙 프로브: 대상의 timeout_share(0.5) 이상이 시간 초과
      이고 그 대상들이 min_services(3)개 이상 서비스에 걸침 → 확산(한 릴리스가 아닌 관측 쪽 문제)
          │ 하나라도 해당 → 저하 구간 기록, vigilante_observer_degraded = 1
          ▼
 [decision.Engine] 규칙이 참이고 조치가 rollback이며, 규칙 지표에 직접 재는 프로브
   (http, tcp, grpc, db, SSH로 읽는 host)의 up, latency_ms, consecutive_failures,
   consecutive_timeouts, timeout 또는 모든 프로브의 probe_error가 있고,
   [now − (최대 window + 최대 for × eval_interval) − grace(1m), now]에 저하 구간이 겹치면
          → FAIL 대신 HOLD("observer degraded … not attributed to the release"), vigilante_observer_holds_total
```

| 항목 | 설계 |
|---|---|
| 기본값 | 켜짐(`safety.observer_guard.disabled: false`). `max_lag` 1s, `loopback_timeout` 1s, `timeout_share` 0.5, `min_services` 3, `grace` 1m |
| 판정 영향 | 대상이 보고한 값(로그·액세스 로그·컨테이너 프로브 지표와 호스트 자원 값)에 기반한 위반은 그대로 FAIL. 지표 이름이 같아도 프로브 유형으로 구분한다: 액세스 로그의 `latency_ms`(서버 자신의 요청 처리 시간)는 판정하고 HTTP 프로브의 `latency_ms`는 HOLD한다. 오케스트레이터가 서비스 설정에서 직접 재는 프로브 ID 목록(`observerProbes`)을 만들어 `decision.Phase.ObserverProbes`로 넘긴다(커밋 `c4f798a`, PR #14. 이전에는 프로브와 무관하게 지표 이름만 보았다). `notify`·`hold` 규칙과 대조군 HOLD는 변화 없음. 관측 창이 끝나면 HELD(종료 코드 4)로 사람에게 넘긴다 |
| 오탐 방지 | 확산 신호는 서비스 3개 이상이 필요하므로 한 서비스의 불량 릴리스만으로는 걸리지 않는다(`TestSpreadNeedsSeveralServices`) |
| 관측성 | `vigilante_observer_degraded`(게이지), `vigilante_observer_degradations_total{signal}`(`scheduling lag`, `loopback`, `spread`), `vigilante_observer_holds_total`, 경고 로그 |
| 한계 | 관측 장치가 저하된 동안에는 프로브 실패만으로 잡히는 진짜 불량 릴리스도 자동 롤백되지 않고 HOLD된다(안전 쪽 선택). 에이전트 샘플은 확산 신호에 쓰지 않는다(중앙 SSH 수집만). 과부하 PC 조건의 재현 시험 기록은 없다 |
| 검증 | `TestObserverDegradedHoldsProbeFailures`, `TestChaosObserverDegradedHolds`, internal/observer 테스트 4건 |

---

# 6. 보안 아키텍처

## 6.1 인증

| 방식 | 토큰 | 저장 | 용도 |
|---|---|---|---|
| 서비스 계정 | `vgl_…` | 설정에 SHA-256만 | CI, 에이전트 |
| OIDC | 사내 IdP JWT | 저장 안 함 | 사용자 |
| OAuth 2.0 client credentials | `vat_…`(기본 1시간, 최대 24h) | 상태 저장소에 SHA-256 | 서버 간 연동 |
| API 키 | `vgk_…` | 상태 저장소에 SHA-256 | 단순 연동 |
| 콘솔 세션 | AES-GCM 암호화 쿠키(ID 토큰) | 서버 상태 없음 | 웹 콘솔 |
| legacy 토큰 | `server.auth_token_env` | 환경변수 | 비상용(break-glass), 호출 한도 미적용 |

`auth`를 설정하지 않으면 인증이 꺼지고(개발용) 시작 시 경고를 남긴다.

**로컬 CLI(PR #13):** `--server` 없이 실행하는 CLI는 상태 저장소에 직접 기록하므로 API의 역할·4-eyes 검사를 거치지 않는다. `auth.local_cli`가 이 경로를 제한한다. `auto`(기본)는 API 인증(서비스 계정, OIDC, `server.auth_token_env`)이 설정되어 있으면 `restricted`, 아니면 `full`로 동작한다. 제한 시 승인 결정(`rollback --approve`·`--reject`), 에스컬레이션 승인(`rollback --approve`), `circuit reset`·`trip`, 동결 중 `--freeze-override`는 `--break-glass REASON`이 있어야 실행되고, 사용은 감사 `breakglass.<action>`과 critical 알림으로 남는다. 로컬 승인 결정에도 `auth.four_eyes`를 적용한다(`--break-glass`면 예외). 평상시 권한 조작은 `--server`와 operator·admin 토큰으로 서버를 거친다.

## 6.2 인가

```
 요청 ─▶ 인증(Principal) ─▶ 역할 grant 판단(역할@범위) ─▶ 스코프 판단(API 클라이언트) ─▶ 허용
                                   │ 거부                       │ 거부
                                   └──────▶ 403 + 감사 기록(action: denied)
```

| 요소 | 값 |
|---|---|
| 역할 | viewer < deployer < operator < admin, 별도 agent |
| 범위 | `*`, `team=<팀>`, `service=<이름>` |
| 스코프 | `deployments:read`, `deployments:write`, `rollbacks:execute`, `approvals:write`, `circuit:admin`, `audit:read`, `metrics:write`, `config:write` |
| 4-eyes | `auth.four_eyes: true`면 배포 생성자·롤백 요청자는 승인·거절 불가 |
| 호출 한도 | 호출자별 토큰 버킷. default(초당 20, 순간 40), emergency(초당 1, 순간 10: 롤백·승인·중단·서킷) |

## 6.3 비밀관리

- 설정에는 비밀 참조만 둔다: `*_ref`(`vault:<mount>/<path>#<key>`, `env:NAME`, `file:/path`) 또는 `*_env`.
- Vault 로그인(token·AppRole·Kubernetes), 만료 30초 전 재로그인, 403 시 1회 재로그인, 메모리 TTL 캐시(기본 5m).
- SSH는 Vault SSH CA로 일회용 ed25519 키를 서명받아 접속할 수 있다(장기 개인키 배포 불필요).
- 해석한 값(6자 이상)은 로그에서 `[REDACTED]`로 가린다. 지원 번들은 설정·로그의 비밀값과 토큰을 제거한다.
- 웹훅 서명 비밀은 마스터 키(`api.webhook_signing_key_ref`)와 구독 ID로 계산하며 저장하지 않는다.

## 6.4 통신 보호

| 경로 | 포트 | 보호 |
|---|---|---|
| API·콘솔·에이전트·CI | 8088 | `server.tls`(TLS 1.2 이상, 1.3 선택), 인증서 파일 변경 시 무중단 교체, 클라이언트 인증서 `optional`·`require`, 또는 TLS 프록시. 콘솔은 HTTPS로 제공될 때 HSTS(`max-age=31536000`, `includeSubDomains` 없음) |
| HA 팔로워 → 리더 | 8088 | https advertise URL이면 `server.ha.tls`(사설 CA, 검증할 `server_name`, `client_auth: require`용 클라이언트 인증서). 없으면 시스템 신뢰 저장소와 URL의 호스트로 검증 |
| SIEM syslog | 6514(TLS), 514 등 | `audit.syslog.address: tls://host[:port]`(RFC 5425 옥텟 카운팅 프레임), `audit.syslog.tls`(`ca_file`, `cert_file`·`key_file`, `server_name`, `min_version` 1.2·1.3). `tcp://`·`udp://`는 평문 |
| 대상 SSH | 22 | 키 또는 SSH CA 단기 인증서, 호스트 키 검증 |
| 장비·클라우드 API | 443 | 전용 계정·최소 역할, 인증서 검증(`tls_skip_verify`는 시험용) |
| Vault | 8200 | AppRole·Kubernetes 인증, 읽기 전용 정책 |
| PostgreSQL | 5432 | 전용 계정, `sslmode=verify-full` 권장 |
| 웹훅 송신 | 443 | `api.webhook_allowed_hosts`로 호스트 제한, 리디렉션 미추종 |

에이전트 mTLS는 `agent.tls`로 지원하지만 인증서 자동 발급·폐기(M5-2)는 구현하지 않았고 사내 CA·cert-manager로 발급한다.

## 6.5 감사 체인

| 항목 | 설계 |
|---|---|
| 기록 | 모든 판정·조치·거부에 actor, source(api·cli·webhook·ui·system), action, 서비스·배포, reason, ticket |
| 무결성 | 각 기록이 직전 기록 해시(`prev`)와 자기 해시(`hash`)를 가진다. 수정·삭제 시 그 지점부터 체인이 끊긴다 |
| 키 체인(선택, PR #13) | 체인만으로는 일관성만 증명한다. 저장소에 쓸 수 있는 사람은 체인을 다시 계산할 수 있기 때문이다. `audit.chain_key_ref`(32바이트 이상, 저장소에 쓰는 모든 노드가 공유)를 설정하면 새 기록마다 `mac` = HMAC-SHA256(키, `hash`)를 붙인다. MAC은 해시 계산에 들어가지 않으며 기록 JSON 본문에 있으므로 스키마 변경이 없다. 키를 설정하면 저장소를 여는 모든 명령이 키를 필요로 한다(`dsn_ref`와 같음) |
| 검증 | `vigilante audit verify`가 위치와 원인을 보고한다. DB 관리자의 직접 SQL 변경도 검출한다. 키가 있으면 MAC도 확인하고(`--key REF`로 대체 가능), 키 도입 전 기록 수(unkeyed)와 키가 보호를 시작한 위치(`keyed_from`)를 보고한다. 보호 시작 뒤 MAC이 없거나 틀린 기록은 손상 |
| 보존 | `audit prune`이 오래된 구간을 아카이브(따로 검증 가능)로 옮기고 앵커 기록으로 대체. 앵커는 마지막 정리 기록의 `hash`와 `mac`을 이어받는다. 자동 삭제는 하지 않는다 |
| 전송 | syslog RFC 5424 또는 CEF, 비동기. TCP·UDP 또는 TLS(PR #13) |

## 6.6 대상 호스트 권한(sudo 범위)

| 설정 | 동작 | 필요한 sudoers |
|---|---|---|
| `sudo: true`, `sudo_scope: all`(기본) | 모든 명령을 `sudo -n sh -c '<명령>'`로 실행 | 사실상 무제한(root와 동일). `validate`·doctor가 경고 |
| `sudo: true`, `sudo_scope: changes`(권장) | 읽기는 sudo 없이, 변경 명령만 하나씩 `sudo -n` | `vigilante sudoers`가 대상별 규칙 생성(명령 경로는 대상에서 확인), doctor가 `sudo -n -l`로 확인 |
| sudo 없음 | 디렉토리·파일 그룹 권한 + `restart_cmd`·`reload_cmd`에 필요한 sudo만 | 운영자가 직접 작성 |

`changes`가 적용되는 실행기·제어기는 symlink, nginx, Envoy, KVM이다(docs/05 M5-3). exec 실행기와 직접 쓴 명령은 쓴 그대로 실행되므로 규칙을 직접 추가한다.

## 6.7 콘솔 보안

- OIDC authorization code + PKCE, ID 토큰을 API와 같은 방식으로 검증, 역할 바인딩이 없는 사용자는 거부.
- 세션: AES-GCM 암호화 HttpOnly 쿠키(`SameSite=Lax`, https면 `Secure`), 최대 12시간. HA 노드는 `console.session_key_ref`를 공유한다.
- CSRF double-submit(`X-CSRF-Token`), CSP(자기 출처만, 인라인 스크립트 없음), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. API 데이터는 텍스트로만 화면에 넣는다.
- 보안 헤더는 정적 파일뿐 아니라 로그인 엔드포인트(`/console/auth/*`)에도 붙는다. 콘솔이 HTTPS로 제공되면(`server.tls`, https `console.redirect_url`, 또는 TLS 요청) `Strict-Transport-Security: max-age=31536000`을 보낸다(PR #13).

## 6.8 실행 환경·공급망

- systemd 유닛: `User=vigilante`, `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome` 등.
- 컨테이너: distroless static, nonroot, 읽기 전용 루트 파일시스템.
- 릴리스: `SHA256SUMS`와 cosign 키 서명, 바이너리별 CycloneDX SBOM, CI의 govulncheck 차단. 신뢰 기준 공개 키는 저장소의 `packaging/cosign.pub`이며, **릴리스 키 쌍은 아직 만들지 않았다**.

---

# 7. 기술 스택 및 의존성

## 7.1 언어·런타임

| 항목 | 값 | 출처 |
|---|---|---|
| 언어 | Go 1.27.0(모듈), 툴체인 go1.27.2 고정 | go.mod |
| 빌드 | `CGO_ENABLED=0` 정적 바이너리 | README |
| 대상 플랫폼 | linux/amd64, linux/arm64, linux/ppc64le, windows/amd64 | README, docs/09 |
| 상태 저장소 | 파일(JSONL), PostgreSQL 16(CI 확인) | docs/09 |
| UI | 바닐라 JS 정적 파일(`go:embed`), 외부 자원 없음 | docs/05 M4 구현 결과 |
| 지표 | Prometheus 텍스트 형식 직접 출력(클라이언트 라이브러리 없음) | docs/05 M0-5 구현 결과 |

## 7.2 직접 의존성 (go.mod require)

| 모듈 | 버전 | 사용처 | 최소 빌드 |
|---|---|---|---|
| github.com/aws/aws-sdk-go-v2 (+config, service/elasticloadbalancingv2) | v1.47.2, v1.33.8, v1.63.3 | 트래픽 제어기 `aws_alb` | 제외 |
| github.com/coreos/go-oidc/v3 | v3.21.0 | OIDC 토큰 검증(auth, console) | 포함 |
| github.com/fergusstrange/embedded-postgres | v1.34.0 | 테스트 전용(pgtest: 내장 PostgreSQL 기동) | 해당 없음 |
| github.com/go-jose/go-jose/v4 | v4.1.4 | 테스트 전용(OIDC 모의 서버) | 해당 없음 |
| github.com/go-sql-driver/mysql | v1.10.1 | DB 프로브 MySQL | 제외 |
| github.com/jackc/pgx/v5 | v5.11.0 | PostgreSQL 상태 저장소, DB 프로브 | 포함 |
| github.com/pb33f/libopenapi, libopenapi-validator | v0.41.3, v0.16.0 | 테스트 전용(v2 계약 테스트) | 해당 없음 |
| github.com/vmware/govmomi | v0.56.0 | 실행기 `vsphere` | 제외 |
| golang.org/x/crypto | v0.57.0 | SSH 클라이언트, SSH CA | 포함 |
| google.golang.org/grpc | v1.84.0 | gRPC 프로브 | 제외 |
| gopkg.in/yaml.v3 | v3.0.1 | 설정·프리셋 파싱 | 포함 |

간접 의존성 중 golang.org/x/net은 govulncheck 결과에 따라 v0.60.0으로 올렸다(docs/05 M8 구현 결과). F5, Nutanix, OpenStack, Docker, Vault, ServiceNow 클라이언트는 외부 SDK 없이 표준 라이브러리로 직접 구현했다.

## 7.3 빌드 변형

| 변형 | 빌드 태그 | 포함 플러그인 | 크기(docs/05 기록) |
|---|---|---|---|
| 전체 빌드 | 없음 | 모든 플러그인 | 38MB |
| 최소 빌드 | `minimal` | vSphere, AWS ALB, gRPC 프로브, MySQL 드라이버 제외 | 18MB |

최소 빌드에서 빠진 플러그인을 쓰는 설정은 시작 시 이름을 들어 거부한다(`TestMinimalBuildRefusesMissingPlugins`).

## 7.4 CI·릴리스 도구

| 도구 | 용도 |
|---|---|
| GitHub Actions `ci.yml` | test(gofmt, vet, `go test -race`, PostgreSQL 16, 교차 빌드), api(Spectral 6.15.0, oasdiff), vuln(govulncheck), package(최소 빌드 테스트, 산출물, 차트 렌더링: 단일·HA·TLS, 인증 없는 설정 거부 확인, 이미지, 번들·서명 확인, deb·rpm 설치), demo(E2E) |
| GitHub Actions `load.yml` | 부하 하네스(`test/load`, 빌드 태그 `load`): 대상 2,000 × 프로브 3·10, 동시 배포 100. 주 1회, 수동, 엔진·하네스 변경 PR(PR #12에서 추가) |
| GitHub Actions `release.yml` | 태그 푸시 시 테스트, 빌드, 이미지 푸시, 번들, 서명, 게시 |
| scripts/release.sh | 바이너리·SBOM·rpm·deb(nfpm)·차트·번들·`SHA256SUMS` 생성 |

---

# 8. 품질 속성

| 품질 속성 | 목표(docs/05, 제안값) | 설계 수단 | 검증 현황 |
|---|---|---|---|
| 판정 정확성 | 파일럿 오탐 0, 미탐 0, HOLD 10% 이하 | 3값 논리, 연속 위반·히스테리시스, 베이스라인 + `min_value`, 대조군, 관측 쿼럼, 증거 충분성, 관측 장치 가드(5.7) | 단위·카오스 테스트 완료. 실제 서비스 품질은 미측정(M8 파일럿) |
| 판정 지연 | 위반부터 롤백 시작까지 `eval_interval × for + 5초` | 위반 즉시 FAIL, 고루틴별 수집, 지연 지표 | 부하 하네스(대상 2,000, 동시 배포 100, 목표 12초). M5-4 기록: 프로브 3개 p50 5.4초·p99 5.5초, 프로브 10개 p99 5.7초. PR #13 실행(관측 장치 가드 켜짐): 프로브 3개 p50 5.16초·p99 5.32초, 프로브 10개 p50 5.73초·p99 5.96초. 데모 환경에서 탐지 약 3초·롤백 약 1초. 실서비스 미측정 |
| 가용성 | 99.9%, RTO 30초 | PostgreSQL lease 리더 선출, 팔로워 전달, Resume | TTL 3초에서 4.1초 전환 기록. 가용성 수치 미측정 |
| 데이터 내구성 | RPO 0 | 기록 후 실행, fsync·DB 커밋, 펜싱, write-behind 큐(PR #12), 선택적 키 체인(PR #13) | `TestChaosStoreOutageDuringRollback` 통과. 저장소 장애 중 크래시, HA에서 `lease_ttl`보다 긴 장애 후 리더 교체 시 큐 유실 가능 |
| 안전성 | 롤백이 장애를 키우지 않음 | 서킷 브레이커, blast radius, 플래핑 제한, 서비스 락, 보상 트랜잭션, 격리 유지 | 단위·통합 테스트, E2E 데모 |
| 규모 | 대상 2,000대, 동시 배포 100건, 대상당 프로브 10개 | 대상당 SSH 연결 1개, 세션 예산, 로그 1초 버킷, 낮은 카디널리티 지표 레이블 | 부하 하네스(GitHub Actions ubuntu-latest, 단일 엔진, HTTP 시뮬레이터). M5-4 기록: 프로브 3개 오판 0·2,804 요청/초·최대 힙 352 MiB·0.45코어, 프로브 10개 오판 0·9,095 요청/초·최대 힙 1.1 GiB·0.72코어. PR #13 실행: 프로브 3개 오판 0·2,821 요청/초·최대 힙 388 MiB·0.34코어, 프로브 10개 오판 0·9,100 요청/초·최대 힙 1,077 MiB·1.24코어. sshd 부하 미측정, 샤딩(M5-1) 미구현 |
| 보안 | TLS 1.2+, 평문 비밀 금지, 최소 권한, 감사 | 6장(PR #13: 로컬 CLI 제한, 키 체인, SIEM·HA 전달 TLS, HSTS, Helm 인증 필수) | 테스트 완료. 외부 보안 점검 기록 없음 |
| 이식성 | 정적 바이너리 4종, 폐쇄망 | CGO 없음, 내장 UI·시간대, 번들 | CI 교차 빌드·패키지 설치 |
| 유지보수성·호환성 | SemVer, v2 추가만 | 명세 원본, 계약 테스트, oasdiff, 마이그레이션 규칙 | CI api |
| 운영성 | 자체 관측, 사전 점검 | `/metrics`, `/readyz`, doctor, support-bundle | 테스트 완료 |
| 신뢰성(장비) | 1차 플러그인 전부 "검증됨" | 호환성 매트릭스, lab 도구 | 검증됨은 http·access_log·log 프로브와 webhook 실행기뿐. 나머지 M8 |

---

# 9. 아키텍처 결정

| 번호 | 결정 | 대안 | 이유 | 출처 |
|---|---|---|---|---|
| AD-01 | Agentless(SSH·API) 우선 + 선택형 경량 에이전트, 같은 코드 경로 | 에이전트 필수 | 대상 설치물 0, 레거시·어플라이언스 동일 방식. 에이전트는 SSH 금지 구간·고빈도 로그·dead-man's switch용 | docs/01 1 |
| AD-02 | 단일 정적 Go 바이너리, 컴파일 타임 플러그인 등록 | 동적 플러그인, 마이크로서비스 | 폐쇄망·레거시 배포 용이, CGO 없음 | docs/03 1.1 |
| AD-03 | 내장 시계열 저장소 | 외부 TSDB·APM | 외부 스택 없이 분 단위 관측 창에 충분 | docs/03 1.3 |
| AD-04 | 상태 저장소 PostgreSQL | etcd | pgx 의존 이미 존재, 운영 친숙도 | docs/05 결정 필요 사항 |
| AD-05 | lease 행 기반 리더 선출 + 펜싱 | 외부 합의 시스템 | 저장소 하나로 해결, 기록 수준 펜싱 | docs/05 M0-1 |
| AD-06 | 인증 OIDC + 서비스 계정 토큰 | LDAP/AD 직접 연동 | 사내 SSO 재사용 | docs/05 결정 필요 사항 |
| AD-07 | 비밀관리 HashiCorp Vault(`*_ref`) | CyberArk, env만 | 결정 사항. CyberArk는 같은 참조 형식으로 추후 | docs/05 M0-3 |
| AD-08 | 기존 키 옆에 `*_ref` 추가 | `password: {secret: ...}` 객체 | 기존 설정과 하위 호환 | docs/05 M0-3 구현 결과 |
| AD-09 | API 인증 OAuth 2.0 client credentials + API 키 | API 키만, mTLS 인증서 | 표준 OAuth 라이브러리 사용 가능, 단순 연동 병행 | docs/05 결정 필요 사항 |
| AD-10 | 명세 원본(spec-first) + 손으로 쓴 핸들러 + 계약 테스트 | oapi-codegen 코드 생성 | 계약 테스트로 명세·구현 일치 강제(구현 결과) | docs/05 M3-1 구현 결과 |
| AD-11 | 오픈 API 사내 전용, 직접 노출 + 내장 호출 한도 | 파트너·외부 공개, APIM 뒤 배치 | 약관·쿼터·지원 체계 불필요 | docs/05 결정 필요 사항 |
| AD-12 | 이벤트를 저장된 기록에서 파생, 클러스터 순번 | 엔진에서 직접 발행 | 발행 누락 방지, 리더 교체 후 순번·커서 유지 | internal/events 주석 |
| AD-13 | 웹훅 비밀을 마스터 키에서 파생 | 구독별 비밀 저장 | 저장소에 비밀이 남지 않음 | docs/05 M3-3 구현 결과 |
| AD-14 | 콘솔 세션을 서버 상태 없는 암호화 쿠키로 | 서버 세션 저장소 | HA 어느 노드나 처리 | docs/05 M4 구현 결과 |
| AD-15 | 초기 운영 모드 승인 모드, 미지정은 `auto` 해석 + 경고 | 처음부터 자동, 일괄 `approve` 전환 | 도입 심사 통과와 기존 CI 파이프라인 호환 | docs/05 결정 필요 사항 |
| AD-16 | OpenStack 클라이언트를 표준 라이브러리로 직접 구현 | gophercloud | 필요한 API가 일부라 의존성·크기 증가 회피 | docs/05 M7 구현 결과 |
| AD-17 | OpenStack 복원: 볼륨 부팅 Cinder revert, 이미지 부팅 Nova rebuild | 새 인스턴스 생성 후 교체 | IP 변경 부담 회피 | docs/05 결정 필요 사항 |
| AD-18 | 서명 cosign 키 방식, 투명성 로그 미사용 | GPG, Rekor 사용 | 사내 릴리스, 폐쇄망에서 openssl로 확인 | docs/05 M6 구현 결과 |
| AD-19 | 최소 빌드는 플러그인 4개만 분리 | 전 플러그인 태그 분리 | 나머지는 표준 라이브러리 구현이라 크기 차이 작음 | docs/05 M6 구현 결과 |
| AD-20 | `sudo_scope` 기본값 `all` 유지, `changes` 권장 | 기본값 `changes` | 하위 호환 | docs/05 M5-3 구현 결과 |
| AD-21 | ITSM ServiceNow 1종 | Jira SM, 웹훅 | 1차 범위 결정 | docs/05 결정 필요 사항 |
| AD-22 | 저장소 장애 시 메모리 write-behind 큐 | 기록 실패 시 버림(이전 동작), 디스크 큐 | 롤백을 막지 않으면서 기록 유실 방지(메모리 한계 있음) | 커밋 `9f9470e`, PR #12 |
| AD-23 | 관측 장치 저하 시 직접 재는 프로브의 실패 기반 위반을 HOLD(기본 켜짐) | 그대로 FAIL(이전 동작), 관측 전체 중단 | 측정 장비 고장을 릴리스 탓으로 돌려 정상 릴리스를 대량 롤백하는 것을 막는다. 대상이 보고한 로그·액세스 로그·컨테이너 지표는 이름이 같아도 계속 판정(프로브 유형 구분은 PR #14) | 커밋 `f696bc9`, PR #13, 커밋 `c4f798a`, PR #14 |
| AD-24 | 저장소 장애 중 롤백은 lease 대기 후 lease 없이 진행(`proceed` 기본) | 롤백 거부(`fail`, 이전 동작) | 저장소는 롤백 경로가 아니며(P1) 롤백 단계는 멱등. 동시 실행 위험은 감사·알림으로 드러낸다 | 커밋 `f696bc9`, PR #13 |
| AD-25 | 감사 체인 키를 HMAC으로 기록 본문(`mac`)에 저장 | 별도 열·테이블, 비대칭 서명 | 스키마 마이그레이션 없이 파일·PostgreSQL 공통, 체인 해시 형식 유지(키 도입 전 기록과 공존) | 커밋 `c9e39f1`, PR #13 |
| AD-26 | 로컬 CLI 권한 조작을 인증 환경에서 기본 제한(`auto`) + `--break-glass` | 로컬 CLI 허용 유지, 로컬 CLI 완전 금지 | 저장소 직접 접근이 RBAC·4-eyes를 우회하던 문제를 막으면서 서버 장애 시 비상 수단을 남긴다 | 커밋 `313f651`, PR #13 |

---

# 10. 아키텍처 위험과 한계

| 항목 | 내용 | 대응 |
|---|---|---|
| 실장비 미검증 | 실행기·트래픽 제어기 대부분이 실험적 | M8 실장비 랩, `lab run` |
| 판정 품질 미측정 | 프리셋 임계치는 추정값 | M8 파일럿, `pilot report` |
| 규모 | 단일 수집 프로세스, 샤딩 없음. 부하 하네스는 HTTP 시뮬레이터 대상이라 sshd 부하는 재지 않았다 | 목표 규모(2,000 × 10)는 단일 엔진으로 통과(PR #12, PR #13 재실행). 서버 크기는 부하 결과(프로브 2만 개에 힙 약 1.1 GiB)를 기준으로 여유 있게 잡는다(Helm 기본 2Gi 한도). 샤딩은 M5-1 |
| 관측 장치 과부하 | 관측 쪽(서버)이 과부하되면 프로브가 시간 초과된다. 과부하된 Windows 개발 PC에서 프로브 10개 규모를 돌리자 정상 배포 90건이 롤백되었다 | 관측 장치 가드(5.7, PR #13, PR #14)가 저하 중 직접 재는 프로브의 실패 기반 위반을 HOLD한다. 저하 동안 진짜 불량 릴리스도 자동 롤백되지 않는 대가가 있고, 과부하 PC 조건의 재현 시험 기록은 없다 |
| lease 없는 롤백 | 저장소 장애 중 `proceed`로 진행한 롤백은 다른 프로세스의 같은 서비스 롤백과 겹칠 수 있다 | 롤백 단계 멱등, `lease.unavailable`·`lease.conflict` 감사·알림. 엄격히 막아야 하면 `on_unavailable: fail` |
| write-behind 큐 | 메모리 전용, 크래시 시 유실. HA에서 `lease_ttl`보다 긴 장애 후 리더가 바뀌면 펜싱되어 버려짐 | `vigilante_store_pending_writes` 감시, 필요 시 설계 보강 |
| 파일 저장소 | 단일 노드용, lease는 같은 파일시스템 프로세스 사이에서만 배타적 | 다중 노드는 PostgreSQL + HA |
| 에이전트 failsafe | LB에 접근하지 않음 | 오케스트레이터 복구 후 격리 처리 |
| 서명 키 | 릴리스 키 쌍 미생성 | 릴리스 관리자 결정(docs/07) |
| Linux 전용 호스트 프로브 | AIX·Solaris·HP-UX 미지원 | `exec` 기반 프로브 추가 필요 |
