---
title: 요구사항 정의서
doc_id: VGL-SI-01
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

이 문서는 Vigilante(통합 롤백 오케스트레이터)의 기능 요구사항과 비기능 요구사항을 정의하고, 각 요구사항의 출처, 우선순위, 구현 상태, 검증 방법을 기록한다. 또한 요구사항에서 설계(VGL-SI-02 아키텍처 설계서, VGL-SI-03 상세 설계서)와 테스트로 이어지는 추적성을 제시한다.

## 1.2 제품 개요

Vigilante는 이기종 하이브리드 인프라(베어메탈, OpenStack·vSphere·Nutanix·KVM, EC2·Azure VM, Docker·Podman, Nginx·HAProxy·Envoy·F5·ALB·Octavia)의 배포를 외부 APM 없이 자체적으로 측정·판정하고, 실패 시 자동으로 롤백하는 단일 Go 바이너리이다(README.md).

```
배포 ─▶ vigilante watch ─▶ 수집(HTTP/gRPC/TCP·/proc·docker.sock·로그·5xx·DB 풀)
                         ─▶ 판정(복합 규칙·베이스라인·연속 실패·대조군·관측 쿼럼)
                         ─▶ 롤백(드레인 → symlink/컨테이너/스냅샷 → 검증 → 복귀)
                         ─▶ 안전장치(서킷 브레이커·blast radius·플래핑·크래시 재개)
```

## 1.3 범위

요구사항의 범위는 docs/05-roadmap.md의 "상용 1차 범위(MVP)"와 2차 범위를 따른다. 상용 1차는 "파일럿 고객 1곳에서 실제 롤백 성공 사례를 만드는 데 필요한 최소 범위"이다.

