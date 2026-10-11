---
title: 상세 설계서
doc_id: VGL-SI-03
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

이 문서는 Vigilante의 패키지별 책임·주요 타입·협력 관계와, 배포 상태 머신, 판정 알고리즘, 롤백 플랜 실행, 안전장치, 이벤트·웹훅, 운영 콘솔, 릴리스·패키징 파이프라인의 상세 설계를 기술한다. 시스템 수준 구조는 VGL-SI-02(아키텍처 설계서), 요구사항은 VGL-SI-01(요구사항 정의서)을 따른다.

## 1.2 기준

- 코드 기준: master 커밋 `537870c`(PR #12 `dba3dbe`, PR #13 `dd9a055`, PR #14 `537870c` 병합). Go 모듈 이름은 `vigilante`이다.
- 기본값은 internal/config/validate.go의 기본값 처리와 docs/02-config-spec.md를 기준으로 한다.
- 함수·타입 이름은 코드에 존재하는 것만 적었다. 테스트 실행 결과: `dd9a055`에서 로컬(Windows) 277개 통과·0개 실패(최상위 239, 하위 38. 42개 패키지 중 32개에 테스트), 최소 빌드 테스트 59개 통과. CI는 PR #13 헤드 `189dfbb`에서 test·api·vuln·package·demo(실행 38091832392)와 load 워크플로우(실행 38091832358)가 모두 통과했고, master `dd9a055` 푸시 CI(실행 38092384199)도 통과했다. PR #14는 CI 실행 38093154231(test·api·vuln·package·demo)과 38093154321(load), master `537870c` 푸시 CI(실행 38093673759)가 통과했고, 기존 테스트에 확인을 더해 테스트 수는 그대로이다.

---

# 2. 패키지 구성

## 2.1 의존 관계

```
cmd/vigilante ──▶ orchestrator ──▶ decision ──▶ rules ──▶ metrics
     │               │  │  │                       ▲
     │               │  │  └──▶ probe ─────────────┘ (Sample → Store)
     │               │  │         └──▶ transport, dockerapi, tmpl
     │               │  └──▶ executor ──▶ transport, dockerapi, secrets
     │               ├──▶ observer (decision.Observer 구현, 프로브 샘플 수신)
     │               ├──▶ safety, store ──▶ journal ──▶ model
     │               ├──▶ notify, itsm, telemetry, presets, config
     ├──▶ api ──▶ orchestrator, auth, events, console, audit
     ├──▶ ha ──▶ store
     ├──▶ agent ──▶ orchestrator, probe, rules
     └──▶ doctor, lab, pilot, support, sudoers, compat, audit, cienv
 공통 하위: config, model, telemetry, secrets, tlsconf
```

## 2.2 패키지 목록

| 영역 | 패키지 |
|---|---|
| 진입점 | cmd/vigilante |
| 설정·입력 | internal/config, internal/presets, internal/cienv, internal/tmpl |
| 수집 | internal/probe, internal/metrics, internal/transport, internal/dockerapi |
| 판정 | internal/rules, internal/decision, internal/observer |
| 실행 | internal/executor |
| 오케스트레이션·안전 | internal/orchestrator, internal/safety |
| 상태·HA·감사 | internal/store, internal/journal, internal/ha, internal/audit |
| API·이벤트·콘솔 | internal/api, internal/events, internal/console, internal/model |
| 보안 | internal/auth, internal/secrets, internal/tlsconf, internal/sudoers |
| 에이전트·연동 | internal/agent, internal/notify, internal/itsm |
| 운영·검증 도구 | internal/doctor, internal/lab, internal/pilot, internal/support, internal/compat |
| 관측성 | internal/telemetry |

---

# 3. 패키지 상세

## 3.1 진입점 — cmd/vigilante

**책임:** 하위 명령 해석과 실행, 종료 코드 반환, 로깅(`VIGILANTE_LOG`, `VIGILANTE_LOG_FORMAT`), 빌드 변형 표시(`flavor_full.go`/`flavor_minimal.go`, 빌드 태그 `minimal`).

| 하위 명령 | 기능 | 구현 파일 |
|---|---|---|
| `validate` | 설정 검증, 실험적 플러그인 경고 | main.go |
| `prepare`, `baseline`, `watch`, `mark-good`, `rollback`, `status` | 배포 수명주기(로컬 엔진 또는 `--server` 원격). `rollback --approve`·`--reject`는 로컬 승인 결정 | main.go, inputs.go, watchremote.go(원격 `watch`) |
| `circuit status/reset/trip` | 서킷 조회·리셋·차단. 로컬 reset·trip은 `auth.local_cli` 제한 대상 | main.go, authcmd.go |
| `server` | REST·콘솔·이벤트, HA 시작 | main.go, server_ha.go |
| `agent` | 에이전트 모드 | main.go |
| `doctor` | 읽기 전용 사전 점검, `--json`·`--junit` | doctor.go |
| `presets` | 프리셋 목록·전개 미리보기 | presets.go |
| `token create`, `whoami` | 서비스 계정 토큰 발급, 신원 확인 | authcmd.go |
| `audit verify/export/prune/query` | 감사 체인 | auditcmd.go |
| `store status/migrate` | PostgreSQL 스키마 | storecmd.go |
| `feedback`, `pilot report` | 판정 평가, 판정 품질 보고서 | pilotcmd.go |
| `lab run/summary` | 실장비 시나리오 | labcmd.go |
| `sudoers` | sudoers 규칙 생성 | sudoerscmd.go |
| `support-bundle` | 진단 zip | supportcmd.go |
| `plugins`, `version` | 플러그인 검증 수준, 빌드 정보 | main.go, build.go |

**주요 함수:** `run`(명령 분기), `resolveInputs`(inputs.go: `--id`·`--version`은 CI 환경, `--previous`는 저널의 마지막 성공 배포), `startHA`(server_ha.go: `tlsconf.HAClient(server.ha.tls)`로 전달용 TLS 설정을 만들고 실패하면 서버 시작 실패, 수동·펜싱 상태로 시작해 선출되면 Reload 후 Resume), `localPrivileged`·`localFourEyes`(authcmd.go, 7.6), `watchRemote`·`apiRequest`·`remoteExitCode`(watchremote.go, 4.3), `cliAudit`(auditcmd.go: 출처 `cli`, 작업자 `cliActor()`로 감사 기록).

## 3.2 설정·입력

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/config | YAML 스키마, 기본값, 엄격 디코딩(알 수 없는 키 거부), 교차 참조 검증, 동결 창 계산 | `Config`, `Service`, `Rule`/`Node`/`Condition`, `PhaseConfig`, `Rollback`, `Step`, `CircuitBreaker`, `BlastRadius`, `Flapping`, `Server`, `HA`, `Auth`, `Credential`, `Target`, `Connection` | presets(프리셋 전개), 모든 패키지가 참조 |
| internal/presets | 내장 프리셋 4종(`go:embed`, builtin/*.yaml)과 조직 프리셋 디렉토리, `name@version` 해석, `[[ ]]` 템플릿 렌더링 | `Preset`(`Ref`, `Render`, `Preview`), `Registry`(`NewRegistry`, `Get`, `List`), `Param` | config |
| internal/cienv | CI 환경변수에서 배포 ID·버전 결정, 출처 기록 | `DeploymentID`, `Version`, `GitDescribe`, `Value` | cmd/vigilante |
| internal/tmpl | 설정 값의 Go 템플릿(`{{.Address}}`, `{{.PreviousVersion}}`, `{{.Checkpoint.KEY}}`, `{{env "NAME"}}`) 렌더링. 없는 라벨은 오류 | `Data`, `ForTarget`, `Render` | probe, executor, orchestrator |

## 3.3 수집

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/probe | 프로브 플러그인과 수집 감독. 유형: http, grpc(전체 빌드), tcp, host, docker, log, access_log, db(MySQL은 전체 빌드) | `Probe`(`Run`), `Checker`(`Check`, probe.verify용), `Factory`, `Register`, `New`, `Collector`(`Build`, `Run`), `Job`, `RemoteFollowCmd`, `ParseAccessLine` | transport(원격 실행), dockerapi, metrics(샘플 저장), tmpl |
| internal/metrics | (대상, 메트릭)별 시계열. 보존 30분, 최대 200,000점, 윈도 질의, 관측점별 질의 | `Store`(`Add`, `Window`, `Sources`, `Count`, `Metrics`), `Aggregate`, `Percentile`, `IsCounterAgg` | rules, decision |
| internal/transport | 실행 추상화. SSH 연결 풀(대상당 1개, 명령마다 세션), bastion, stream-local 소켓 터널, 세션 예산, sudo 접두사, dry-run 래퍼, 로컬 실행(Windows 인자 따옴표 처리) | `Runner`(`Run`, `Stream`, `Dial`), `Manager`(`ForTarget`), `SSH`, `Local`, `DryRun`, `Mock`, `Urgent`/`IsUrgent`, `Sudo`, `ShellQuote` | secrets(SSH CA), probe, executor |
| internal/dockerapi | Docker Engine API 최소 클라이언트(Podman 호환). 다이얼러 주입으로 SSH 터널 소켓 사용 | `Client`(`Inspect`, `Stop`, `Start`, `Rename`, `Remove`, `Create`, `ImageExists`, `Pull`, `Events`), `CloneCreateBody` | probe(docker), executor(container) |

**수집 동시성:** `Collector.Run`은 (대상 × 프로브)마다 고루틴 하나를 띄우고, 프로브가 오류로 끝나면 `probe_error`를 남긴 뒤 1초부터 최대 30초까지 지수 백오프로 재시작한다(1분 이상 정상 동작 후 리셋). 로그 계열은 1초 버킷으로 집계하고 지연 원시값은 초당 256개로 리저버 샘플링한다(docs/03 1.2).

**SSH 세션 예산(sessions.go):** 대상마다 `max_sessions`(기본 8) 세마포어를 두고 `reserved_sessions`(기본 2)는 `transport.Urgent` 컨텍스트(롤백 단계, 트래픽 드레인·복귀, 승인 모드의 격리·복귀)만 쓴다. 수집은 실패하지 않고 기다리며 `vigilante_ssh_session_waits_total{priority}`로 보인다.

## 3.4 판정

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/rules | 규칙 트리의 3값 평가, 연속 카운터, 베이스라인 | `Evaluator`(`Eval`, `Value`), `Result`, `Tri`(True·False·Unknown), `Scope`, `Baseline` 인터페이스, `Snapshot`(`Capture`, `Save`, `LoadSnapshot`), `Control`, `Chain`, `Metrics`, `BaselineConditions` | metrics |
| internal/decision | 단계 판정: warmup, 관측 창, 환경 요인, 관측 쿼럼, 증거 충분성, 관측 장치 저하 시 HOLD | `Phase`(PR #14부터 필드 `ObserverProbes`: 직접 재는 프로브 ID 맵, nil이면 모든 프로브), `Engine`(`New`, `Evaluate`, `Run`, 필드 `Observer`), `Observer` 인터페이스(`Degraded(from, to) (bool, string)`), `Evaluation`(Fail·Hold·Warn·Known·Unknown), `Outcome` | rules, metrics, telemetry |
| internal/observer | 오케스트레이터 자신의 측정 신뢰성 감시(관측 장치 가드, PR #13). 스케줄링 지연·루프백·프로브 시간 초과 확산으로 저하 구간을 기록 | `Guard`(`New`, `Enabled`, `Start`, `Observe`, `Degraded`, `Report`, `Clear`, `Spread`), 내부 `loopback` | config(`safety.observer_guard`), model(Sample), telemetry |

알고리즘은 5장에 기술한다(관측 장치 가드는 5.8).

## 3.5 실행 — internal/executor

**책임:** 롤백 전략(실행기)과 트래픽 제어기의 등록·생성, 체크포인트, 사전 진단, sudo 규칙 선언.

| 인터페이스 | 메서드 | 설명 |
|---|---|---|
| `Executor` | `Rollback(ctx, rc)`, `Verify(ctx, rc)` | 롤백은 반드시 멱등, Verify는 버전 사실 확인 |
| `Preparer` | `Prepare(ctx, rc) (map[string]string, error)` | 배포 전 체크포인트 |
| `TrafficController` | `MemberID`, `Pool`, `Drain`, `Enable` | `Pool`은 관리 외 멤버까지 반환(blast radius 계산) |
| `Diagnoser`, `TrafficDiagnoser` | 진단 결과 `Finding` | doctor가 사용 |
| `SudoRules`, `TrafficSudoRules` | `SudoRule` 목록 | `sudo_scope: changes`의 규칙 선언 |

| 유형 | 파일 | 동작 요지 | 검증 수준 |
|---|---|---|---|
| symlink | symlink.go | `test -d` 선확인 → `ln -sfn` 임시 링크 → `mv -Tf`(원자적) → 재시작 | 실험적 |
| container | container.go | 구 컨테이너 stop → rename → 동일 설정으로 이전 태그 생성·기동, 실패 시 원상 복구(보상), 이미 이전 이미지면 no-op | 실험적 |
| vsphere | vsphere.go(전체 빌드) | govmomi 스냅샷 생성·되돌리기 + 전원 확인 | 실험적(vcsim) |
| nutanix | hypervisor.go | Prism v2 스냅샷 → restore → 전원 ON, 태스크 폴링 | 실험적 |
| kvm | hypervisor.go | `virsh snapshot-create-as --atomic` / `snapshot-revert --running` | 실험적 |
| openstack | openstack.go, openstack_client.go | 볼륨 부팅: 정지 → Cinder revert(3.40) → 기동, 이미지 부팅: Nova rebuild. 재실행 안전, 최신 `keep_snapshots`(기본 3)개 유지 | 실험적 |
| exec, webhook | generic.go | 임의 명령, 외부 배포 콘솔 호출 | exec 실험적, webhook 검증됨 |
| nginx, haproxy, envoy | traffic_sw.go | upstream `down` 토글 + `nginx -t` 실패 시 백업 복원 / Runtime API drain / EDS `DRAINING` + 원자적 mv | 실험적 |
| f5, aws_alb | traffic_hw.go, awsalb.go(전체 빌드) | iControl REST session·state / Deregister·Register + 상태 대기 | 실험적 |
| octavia | octavia.go | `admin_state_up` 변경, LB `ACTIVE` 대기, 409 재시도, 순차 적용 | 실험적 |

**주요 함수:** `Register`, `RegisterTraffic`, `New`, `NewTraffic`, `Types`. `RunContext`는 대상, 템플릿 데이터, 대상 Runner, 다른 대상 Runner 조회, 자격증명, `DryRun`을 담아 실행기가 실행 위치(SSH·로컬)를 모르게 한다.

## 3.6 오케스트레이션·안전

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/orchestrator | 배포 수명주기와 롤백 플랜 실행의 중심. 상태 기록, 승인, 동결, 변경 게이트, Operation·멱등 기록, API 클라이언트·토큰 | `Engine`(`New`, `Reload`, `Create`, `Prepare`, `CaptureBaseline`, `Watch`, `Rollback`, `Resume`, `Abort`, `DecideRollback`, `ExpireApprovals`, `Gate`, `FreezeGate`, `ChangeGate`, `MarkGood`, `LastGoodVersion`, `SetFeedback`, `StartOperation`, `Idempotent`, `IssueToken`, `Record`, `Audit`, `PendingWrites`, `Flush`, `Close`), 필드 `Observer *observer.Guard`, 내부 `rollbackUnleased`·`rollbackLeaseConflict`·`leaseNotice`(7.4), `RollbackOptions`, `ErrNeedsApproval` | decision, probe, executor, safety, store, journal, notify, itsm, observer, telemetry |
| internal/safety | 서킷 브레이커, 서비스 락(lease), 플래핑 제한, blast radius 계산 | `Breaker`(`Allow`, `Success`, `Failure`, `Trip`, `Reset`, `State`), `CircuitState`, `Guard`(`Acquire`, `Check`, `Record`, 필드 `LeaseWait`, `ProceedUnleased`, `Unleased`, `Conflict`, `TTL`), `Leases`, `DrainBatch`, 오류 `ErrCircuitOpen`·`ErrLocked`·`ErrLeaseUnavailable`·`ErrFlapping`·`ErrCooldown` | config, store(lease) |

## 3.7 상태·HA·감사

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/store | 상태 저장소 인터페이스와 파일·PostgreSQL 구현, lease, 펜싱, 체인 키, 보존 정리, 마이그레이션 | `Store`(`Append`, `Load`, `Scan`, `Prune`, `TryLease`, `ReleaseLease`, `LeaseHolder`, `Fence`, `SetChainKey`, `Ping`, `Describe`, `Close`), `Open`(체인 키를 해석해 `SetChainKey`), `OpenFile`, `OpenPostgres`, `ChainKey`, `ResolveChainKey`, `MinChainKey`(32), `Migrate`, `Schema`, `ErrFenced` | journal, secrets, pgx |
| internal/journal | 기록 형식(`Entry`), 기록 종류, 해시 체인과 MAC, 재생으로 상태 복원 | `Entry`(`ChainHash`, `Chain`, `Seal`), `ChainMAC`, `Verifier`(필드 `Key`, `Checked`, `Legacy`, `Keyed`, `Unkeyed`, `KeyedFrom`), `State`(`Apply`, `InFlight`), `Replay`, `Compact`, `Bookkeeping` | model, safety |
| internal/ha | lease 기반 리더 선출(TTL/3 갱신, 만료 시 인계) | `Elector`(`Run`, `IsLeader`, `Leader`, `Owner`) | store |
| internal/audit | 체인·MAC 검증, 조회 필터, 내보내기, syslog(RFC 5424·CEF) 전송(tcp·udp·tls) | `Verify(ctx, st, key)`, `VerifyFile(path, key)`, `VerifyReader`, `Report`(`key_checked`, `keyed`, `unkeyed`, `keyed_from` 포함), `Query`, `Filter`, `Export`, `Exporter`(`NewExporter`, `Send`, `Run`, `Format`), `FormatCEF` | store, journal, tlsconf |

**PostgreSQL 스키마(001_init.sql):** `vigilante_events`(seq bigserial, at, kind, service, deployment_id, body jsonb; 인덱스 deployment_id, (kind, at))와 `vigilante_leases`(key, owner, expires_at). 마이그레이션은 한 트랜잭션에서 `pg_advisory_xact_lock`으로 한 번만 실행된다.

## 3.8 API·이벤트·콘솔

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/api | REST v1/v2 핸들러, 인증 미들웨어, 역할·스코프 판단, 호출 한도(default·emergency), OAuth 토큰 발급, API 클라이언트 관리, 팔로워 → 리더 전달(https면 `HATLS`), `/healthz`·`/readyz`·`/metrics`, 수신 웹훅, ITSM 작업자 | `Server`(`New`, `Handler`, `Close`, 필드 `HA`, `HATLS`), `Leadership`, `ParseWebhook`, v2 라우트 표(`v2Routes`), v1 생성 본문 `createV1`(`change_ticket`, `freeze_override`), `writeGateErr`·`gateStatus`·`gateCode`, `itsmCall`, `openIncident` | orchestrator, auth, events, console, audit, itsm |
| internal/events | 저장된 기록에서 CloudEvents 생성, 최근 이벤트 보관(SSE 재개), 웹훅 구독·전달·서명 | `Bus`(`New`, `Start`, `Observe`, `Emit`, `Since`, `Subscribe`, `CreateWebhook`, `UpdateWebhook`, `RotateSecret`, `Deliveries`, `Redeliver`, `Ping`, `Reload`), `Sign`, `Verify`, `Matches` | orchestrator(후크), journal |
| internal/console | 내장 정적 UI(index.html, app.js, app.css), OIDC 로그인, 세션 쿠키, CSRF, 보안 헤더·HSTS | `Console`(`New`, `Register`, `SessionToken`, `securityHeaders`), `Verify` | api(v2만 호출), auth |
| internal/model | 계층 공용 타입 | `Sample`, `Deployment`, `State`, `Verdict`, `Phase`, `Breach`, `PendingRollback`, `Operation`, `CloudEvent`, `Webhook`, `APIClient`, `Freeze`, `ChangeTicket`, `Feedback`, `ExitCode` | 전 패키지 |

## 3.9 보안

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/auth | 호출자 식별(서비스 계정, OIDC, legacy, API 클라이언트 위임)과 권한 판단 | `Authenticator`(`New`, `Authenticate`, `AuthenticateToken`), `Principal`(`Can`, `CanSomewhere`, `HasScope`), `Role`, `Scope`, `Binding`, `Action`, `Required`, `ScopeFor`, `NewToken`, `HashSecret` | config, go-oidc |
| internal/secrets | `vault:`·`env:`·`file:` 참조 해석, TTL 캐시, 로그 가림, Vault SSH CA 서명 | `Resolver`(`Resolve`, `Redact`), `Configure`, `Value`, `RedactHandler`, `SignSSHKey`, `VaultError` | transport, executor, notify, itsm |
| internal/tlsconf | 서버·에이전트·HA 전달·SIEM TLS 설정, 인증서 파일 변경 감지 재로딩 | `Server`, `Client`, `HAClient`(`server.ha.tls`, `ServerName` 지정), `Syslog`(`audit.syslog.tls`, 기본 서버 이름은 주소 호스트, `min_version` 1.3 선택) | api, agent, audit, cmd/vigilante |
| internal/sudoers | `sudo_scope: changes` 명령을 sudoers 규칙으로 변환, 대상에서 명령 경로 확인 | `Rules`, `Resolve`, `Group`, `Host`(`Render`), `Rule` | executor(규칙 선언), transport, doctor |

## 3.10 에이전트·연동

| 패키지 | 책임 | 주요 타입·함수 | 협력 |
|---|---|---|---|
| internal/agent | 로컬 프로브 실행, 샘플 push, 하트비트, failsafe 로컬 판정·롤백 | `Agent`(`Run`), 내부 `failsafe` | orchestrator(로컬 엔진), probe, rules |
| internal/notify | 알림 채널(slack, teams, email, pagerduty, webhook), 수준·서비스·팀 라우팅, 10분 중복 억제 | `Notifier`(`New`, `Send`), `Message`, `Level`(info·warning·critical) | secrets |
| internal/itsm | ServiceNow 변경 티켓 확인, 인시던트 생성(correlation_id로 중복 방지), 작업 노트, 상태 코드 기반 재시도 | `ServiceNow`(`Change`, `Check`, `EnsureIncident`, `WorkNote`, `Retry`, 필드 `Backoff`), `Change`, `ErrInvalidChange`, `StatusError`, `Retryable`, `DefaultBackoff`(1s, 2s, 4s) | orchestrator(게이트), api(작업자) |

## 3.11 운영·검증 도구

| 패키지 | 책임 | 주요 타입·함수 |
|---|---|---|
| internal/doctor | 읽기 전용 사전 점검(접속, sudo 규칙, 로그 형식, 이전 릴리스·이미지, LB 풀, Vault 참조, SSH 세션 예산, OpenStack) | `Run`, `Check`, `Options`, `Summary`, `Hint` |
| internal/lab | 실장비 시나리오 실행과 요약 | `Run`, `Options`, `Result`, `Step`, `Summarize`, `Markdown`, `Plugins` |
| internal/pilot | 판정 품질 계산과 출시 게이트 | `Compute`, `Report`(`Markdown`), `Gate`, `GateCheck`, `HoldCause` |
| internal/support | 지원 번들 zip, 비밀 제거 | `Bundle`(`Add`, `AddJSON`, `Note`, `Close`), `RedactYAML`, `RedactText` |
| internal/compat | 플러그인별 검증 수준 매트릭스와 경고 | `Lookup`, `InUse`, `Warning`, `Kinds`, `Entry`, `Level` |

## 3.12 관측성 — internal/telemetry

클라이언트 라이브러리 없이 Prometheus 텍스트 형식(0.0.4)을 출력한다. `Vec`(카운터·게이지), `Histogram`, `Write`를 제공하며, 레이블은 서비스·단계·프로브 유형·결과 등 낮은 카디널리티만 쓴다(대상 이름·배포 ID 금지). 주요 지표는 `vigilante_rollback_trigger_seconds`, `vigilante_rollbacks_total`, `vigilante_verdicts_total`, `vigilante_evaluation_seconds`, `vigilante_circuit_state`, `vigilante_leader`, `vigilante_engine_active`, `vigilante_store_append_seconds`, `vigilante_store_errors_total`, `vigilante_store_pending_writes`(M5-4, PR #12), `vigilante_observer_degraded`·`vigilante_observer_degradations_total{signal}`·`vigilante_observer_holds_total`(PR #13), `vigilante_itsm_calls_total{kind,result}`(`result`: `ok`, `error`, PR #13부터 `retry`)이다.

---

# 4. 배포 상태 머신

## 4.1 상태

상태는 internal/model `State`에 정의되어 있다.

| 상태 | 의미 | 종료 상태 | CI 종료 코드 |
|---|---|---|---|
| PENDING | 배포 등록됨 | 아니오 | 1 |
| BASELINE | 정의만 있고 현재 코드에서 전이에 쓰이지 않음 | 아니오 | 1 |
| OBSERVING | 단계 관측 중 | 아니오 | 1 |
| PROMOTED | 단계 PASS, 다음 단계 대기 | 아니오 | 0 |
| SUCCEEDED | `full` 단계 PASS | 예 | 0 |
| HELD | HOLD·INCONCLUSIVE 또는 승인 거절. 사람 판단 필요 | 아니오 | 4 |
| ROLLING_BACK | 롤백 플랜 실행 중 | 아니오 | 1 |
| ROLLED_BACK | 모든 대상 복구 | 예 | 2 |
| AWAITING_APPROVAL | 승인 모드 계획 대기 또는 승인 필요 에스컬레이션만 남음 | 아니오 | 3 |
| ROLLBACK_FAILED | 롤백 실패, 안전장치 차단(서킷·플래핑·동결) | 예 | 3 |
| ABORTED | 관측 중단 | 예 | 1 |

## 4.2 전이

```
                          Create
                            │
                            ▼
                ┌─────── PENDING ───────┐
                │ Watch(phase)           │ Watch 시작 실패(서킷 OPEN·동결·티켓 등): MarkBlocked → HELD
                ▼                        │ (CLI 종료 코드: 게이트 거부 3, 그 외 1)
            OBSERVING ──PASS(canary/rolling)──▶ PROMOTED ──Watch(next)──▶ OBSERVING
              │  │  │ ──PASS(full)──▶ SUCCEEDED
              │  │  └─HOLD/INCONCLUSIVE──▶ HELD ──Watch(재관측)──▶ OBSERVING
              │  └─취소(Abort)──▶ ABORTED
              └─FAIL─┬─mode=auto──▶ ROLLING_BACK ──전부 복구──▶ ROLLED_BACK
                     │                 │  └─승인 필요 단계만 남음──▶ AWAITING_APPROVAL
                     │                 └─실패──▶ ROLLBACK_FAILED (Breaker.Failure)
                     ├─mode=auto, 안전장치 차단──▶ ROLLBACK_FAILED (격리만)
                     └─mode=approve──▶ AWAITING_APPROVAL
                                          ├─approve──▶ ROLLING_BACK
                                          ├─reject──▶ HELD (격리 복귀)
                                          └─만료(on_timeout: rollback)──▶ ROLLING_BACK
```

| 출발 | 사건 | 도착 | 구현 |
|---|---|---|---|
| (없음) | `Create` | PENDING | `Engine.Create` |
| PENDING·PROMOTED·HELD 등 | `Watch` 시작(게이트 통과) | OBSERVING | `Engine.Watch` → `Gate` |
| PENDING·PROMOTED·HELD 등 | `Watch` 시작 실패(게이트 거부 등) | HELD | `Engine.MarkBlocked`(CLI, API v1·v2에서 호출) |
| OBSERVING | PASS, 단계가 full | SUCCEEDED | `Engine.Watch` |
| OBSERVING | PASS, 단계가 canary·rolling | PROMOTED | `Engine.Watch` |
| OBSERVING | HOLD 또는 INCONCLUSIVE | HELD | `Engine.Watch` |
| OBSERVING | 컨텍스트 취소 | ABORTED | `Engine.Watch`, `Engine.Abort` |
| OBSERVING | FAIL, `mode: approve` | AWAITING_APPROVAL | `requestApproval` |
| OBSERVING | FAIL, `mode: auto` | ROLLING_BACK 또는 ROLLBACK_FAILED(차단) | `Engine.Rollback`, `blocked` |
| ROLLING_BACK | 전 대상 성공 | ROLLED_BACK | `Engine.Rollback` |
| ROLLING_BACK | 실패가 전부 `ErrNeedsApproval` | AWAITING_APPROVAL | `Engine.Rollback` |
| ROLLING_BACK | 그 외 실패 | ROLLBACK_FAILED | `Engine.Rollback` |
| AWAITING_APPROVAL | 승인 | ROLLING_BACK | `DecideRollback`(수동 롤백으로 실행) |
| AWAITING_APPROVAL | 거절 | HELD | `DecideRollback`, `restoreTraffic` |
| AWAITING_APPROVAL | 만료, `on_timeout: rollback` | ROLLING_BACK | `ExpireApprovals` |
| ROLLING_BACK(재시작) | 프로세스 재시작·리더 교체 | ROLLING_BACK 재개 | `Engine.Resume` |

API는 `checkLaunch`로 OBSERVING·ROLLING_BACK, 그리고 SUCCEEDED를 제외한 종료 상태에서의 새 단계 시작을 거부한다(internal/api/server.go).

## 4.3 종료 코드 계산

`model.ExitCode`가 상태에서 종료 코드를 계산한다: PROMOTED·SUCCEEDED는 0, ROLLED_BACK은 2, ROLLBACK_FAILED·AWAITING_APPROVAL은 3, HELD는 4, 나머지는 1. 단, CLI `watch`는 단계 시작이 게이트(`ErrBlocked`, `ErrCircuitOpen`: 서킷 OPEN, 변경 동결, 변경 티켓)로 거부되면 상태와 별도로 종료 코드 3을, 그 밖의 시작 오류는 1을 돌려준다(cmd/vigilante/main.go). 로컬에서 `--freeze-override`가 `auth.local_cli` 제한으로 거부되어도 3이다(`createGated`).

**원격 모드(`watch --server`, watchremote.go, PR #13):** `POST /v1/deployments` 본문에 `--ticket`(`change_ticket`, 이전 서버 호환을 위해 `X-Change-Ticket` 헤더도 함께)과 `--freeze-override`(`freeze_override`)를 실어 보낸다. 생성이 실패하면 `apiRequest`가 응답 본문의 `code`를 `apiError.Code`로 읽고, `remoteExitCode`가 게이트 코드(`circuit_open`, `change_frozen`, `change_ticket_invalid`, `itsm_unavailable`)이거나 `code` 없는 409(코드 도입 전 서버)면 3, 그 외(400, 403, `not_leader` 등)는 1을 돌려준다. 이후 2초(`remotePoll`)마다 상태를 조회해 판정의 종료 코드를 쓴다.

---

# 5. 판정 알고리즘

## 5.1 규칙 구조

```yaml
rules:
  - name: fatal-errors
    action: rollback          # rollback(기본) / notify / hold
    when:
      any:
        - {metric: access.count_5xx, ratio_of: access.requests, window: 30s, op: ">", value: 2, for: 2}
        - {metric: health.consecutive_timeouts, op: ">=", value: 3}
  - name: latency-regression
    when: {metric: health.latency_ms, agg: p99, window: 1m, op: ">", baseline: {increase_pct: 200}, min_value: 300, for: 3, reset_after: 2}
```

## 5.2 leaf 평가

| 단계 | 처리 |
|---|---|
| 1. 데이터 | `metrics.Store.Window(대상, 메트릭, window, now, source)` |
| 2. 집계 | `agg`: last, avg, min, max, sum, count, rate(합계 ÷ 창 초), p50, p90, p95, p99(정렬 기반 nearest-rank). 카운터 집계(sum·count·rate)는 데이터가 없으면 0, 게이지 집계는 알 수 없음 |
| 3. 비율 | `ratio_of`: sum(metric) ÷ sum(ratio_of) × 100. 분모가 0이면 알 수 없음 |
| 4. 범위 | `scope: target`(대상별) 또는 `service`(배포 대상 전체 합산) |
| 5. 비교 | 절대값 `value` 또는 `baseline.increase_pct`(`>`·`>=`는 기준 × (1 + pct/100) 초과, `<`·`<=`는 기준 × (1 − pct/100) 미만), `min_value` 절대 하한 |
| 6. 없음 처리 | `absent`: unknown(기본), breach, ok |
| 7. 연속 카운터 | 키 `규칙경로/대상/관측점`. 위반이면 증가, `for` 이상이면 참. 정상이 `reset_after`회 연속이어야 초기화 |

## 5.3 트리 결합(3값 논리)

| 연산 | 결과 |
|---|---|
| `any` | 하나라도 참이면 참, 아니면 알 수 없음이 하나라도 있으면 알 수 없음, 아니면 거짓 |
| `all` | 하나라도 거짓이면 거짓, 아니면 알 수 없음이 있으면 알 수 없음, 아니면 참 |
| `not` | 참과 거짓을 바꾸고 알 수 없음은 유지 |

단락 평가를 하지 않는다. `any`의 첫 자식이 참이어도 모든 자식을 평가해 연속 카운터가 함께 전진하게 한다.

## 5.4 베이스라인

`rules.Chain{Control, Snapshot}` 순서로 조회한다. 라이브 대조군(`control_targets`, 미배포 대상의 같은 집계)이 있으면 우선하고, 없으면 `vigilante baseline`이 만든 스냅샷 파일을 쓴다.

## 5.5 단계 판정(decision.Engine.Run)

```
start = now; warmupEnd = start + warmup; end = start + observation_window
매 eval_interval 틱:
  if ctx 취소: return HOLD("observation aborted")      → Watch가 ABORTED로 기록
  if now < warmupEnd: continue                          (수집은 계속)
  ev = Evaluate(now)
  if len(ev.Fail) > 0: return FAIL(즉시)
  if now >= end: return conclude(ev)

Evaluate(now): 활성 규칙(단계 rules 부분집합) × 대상
  1. 대조군을 먼저 평가: 참인 대조군이 있으면 controlFired
  2. 배포 대상마다 evalTarget(쿼럼 적용)
     Unknown → Unknown 카운트
     False   → Known, 관측 불일치가 있으면 Hold에 추가
     True    → Known, 그리고
               action=notify   → Warn
               controlFired    → Hold(Environmental=true)
               action=hold     → Hold
               observerDegraded(rule, now) != ""
                               → Hold(action=hold, detail에 "observer degraded (…)", 5.8)
               그 외           → Fail

conclude(ev):
  Hold가 남아 있으면                         → HOLD
  표본 < min_samples 또는 (Known=0, Unknown>0) → on_inconclusive: pass→PASS, rollback→FAIL, hold→INCONCLUSIVE
  그 외                                      → PASS
```

## 5.6 관측 쿼럼(evalTarget)

`safety.observer_quorum`이 켜져 있고 규칙의 메트릭에 관측점(source)이 2개 이상(central, agent:NAME) 있으면 관측점마다 평가한다.

| 관측점 결과 | 판정 |
|---|---|
| 참만 있고 거짓 없음 | 참 |
| 참과 거짓이 섞임 | 거짓 + "observer disagreement" → Hold |
| 거짓만 있음 | 거짓 |
| 전부 알 수 없음 | 알 수 없음 |

## 5.7 판정 결과와 후속 처리

| 판정 | 배포 상태 | 후속 |
|---|---|---|
| PASS | PROMOTED 또는 SUCCEEDED | 다음 단계 대기. `full` PASS는 다음 배포의 `--previous` 후보 |
| FAIL | ROLLING_BACK·AWAITING_APPROVAL·ROLLBACK_FAILED | 6장 |
| HOLD | HELD | 사람 판단. 종료 코드 4 |
| INCONCLUSIVE | HELD | `on_inconclusive: hold`일 때 |

판정 결과는 `vigilante_verdicts_total{service,phase,verdict}`로 집계하고, FAIL 시점(`Outcome.EndedAt`)은 롤백 시작 지연 측정(`vigilante_rollback_trigger_seconds`)의 기준이 된다.

## 5.8 관측 장치 가드 (internal/observer, PR #13, PR #14)

**연결:** `orchestrator.New`가 `observer.New(cfg)`로 가드를 만든다(대상 → 서비스 목록 맵 구성). `Engine.Watch`는 관측 시작 시 `Observer.Start()`(참조 계수: 첫 관측이 자체 점검 고루틴을 띄우고 마지막 관측이 끝나면 멈춤)를 호출하고, 가드가 켜져 있으면 `decision.Engine.Observer`에 넣는다. 중앙 수집기의 `Sink`는 샘플을 `metrics.Store`와 `Observer.Observe`에 함께 보낸다.

| 신호 | 측정 | 저하 조건 | 주기 |
|---|---|---|---|
| 스케줄링 지연 | 250ms 타이머가 실제로 깨어난 간격 − 250ms | > `max_lag`(기본 1s) | 틱마다 |
| 루프백 | `127.0.0.1` 임의 포트의 프로세스 내 TCP 에코 서버에 1바이트 왕복 | 연결·쓰기·읽기 실패 또는 `loopback_timeout`(기본 1s) 초과. 다음 점검 때까지 응답이 없어도 저하 | 1초(4틱)마다 |
| 확산(`Spread`) | `Observe`가 받은 중앙(`central`) 샘플 중 `<probe>.consecutive_timeouts`의 최근 값(30초 지난 것은 버림) | 관측 대상 중 시간 초과 대상 비율 ≥ `timeout_share`(0.5) 그리고 시간 초과 대상이 속한 서비스 수 ≥ `min_services`(3) | 1초마다 |

```
run(): 틱마다 reasons 수집
  reasons 있음 → Report(now, "; "로 이은 사유)   열린 구간이 있으면 끝 시각만 연장
                                                 새 구간이면 추가, ObserverDegraded=1,
                                                 ObserverDegradations{signal=첫 사유의 ':' 앞}, 경고 로그
  4틱마다 사유 없음 → Clear(now)                 열린 구간 닫기, ObserverDegraded=0
  종료(마지막 관측 해제) → Clear
구간은 최근 2시간분만 보관한다.

Degraded(from, to): from −= grace(기본 1m). 최신 구간부터 [from, to]와 겹치는지 확인
                   (열린 구간의 끝은 to로 본다). 겹치면 (true, 사유)

decision.Engine.observerDegraded(rule, now):
  Observer == nil → ""
  rules.Metrics(rule.When)의 각 지표 "<probe>.<이름>"에서
     measured = Phase.ObserverProbes == nil(모든 프로브) 또는 ObserverProbes[probe]
                또는 이름 == probe_error(어느 프로브든 수집 중단)
     measured이고 이름(마지막 '.' 뒤)이 up, latency_ms, consecutive_failures,
     consecutive_timeouts, timeout, probe_error 중 하나인 지표가 없으면 → ""
  lookback = 규칙 트리의 최대 window + 최대 for × eval_interval
  Observer.Degraded(now − lookback, now)가 참이면 사유 반환
```

| 항목 | 내용 |
|---|---|
| 대상 프로브(PR #14) | `Engine.Watch`가 `observerProbes(svc)`로 서비스 프로브 중 유형이 `http`, `tcp`, `grpc`, `db`(오케스트레이터가 보내는 요청), `host`(SSH로 실행하는 명령, `up`은 오케스트레이터에서 본 SSH 도달성)인 프로브 ID를 모아 `decision.Phase.ObserverProbes`에 넣는다. 따라서 `log`·`access_log`·`docker` 프로브의 같은 이름 지표(예: 액세스 로그의 `latency_ms`, 서버 자신의 요청 처리 시간)는 저하 중에도 그대로 판정한다. `probe_error`는 어느 프로브든 수집 자체의 문제라 보류한다. 설정 키 변경은 없다(커밋 `c4f798a`) |
| 결과 | 해당 위반은 `action: hold`로 바뀌어 `Evaluation.Hold`에 들어가고 `vigilante_observer_holds_total`이 증가한다. 관측 창 끝에 HOLD → 배포 HELD(종료 코드 4), 사유에 "observer degraded" |
| 설정 검증 | `timeout_share`는 (0, 1], `min_services` ≥ 1, `grace` ≥ 0(validate.go). `disabled: true`면 `Enabled()`가 거짓이라 아무 것도 하지 않는다 |
| 테스트 | `TestDegradedSpansAndGrace`(구간·grace), `TestSpreadNeedsSeveralServices`(서비스 수 조건), `TestLoopbackAndStartStop`(루프백, 참조 계수 시작·정지), `TestDisabled`, `TestObserverDegradedHoldsProbeFailures`(internal/decision: 프로브 실패 규칙만 HOLD, PR #14부터 저하 중 `access.latency_ms` 위반은 FAIL·`http.latency_ms` 위반은 HOLD도 확인), `TestChaosObserverDegradedHolds`(엔진: 저하 중 불량 canary가 롤백되지 않고 HOLD) |

---

# 6. 롤백 플랜 실행

## 6.1 진입과 게이트(Engine.Rollback)

```
Rollback(d, opt):
  ctx = transport.Urgent(ctx)                         예약 SSH 세션 사용 가능
  if !Active(): return ErrInactive                    (펜싱된 노드)
  targets = opt.Targets 또는 scope(deployed: 단계 대상 전체 / failed: 위반 대상)
  if 자동(!Manual):
     활성 동결이 allow_rollback=false → blocked()
     Guard.Check(플래핑·cooldown)     실패 → blocked()
     Breaker.Allow()                  실패 → blocked()
     감사 기록 rollback.auto
  Guard.Acquire(service)              실패 → ROLLBACK_FAILED("rollback not started: …")
                                      (저장소 불통이면 lease 재시도 후 proceed/fail, 7.4)
  Guard.Record; 기록 rollback.start; 상태 ROLLING_BACK; critical 알림
  failures = run.execute(targets)
  if !Active(): result=handed_over (새 리더가 이어서 완료)
  failures 없음        → ROLLED_BACK, Breaker.Success()
  전부 ErrNeedsApproval → AWAITING_APPROVAL (서킷 실패로 세지 않음)
  그 외                → Breaker.Failure(), ROLLBACK_FAILED, 실패 대상은 격리 유지
```

수동 롤백(CLI·API·승인된 롤백·Resume)은 `Manual: true`로 동결·플래핑·서킷 검사를 건너뛰지만 서비스 락은 잡는다.

## 6.2 플랜 단계

| action | 동작 | 실패 시 |
|---|---|---|
| `traffic.drain` | `TrafficController.Drain` 후 `drain_wait` 대기 | 이벤트 기록 후 제자리 롤백 계속 |
| `app.rollback` | `Executor.Rollback`(멱등) | 에스컬레이션 |
| `app.verify` | `Executor.Verify`(버전 사실 확인) | 에스컬레이션 |
| `probe.verify` | 지정 프로브 `Check()`가 연속 `successes`회(기본 3) 성공 | 에스컬레이션 |
| `traffic.enable` | `TrafficController.Enable`. 재개 시에도 항상 실행(멱등) | 대상 실패 |
| `wait` | `duration` 대기 | — |

플랜을 생략하면 `traffic`이 있을 때 `[traffic.drain] → app.rollback → app.verify → [traffic.enable]`이 기본값이다.

## 6.3 배치와 blast radius(rollbackRun.execute)

```
batch = rollback.parallelism (기본 1)
if 트래픽 제어기 있음:
  pool, err = tc.Pool()
  err        → batch = 1, 이벤트 "pool state unavailable"
  else n = DrainBatch(len(pool), enabled, len(targets), blast_radius)
       n == 0 → drain 안 함, batch = 1 (제자리 롤백)
       else   → batch = min(batch, n)
targets를 batch 크기로 나눠 묶음 안은 병렬(고루틴), 묶음 사이는 순차
```

## 6.4 단계 재시도·시간 제한(rollbackRun.step)

| 항목 | 기본값 | 설명 |
|---|---|---|
| `step_timeout` | 2m | 시도마다 적용. 단계별 `timeout`이 있으면 우선 |
| `retry.attempts` | 3 | `probe.verify`·`wait`는 내부 반복이므로 1회 |
| `retry.backoff` | 2s | 실패 후 대기, 매번 2배 |
| `retry.max_backoff` | 30s | 대기 상한 |

각 시도 전에 엔진이 활성인지 확인하고, 재시도마다 `retry` 이벤트를 남긴다. 성공한 단계는 `rollback.step` 기록(대상, 단계 번호)으로 남겨 재개 시 건너뛴다.

## 6.5 에스컬레이션(rollbackRun.escalate)

```
app.* 단계 최종 실패:
  for esc in rollback.escalation (순서대로):
     esc.require_approval && !opt.Approved → ErrNeedsApproval{esc.executor} 반환
     ex = executor.New(esc.executor)
     app.rollback → app.verify → (플랜의 probe.verify)를 ex로 실행
     성공 → 플랜의 나머지 비-app 단계(traffic.enable 등) 실행 후 대상 성공
  전부 실패 → "all strategies failed" (traffic.enable 미실행 = 격리 유지)
```

## 6.6 승인 모드(approval.go)

| 단계 | 동작 |
|---|---|
| 계획 생성 | FAIL 시 `PendingRollback{Reason, Targets, RequestedAt, ExpiresAt, DetectedAt}`를 배포에 기록하고 AWAITING_APPROVAL, critical 알림 |
| 선격리 | `approval.drain_first: true`이면 위반 대상을 blast radius 안에서 먼저 드레인하고 `Drained`에 기록 |
| 승인 | `DecideRollback(approve)`: `ApprovedBy` 기록 후 계획 대상으로 수동 롤백(서킷·플래핑 미적용) |
| 거절 | 드레인한 대상을 `Enable`로 복귀(`restoreTraffic`), 새 버전 유지, HELD |
| 만료 | 리더가 주기적으로 `ExpireApprovals` 실행(docs/02: 15초마다). `on_timeout: rollback`이면 자동 롤백(서킷·플래핑 적용), `hold`(기본)면 한 번 더 상위 호출(`Escalated`) 후 계속 대기 |
| 기본값 | `timeout` 30m, `on_timeout` hold |
| 4-eyes | `auth.four_eyes`이면 API 계층(v1 승인, v2 approvals)에서 배포 생성자·롤백 요청자의 결정을 거부 |
| 에이전트 | 승인 모드 서비스는 failsafe가 rollback이어도 보류 |
| 지표 | `vigilante_rollback_approvals_total{decision}` |

## 6.7 차단 시 격리(blocked, isolateBreaches)

자동 롤백이 서킷·플래핑·동결로 막히면 위반 대상만 blast radius 안에서 드레인하고 ROLLBACK_FAILED로 사람에게 넘긴다. 풀 조회가 실패하면 드레인하지 않는다.

## 6.8 크래시 재개(Resume)

`Reload`가 저장소를 재생해 `stepsDone`(배포 → 대상 → 완료 단계 수)과 진행 중 배포(`InFlight`)를 복원하고, `Resume`이 각 배포를 `Manual: true`로 다시 `Rollback`한다. 완료 단계는 건너뛰고, `traffic.drain`은 지금 드레인이 허용되지 않아도 완료로 표시하며, `traffic.enable`은 항상 실행한다.

---

# 7. 안전장치

## 7.1 서킷 브레이커

```
 CLOSED ──(창 안 실패 N회 또는 수동 trip)──▶ OPEN ──(open_duration 경과)──▶ HALF_OPEN
   ▲                                          ▲ (open_duration 0s면 수동 reset만)  │
   │                                          └──────── 시험 롤백 실패 ─────────────┤
   └──────────────── 시험 롤백 성공 / 수동 reset ───────────────────────────────────┘
```

| 항목 | 값 |
|---|---|
| `failure_threshold` | 기본 3(docs/02 예시는 2) |
| `window` | 기본 1h. 창 밖 실패는 제거 |
| `open_duration` | 0s면 수동 리셋만 |
| HALF_OPEN | 시험 1건만 허용(`HalfOpenUsed`) |
| 영속화 | 상태 변경마다 `circuit` 기록. 재시작·CI 잡 간 유지 |
| OPEN 영향 | 자동 롤백 금지, 새 단계 게이트 폐쇄(종료 코드 3). 격리(드레인)와 수동 롤백은 허용 |

## 7.2 blast radius(DrainBatch)

```
minRequired = max(min_healthy, ceil(poolSize × min_healthy_percent / 100))
allowed     = enabled − minRequired
allowed ≤ 0        → 0  (드레인 거부, 제자리 롤백 1대씩)
allowed < toDrain  → allowed (배치 처리)
그 외              → toDrain
```

기본값: `min_healthy`와 `min_healthy_percent`가 모두 0이면 `min_healthy: 1`.

## 7.3 플래핑 제한(Guard.Check)

서비스별 롤백 이력에서 최근 1시간 롤백 수가 `max_rollbacks_per_hour`(기본 3) 이상이거나, 마지막 롤백 후 `cooldown`이 지나지 않았으면 자동 롤백을 거부한다(`ErrFlapping`, `ErrCooldown`). 이력은 `rollback.start` 기록에서 복원된다.

## 7.4 서비스 락(Guard.Acquire)

프로세스 내 맵과 저장소 lease(키 `service:<이름>`, TTL 2분, TTL/3마다 갱신, 해제 시 반납)를 함께 쓴다. 이미 잡혀 있으면 `ErrLocked`(보유자 표시)로 거부한다. 파일 저장소의 lease는 같은 파일시스템을 쓰는 프로세스 사이에서만 배타적이다.

**저장소 불통 시(PR #13):** `Reload`가 `Guard.LeaseWait = safety.rollback_lease.wait`, `ProceedUnleased = (on_unavailable != "fail")`, `Unleased = rollbackUnleased`, `Conflict = rollbackLeaseConflict`를 설정한다.

```
Acquire(service):
  프로세스 내 락이 있으면 ErrLocked
  프로세스 내 락 설정
  Leases 없음 → 반환
  tryLease: LeaseWait 동안 TryLease 반복
            시도 제한 시간 = min(5초, max(남은 시간, 1초)), 실패 간 대기 250ms부터 2배(상한 2초)
            명확한 답(획득·타인 보유)은 즉시 반환
  타인 보유              → 락 해제, ErrLocked "(held by <보유자>)"
  저장소 오류 & !Proceed  → 락 해제, ErrLeaseUnavailable("state store unreachable: …")
  저장소 오류 & Proceed   → Unleased(service, err): 이벤트(safety)·감사 lease.unavailable·경고 알림
  renew(held): 주기 = TTL/3 (lease 미보유면 min(TTL/3, 5초))
     매 주기 TryLease. 미보유 상태에서 저장소가 돌아오면 lease 획득,
     타인이 보유 중이면 Conflict(service, holder) 1회: 감사 lease.conflict·경고 알림
  해제: 프로세스 내 락 해제, 갱신 중지, 자기 lease만 반납
```

`leaseNotice`는 그 서비스의 가장 최근 비종료 배포에 이벤트를 남기고(없으면 로그), 감사 기록(`actor: system`, `source: system`)과 경고 알림을 보낸다. 감사 기록 자체는 저장소가 돌아올 때까지 쓰기 대기열(9.3)에 있다. 검증: `TestGuardStoreUnreachable`, `TestChaosStoreOutageDuringRollback`, `TestChaosStoreOutageLeaseFailMode`.

## 7.5 변경 동결

| 항목 | 설계 |
|---|---|
| 정의 | 설정 `change_freeze`(주간 반복·절대 기간, 서비스·팀 범위, 시간대) + API 선언(`POST /v2/freezes`, 저장소에 기록) |
| 막는 것 | 새 배포 등록과 단계 시작(`FreezeGate`) |
| 예외 | admin의 `freeze_override`(사유 기록, 감사 `freeze.override`). v1 `POST /v1/deployments` 본문에서도 받는다(비admin은 403 + `denied`, PR #13). 로컬 CLI는 7.6 |
| 롤백 | 기본 허용. `allow_rollback: false` 기간에는 자동 롤백 대신 격리(6.7). 수동 롤백은 항상 가능 |

## 7.6 로컬 CLI 권한 제한 (cmd/vigilante/authcmd.go, PR #13)

`--server` 없이 실행한 CLI는 엔진을 직접 열어 저장소에 기록하므로 API의 역할·스코프·4-eyes 검사가 없다. 다음 함수가 이 경로를 막는다.

| 함수 | 동작 |
|---|---|
| `Config.LocalCLIRestricted()` | `auth.local_cli`가 `full`이면 거짓, `restricted`면 참, 그 외(`auto`, 빈 값)는 `server.auth_token_env`, `auth.service_accounts`, `auth.oidc` 중 하나라도 있으면 참 |
| `localPrivileged(ctx, e, c, action, d)` | `--break-glass`가 없으면: 제한 아니면 통과, 제한이면 "`<action>` refused … Run it through the server with --server … or pass --break-glass REASON" 오류. `--break-glass REASON`이 있으면 감사 `breakglass.<action>`(사유 = REASON)과 critical 알림("Break-glass: <작업자> ran <action> locally") 후 통과 |
| `localFourEyes(e, c, d)` | `auth.four_eyes`가 켜져 있고 `--break-glass`가 없으며 작업자(`cliActor()`)가 배포 생성자 또는 롤백 요청자면 거부 |

| 명령 | action | 순서 | 거부 시 종료 코드 |
|---|---|---|---|
| `rollback --id ID --approve`·`--reject`(승인 모드 대기 배포) | `rollback.approve`, `rollback.reject` | `localFourEyes` → `localPrivileged` → 감사 → `DecideRollback` | 1 |
| `rollback --approve`(에스컬레이션 승인) | `escalation.approve` | `localFourEyes` → `localPrivileged` → 롤백 | 1 |
| `circuit reset`, `circuit trip` | `circuit.reset`, `circuit.trip` | `localPrivileged` → 조작 | 1 |
| `watch`·`prepare --freeze-override`(활성 동결이 있을 때) | `freeze.override` | `localPrivileged` → `FreezeGate` | 3 |

검증: `TestLocalCLIRestricted`(서비스 계정이 있는 설정에서 기본 `auto`는 `circuit trip`을 `--break-glass` 안내와 함께 거부, `status`는 허용, `--break-glass` 리셋은 `breakglass.circuit.reset` 감사(작업자 `cli:…`), `full`이면 허용), `TestLocalFourEyes`(생성자·요청자 거부, 다른 사람·break-glass·`four_eyes` 꺼짐은 허용). critical 알림 전송 자체는 테스트로 확인하지 않는다.

---

# 8. 이벤트 버스와 웹훅

## 8.1 이벤트 생성

`events.Bus.Observe`가 엔진 후크로 저장된 기록을 받아 배포 상태 변화, 서킷, 승인에서 CloudEvents 1.0 이벤트를 만든다. 이벤트마다 클러스터 전체 순번(`sequence`)을 붙여 `event` 기록으로 저장하므로 새 리더가 순번과 웹훅 커서를 이어받는다.

| 이벤트 종류(접두어 `vigilante.`) | 개수 |
|---|---|
| `deployment.created`, `deployment.marked_good` | 2 |
| `observation.started`, `.passed`, `.failed`, `.held`, `.aborted` | 5 |
| `rollback.started`, `.completed`, `.failed` | 3 |
| `approval.requested`, `.decided` | 2 |
| `circuit.opened`, `.half_opened`, `.closed` | 3 |
| `agent.lost`, `webhook.disabled`, `ping` | 3 |

## 8.2 SSE

`GET /v2/events`는 `Subscribe(after, filter)`로 빠진 구간과 실시간 채널을 함께 받는다. `Last-Event-ID`로 재연결하면 최근 10,000건 안에서 누락 없이 이어 받는다. 서비스 이벤트는 그 서비스를 읽을 수 있는 호출자에게만 보낸다.

## 8.3 웹훅 전달

| 항목 | 설계 |
|---|---|
| 순서 | 구독마다 한 건씩 순서대로, 최소 한 번(수신 측은 `webhook-id`로 중복 제거) |
| 성공 | 10초 안 2xx |
| 재시도 | 1초, 5초, 30초, 2분, 10분, 30분 후 |
| 최종 실패 | dead-letter 목록에 남기고 다음 이벤트로 진행 |
| 비활성화 | 연속 5건 dead-letter면 구독 비활성화 + `webhook.disabled` + 운영 알림 |
| 서명 | Standard Webhooks(`webhook-id`, `webhook-timestamp`, `webhook-signature` = HMAC-SHA256), 비밀은 마스터 키와 구독 ID로 파생 |
| 커서 | `webhook.cursor` 기록으로 리더 교체 후 이어서 전달 |
| 제한 | `api.webhook_allowed_hosts`, 리디렉션 미추종 |
| 관리 API | 생성·변경·비밀 회전·삭제, 재전송(`redeliveries`), 테스트(`pings`), 전달 이력(최근 100건) |
| 실행 노드 | 활성 리더만 전달 |

---

# 9. 상태 저장 상세

## 9.1 기록 종류(journal.Kind)

| 종류 | 내용 | 감사 조회 대상 |
|---|---|---|
| `deployment` | 배포 전체 스냅샷 | 아니오(상태 복원용) |
| `circuit` | 서킷 상태 | 예 |
| `rollback.start` | 서비스 롤백 시작(플래핑 이력) | 예 |
| `rollback.step` | 대상의 플랜 N단계 완료 | 아니오 |
| `audit` | 누가 무엇을 했는가(거부 포함) | 예 |
| `anchor` | 보존 정리 후 체인 시작점 | 예 |
| `operation`, `idempotency` | 장기 작업, 멱등 응답 | 아니오 |
| `api-client`, `access-token`, `api-client.used` | API 클라이언트·토큰(SHA-256), 마지막 사용 | 아니오 |
| `event`, `webhook`, `webhook.cursor` | 이벤트, 구독, 전달 위치 | 아니오 |
| `freeze` | API로 선언·종료한 동결 | 아니오 |

"아니오"는 `Bookkeeping` 종류로, 의미는 별도 `audit` 기록이 담는다.

## 9.2 해시 체인

```
hash_n = SHA-256( prev_hash + "\n" + JSON(entry_n with Prev=prev_hash, Hash="") )
```

**키 체인(PR #13):** `audit.chain_key_ref`가 있으면 `store.Open`이 키(32바이트 이상, `ResolveChainKey`)를 해석해 `SetChainKey`로 넘기고, `Append`는 `Entry.Seal(prev, key)`로 봉인한다.

```
mac_n = hex(HMAC-SHA256(key, hash_n))      (ChainHash 계산 때 mac은 비움 → 해시 형식 불변)
```

`Verifier{Key}`는 MAC이 있는 첫 기록의 위치를 `KeyedFrom`으로 잡고, 그 전의 MAC 없는 체인 기록은 `Unkeyed`(키 도입 전)로 센다. 그 뒤로 MAC이 없거나 맞지 않으면 손상이다(키 없이 다시 쓴 기록). 첫 MAC부터 틀리면 "wrong chain key, or the entry was rewritten without it". 앵커는 마지막 정리 기록의 `hash`와 `mac`을 그대로 가지므로 정리 후에도 검증된다. `vigilante audit verify`는 `-c`의 `audit.chain_key_ref` 또는 `--key REF`로 키를 받고(아카이브 `--file`은 `-c`를 줄 때만 설정을 읽음), 키가 없으면 "MACs not checked" 경고를, MAC이 하나도 없으면 WARNING을 출력한다. 지원 번들의 체인 검증도 같은 키를 쓴다. 검증: `TestKeyedChainDetectsRecomputedTampering`(MAC 유지·제거 두 경우), `TestKeyedChainWrongKey`, `TestKeyedChainLegacyEntries`, `TestKeyedChainCoversUnkeyedPrefix`, `TestKeyedChainAcrossPrune`(각 file·postgres), `TestResolveChainKey`.

`Verifier.Next`가 순서대로 다시 계산해 끊긴 위치와 원인(수정·삭제)을 보고한다. `Prune`은 지울 구간을 아카이브(JSONL, 단독 검증 가능)에 쓴 뒤 그 구간이 만든 필요 상태(진행 중 배포·롤백 단계, 마지막 성공 버전, 서킷, 최근 롤백)를 앵커 기록에 담아 대체하고, 남은 체인은 앵커에서 이어진다.

## 9.3 기록 경로와 write-behind 큐 (M5-4, PR #12)

```
record(en):
  Time·Actor 기본값 채움
  lock(pendMu)
  if 큐 비어 있지 않음: 큐 뒤에 추가 (순서 유지), return
  err = appendOne(en)                     (시도당 시간 제한 5초)
  if err && !ErrFenced: 오류 로그, 큐에 추가 (최대 100,000건, 초과 시 dropped 집계)
  unlock
  err == nil   → stored(en): OnRecord, 후크(이벤트 버스, SIEM)  ← 잠금 밖에서 실행
  ErrFenced    → fenced(): SetActive(false)

flush() (큐에 첫 항목이 들어오면 고루틴 1개):
  대기 200ms에서 시작, 실패마다 2배, 상한 5초
  성공 → 큐에서 제거, 후크 실행, 대기 초기화
  ErrFenced → 큐 폐기(다른 노드가 리더), fenced()
Close(): 큐가 있으면 최대 10초 Flush 후 종료
```

이 변경은 커밋 `9f9470e`(PR #12, 병합 `dba3dbe`)에 있고, `TestChaosStoreOutageDuringRollback`(internal/orchestrator/chaos_test.go)이 롤백 중 저장소 단절에서 롤백 계속·기록 무손실·해시 체인 유지를 확인한다. 큐 길이는 `vigilante_store_pending_writes`, 실패 사유는 `vigilante_store_errors_total{reason}`(`error`: 큐에 넣고 재시도, `dropped`: 큐 가득 참, `fenced`: 리더 상실)로 보인다. 한계: 큐는 메모리에만 있어 장애 중 크래시 시 유실되고, HA에서 장애가 `lease_ttl`보다 길면 리더가 물러나며 다른 노드가 리더가 되면 큐는 펜싱되어 버려진다.

---

# 10. 운영 콘솔

| 항목 | 설계 |
|---|---|
| 배포 | `/console/` 경로, `go:embed` 정적 파일(index.html, app.js, app.css), 바닐라 JS, 외부 자원 없음 |
| 데이터 | v2 공개 API만 호출, 실시간 갱신은 SSE(`GET /v2/events`) |
| 화면 | 현황(서킷·조치 필요·진행 중·최근 배포·실시간 이벤트), 배포 목록·상세(승인·거절, 롤백, 중단, 위반, 작업, 타임라인, 판정 평가), 서비스, 변경 동결, 감사 기록 |
| 조작 | 사유 필수, 역할에 맞는 버튼만 표시, 감사 출처 `ui` |
| 로그인 | OIDC authorization code + PKCE(`redirect_url` 설정 시). 없으면 토큰 로그인(브라우저 탭 sessionStorage) |
| 세션 | ID 토큰을 AES-GCM으로 암호화한 HttpOnly 쿠키, 최대 12시간, `session_key_ref`(32자 이상) 공유로 HA 대응 |
| 요청 인증 | `Authorization` 헤더가 없을 때만 쿠키 사용. 쿠키 변경 요청은 `X-CSRF-Token` 필수 |
| 보안 헤더 | CSP(자기 출처, 인라인 스크립트 없음), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. `Register`의 `handle` 래퍼가 정적 파일과 `/console/auth/mode`·`login`·`callback`·`logout` 모두에 적용. `hsts`(= `server.tls` 설정 또는 `redirect_url`이 `https://`)이거나 요청이 TLS면 `Strict-Transport-Security: max-age=31536000`(하위 도메인 미포함, PR #13, `TestHSTSOverHTTPS`) |
| 1차 제외 | 위반 시점 지표 그래프, doctor 결과 화면, 설정 편집, 데모 재생, 메신저 승인 버튼 |

---

# 11. 외부 연동

## 11.1 ServiceNow

| 기능 | 동작 |
|---|---|
| 변경 티켓 게이트 | 배포 등록 시와 단계 시작마다 확인: 승인됨, 허용 상태(기본 `-2`, `-1`), 계획된 작업 시간 안(`check_window`) |
| 장애 시 | `on_error: closed`(기본, 503 `itsm_unavailable`) 또는 `open`(진행, `unverified` 표시) |
| 인시던트 | 롤백 실패·서킷 열림 시 `correlation_id`로 한 번만 생성, 번호를 감사·타임라인에 기록 |
| 작업 노트 | 검증된 티켓의 배포 진행을 work notes에 기록 |
| 실행 위치 | 인시던트·작업 노트는 리더의 ITSM 작업자가 이벤트를 따라 처리하고, 실패는 기록·집계만 한다. 인시던트는 `Server.background`로 따로 실행해 느린 ServiceNow가 이벤트 처리와 뒤 작업 노트를 지연시키지 않는다(PR #13). CI 단발 실행은 게이트만 동작 |
| 재시도(PR #13) | `itsmCall`이 `ServiceNow.Retry`로 호출한다. 시도마다 30초 제한(`itsmCallTimeout`, HTTP 클라이언트 자체 제한 15초), 실패 후 `Backoff`(기본 1s, 2s, 4s) 대기, 최대 4회 시도. `Retryable`: 연결 오류·시간 초과·429·5xx는 재시도, 그 밖의 4xx(`StatusError`)와 `ErrInvalidChange`는 즉시 중단. 이전 구현은 오류 문자열에서 ": 4"를 찾아 판단했다. 인시던트 재시도는 `EnsureIncident`가 같은 `correlation_id`의 활성 인시던트를 먼저 찾으므로 중복을 만들지 않는다 |
| 지표 | `vigilante_itsm_calls_total{kind,result}`: 호출마다 `ok` 또는 `error`(재시도 후) 1회, 반복 시도마다 `retry` |
| 검증 | `TestIncidentRetriedUntilServiceNowRecovers`, `TestRetryAfterLostCreateDoesNotDuplicate`, `TestRetryIsBounded`, `TestRetryable`, `TestV2ServiceNowGateIncidentsAndNotes`(처음 두 시도 실패 후 인시던트 1건, `retry` 지표, 감사 `itsm.incident`) |

## 11.2 알림

`notify.Notifier.Send`가 수준(`min_level`)과 서비스·팀 범위로 채널을 고르고, 같은 배포의 같은 알림은 채널마다 10분에 한 번만 보낸다. PagerDuty는 `dedup_key`(배포·제목)로 묶는다. 알림 실패는 롤백을 막지 않는다.

---

# 12. 릴리스·패키징 파이프라인

## 12.1 산출물(scripts/release.sh)

| 산출물 | 내용 |
|---|---|
| 바이너리 | 전체·최소 × linux amd64·arm64·ppc64le, windows amd64 |
| SBOM | 바이너리별 CycloneDX(`*.cdx.json`) |
| 패키지 | rpm·deb(nfpm, packaging/nfpm.yaml): `/usr/bin/vigilante`, systemd 유닛 2개, loopback 전용 기본 설정 |
| 이미지 | packaging/container/Dockerfile: distroless static nonroot, 서명된 바이너리를 그대로 복사 |
| 차트 | deploy/helm/vigilante |
| 폐쇄망 번들 | 아키텍처별 tar.gz + 설치 스크립트(packaging/install-airgap.sh) |
| 무결성 | 전체 `SHA256SUMS`, `SHA256SUMS.sig`(cosign 키) |

## 12.2 CI(매 PR, ci.yml)

```
 test    : go.mod 툴체인 일치 → gofmt → vet → go test -race(PostgreSQL 16) → 정적 교차 빌드
 api     : Spectral 명세 린트(경고도 실패) → oasdiff 하위호환 검사(기준 브랜치 대비)
 vuln    : govulncheck
 package : 최소 빌드 vet·테스트(+ `go vet -tags load ./test/load`, PR #12) → release.sh build(바이너리·SBOM·rpm·deb·차트)
           → 차트 렌더링(단일·HA, 파일 저장소 HA 거부, 인증 없는 설정 거부, HA·TLS 값으로
             https advertise URL·scheme HTTPS·GOMEMLIMIT 1843MiB 확인, PR #13)
           → 이미지 → 번들·임시 키 cosign 서명·확인 → Debian 12·Rocky Linux 9 컨테이너에 설치
 demo    : examples/demo/run-demo.sh (실제 프로세스 E2E)
```

부하 시험은 별도 워크플로우 `.github/workflows/load.yml`(PR #12)이 주 1회·수동·엔진 변경 PR에서 대상 2,000 × 프로브 3·10, 동시 배포 100으로 실행한다. 2026-10-11 PR #13 헤드 `189dfbb`에서 ci.yml의 다섯 작업(실행 38091832392)과 load 워크플로우 두 규모(실행 38091832358)가 모두 통과했다.

E2E 데모 시나리오: known-good 등록 → 기준선 → 불량 v2 자동 롤백(종료 코드 2) → 롤백 경로 고장으로 실패·서킷 OPEN(3) → OPEN 중 게이트 폐쇄(3) → 리셋 → 정상 v1.1 PASS(0) → lab 시나리오 → pilot 보고서(게이트 미달, 4).

## 12.3 릴리스(release.yml)

태그 `vX.Y.Z` 푸시 → 버전 결정 → 테스트 → 빌드 → 멀티 아키텍처 이미지 푸시 → 폐쇄망 번들·체크섬 → 서명(`packaging/cosign.pub`와 짝이 맞지 않으면 거부) → 게시. 접미사 태그(`-rc.1`)는 사전 릴리스이다. **릴리스 서명 키 쌍이 아직 없어 워크플로우는 키 생성 전까지 실패한다**(docs/07).

## 12.4 버전과 마이그레이션 규칙

- SemVer. 현재 0.x이며 첫 릴리스는 1.0.0(출시 게이트 충족 후).
- 마이그레이션 파일은 `internal/store/migrations/NNN_설명.sql`, 되돌리기는 `.down.sql`, 호환 불가 변경은 `-- vigilante:breaking` 표시와 MAJOR 릴리스.

---

# 13. 설계 제약 및 미구현 사항

| 구분 | 내용 | 출처 |
|---|---|---|
| 미검증 | 실행기·트래픽 제어기 대부분이 모의 서버·시뮬레이터 기준(실험적) | docs/09 |
| 검증 | 롤백 단계 재시도·시간 제한(6.4)은 LB 카오스 테스트 `TestChaosLoadBalancerTransientErrors`(오류 후 재시도)·`TestChaosLoadBalancerLatency`(시간 제한 후 재시도)가 다루고, LB 완전 장애 시 ROLLBACK_FAILED는 `TestChaosLoadBalancerDown`이 확인한다(PR #12) | internal/orchestrator/chaos_test.go |
| 부하 결과 | 부하 하네스(`test/load`, 빌드 태그 `load`, `.github/workflows/load.yml`, GitHub Actions ubuntu-latest, 대상 2,000, 동시 배포 100, 목표 12초). M5-4 기록: 프로브 3개 오판 0·판정 지연 p99 5.5초, 프로브 10개 오판 0·p99 5.7초. PR #13 실행: 프로브 3개 오판 0·p99 5.32초, 프로브 10개 오판 0·p99 5.96초 | PR #12 커밋 `a0c000e`, load 실행 38091832358 |
| 관측 장치 가드 한계 | 5.8의 가드는 오케스트레이터 자신의 저하만 본다. 저하 중에는 프로브 실패로만 드러나는 진짜 불량도 HOLD되며, 확산 신호는 중앙 수집 샘플만 쓴다. (해소) 지표 이름만으로 민감 지표를 골라 액세스 로그 프로브의 `latency_ms`도 HOLD하던 문제는 PR #14(`537870c`)에서 직접 재는 프로브만 보류하도록 고쳤다(5.8) | internal/decision, internal/observer |
| lease 없는 롤백 | 7.4의 `proceed`는 저장소 장애 중 다른 프로세스와의 동시 롤백 가능성을 감수한다(멱등 단계 전제, `lease.conflict` 알림) | internal/safety |
| 미사용 정의 | 상태 `BASELINE`은 정의되어 있으나 전이에 쓰이지 않는다 | internal/model |
| 문서 차이 | docs/04 S11은 서비스 락을 "락 파일(30분 stale 회수)"로 적었으나 현재 코드는 저장소 lease(TTL 2분, 갱신)이다 | internal/safety |
| 미구현 | OpenStack 루트 볼륨 교체 대체 경로, 서버 정기 doctor, 에이전트 인증서 자동 발급, 수집 샤딩, 설정 v2·`config migrate`, LDAP, CyberArk, Kafka | docs/05 |
| 제약 | host 프로브 Linux 전용, 에이전트 failsafe는 LB 미접근, 스냅샷은 디스크만 복원 | README, docs/04, docs/02 |