| 구분 | 포함 마일스톤 | 비고 |
|---|---|---|
| 1단계 프로토타입 | 핵심 흐름(수집 → 판정 → 롤백 → 안전장치) | 커밋 `d1e6411` |
| 상용 1차 | M0 기반, M1 입력 간소화, M3-1~M3-3 오픈 API 핵심, M7 OpenStack, M8 실장비 검증·파일럿, M4 운영 콘솔·변경관리, M6 출시·지원 | 출시 게이트는 M8 결과에 따름 |
| 2차 | M2 온보딩·거버넌스, M3-4~M3-6 API 확장, M5 규모·신뢰성 | M5-3은 선행 구현 완료(PR #11), M5-4는 구현 완료(PR #12, 병합 커밋 `dba3dbe`) |

현재 릴리스는 출시되지 않았다. 버전은 0.x 계열이며, 첫 릴리스 1.0.0은 M8 실장비 랩과 파일럿 결과(출시 게이트)를 충족한 뒤에 낸다(CHANGELOG.md "Unreleased").

## 1.4 용어 정의

| 용어 | 정의 |
|---|---|
| 단계(phase) | 배포 관측 단위. `canary`, `rolling`, `full` 순서로 진행한다 |
| 판정(verdict) | 단계 관측 결과. PASS, FAIL, HOLD, INCONCLUSIVE |
| 대조군(control) | 같은 서비스에서 아직 새 버전을 받지 않은 대상. 환경 요인 판별과 라이브 베이스라인에 쓴다 |
| 관측 쿼럼 | 중앙(SSH)과 에이전트 두 관측점의 판정이 일치해야 위반으로 인정하는 장치 |
| 실행기(executor) | 롤백 전략 구현. symlink, container, vsphere, nutanix, kvm, openstack, exec, webhook |
| 트래픽 제어기 | 로드밸런서 풀에서 대상을 빼고 넣는 구현. nginx, haproxy, envoy, f5, aws_alb, octavia |
| 서킷 브레이커 | 롤백 실패가 누적되면 자동 롤백과 배포 게이트를 동결하는 전역 비상 정지 장치 |
| blast radius | 드레인 후에도 풀에 남아야 하는 최소 정상 멤버 수 계산 |
| 승인 모드 | `rollback.mode: approve`. FAIL 시 롤백 계획을 만들고 사람의 승인 후 실행한다 |
| 저널 / 상태 저장소 | 모든 결정과 조치를 기록하는 append-only 로그. 파일(JSONL) 또는 PostgreSQL |
| 검증됨 / 실험적 | 플러그인 검증 수준. 실제 시스템에서 확인했으면 검증됨, 모의 서버·시뮬레이터만 거쳤으면 실험적(docs/09) |
| 출시 게이트 | 상용 1차 출시 조건(docs/05 "상용 1차 범위", docs/11) |

## 1.5 참조 문서

| 문서 | 경로 |
|---|---|
| 아키텍처 | docs/01-architecture.md |
| 설정 명세 | docs/02-config-spec.md |
| 엔진 설계 | docs/03-engine-design.md |
| 안전장치 | docs/04-safety-circuit-breaker.md |
| 로드맵 | docs/05-roadmap.md |
| 오픈 API | docs/06-api.md, api/openapi.yaml |
| 설치 / 업그레이드 | docs/07-install.md, docs/08-upgrade.md |
| 호환성 매트릭스 | docs/09-compatibility.md |
| 보안 가이드 | docs/10-security.md |
| 파일럿 운영 | docs/11-pilot.md |
| 아키텍처 설계서 / 상세 설계서 | VGL-SI-02, VGL-SI-03 |

## 1.6 표기 규칙

| 항목 | 값과 의미 |
|---|---|
| ID | `FR-<영역>-<번호>`(기능), `NFR-<영역>-<번호>`(비기능) |
| 출처 | 로드맵 절(예: docs/05 M0-1) 또는 설계 문서·결정 사항 |
| 우선순위 | **1차**: 상용 1차 출시 범위, **2차**: 1차 이후 |
| 구현 상태 | **완료**: 코드와 자동 테스트 존재, **부분**: 일부만 구현(차이를 비고에 기재), **미착수**: 코드 없음. 괄호 안은 커밋 ID 또는 PR 번호 |
| 검증 방법 | Go 테스트 이름(`internal/...`의 `TestXxx`), CI 작업(`.github/workflows/ci.yml`의 test·api·vuln·package·demo, `.github/workflows/load.yml`의 부하 시험), 또는 실장비가 필요하면 "M8 실장비 랩" |

> 구현 상태는 master 커밋 `537870c`(PR #14 병합, 2026-10-11) 기준이다. PR #12(M5-4)는 `dba3dbe`로, PR #13(리뷰 결함 수정)은 `dd9a055`로, PR #14(관측 장치 가드의 프로브 유형 구분)는 `537870c`로 master에 병합되었다. 표에 적은 테스트 이름은 모두 저장소에 존재함을 확인했다. 실행 결과: `dd9a055`에서 로컬(Windows) `go test` 277개 통과·0개 실패(최상위 239, 하위 38. 42개 패키지 중 32개에 테스트), 최소 빌드(`-tags minimal`) 테스트 59개(최상위 50, 하위 9) 통과. CI는 PR #13 헤드 커밋 `189dfbb`에서 test·api·vuln·package·demo(실행 38091832392)와 load 워크플로우 두 규모(프로브 3개·10개, 실행 38091832358)가 모두 통과했고, 병합 후 master `dd9a055` 푸시 CI(실행 38092384199)도 다섯 작업 모두 통과했다. PR #14는 헤드 `c4f798a`에서 CI 다섯 작업(실행 38093154231)과 load 두 규모(실행 38093154321)가, 병합 후 master `537870c` 푸시 CI(실행 38093673759)가 통과했다. 위 로컬 테스트 수는 `dd9a055` 측정값이며, PR #14는 기존 테스트 `TestObserverDegradedHoldsProbeFailures`에 확인을 더했을 뿐(하위 테스트 없음) 테스트 수를 바꾸지 않았다(VGL-SI-06).

---

# 2. 진행 현황 요약

docs/05-roadmap.md "진행 현황"과 git 이력을 대조한 결과이다. PR 번호: #1 M1, #3 M0, #4 M3, #5 로드맵 상용화 검토, #6 M7, #7 M8 CI, #8 M4, #9 M6, #10 M8 도구, #11 M5-3, #12 M5-4(병합 `dba3dbe`), #13 리뷰 결함 수정(병합 `dd9a055`), #14 관측 장치 가드 프로브 유형 구분(병합 `537870c`).

| 마일스톤 | 상태 | 근거(커밋·PR) |
|---|---|---|
| 1단계 프로토타입 | 완료 | `d1e6411` |
| M0 기반(저장소·HA, 인증, 비밀, 감사, 관측성) | 완료 | `5df283c`, `6e29a18`, `31d5c34`, `dc88133`, `9fd9e74`, PR #3 |
| M1 입력 간소화 | 완료 | `2a2ab09`, `c6ab1d6`, `50806e1`, PR #1 |
| M3 오픈 API 핵심(M3-1~M3-3) | 완료 | `a9d7d25`, `44763bd`, `bb68418`, PR #4 |
| M7 OpenStack | 구현 완료, 실장비 검증 대기 | `6181136`, PR #6 |
| M8 CI | 완료 | `74d9fb2`, PR #7 |
| M8 랩·파일럿 도구 | 완료(도구), 랩·파일럿 미실시 | `da5a97a`, `ba7a70b`, PR #10 |
| M4 운영 콘솔·변경관리 | 1차 범위 완료 | `5474c0c`, `0bd6a2b`, `5c86a1a`, `134257d`, PR #8 |
| M6 출시·지원 | 1차 범위 완료, 출시는 M8 대기 | `aff2841`, `e03d099`, `b590b1c`, `ce19709`, PR #9 |
| M5-3 SSH·대상 부하 개선 | 완료 | `cfb1daf`, `23c00a4`, `2641d9b`, PR #11 |
| M5-4 부하·카오스 | 완료 | `9f9470e`(저장소 장애 중 기록 큐), `9eac97d`(카오스 시나리오·부하 하네스), `a0c000e`(부하 결과 기록), PR #12(병합 `dba3dbe`) |
| 리뷰 결함 수정 | 완료 | `f696bc9`(관측 장치 과부하 가드, 저장소 장애 중 롤백 lease), `313f651`(로컬 CLI 권한 제한), `277dc3d`(Helm 인증·TLS·메모리), `22dc927`(HA 전달 TLS), `c9e39f1`(SIEM TLS, 키 체인), `2195530`(`watch --server` 게이트 플래그, ServiceNow 재시도, HSTS), PR #13(병합 `dd9a055`). `c4f798a`(관측 장치 가드가 직접 재는 프로브의 지표만 보류), PR #14(병합 `537870c`) |
| M2, M3-4~M3-6, M5-1, M5-2 | 미착수 | 2차 |

---

# 3. 기능 요구사항

## 3.1 수집(관측) — FR-COL

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-COL-01 | HTTP 헬스 프로브: 상태 코드, 본문 정규식, JSON 경로 검사, 지연·연속 실패·연속 타임아웃 지표를 낸다 | docs/02 probes | 1차 | 완료 (`d1e6411`) | `TestHTTPProbeAndJSONPath`, `TestHTTPTimeoutsCounted`, CI demo(검증됨) |
| FR-COL-02 | gRPC(`grpc.health.v1`)·TCP 프로브 | docs/02 probes | 1차 | 완료 (`d1e6411`) | 단위 테스트, 실제 gRPC 서버는 M8 실장비 랩 |
| FR-COL-03 | 호스트 프로브: `/proc`을 1회 왕복으로 읽어 load·CPU·메모리·디스크 지표 산출(Linux 전용) | docs/02, docs/03 1.2 | 1차 | 완료 (`d1e6411`) | `TestHostParse`, `TestHostProbeOverRunner` |
| FR-COL-04 | Docker 프로브: 원격 docker.sock을 SSH 채널로 터널링해 상태·재시작·OOM 이벤트 수집. Podman 호환 소켓 지원 | docs/03 1.2 | 1차 | 완료 (`d1e6411`) | `TestDockerProbe`, 실제 엔진은 M8 실장비 랩 |
| FR-COL-05 | 앱 로그·액세스 로그 프로브: 원격 `tail -F`, 로컬 로테이션 감지, 1초 버킷 집계, 지연 리저버 샘플링 | docs/03 1.2 | 1차 | 완료 (`d1e6411`) | `TestParseAccessLine`, `TestAccessAndAppLogLocalFollowWithRotation`, CI demo(검증됨) |
| FR-COL-06 | DB 프로브: 매 주기는 유지 커넥션 1개로 쿼리, `pool_check_interval`(기본 1m)마다 `pool_size`개 동시 확보 점검 | docs/05 M5-3 | 2차 | 완료 (`2641d9b`) | `TestDBProbeFullCheckCadence`, 실제 DB 서버는 M8 실장비 랩 |
| FR-COL-07 | 원격 로그 필터 `log.remote_grep`: 대상에서 `grep -E`로 먼저 걸러 전송. 이때 `lines` 지표를 쓰는 규칙은 거부 | docs/05 M5-3 | 2차 | 완료 (`2641d9b`) | `TestLogRemoteGrep`, `TestRemoteGrepHasNoLineCount` |
| FR-COL-08 | 프로브 장애 시 `<id>.probe_error`를 남기고 지수 백오프(1s~30s)로 재시작 | docs/03 1.2 | 1차 | 완료 (`d1e6411`) | `TestCollectorRestartsFailingProbe` |
| FR-COL-09 | 내장 시계열 저장소: (대상, 메트릭)별 보존 30분, 윈도 집계(avg·rate·p50~p99 등), 관측점(source) 보존 | docs/03 1.3 | 1차 | 완료 (`d1e6411`) | `TestAggregations`, `TestWindowBoundsAndSource`, `TestOutOfOrderAndRetention`, `TestSustainedHighRateStaysLinear` |
| FR-COL-10 | SSH 세션 예산: 대상별 `max_sessions`(기본 8) 중 `reserved_sessions`(기본 2)는 롤백·트래픽 변경 전용 | docs/05 M5-3 | 2차 | 완료 (`cfb1daf`) | `TestSessionBudgetKeepsRoomForRollback`, `TestSessionBudgetDefaults`, `TestDoctorWarnsOnSSHSessionBudget` |
| FR-COL-11 | 에이전트 없이 SSH(bastion 다단 점프 포함)와 장비 API로 수집·제어한다 | docs/01 1 | 1차 | 완료 (`d1e6411`) | 실행기·프로브 단위 테스트(모의 Runner), 실제 대상은 M8 실장비 랩 |

## 3.2 판정 — FR-DEC

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-DEC-01 | 복합 규칙: `any`/`all`/`not` 트리, 3값 논리(참·거짓·알 수 없음), 단락 평가 금지 | docs/02 rules, docs/03 2.1 | 1차 | 완료 (`d1e6411`) | `TestCompositeAnyRule`, `TestNotAndAll`, `TestNodeYAMLShape` |
| FR-DEC-02 | 가중 비율(`ratio_of`): 트래픽 0이면 알 수 없음 | docs/02 rules | 1차 | 완료 (`d1e6411`) | `TestErrorRatioWeighted` |
| FR-DEC-03 | 연속 위반(`for`)과 히스테리시스(`reset_after`) | docs/02 rules | 1차 | 완료 (`d1e6411`) | `TestConsecutiveAndHysteresis` |
| FR-DEC-04 | 베이스라인: 배포 전 스냅샷(`vigilante baseline`), 라이브 대조군, 체인(대조군 우선), 절대 하한 `min_value` | docs/03 2.2 | 1차 | 완료 (`d1e6411`) | `TestBaselineIncrease`, `TestCaptureAndControlBaseline` |
| FR-DEC-05 | 단계 관측: 관측 창, warmup(수집만), `eval_interval`, 위반 시 즉시 FAIL | docs/02 phases | 1차 | 완료 (`d1e6411`) | `TestFailFast`, `TestPassAfterWindow`, `TestWarmupIgnoresEarlyErrors`, `TestCanaryPassPromotes` |
| FR-DEC-06 | 환경 요인 분리: 대조군에서도 같은 규칙이 참이면 HOLD | docs/03 2.3 | 1차 | 완료 (`d1e6411`) | `TestEnvironmentalHold`, `TestEnvironmentalProblemHolds` |
| FR-DEC-07 | 관측 쿼럼: 중앙과 에이전트 관측이 엇갈리면 HOLD | docs/03 2.3 | 1차 | 완료 (`d1e6411`) | `TestObserverQuorum` |
| FR-DEC-08 | 증거 충분성: `min_samples` 미달 또는 전부 알 수 없음이면 INCONCLUSIVE, `on_inconclusive`(hold·pass·rollback) 적용 | docs/03 2.3 | 1차 | 완료 (`d1e6411`) | `TestInconclusivePolicies` |
| FR-DEC-09 | 규칙 조치 `notify`(경고만)와 `hold`(사람 판단) | docs/02 rules | 1차 | 완료 (`d1e6411`) | `TestNotifyRuleDoesNotFail` |
| FR-DEC-10 | rollback 규칙이 없는 서비스·단계는 설정 검증에서 거부 | docs/05 진행 현황(선행 수정) | 1차 | 완료 (`3969470`) | `TestServiceWithoutRollbackRuleRejected` |
| FR-DEC-11 | CI 종료 코드 계약: 0 PASS, 1 오류, 2 롤백 완료, 3 롤백 실패·서킷 OPEN·승인 대기·동결·변경 티켓 게이트 거부, 4 HOLD. 서버 위임(`watch --server`)도 게이트 거부 시 3 | docs/04 4, docs/08 | 1차 | 완료 (`d1e6411`, 원격 모드 게이트 종료 코드는 `2195530`, PR #13) | CI demo(시나리오별 종료 코드 확인), `TestWatchRemoteGateRefusalExitsThree` |
| FR-DEC-12 | 규칙 프리셋: 내장 4종(`java-web`, `container-api`, `static-web`, `worker`), 조직 프리셋 디렉토리, `name@version` 고정 | docs/05 M1-2 | 1차 | 완료 (`c6ab1d6`) | `TestBuiltinPresetsProduceValidServices`, `TestLatestAndPinnedVersions`, `TestOrganisationPresetDir` |
| FR-DEC-13 | 관측 장치 과부하 가드 `safety.observer_guard`(기본 켜짐): 스케줄링 지연(`max_lag` 1s), 루프백 왕복(`loopback_timeout` 1s), 3개(`min_services`) 이상 서비스에 걸친 대상 50%(`timeout_share`) 이상의 프로브 시간 초과 중 하나라도 걸리면 관측 장치 저하로 보고, 오케스트레이터가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, SSH로 읽는 `host`)의 `up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts`, `timeout` 지표와 모든 프로브의 `probe_error`에 기반한 위반을 롤백 대신 HOLD. 같은 이름이라도 `log`·`access_log`·`docker` 프로브의 지표(예: 액세스 로그의 `latency_ms`)는 그대로 판정. 회복 후 `grace`(1m) 동안 유지 | docs/05 M5-4 후속, docs/04 S14a | 1차 | 완료 (`f696bc9`, PR #13. 프로브 유형 구분은 `c4f798a`, PR #14) | `TestObserverDegradedHoldsProbeFailures`, `TestChaosObserverDegradedHolds`, `TestDegradedSpansAndGrace`, `TestSpreadNeedsSeveralServices`, `TestLoopbackAndStartStop`, `TestDisabled` |

## 3.3 롤백 실행 — FR-RB

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-RB-01 | 롤백 플랜: `traffic.drain → app.rollback → app.verify → probe.verify → traffic.enable`(+ `wait`). 생략 시 기본 플랜은 `[traffic.drain] → app.rollback → app.verify → [traffic.enable]`(트래픽 제어기가 있을 때만 괄호 단계 포함, `probe.verify`는 명시해야 함) | docs/02 rollback | 1차 | 완료 (`d1e6411`) | `TestCanaryFailDrainsRollsBackAndEnables` |
| FR-RB-02 | 단계별 timeout(기본 2m)과 재시도(기본 3회, 2s부터 2배, 상한 30s) | docs/04 S1 | 1차 | 완료 (`d1e6411`) | `TestChaosLoadBalancerTransientErrors`(오류 후 재시도), `TestChaosLoadBalancerLatency`(단계 시간 제한 후 재시도). PR #12(`9eac97d`) |
| FR-RB-03 | 에스컬레이션 래더: 1차 전략 실패 시 다음 실행기를 순서대로 시도, `require_approval`이면 승인 대기 | docs/04 S2·S3 | 1차 | 완료 (`d1e6411`) | `TestEscalationRecovers`, `TestApprovalGate` |
| FR-RB-04 | 모든 전략 실패 시 `traffic.enable`을 실행하지 않아 대상을 격리 상태로 유지 | docs/04 S4 | 1차 | 완료 (`d1e6411`) | `TestRollbackFailureIsolatesAndOpensCircuit`. LB 완전 장애 시 제자리 롤백 후 ROLLBACK_FAILED는 `TestChaosLoadBalancerDown`(PR #12) |
| FR-RB-05 | 실행기 symlink(원자적 링크 전환), container(보상 트랜잭션), exec, webhook | docs/03 3.2 | 1차 | 완료 (`d1e6411`) | `TestSymlinkRollbackAndVerify`, `TestContainerSwitch`, `TestContainerCompensatesOnStartFailure`, `TestWebhookExecutor`. symlink·container·exec는 M8 실장비 랩 |
| FR-RB-06 | 실행기 vsphere·nutanix·kvm(VM 스냅샷 복원) | docs/03 3.2 | 1차(M8 검증 통과분만 정식) | 완료 (`d1e6411`), 실험적 | `TestVSphereSnapshotRevert`(vcsim), `TestNutanixRestore`, `TestKVMSnapshotRevert`, M8 실장비 랩 |
| FR-RB-07 | 실행기 openstack: 볼륨 부팅은 Cinder 스냅샷 revert, 이미지 부팅은 Nova rebuild, 재실행 안전, 최신 N개 유지 | docs/05 M7-2 | 1차 | 완료 (`6181136`), 실험적 | `TestOpenStackVolumeSnapshotRevertAndReplay`, `TestOpenStackImageSnapshotRebuild`, `TestOpenStackRevertRefusedLeavesClearError`, M8 실장비 랩 |
| FR-RB-08 | 체크포인트(`vigilante prepare`): 배포 전 링크 대상·이미지·스냅샷 기록 | docs/01 3 | 1차 | 완료 (`d1e6411`) | 실행기 단위 테스트, CI demo |
| FR-RB-09 | 수동 롤백(`vigilante rollback`, `POST /v2/deployments/{id}/rollbacks`): 서킷·플래핑 검사를 거치지 않음, 실행기 지정 가능 | docs/04 5 | 1차 | 완료 (`d1e6411`, v2는 `a9d7d25`) | `TestV2ContractLifecycle` |
| FR-RB-10 | dry-run: 읽기 명령과 체크포인트만 실행하고 변경은 로그로 남김 | docs/04 S18 | 1차 | 완료 (`d1e6411`) | `TestDryRunDoesNotMutate`, `TestOctaviaDryRun` |
| FR-RB-11 | 크래시 재개: 저널에 기록된 완료 단계를 건너뛰고 나머지를 이어서 실행 | docs/04 S12 | 1차 | 완료 (`d1e6411`) | `TestResumeSkipsCompletedSteps` |
| FR-RB-12 | 이전 버전 자동 결정: 서비스별 마지막 정상 버전, `mark-good` 등록, 롤백 대상 미상이면 관측 전 거부 | docs/05 M1-1 | 1차 | 완료 (`2a2ab09`) | `TestLastGoodVersionAndMarkGood`, `TestRequireRollbackTarget`, `TestCreateFillsPreviousFromLastGood` |

## 3.4 트래픽 제어 — FR-TR

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-TR-01 | nginx: upstream 파일 `down` 토글, `nginx -t` 실패 시 백업 복원 | docs/03 3.2 | 1차 | 완료 (`d1e6411`), 실험적 | `TestNginxDownRewrite`, `TestNginxController`, M8 실장비 랩 |
| FR-TR-02 | haproxy: Runtime API drain/maint | docs/03 3.2 | 1차 | 완료 (`d1e6411`), 실험적 | `TestHAProxyRuntimeAPI`, M8 실장비 랩 |
| FR-TR-03 | envoy: 파일 기반 EDS `DRAINING` + 원자적 교체 | docs/03 3.2 | M8 결과에 따름 | 완료 (`d1e6411`), 실험적 | `TestEnvoyEDS`, M8 실장비 랩 |
| FR-TR-04 | f5: iControl REST 멤버 session/state 변경, 토큰 인증 캐시 | docs/03 3.2 | 1차 | 완료 (`d1e6411`), 실험적 | `TestF5iControl`, M8 실장비 랩 |
| FR-TR-05 | aws_alb: Deregister/Register와 상태 대기 | docs/03 3.2 | M8 결과에 따름 | 완료 (`d1e6411`), 실험적 | `TestALB`, M8 실장비 랩 |
| FR-TR-06 | octavia: 멤버 `admin_state_up` 변경, LB `PENDING_*` 대기, 409 재시도, 순차 적용 | docs/05 M7-3 | 1차 | 완료 (`6181136`), 실험적 | `TestOctaviaDrainEnableWaitsForActive`, `TestOpenStackCanaryRollsBackThroughOctavia`, M8 실장비 랩 |
| FR-TR-07 | Azure LB·Application Gateway, Citrix ADC | docs/05 MVP 표 | 2차 | 미착수(`exec`/`webhook`로 우회) | 해당 없음 |

## 3.5 안전장치 — FR-SAF

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-SAF-01 | 서킷 브레이커: 창(기본 1h) 안 롤백 실패 N회(기본 3)면 OPEN, 자동 롤백 금지·배포 게이트 폐쇄, HALF_OPEN 시험 1건, 수동 trip/reset | docs/04 1 | 1차 | 완료 (`d1e6411`) | `TestBreakerLifecycle`, `TestBreakerWindowPrunes`, `TestManualTripNeedsReset`, `TestRollbackFailureIsolatesAndOpensCircuit` |
| FR-SAF-02 | 서킷 상태 영속화: 재시작·CI 잡 간 유지 | docs/04 1 | 1차 | 완료 (`d1e6411`) | `TestRollbackFailureIsolatesAndOpensCircuit` |
| FR-SAF-03 | blast radius: `min_healthy`·`min_healthy_percent`로 드레인 배치 크기 결정, 불가하면 제자리 롤백 | docs/04 3 | 1차 | 완료 (`d1e6411`) | `TestDrainBatch`, `TestBlastRadiusRefusesDrain` |
| FR-SAF-04 | 플래핑 제한: 시간당 최대 롤백 수(기본 3)와 cooldown 초과 시 자동 롤백 차단, 실패 대상만 격리 | docs/04 S10 | 1차 | 완료 (`d1e6411`) | `TestGuardLockAndFlapping`, `TestFlappingGuardBlocksAndIsolates` |
| FR-SAF-05 | 서비스 락: 같은 서비스의 동시 롤백 거부(프로세스 내 + 저장소 lease) | docs/04 S11, docs/05 M0-1 | 1차 | 완료 (`d1e6411`, lease는 `5df283c`) | `TestGuardLockAndFlapping`, `TestLeases`, `TestLeaseRaceHasOneWinner` |
| FR-SAF-06 | 설정 검증(`validate`): 알 수 없는 키 거부, 교차 참조 검사 | docs/04 S17 | 1차 | 완료 (`d1e6411`) | `TestUnknownFieldsRejected`, `TestValidationCatchesBrokenReferences`, `TestReferenceConfigValidates` |
| FR-SAF-07 | 롤백 시작 시 상태 저장소 장애: 서비스 lease를 `safety.rollback_lease.wait`(기본 10s) 동안 재시도하고, `on_unavailable: proceed`(기본)면 프로세스 내 락만으로 롤백 진행(이벤트·감사 `lease.unavailable`·경고 알림), 저장소 복구 후 다른 프로세스가 lease를 가지면 `lease.conflict` 알림. `fail`이면 ROLLBACK_FAILED(이전 동작) | docs/04 S12a | 1차 | 완료 (`f696bc9`, PR #13) | `TestGuardStoreUnreachable`, `TestChaosStoreOutageDuringRollback`, `TestChaosStoreOutageLeaseFailMode` |

## 3.6 상태 저장·고가용성 — FR-HA

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-HA-01 | 상태 저장소 추상화: 파일(JSONL)과 PostgreSQL 백엔드, append-only, 재생으로 상태 복원 | docs/05 M0-1 | 1차 | 완료 (`5df283c`) | `TestAppendAndLoad`(파일·PostgreSQL), CI test(PostgreSQL 16) |
| FR-HA-02 | 리더 선출: PostgreSQL lease(기본 TTL 15s, TTL/3 갱신), 리더만 판정·롤백 | docs/05 M0-1 | 1차 | 완료 (`5df283c`) | `TestFailoverOnPartition`, `TestGracefulShutdownHandsOverFast` |
| FR-HA-03 | 펜싱: 리더 자리를 잃은 노드의 기록은 저장소가 거부 | docs/04 S12 | 1차 | 완료 (`5df283c`) | `TestPostgresFencing` |
| FR-HA-04 | 새 리더가 중단된 롤백을 이어서 완료 | docs/05 M0 검증 기준 | 1차 | 완료 (`5df283c`) | `TestHAFailoverFinishesInterruptedRollback` |
| FR-HA-05 | 팔로워는 API 요청을 리더로 전달. 리더 advertise URL이 https면 `server.ha.tls`(`ca_file`, `server_name`, 클라이언트 인증서)로 리더 인증서를 파드 IP가 아닌 설정한 이름으로 검증 | docs/02 server | 1차 | 완료 (`5df283c`, TLS 전달은 `22dc927`, PR #13) | `TestFollowerForwardsToLeader`, `TestHAClientServerName` |
| FR-HA-06 | 스키마 마이그레이션 제어: 되돌리기 파일, `store migrate --down-to`, 다운그레이드 가드, `store status`, `auto_migrate: false` | docs/05 M6-2 | 1차 | 완료 (`e03d099`) | `TestMigrateUpgradeDowngradeAndGuard`, `TestDowngradeIsAllOrNothing`, `TestStoreCommand` |
| FR-HA-07 | 저장소 장애 중 기록을 메모리 큐에 순서대로 보관 후 복구 시 기록(write-behind), 지표 `vigilante_store_pending_writes` | docs/05 M5-4(저장소 일시 단절) | 2차 | 완료 (`9f9470e`, PR #12) | `TestChaosStoreOutageDuringRollback`(롤백 계속, 기록 무손실, 해시 체인 유지). 한계: 장애 중 크래시 시 큐 유실, HA에서 장애가 `lease_ttl`보다 길면 리더가 물러나고 다른 노드가 리더가 되면 큐는 펜싱되어 버려짐 |
| FR-HA-08 | 수집 샤딩(워커 노드, 일관 해싱, 30초 내 재배정) | docs/05 M5-1 | 2차 | 미착수 | 해당 없음 |

## 3.7 인증·권한 — FR-AUTH

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-AUTH-01 | OIDC 사용자 인증과 그룹→역할 매핑 | docs/05 M0-2 | 1차 | 완료 (`6e29a18`) | `TestOIDCGroupsAndUsers` |
| FR-AUTH-02 | 서비스 계정 토큰(`vgl_…`, SHA-256 저장, 만료), 비상용 legacy 토큰 | docs/05 M0-2 | 1차 | 완료 (`6e29a18`) | `TestServiceAccountsAndLegacy` |
| FR-AUTH-03 | 역할 viewer·deployer·operator·admin(+agent), 범위 `*`·`team=`·`service=` | docs/02 auth | 1차 | 완료 (`6e29a18`) | `TestRoleScopeMatrix`, `TestRoleAndScopeEnforcement` |
| FR-AUTH-04 | 4-eyes: 배포 생성자·롤백 요청자는 승인 불가 | docs/05 M0-2 | 1차 | 완료 (`6e29a18`) | `TestFourEyesApproval` |
| FR-AUTH-05 | OAuth 2.0 client credentials, API 키, 스코프 8종 | docs/05 M3-2 | 1차 | 완료 (`44763bd`) | `TestV2OAuthClientsAndScopes`, `TestV2Scopes` |
| FR-AUTH-06 | 호출자별 토큰 버킷 호출 한도, 비상 조치 별도 버킷 | docs/05 M3-2 | 1차 | 완료 (`44763bd`) | `TestV2RateLimits`, `TestRateLimitConfigValidation` |
| FR-AUTH-07 | LDAP/AD 직접 연동 | docs/05 M0-2(대안) | 2차 | 미착수 | 해당 없음 |
| FR-AUTH-08 | API 게이트웨이 뒤 배치(신뢰 프록시 헤더, 게이트웨이 mTLS 종료) | docs/05 M3-2 | 2차 | 미착수(수요 시 추가) | 해당 없음 |
| FR-AUTH-09 | 로컬 CLI 권한 제한 `auth.local_cli`(`auto` 기본, `full`, `restricted`): `auto`는 인증이 설정되어 있으면 제한. 제한 시 서버 없이 실행하는 승인 결정(`rollback --approve`·`--reject`), 에스컬레이션 승인, `circuit reset`·`trip`, `--freeze-override`는 `--break-glass REASON`이 있어야 하며 감사(`breakglass.<action>`)와 critical 알림을 남김. 로컬 승인 결정에도 `four_eyes` 적용 | docs/10, CHANGELOG Fixed (security) | 1차 | 완료 (`313f651`, PR #13) | `TestLocalCLIRestricted`, `TestLocalFourEyes` |

## 3.8 비밀관리 — FR-SEC

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-SEC-01 | 비밀 참조 `*_ref`: `vault:`, `env:`, `file:`. 설정에 평문 비밀번호 키 없음 | docs/05 M0-3 | 1차 | 완료 (`31d5c34`) | `TestEnvAndFileRefs`, `TestSecretsValidation` |
| FR-SEC-02 | Vault KV v2: token·AppRole·Kubernetes 로그인, 네임스페이스, 사설 CA, 403 시 재로그인, TTL 캐시 | docs/02 secrets | 1차 | 완료 (`31d5c34`) | `TestVaultAppRoleKVCacheAndRelogin`, `TestKubernetesAuth`, `TestVaultErrors` |
| FR-SEC-03 | Vault SSH CA 단기 인증서로 SSH 접속(수명 80% 경과 시 재발급) | docs/05 M0-3 | 1차 | 완료 (`31d5c34`) | `TestSignSSHKey`, `TestSSHWithVaultCertificate`, `TestSSHCertificateWrongPrincipalRejected` |
| FR-SEC-04 | 해석한 비밀값을 로그에서 `[REDACTED]`로 가림 | docs/02 secrets | 1차 | 완료 (`31d5c34`) | `TestRedactHandler` |
| FR-SEC-05 | CyberArk 연동 | docs/05 M0-3 | 2차 | 미착수(수요 확인 후) | 해당 없음 |

## 3.9 감사 — FR-AUD

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-AUD-01 | 모든 기록에 actor·source·action·reason·ticket, 권한 거부도 기록 | docs/05 M0-4 | 1차 | 완료 (`dc88133`) | `TestAuditTrailOverAPI` |
| FR-AUD-02 | 해시 체인 변조 검출, `vigilante audit verify` | docs/05 M0-4 | 1차 | 완료 (`dc88133`) | `TestChainDetectsTampering`, `TestLegacyEntriesBeforeChain` |
| FR-AUD-03 | SIEM 전송: syslog RFC 5424 또는 CEF, 비동기(롤백 비차단). 전송로 `tcp://`, `udp://`, `tls://`(RFC 5425 옥텟 카운팅, 기본 포트 6514, `audit.syslog.tls`로 CA·클라이언트 인증서·`server_name`·`min_version`) | docs/02 audit | 1차 | 완료 (`dc88133`, TLS는 `c9e39f1`, PR #13) | `TestSyslogExporter`, `TestSyslogExporterTLS`, `TestSyslogExporterTLSRejectsUntrustedCollector`, `TestNewExporterTLSConfig`, `TestAuditValidation` |
| FR-AUD-04 | 보존 정리(`audit prune`): 아카이브 후 앵커 기록으로 대체 | docs/02 audit | 1차 | 완료 (`dc88133`) | `TestPruneArchivesAndKeepsState` |
| FR-AUD-05 | 감사 조회·내보내기(CLI, `GET /v1/audit`, `GET /v2/audit-events`) | docs/05 M0-4 | 1차 | 완료 (`dc88133`, v2는 `a9d7d25`) | `TestAuditTrailOverAPI`, `TestV2ContractLifecycle` |
| FR-AUD-06 | Kafka 내보내기 | docs/05 M0-4(선택) | 2차 | 미착수 | 해당 없음 |
| FR-AUD-07 | 키 체인: `audit.chain_key_ref`(32바이트 이상)를 설정하면 새 기록마다 체인 해시의 HMAC-SHA256(`mac`)을 붙여, 저장소 쓰기 권한만 있고 키가 없는 사람이 기록을 고친 뒤 체인을 다시 계산하는 것을 검출. `audit verify`가 MAC 확인(`--key REF`로 대체 가능), 키 도입 전 기록은 unkeyed로 보고. 스키마 마이그레이션 없음 | CHANGELOG Fixed (security) | 1차 | 완료 (`c9e39f1`, PR #13) | `TestKeyedChainDetectsRecomputedTampering`, `TestKeyedChainWrongKey`, `TestKeyedChainLegacyEntries`, `TestKeyedChainCoversUnkeyedPrefix`, `TestKeyedChainAcrossPrune`(앞 5건은 file·postgres 각각), `TestResolveChainKey` |

## 3.10 입력 간소화·사전 점검 — FR-INP

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-INP-01 | CI 환경변수(Jenkins·GitLab·GitHub·`VIGILANTE_*`, `git describe`)로 배포 ID·버전 자동 채움, 출처 기록 | docs/05 M1-1 | 1차 | 완료 (`2a2ab09`) | `TestDeploymentID`, `TestVersion`, `TestResolveInputsFromCIAndJournal`, `TestResolveInputsKeepsExplicitFlags` |
| FR-INP-02 | `vigilante doctor`: 자격증명·접속·sudo·프로브·로그 형식·실행기·LB 풀 읽기 전용 점검, 조치 힌트, `--json`·`--junit` | docs/05 M1-3 | 1차 | 완료 (`50806e1`) | `TestDoctorFindsRealProblems`, `TestDoctorAccessLogFormatMismatch`, `TestHint`, `TestDoctorChecksVaultReferences` |
| FR-INP-03 | OpenStack doctor 점검(Keystone, 부팅 방식, Cinder 마이크로버전, 쿼터, Octavia 풀) | docs/05 M7-4 | 1차 | 완료 (`6181136`) | `TestOpenStackDiagnose` |
| FR-INP-04 | 서버 정기 doctor 실행과 결과 지표·콘솔 노출, `doctor.failed` 이벤트 | docs/05 M1-3, M3-3 | 2차 | 미착수 | 해당 없음 |
| FR-INP-05 | `init`·`discover`·`inventory import`·`suggest` | docs/05 M2-3 | 2차 | 미착수 | 해당 없음 |
| FR-INP-06 | 설정 분리·멀티테넌시, GitOps·정책 검증 | docs/05 M2-1, M2-2 | 2차 | 미착수 | 해당 없음 |

## 3.11 오픈 API — FR-API

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-API-01 | OpenAPI 3.1 명세(`api/openapi.yaml`)를 원본으로 하고 서버 라우트와 일치 | docs/05 M3-1 | 1차 | 완료 (`a9d7d25`) | `TestV2RoutesMatchSpec`, CI api(Spectral 린트) |
| FR-API-02 | v2 공통 규약: problem+json 오류, 커서 페이지, `Idempotency-Key`(24시간), `ETag`/`If-Match`, 202 + Operation | docs/06 공통 규약 | 1차 | 완료 (`a9d7d25`) | `TestV2ContractLifecycle`, `TestOperationsAndIdempotencySurviveRestart` |
| FR-API-03 | v2 하위 호환 깨짐 자동 차단 | docs/05 M3-6 | 1차 | 완료 (`74d9fb2`) | CI api(oasdiff) |
| FR-API-04 | REST v1 유지(기존 CI 연동), 수신 웹훅(GitHub HMAC·GitLab·Jenkins) | docs/06 | 1차 | 완료 (`d1e6411`) | `TestAuthAndLifecycle`, `TestGitHubWebhookSignature`, `TestParseWebhookIgnoresNonSuccess` |
| FR-API-08 | v1 배포 생성 게이트 필드: `POST /v1/deployments` 본문 `change_ticket`, `freeze_override`(admin만), 게이트 거부 시 `{"error", "code"}`(`circuit_open`, `change_frozen`, `change_ticket_invalid` 409, `itsm_unavailable` 503 + `Retry-After`). `watch --server`가 `--ticket`·`--freeze-override`를 전달 | CHANGELOG Fixed (integration) | 1차 | 완료 (`2195530`, PR #13) | `TestV1CreateGateFields`, `TestWatchRemoteSendsTicketAndFreezeOverride`, `TestWatchRemoteGateRefusalExitsThree` |
| FR-API-05 | 판정 평가 API `PUT /v2/deployments/{id}/feedback` | docs/05 M8-2 | 1차 | 완료 (`ba7a70b`) | `TestV2Feedback` |
| FR-API-06 | 외부 지표 수신, 외부 실행기 프로토콜, 외부 판정 훅 | docs/05 M3-4 | 2차 | 미착수 | 해당 없음 |
| FR-API-07 | SDK 4종, 개발자 포털, 샌드박스 | docs/05 M3-5 | 2차 | 미착수 | 해당 없음 |

## 3.12 이벤트 — FR-EVT

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-EVT-01 | CloudEvents 1.0 이벤트 18종, 클러스터 전체 순번. `approval.decided` 데이터(`deployment_id`, `service`, `kind`, `decision`, `decided_by`, `comment`)가 명세와 일치 | docs/05 M3-3 | 1차 | 완료 (`bb68418`, 명세 정정은 `2195530`, PR #13) | `TestEventsFollowDeploymentLifecycle`, `TestSequenceContinuesAfterReload`, `TestV2ApproveModeDecisions`(명세 필드 대조) |
| FR-EVT-02 | SSE 스트림 `GET /v2/events`, `Last-Event-ID` 재연결(최근 10,000건) | docs/06 이벤트 | 1차 | 완료 (`bb68418`) | `TestV2EventStreamResumesAndFilters`, `TestSubscribeHasNoHoles` |
| FR-EVT-03 | 웹훅 구독: 순서 보장·최소 한 번 전달, Standard Webhooks 서명, 6회 재시도(1초~30분), dead-letter, 연속 5건 실패 시 비활성화 | docs/06 웹훅 | 1차 | 완료 (`bb68418`) | `TestV2WebhooksDeliverInOrderSignedAndRecover`, `TestMatchesAndSignature`, `TestWebhooksSurviveReload` |
| FR-EVT-04 | 서명 비밀을 마스터 키에서 파생하고 저장하지 않음 | docs/02 api | 1차 | 완료 (`bb68418`) | `TestV2WebhooksNeedSigningKey` |
| FR-EVT-05 | 알림 채널 Slack·Teams·이메일·PagerDuty·웹훅, 서비스·팀 라우팅, 10분 중복 억제 | docs/05 M4-3 | 1차 | 완료 (`134257d`) | `TestRoutingDedupAndChannelFormats`, `TestEmail` |
| FR-EVT-06 | Opsgenie, Slack/Teams 승인 버튼 | docs/05 M4 | 2차 | 미착수 | 해당 없음 |

## 3.13 운영 콘솔·변경관리 — FR-OPS

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-OPS-01 | 내장 웹 콘솔(`/console/`, 외부 자원 없음): 현황, 배포 목록·상세, 서비스, 동결, 감사 | docs/05 M4-1 | 1차 | 완료 (`5474c0c`) | `TestStaticFilesAndTokenMode`, `TestConsoleSessionUsesTheAPI` |
| FR-OPS-02 | 콘솔 로그인: OIDC code + PKCE, 암호화 쿠키 세션, CSRF double-submit, SSO 없으면 토큰 로그인. HTTPS로 제공되면 `Strict-Transport-Security: max-age=31536000`, 보안 헤더를 로그인 엔드포인트에도 적용 | docs/02 console | 1차 | 완료 (`5474c0c`, HSTS는 `2195530`, PR #13) | `TestOIDCSignInSessionAndCSRF`, `TestCallbackRejections`, `TestSessionKeyMustBeLongEnough`, `TestConsoleRefusesUsersWithoutRoles`, `TestHSTSOverHTTPS` |
| FR-OPS-03 | 콘솔 조작(승인·거절·롤백·중단·서킷·동결)은 사유 필수, v2 API만 사용 | docs/05 M4-1 | 1차 | 완료 (`5474c0c`) | `TestConsoleSessionUsesTheAPI`, `TestAppAvoidsHTMLSinks` |
| FR-OPS-04 | 승인 모드 `rollback.mode: approve`: 계획 생성, 만료(`hold`·`rollback`), `drain_first`, 거절 시 격리 복귀, 재시작 후 유지. 미지정은 `auto`로 해석하고 경고 | docs/05 M4-2 | 1차 | 완료 (`0bd6a2b`) | `TestApproveModeWaitsDrainsAndApproves`, `TestApproveModeRejectRestoresTraffic`, `TestApprovalTimeouts`, `TestPendingApprovalSurvivesRestart`, `TestRollbackModeValidationAndWarning`, `TestV2ApproveModeDecisions` |
| FR-OPS-05 | 변경 동결: 설정(주간·기간)과 API 선언, admin 예외, 기간별 자동 롤백 허용 여부 | docs/05 M4-2 | 1차 | 완료 (`5c86a1a`) | `TestWeeklyFreezeWindows`, `TestFreezeBlocksNewPhasesUnlessOverridden`, `TestFreezeRollbackPolicy`, `TestDeclaredFreezesPersistAndEnd`, `TestV2ChangeFreeze` |
| FR-OPS-06 | ServiceNow 변경 티켓 게이트(fail-open/closed), 인시던트(중복 없음), 작업 노트. 인시던트·작업 노트는 HTTP 상태로 재시도 여부를 판단(연결 오류·429·5xx만 1s·2s·4s 후 재시도), 인시던트는 백그라운드로 생성 | docs/05 M4-3 | 1차 | 완료 (`134257d`, 재시도는 `2195530`, PR #13) | `TestChangeChecks`, `TestIncidentsAreDeduplicatedAndNotesWritten`, `TestV2ServiceNowGateIncidentsAndNotes`, `TestIncidentRetriedUntilServiceNowRecovers`, `TestRetryAfterLostCreateDoesNotDuplicate`, `TestRetryIsBounded`, `TestRetryable` |
| FR-OPS-07 | 콘솔 설정 편집, 위반 시점 지표 그래프, 데모 재생, 전략별 2인 승인, Jira SM | docs/05 M4 "1차에서 뺀 것" | 2차 | 미착수 | 해당 없음 |

## 3.14 출시·지원 — FR-REL

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-REL-01 | 최소 빌드(`-tags minimal`): vSphere·AWS ALB·gRPC 프로브·MySQL 드라이버 제외, 빠진 플러그인 사용 설정은 시작 시 거부 | docs/05 M6-1 | 1차 | 완료 (`aff2841`) | `TestMinimalBuildRefusesMissingPlugins`, CI package |
| FR-REL-02 | 산출물: 정적 바이너리, rpm·deb, distroless 이미지, Helm 차트, CycloneDX SBOM, 폐쇄망 번들, `SHA256SUMS` | docs/05 M6-1 | 1차 | 완료 (`aff2841`) | CI package(Debian 12·Rocky Linux 9 설치 확인, 차트 렌더링) |
| FR-REL-03 | 서명: cosign 키 방식, openssl만으로 확인 가능 | docs/05 M6-1 | 1차 | 부분: 절차·워크플로우 완료, **릴리스 키 쌍 미생성** | CI package(임시 키로 서명·확인) |
| FR-REL-04 | 호환성 매트릭스(`internal/compat`)와 실험적 플러그인 경고(`plugins`, `validate`, 서버 시작) | docs/05 M6-3 | 1차 | 완료 (`b590b1c`) | `TestMatrixCoversEveryPlugin`, `TestDocMatchesMatrix`, `TestWarningListsExperimentalPluginsInUse` |
| FR-REL-05 | 지원 번들(`support-bundle`): 비밀값 제거, 설정이 깨져도 수집, 무변경 | docs/05 M6-4 | 1차 | 완료 (`ce19709`) | `TestSupportBundle`, `TestBundleRedactsAndListsFiles`, `TestRedactYAML`, `TestRedactText` |
| FR-REL-06 | 서버 직접 HTTPS(`server.tls`, 인증서 무중단 교체, 선택적 클라이언트 인증서), 에이전트 `agent.tls` | docs/05 M6-1 구현 결과 | 1차 | 완료 (`ce19709`) | `TestServerAndAgentTLS`, `TestServerCertificateReloads`, `TestServerConfigErrors` |
| FR-REL-07 | `sudo_scope: changes`, `vigilante sudoers` 규칙 생성, doctor의 규칙 확인 | docs/05 M5-3 | 2차 | 완료 (`23c00a4`) | `TestSymlinkSudoChangesOnly`, `TestNginxSudoChangesOnly`, `TestRulesAndRender`, `TestDoctorChecksSudoRules` |
| FR-REL-08 | 설정 `version: v2`와 `config migrate` | docs/05 M6-2 | 2차 | 미착수(v2 설정이 생길 때) | 해당 없음 |
| FR-REL-09 | Windows 서비스 래퍼 | docs/05 M6-1 구현 결과 | 2차 | 미착수(Windows 바이너리는 CLI·CI 용도) | 해당 없음 |
| FR-REL-10 | Helm 차트 안전 기본값: 인증 설정이 없으면 렌더링 거부(`auth.allowAnonymous: true`로만 허용), 서버 TLS면 HA advertise URL·프로브·포트 이름·Ingress 백엔드·ServiceMonitor를 https로(`tls.enabled`, `tls.secretName`, `serviceMonitor.tlsConfig`), `client_auth: require` 거부, 메모리 512Mi 요청·2Gi 한도와 `GOMEMLIMIT` 한도의 90% | CHANGELOG Fixed (security, deployment) | 1차 | 완료 (`277dc3d`, PR #13) | CI package(차트 렌더링: 인증 없는 설정 거부, HA·TLS에서 `https://$(POD_IP):8088`, `scheme: HTTPS`, `GOMEMLIMIT` `1843MiB` 확인) |

## 3.15 검증 도구 — FR-VAL

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-VAL-01 | `vigilante lab run`: 체크포인트 → 불량 배포 주입 → 탐지 → 드레인 → 복원 → 복귀를 반복하고 JSON으로 기록, `lab summary` | docs/05 M8-1 | 1차 | 완료 (`da5a97a`) | `TestScenarioPassesAndSummarizes`, `TestInjectFailureStopsAndResets`, CI demo 시나리오 7 |
| FR-VAL-02 | `vigilante feedback`·콘솔·API로 판정 평가(correct·false_positive·false_negative·unclear) | docs/05 M8-2 | 1차 | 완료 (`ba7a70b`) | `TestFeedbackAndPilotReport`, `TestV2Feedback` |
| FR-VAL-03 | `vigilante pilot report`: 단계 판정, HOLD 원인, 평가, 롤백·시간 통계, 출시 게이트 판정(미달 시 종료 코드 4) | docs/05 M8-2, docs/11 | 1차 | 완료 (`ba7a70b`) | `TestComputeVerdictsTimingAndGate`, `TestGatePasses`, `TestHoldCause`, CI demo 시나리오 8 |
| FR-VAL-04 | 실장비 랩 실시(OpenStack, F5 VE, vCenter 7·8, Nutanix CE, AWS, HAProxy·Nginx·Envoy, Docker·Podman) | docs/05 M8-1 | 1차 | 미착수(장비 확보 선행) | M8 실장비 랩 |
| FR-VAL-05 | 파일럿 3개월(1개월 dry-run, 2개월 승인 모드), 배포 30건 이상 | docs/05 M8-2 | 1차 | 미착수(대상 서비스 확보 선행) | M8 파일럿 |

## 3.16 에이전트 — FR-AGT

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 |
|---|---|---|---|---|---|
| FR-AGT-01 | 같은 바이너리의 `vigilante agent`: 로컬 프로브 실행, 샘플 push(실패 시 최대 50,000개 버퍼), 하트비트 | docs/01 1, docs/02 agent | 1차 | 완료 (`d1e6411`) | `TestAgentPushesAndFailsafeRollsBack`, `TestSampleIngestTagsAgentSource` |
| FR-AGT-02 | Dead-man's switch: 하트비트가 `failsafe_after` 동안 끊기면 로컬 판정, `failsafe: rollback`이면 자기 호스트만 롤백(트래픽 단계 제외). 승인 모드 서비스는 보류 | docs/04 S13, docs/02 rollback | 1차 | 완료 (`d1e6411`, 승인 모드 예외는 `0bd6a2b`) | `TestAgentPushesAndFailsafeRollsBack` |
| FR-AGT-03 | 에이전트 등록 토큰, mTLS 인증서 자동 발급·폐기, 원격 업그레이드, 콘솔의 에이전트 상태 | docs/05 M5-2 | 2차 | 미착수(인증서는 사내 CA·cert-manager로 발급) | 해당 없음 |

---

# 4. 비기능 요구사항

docs/05의 "비기능 목표"는 **제안값이며 확정이 필요하다**고 명시되어 있다. 아래 표의 "현재 근거"는 저장소 문서에 기록된 값만 인용했으며, 기록이 없으면 "미측정"으로 표시했다.

| ID | 요구사항 | 출처 | 우선순위 | 구현 상태 | 검증 방법 / 현재 근거 |
|---|---|---|---|---|---|
| NFR-SCL-01 | 클러스터당 대상 2,000대, 동시 관측 배포 100건, 대상당 프로브 10개 | docs/05 비기능 목표 | 2차 | 부분: 단일 엔진·HTTP 시뮬레이터 대상으로 목표 규모 측정 통과(부하 하네스 `test/load`, 빌드 태그 `load`, PR #12). 수집 샤딩(M5-1) 미착수 | `.github/workflows/load.yml`(GitHub Actions ubuntu-latest). M5-4 기록(docs/05, 커밋 `a0c000e`): 대상 2,000 × 프로브 3, 동시 배포 100 → 오판 0, 2,804 요청/초, 최대 힙 352 MiB, 0.45코어. 대상 2,000 × 프로브 10 → 오판 0, 9,095 요청/초, 최대 힙 1.1 GiB, 0.72코어. PR #13(실행 38091832358, 관측 장치 가드 켜짐): 프로브 3 → 오판 0, 2,821 요청/초, 최대 힙 388 MiB, 0.34코어. 프로브 10 → 오판 0, 9,100 요청/초, 최대 힙 1,077 MiB, 1.24코어. SSH 대상(sshd) 부하는 재지 않음. 과부하된 Windows 개발 PC에서 관측 쪽 프로브 시간 초과를 대상 장애로 보아 정상 배포 90건을 롤백한 기록(docs/05 M5-4)은 FR-DEC-13(관측 장치 가드)으로 대응했으나, 같은 과부하 PC 조건의 재측정 기록은 없다 |
| NFR-PRF-01 | 판정 지연: 위반 발생부터 롤백 시작까지 `eval_interval × for + 5초` 이내 | docs/05 비기능 목표 | 1차 | 부분: 지표 `vigilante_rollback_trigger_seconds`·`vigilante_evaluation_seconds` 구현(`9fd9e74`), 부하 하네스 측정 통과(PR #12, PR #13) | 부하 하네스(불량 주입 → 롤백 요청, 목표 12초 = 프로브 주기 2s × 연속 3회 + eval 1s + 5s): M5-4 기록 프로브 3개 p50 5.4초·p99 5.5초, 프로브 10개 p50 5.4초·p99 5.7초. PR #13 실행 프로브 3개 p50 5.16초·p99 5.32초, 프로브 10개 p50 5.73초·p99 5.96초. E2E 데모에서 탐지 약 3초·롤백 약 1초(docs/05 M8-1, 데모 환경 한정). 실서비스는 M8 파일럿에서 측정 예정 |
| NFR-AVL-01 | 제어 평면 가용성 99.9% | docs/05 비기능 목표 | 1차 | 설계 반영(HA, `5df283c`) | 미측정. M8 파일럿에서 검증 예정 |
| NFR-AVL-02 | 리더 장애 시 30초 이내 인계(RTO) | docs/05 비기능 목표 | 1차 | 완료(기본 `lease_ttl` 15s) | `TestHAFailoverFinishesInterruptedRollback`, `TestFailoverOnPartition`. TTL 3초 설정에서 4.1초 전환 기록(docs/01 5). 기본값 15s의 실측은 미측정 |
| NFR-AVL-03 | 결정 기록 유실 0(RPO 0, 동기 커밋) | docs/05 비기능 목표 | 1차 | 부분: 정상 시 동기 기록(파일 fsync, PostgreSQL 커밋). PR #12(`9f9470e`)부터 저장소 장애 중 기록은 메모리 큐(최대 100,000건, 초과분은 `vigilante_store_errors_total{reason="dropped"}`)에 순서대로 두었다가 복구 시 기록한다. 그 사이 프로세스가 죽으면 큐 내용은 유실되고, HA에서 장애가 `lease_ttl`보다 길면 리더가 물러나며 다른 노드가 리더가 되면 큐는 펜싱되어 버려진다(engine.go, docs/05 M5-4) | `TestChaosStoreOutageDuringRollback`(롤백 중 저장소 단절: 기록 무손실, 해시 체인 유지). 크래시·장기 장애 시 유실은 설계상 한계 |
| NFR-AUD-01 | 모든 결정·조치에 작업자와 근거, 변조 검출, 보존 기간 설정(기본 1년) | docs/05 비기능 목표 | 1차 | 완료 (`dc88133`). 저장소 쓰기 권한자의 체인 재계산까지 검출하려면 `audit.chain_key_ref` 키 체인(`c9e39f1`, PR #13, 선택). 로컬 CLI 비상 조치는 `breakglass.<action>`으로 감사(`313f651`). 보존은 `audit.retention`(예시 8760h) 기준 수동 `audit prune`, 자동 삭제 없음 | `TestChainDetectsTampering`, `TestPruneArchivesAndKeepsState`, `TestKeyedChainDetectsRecomputedTampering`, `TestKeyedChainAcrossPrune` |
| NFR-SEC-01 | 내부 통신 TLS 1.2 이상, 에이전트 mTLS | docs/05 비기능 목표 | 1차 | 부분: `server.tls`(최소 1.2, 1.3 선택), 선택적 클라이언트 인증서, `agent.tls` 완료(`ce19709`). PR #13에서 HA 팔로워→리더 전달 검증(`server.ha.tls`), SIEM syslog over TLS(`audit.syslog.tls`), 콘솔 HSTS 추가. 인증서 자동 발급은 미착수(M5-2) | `TestServerAndAgentTLS`, `TestHAClientServerName`, `TestSyslogExporterTLS`, `TestHSTSOverHTTPS` |
| NFR-SEC-02 | 비밀값 디스크 평문 저장 금지 | docs/05 비기능 목표 | 1차 | 완료 (`31d5c34`): `*_ref`·`*_env`만 허용, 토큰·API 키는 SHA-256만 저장 | `TestSecretsValidation`, `TestServiceAccountsAndLegacy`, `TestV2OAuthClientsAndScopes` |
| NFR-SEC-03 | 최소 권한(대상 sudo 범위 축소) | docs/05 비기능 목표, docs/10 | 1차 | 부분: `sudo_scope: changes` 완료(`23c00a4`). 하위 호환을 위해 기본값은 `all`이며 `validate`·doctor가 경고 | `TestSymlinkSudoChangesOnly`, `TestDoctorChecksSudoRules` |
| NFR-QLT-01 | 판정 품질: 파일럿 3개월·배포 30건 이상에서 오탐 0, 미탐 0, HOLD 10% 이하 | docs/05 비기능 목표, docs/11 | 1차(출시 게이트) | 미착수(측정 도구는 `ba7a70b` 완료) | M8 파일럿에서 검증 예정. 현재 실제 서비스 측정치 없음 |
| NFR-DEP-01 | 롤백 경로 의존성 최소: 콘솔·ITSM·SSO·Vault·SIEM·알림 장애가 판정·롤백을 막지 않음(명시적 승인 게이트 제외) | docs/05 설계 원칙 1 | 1차 | 완료(설계·코드): ITSM 작업은 리더의 이벤트 후속 처리(인시던트는 백그라운드, PR #13), SIEM·알림은 비동기, Vault는 캐시. 롤백 시작 시 저장소 장애도 기본(`rollback_lease.on_unavailable: proceed`)으로는 롤백을 막지 않음(PR #13) | 코드 확인(internal/api/itsm.go, internal/audit/syslog.go). 카오스 테스트(PR #12, #13)는 저장소·LB 장애와 관측 장치 저하를 다룬다. ServiceNow 일시 장애 중 인시던트 재시도는 `TestIncidentRetriedUntilServiceNowRecovers`, `TestV2ServiceNowGateIncidentsAndNotes`(처음 두 번 실패)로 확인. 콘솔·SSO·Vault·SIEM·알림 장애 주입 시험은 없다 |
| NFR-PRT-01 | 단일 정적 바이너리, CGO 없음, linux amd64·arm64·ppc64le, windows amd64 | README, docs/09 | 1차 | 완료 | CI test(정적 교차 빌드) |
| NFR-PRT-02 | 폐쇄망 동작: 외부 CDN 없는 콘솔, 폐쇄망 번들, openssl만으로 서명 확인, 시간대 데이터 내장 | docs/07, docs/02 | 1차 | 완료 (`aff2841`, `5474c0c`) | CI package(번들·서명 확인) |
| NFR-SIZ-01 | 최소 빌드 제공으로 바이너리 크기 축소 | docs/05 M6-1 | 1차 | 완료: 38MB → 18MB(docs/05 M6 구현 결과 기록) | CI package(minimal build) |
| NFR-CMP-01 | 하위 호환: SemVer, v2는 추가만, v1 동결, 설정 키 추가만, CLI 종료 코드 고정 | docs/05 설계 원칙 4, docs/08 | 1차 | 완료(정책·CI) | CI api(oasdiff), CI demo(종료 코드) |
| NFR-OBS-01 | 자체 관측성: `/metrics`(Prometheus), `/healthz`, `/readyz`, JSON 로그와 `request_id` | docs/05 M0-5 | 1차 | 완료 (`9fd9e74`) | `TestMetricsEndpoint`, `TestReadyzAndRequestID`, `TestExpositionFormat`, `TestTelemetryFollowsVerdictsAndRollbacks` |
| NFR-SUP-01 | 공급망: SBOM, 서명, 의존성 취약점 스캔 CI 차단 | docs/05 M6-1 | 1차 | 부분: SBOM·govulncheck 완료, 릴리스 서명 키 미생성 | CI vuln, CI package |
| NFR-MNT-01 | 품질 게이트: gofmt, vet, `go test -race`, 명세 린트 | docs/05 검증 기준 | 1차 | 완료 (`74d9fb2`) | CI test, CI api |
| NFR-REL-01 | 1차 범위 실행기·트래픽 제어기가 호환성 매트릭스에서 모두 "검증됨" | docs/05 출시 게이트 | 1차(출시 게이트) | 미착수: 현재 검증됨은 http·access_log·log 프로브와 webhook 실행기뿐(docs/09) | M8 실장비 랩 |
| NFR-REL-02 | 부하·카오스: 리더 강제 종료, 저장소 단절, 네트워크 분단, LB 지연·오류 4종 통과 | docs/05 M5-4 | 2차 | 완료(PR #12, PR #13 보강). 네트워크 분단은 관측점 불일치 → HOLD로, 관측 장치 자체 과부하는 관측 장치 가드 → HOLD로 다룬다(FR-DEC-13) | 리더 강제 종료 `TestHAFailoverFinishesInterruptedRollback`(기존), 저장소 단절 `TestChaosStoreOutageDuringRollback`·`TestChaosStoreOutageLeaseFailMode`, 관측점 불일치 `TestObserverQuorum`(기존), 관측 장치 저하 `TestChaosObserverDegradedHolds`, LB `TestChaosLoadBalancerTransientErrors`·`TestChaosLoadBalancerLatency`·`TestChaosLoadBalancerDown`, 부하 `test/load`(load.yml) |
| NFR-TGT-01 | 대상 부하 최소화: SSH 연결 대상당 1개 풀링, `/proc` 1회 왕복, 로그 1초 버킷 | docs/03 1.2 | 1차 | 완료 (`d1e6411`, M5-3 보강) | `TestSustainedHighRateStaysLinear`, `TestSessionBudgetKeepsRoomForRollback`. 실제 대상(sshd) 부하 수치는 미측정(부하 하네스는 HTTP 시뮬레이터 대상) |

## 4.1 출시 게이트

상용 1차 출시는 아래 조건을 모두 충족해야 한다(docs/05 "상용 1차 범위", docs/11).

| 조건 | 기준 | 현재 상태 |
|---|---|---|
| 호환성 매트릭스 | 1차 범위의 실행기·트래픽 제어기 전부 "검증됨" | 미충족(M8 랩 미실시) |
| 파일럿 판정 품질 | 배포 30건 이상, 오탐 0, 미탐 0, HOLD 10% 이하, FAIL·HOLD 전부 평가 | 미충족(파일럿 미실시) |
| 실제 롤백 | 파일럿에서 실제 롤백(승인 모드 포함) 1건 이상 | 미충족 |
| 출시 산출물 | M6-1 서명·SBOM·폐쇄망 번들 완료 | 부분(릴리스 서명 키 미생성) |

---

# 5. 제약사항 및 가정

| 구분 | 내용 | 출처 |
|---|---|---|
| 제약 | 외부 장비(F5·AWS·Nutanix·vCenter·OpenStack)와 실제 연동 시험을 하지 않았다. 시뮬레이터·모의 서버 기준이다 | README "알려진 한계" |
| 제약 | Nutanix는 Prism Element v2 API 경로 기준이며 AOS 버전별 확인이 필요하다 | README |
| 제약 | Cinder `revert_to_snapshot`은 백엔드·릴리스에 따라 사용 중 볼륨을 거부할 수 있다. "새 볼륨으로 루트 교체" 대체 경로는 구현하지 않았다 | docs/05 M7 구현 결과 |
| 제약 | `host` 프로브는 Linux `/proc` 전용이다. AIX·Solaris·HP-UX는 `exec` 기반 프로브가 필요하다 | README |
| 제약 | `state.backend: file`은 단일 노드용이다. 여러 서버 노드는 PostgreSQL과 HA를 써야 한다 | docs/04 6 |
| 제약 | HA 펜싱은 상태 기록만 막는다. 리더 교체 순간 진행 중이던 한 단계는 새 리더가 한 번 더 실행할 수 있으므로 모든 롤백 단계는 멱등이어야 한다 | docs/04 6 |
| 제약 | 에이전트 failsafe 롤백은 LB에 접근하지 않는다 | docs/04 6 |
| 제약 | 스냅샷 계열 롤백은 디스크 상태만 되돌린다. 메모리·외부 DB는 되돌리지 않는다 | docs/02 OpenStack |
| 가정 | 프리셋 임계치는 추정값이며 M8 파일럿에서 보정한다 | README |
| 가정 | 일정 추정은 실측이 아니며 1.5~2배를 본다 | docs/05 |
| 가정 | 오픈 API는 사내 전용으로 공개한다 | docs/06 |

---

# 6. 추적성 매트릭스

요구사항 영역별로 설계 문서의 해당 절과 구현 패키지, 대표 테스트를 연결한다. SI-02는 아키텍처 설계서, SI-03은 상세 설계서를 뜻한다.

## 6.1 기능 요구사항

| 요구사항 | 설계(SI-02) | 설계(SI-03) | 구현 패키지 | 대표 테스트 |
|---|---|---|---|---|
| FR-COL-01~09, 11 | 3.2, 3.3 | 3.3, 5.2 | internal/probe, internal/metrics, internal/transport, internal/dockerapi | `TestHTTPProbeAndJSONPath`, `TestAggregations`, `TestCollectorRestartsFailingProbe` |
| FR-COL-10 | 5.5 | 3.3 | internal/transport | `TestSessionBudgetKeepsRoomForRollback` |
| FR-DEC-01~09 | 3.3 | 5 | internal/rules, internal/decision | `TestCompositeAnyRule`, `TestConsecutiveAndHysteresis`, `TestEnvironmentalHold`, `TestObserverQuorum` |
| FR-DEC-10, 12 | 3.2 | 3.2 | internal/config, internal/presets | `TestServiceWithoutRollbackRuleRejected`, `TestBuiltinPresetsProduceValidServices` |
| FR-DEC-11 | 2.2 | 4.3 | internal/model, cmd/vigilante | CI demo, `TestWatchRemoteGateRefusalExitsThree` |
| FR-DEC-13 | 3.2, 5.7 | 3.4, 5.8 | internal/observer, internal/decision, internal/orchestrator | `TestObserverDegradedHoldsProbeFailures`, `TestChaosObserverDegradedHolds`, `TestSpreadNeedsSeveralServices` |
| FR-RB-01~04, 09, 11 | 3.3, 5.3 | 6 | internal/orchestrator | `TestCanaryFailDrainsRollsBackAndEnables`, `TestEscalationRecovers`, `TestResumeSkipsCompletedSteps` |
| FR-RB-05~08, 10 | 3.2 | 3.5 | internal/executor, internal/dockerapi | `TestSymlinkRollbackAndVerify`, `TestContainerCompensatesOnStartFailure`, `TestOpenStackVolumeSnapshotRevertAndReplay`, `TestDryRunDoesNotMutate` |
| FR-RB-12 | 3.2 | 3.2 | internal/cienv, internal/orchestrator | `TestLastGoodVersionAndMarkGood` |
| FR-TR-01~06 | 3.2 | 3.5 | internal/executor | `TestNginxController`, `TestF5iControl`, `TestOctaviaDrainEnableWaitsForActive` |
| FR-SAF-01~05 | 3.2, 8 | 7 | internal/safety, internal/orchestrator | `TestBreakerLifecycle`, `TestDrainBatch`, `TestFlappingGuardBlocksAndIsolates` |
| FR-SAF-06 | 3.2 | 3.2 | internal/config | `TestUnknownFieldsRejected` |
| FR-SAF-07 | 5.4 | 7.4 | internal/safety, internal/orchestrator | `TestGuardStoreUnreachable`, `TestChaosStoreOutageDuringRollback`, `TestChaosStoreOutageLeaseFailMode` |
| FR-HA-01~06 | 4.3, 5, 6.4 | 3.7, 9 | internal/store, internal/ha, internal/journal, internal/tlsconf | `TestAppendAndLoad`, `TestPostgresFencing`, `TestHAFailoverFinishesInterruptedRollback`, `TestMigrateUpgradeDowngradeAndGuard`, `TestHAClientServerName` |
| FR-HA-07 | 5.4 | 9.3 | internal/orchestrator, internal/telemetry | `TestChaosStoreOutageDuringRollback` |
| FR-AUTH-01~06 | 6.1, 6.2 | 3.8, 3.9 | internal/auth, internal/api | `TestRoleScopeMatrix`, `TestFourEyesApproval`, `TestV2OAuthClientsAndScopes`, `TestV2RateLimits` |
| FR-AUTH-09 | 6.1 | 3.1, 7.6 | cmd/vigilante, internal/config | `TestLocalCLIRestricted`, `TestLocalFourEyes` |
| FR-SEC-01~04 | 6.3 | 3.9 | internal/secrets, internal/transport | `TestVaultAppRoleKVCacheAndRelogin`, `TestSSHWithVaultCertificate`, `TestRedactHandler` |
| FR-AUD-01~05 | 6.4, 6.5 | 3.7, 9.2 | internal/journal, internal/audit, internal/tlsconf | `TestChainDetectsTampering`, `TestSyslogExporter`, `TestSyslogExporterTLS` |
| FR-AUD-07 | 6.5 | 3.7, 9.2 | internal/journal, internal/store, internal/audit | `TestKeyedChainDetectsRecomputedTampering`, `TestKeyedChainCoversUnkeyedPrefix`, `TestKeyedChainAcrossPrune` |
| FR-INP-01~03 | 3.2 | 3.2, 3.11 | internal/cienv, internal/doctor | `TestResolveInputsFromCIAndJournal`, `TestDoctorFindsRealProblems` |
| FR-API-01~05 | 3.2, 6.2 | 3.8 | internal/api, api/openapi.yaml | `TestV2RoutesMatchSpec`, `TestV2ContractLifecycle`, CI api |
| FR-API-08 | 2.2 | 3.1, 3.8, 4.3 | internal/api, cmd/vigilante | `TestV1CreateGateFields`, `TestWatchRemoteSendsTicketAndFreezeOverride`, `TestWatchRemoteGateRefusalExitsThree` |
| FR-EVT-01~05 | 3.4 | 8, 11 | internal/events, internal/notify | `TestV2WebhooksDeliverInOrderSignedAndRecover`, `TestV2EventStreamResumesAndFilters`, `TestRoutingDedupAndChannelFormats`, `TestV2ApproveModeDecisions` |
| FR-OPS-01~03 | 6.7 | 10 | internal/console | `TestOIDCSignInSessionAndCSRF`, `TestConsoleSessionUsesTheAPI`, `TestHSTSOverHTTPS` |
| FR-OPS-04 | 3.3 | 6.6 | internal/orchestrator | `TestApproveModeWaitsDrainsAndApproves`, `TestApprovalTimeouts` |
| FR-OPS-05, 06 | 5.5 | 7.5, 11.1 | internal/orchestrator, internal/itsm, internal/api, internal/config | `TestFreezeRollbackPolicy`, `TestChangeChecks`, `TestIncidentRetriedUntilServiceNowRecovers`, `TestRetryIsBounded` |
| FR-REL-01~06 | 4, 6.8, 7.3 | 12 | cmd/vigilante, internal/compat, internal/support, internal/tlsconf, scripts/release.sh | `TestMinimalBuildRefusesMissingPlugins`, `TestDocMatchesMatrix`, `TestSupportBundle`, CI package |
| FR-REL-07 | 6.6 | 3.9 | internal/sudoers, internal/executor | `TestRulesAndRender`, `TestSymlinkSudoChangesOnly` |
| FR-REL-10 | 4.4 | 12 | deploy/helm/vigilante | CI package(차트 렌더링) |
| FR-VAL-01~03 | 8 | 3.11 | internal/lab, internal/pilot | `TestScenarioPassesAndSummarizes`, `TestComputeVerdictsTimingAndGate` |
| FR-AGT-01, 02 | 4.6 | 3.10 | internal/agent | `TestAgentPushesAndFailsafeRollsBack` |

## 6.2 비기능 요구사항

| 요구사항 | 설계(SI-02) | 설계(SI-03) | 검증 |
|---|---|---|---|
| NFR-SCL-01, NFR-TGT-01 | 8 | 3.3 | 부하 하네스 `test/load`(load.yml, PR #12·#13 실행). sshd 부하는 미측정 |
| NFR-PRF-01 | 8 | 5 | 지표 구현, 부하 하네스 p99 5.3~6.0초(목표 12초, M5-4 기록과 PR #13 실행). 실서비스 실측은 M8 파일럿 |
| NFR-AVL-01~03 | 5 | 9 | HA 테스트, `TestChaosStoreOutageDuringRollback`, `TestChaosStoreOutageLeaseFailMode` |
| NFR-AUD-01 | 6.5 | 9.2 | `TestChainDetectsTampering`, 키 체인 테스트 |
| NFR-SEC-01~03 | 6 | 3.9 | TLS·비밀·sudo 테스트 |
| NFR-QLT-01 | 8 | 3.11 | M8 파일럿 |
| NFR-DEP-01 | 5.5, 9 | 11 | 코드 확인, 저장소·LB 카오스 테스트(PR #12, #13), ServiceNow 재시도 테스트 |
| NFR-PRT-01, 02, NFR-SIZ-01 | 4.5, 7 | 12 | CI test, CI package |
| NFR-CMP-01 | 9 | 12 | CI api, CI demo |
| NFR-OBS-01 | 8 | 3.12 | `TestMetricsEndpoint` |
| NFR-SUP-01, NFR-MNT-01 | 6.8 | 12 | CI vuln, CI package, CI test |
| NFR-REL-01, 02 | 8 | 13 | M8 실장비 랩, 카오스·부하 테스트(PR #12, #13) |

---

# 7. 미결 사항

| 항목 | 내용 | 출처 |
|---|---|---|
| 비기능 목표 확정 | 규모·판정 지연·가용성 목표는 제안값이다 | docs/05 비기능 목표 |
| 지원 OpenStack 버전 | 사내 CMP가 관리하는 릴리스 기준으로 확정 | docs/05 결정 필요 사항 |
| 파일럿 대상 | 사내 서비스 2개(OpenStack+Octavia 1, 베어메탈 또는 컨테이너 1) 권장, 확보 필요 | docs/05 결정 필요 사항 |
| 실장비 확보 | F5 VE, vCenter 7·8, Nutanix CE, AWS 테스트 계정, 사내 OpenStack | docs/05 M8-1 |
| 릴리스 서명 키 | 릴리스 관리자가 cosign 키 쌍 생성·보관(`packaging/cosign.pub` 없음) | docs/07 |
| 패키지 메타데이터 | `packaging/nfpm.yaml`의 `maintainer`·`license`가 임시 값이다 | packaging/nfpm.yaml |
| 규제 요건 | 감사 보존 1년, 4-eyes 선택 적용. 금융권은 상향 가능 | docs/05 결정 필요 사항 |
| 관측 장치 가드 실측 | `safety.observer_guard`(FR-DEC-13)로 관측 쪽 과부하 시 직접 재는 프로브의 실패 기반 위반을 HOLD한다(PR #14부터 액세스 로그 등 대상이 보고한 지표는 판정). 단위·카오스 테스트로 확인했으나, 정상 배포 90건이 롤백되었던 과부하 Windows PC 조건을 가드를 켠 채 재현한 기록은 없다. 기본 임계값(1s, 1s, 0.5, 3, 1m)도 실서비스 검증 전이다 | docs/05 M5-4 구현 결과, PR #13 |
| 저장소 장애 중 lease 없는 롤백 | `rollback_lease.on_unavailable: proceed`(기본)는 저장소 장애 중 다른 프로세스와 같은 서비스를 동시에 롤백할 가능성을 받아들인다(롤백 단계 멱등 전제, 복구 후 `lease.conflict` 알림). 규제 환경에서 `fail`로 바꿀지 결정 필요 | docs/04 S12a, PR #13 |
| 로컬 CLI 제한의 운영 절차 | `auth.local_cli: auto`에서 인증을 켠 환경은 로컬 승인·서킷 조작에 `--server` 또는 `--break-glass`가 필요하다. 기존 운영 절차(로컬 `circuit reset` 등)를 바꿔야 한다 | docs/04 1, PR #13 |
