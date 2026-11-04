---
title: 테스트 계획·결과서
doc_id: VGL-SI-06
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

본 문서는 Vigilante의 테스트 계획(전략, 수준, 환경, 진입·종료 기준, 출시 게이트)과 테스트 결과(실측)를 기술한다. 결과는 (1) 2026-10-11 로컬 환경에서 master 커밋 `dd9a055`로 직접 실행한 `go test` 결과(JSON 출력) 집계와 (2) 2026-10-11 기준으로 확인한 GitHub Actions 결과다. GitHub Actions는 PR #13의 `ci.yml` 실행 38091832392(다섯 작업)와 `load.yml` 실행 38091832358(부하 시험 두 규모, 결과 JSON 아티팩트 내려받아 확인), 그리고 병합 후 master `dd9a055` 푸시의 `ci.yml` 실행 38092384199를 확인했다. PR 실행의 헤드 커밋은 `189dfbb`이며, `dd9a055`는 이것을 master(`dba3dbe`)에 병합한 커밋이다. 그 뒤 병합된 PR #14(`537870c`, 관측 장치 가드의 프로브 유형 구분)는 CI 결과로 확인했고 로컬 재측정은 하지 않았다(4.8, 4.10).

## 1.2 대상

| 항목 | 내용 |
|---|---|
| 제품 | Vigilante — Unified Rollback Orchestrator (단일 Go 바이너리) |
| 시험 대상 소스 | master 커밋 `dd9a055` ("Merge review fixes: observer overload guard, store-outage rollback, security and Helm (#13)"). PR #12(M5-4)는 `dba3dbe`로 병합됨. CI 확인: PR #13 헤드 `189dfbb`(ci·load), master `dd9a055`(ci). 문서 기준은 master `537870c`(PR #14 병합)이며, PR #14는 PR 헤드 `c4f798a`(ci·load)와 master `537870c`(ci)의 CI로 확인 |
| 릴리스 상태 | 출시 전. 첫 릴리스 1.0.0은 M8 실장비 랩·파일럿 통과가 조건 (CHANGELOG `[Unreleased]`) |
| 제외 | 부하 하네스 `test/load`(빌드 태그 `load`), E2E 데모, 패키징·설치는 로컬에서 실행하지 않았고 CI 결과로 확인했다(2.4, 4.7, 4.8 참조) |

## 1.3 참고 문서

| 문서 | 내용 |
|---|---|
| `.github/workflows/ci.yml`, `load.yml`, `release.yml` | CI·부하 시험·릴리스 파이프라인 |
| `docs/05-roadmap.md` | 마일스톤별 검증 기준, 비기능 목표, 출시 게이트 |
| `docs/09-compatibility.md` | 플러그인별 검증 수준과 M8 랩 기록 방법 |
| `docs/11-pilot.md` | 파일럿 운영과 판정 품질 게이트 |
| `examples/demo/run-demo.sh` | E2E 데모 시나리오 |
| VGL-SI-04, VGL-SI-05 | 인터페이스 정의서, 데이터 설계서 |

---

# 2. 테스트 계획

## 2.1 테스트 전략

| 원칙 | 내용 |
|---|---|
| 롤백 경로 우선 | 오판정·롤백 실패가 곧 장애이므로 판정(규칙, 관측 쿼럼, 환경 요인), 롤백 계획(드레인, 복원, 확인, 복귀), 안전장치(서킷, blast radius, 플래핑), 크래시·리더 교체 재개를 가장 두껍게 시험한다 |
| 계약 우선 | REST API v2는 `api/openapi.yaml`이 원본이다. 계약 테스트가 모든 v2 요청·응답을 명세로 검증하고, 명세 연산과 서버 라우트의 일치를 확인한다. CI는 명세 린트와 하위호환 검사를 PR 차단 조건으로 둔다 |
| 실제 저장소 | 상태 저장소 테스트는 같은 시험을 file과 PostgreSQL 양쪽에 실행하며, PostgreSQL은 모의가 아닌 실제 서버를 쓴다(로컬 임베디드, CI `postgres:16` 서비스) |
| 외부 장비 대체 | 장비가 필요한 플러그인은 모의 서버(httptest), 시뮬레이터(govmomi vcsim), 상태 있는 모의 OpenStack, 모의 SSH 실행기로 시험하고 "실험적"으로 표시한다. 실장비 검증은 M8 랩에서 한다 |
| 실제 프로세스 E2E | E2E 데모가 실제 프로세스(`fakeapp`)를 대상으로 불량 배포 탐지·롤백, 롤백 경로 고장, 서킷 OPEN, 리셋, 정상 배포, 랩 시나리오, 파일럿 게이트를 종료 코드로 확인한다 |
| 공급망·보안 | govulncheck, gofmt·vet, race detector, 서명·SBOM·설치 시험을 CI에서 매번 실행한다 |

## 2.2 테스트 수준

| 수준 | 범위 | 수단 | 실행 위치 | 상태 |
|---|---|---|---|---|
| 단위 | 규칙 평가, 판정, 설정 검증, 프로브 파싱, 실행기·트래픽 제어기 로직, 인증·권한, 비밀값, 알림, 지표 | `go test`, 모의 서버·실행기 | 로컬, CI | 수행(4장) |
| 계약 | REST API v2 요청·응답과 명세 일치, 라우트 일치, `approval.decided` 이벤트 데이터 필드와 명세 설명 일치(PR #13) | `internal/api/contract_test.go`(libopenapi-validator), Spectral 린트, oasdiff | 로컬(계약 테스트), CI(린트·하위호환) | 계약 테스트 수행(로컬·CI). 린트·하위호환은 CI `api` 작업 통과(PR #13 실행, master 실행) |
| 통합(실제 PostgreSQL) | 저장소 추가·재생, 리스 경합, 펜싱, 마이그레이션·다운그레이드 가드, 감사 체인 변조 검출, 키 체인 MAC 검증, 보존 정리, HA 리더 선출·장애 조치, 롤백 중 리더 교체, DB 프로브 | `pgtest`(임베디드 PostgreSQL 또는 `VIGILANTE_TEST_PG_DSN`) | 로컬, CI | 수행. 6개 패키지(`cmd/vigilante`, `internal/audit`, `internal/ha`, `internal/orchestrator`, `internal/probe`, `internal/store`)의 테스트가 실제 PostgreSQL 사용 |
| 시나리오(엔진) | canary 실패 → 드레인 → 롤백 → 복귀, 롤백 실패 격리·서킷, 승인 모드, 동결, 크래시 재개, M7 OpenStack 시나리오 | `internal/orchestrator` 테스트 | 로컬, CI | 수행 |
| E2E 데모 | 실제 프로세스 8개 단계(3.4) | `examples/demo/run-demo.sh` | CI `demo` 작업 | CI 통과(PR #13 실행 38091832392, master 실행 38092384199). 로컬 미실행(저장소 `bin/`·`examples/demo/run`에 파일을 쓰므로) |
| 최소 빌드 | `-tags minimal` 빌드의 vet·테스트 | `go test -tags minimal` | 로컬, CI `package` | 수행(로컬 59건 통과: 최상위 50, 하위 9. CI 통과) |
| 패키징·설치 | 바이너리·SBOM·rpm·deb·Helm·이미지·폐쇄망 번들, 서명·검증, Debian 12·Rocky 9 설치, Helm 렌더링(단일·HA·TLS, 인증 없는 설정 거부) | `scripts/release.sh`, Docker, `helm template` | CI `package`, 릴리스 | CI `package` 통과(PR #13 실행, master 실행). 로컬 미실행 |
| 보안 점검 | 알려진 취약점(govulncheck), 데이터 레이스(`-race`), 정적 검사(gofmt, vet), 비밀값 제거·CSRF·HTML 주입 방지·HSTS·로컬 CLI 권한 제한·키 체인·TLS 단위 테스트 | CI `vuln`, `test` | CI(단위 테스트는 로컬도) | `-race`·govulncheck는 CI 전용, PR #13 실행과 master 실행에서 통과 |
| M8 실장비 랩 | 조합별 체크포인트 → 불량 배포 → 탐지 → 드레인 → 복원 → 복귀, 3회 연속 통과 시 "검증됨" | `vigilante lab run`, `lab summary` | 랩 장비 | 미착수(장비 확보 대기) |
| M8 파일럿 | 실제 서비스 3개월, 배포 30건 이상, 판정 품질 측정 | `vigilante feedback`, `pilot report` | 사내 서비스 | 미착수 |
| 카오스 (M5-4, PR #13) | 롤백 중 저장소 단절(lease 없이 진행, `fail` 모드), LB API 일시 오류·지연·완전 장애, 관측 장치 저하. 리더 강제 종료와 관측점 불일치는 기존 HA·판정 테스트 | `internal/orchestrator/chaos_test.go` | 로컬, CI | 수행(6건, 로컬 `dd9a055`·CI 통과, 4.9) |
| 부하 (M5-4) | 시뮬레이터 HTTP 대상 2,000대 × 프로브 3·10개, 동시 배포 100건(10건 불량). 판정 지연·오판·힙·CPU·프로브 처리량 | `test/load`(빌드 태그 `load`), `.github/workflows/load.yml` | CI `load`(주 1회, 수동, 엔진 변경 PR) | CI `load` 통과(M5-4 기록과 PR #13 실행 모두 두 규모 오판 0, 4.7). 로컬 미실행 |
| 결함 수정 (PR #13) | 관측 장치 가드, 저장소 장애 중 롤백 lease, 로컬 CLI 권한 제한, 키 감사 체인, SIEM TLS, HA 전달 TLS, 콘솔 HSTS, `watch --server` 게이트 플래그·종료 코드, v1 게이트 필드, `approval.decided` 명세, ServiceNow 재시도, Helm 차트 | 단위·계약·카오스 테스트(신규 최상위 29건), CI `package` 차트 렌더링 | 로컬, CI | 수행(4.10) |
| 결함 수정 (PR #14) | 관측 장치 가드가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, `host`)의 지표와 모든 프로브의 `probe_error`만 HOLD | 기존 `TestObserverDegradedHoldsProbeFailures` 확장(신규 테스트 없음) | CI | 수행(4.10) |

## 2.3 테스트 환경

| 구분 | 로컬 개발 환경 (본 측정) | CI (GitHub Actions) |
|---|---|---|
| OS | Windows 11 Home 10.0.26200 (`ver`: 10.0.26200.9457) | `ubuntu-latest` |
| Go | go1.27.2 windows/amd64 | `GO_VERSION: "1.27.2"`(go.mod `toolchain go1.27.2`와 일치 검사) |
| cgo | `CGO_ENABLED=0` (cgo 없음) | 기본(cgo 사용 가능) |
| PostgreSQL | 임베디드 PostgreSQL 18.3.0 (`embedded-postgres` v1.34.0 기본값, 캐시 `embedded-postgres-binaries-windows-amd64-18.3.0.txz`). 테스트 바이너리마다 1회 기동, 테스트마다 새 DB | 서비스 컨테이너 `postgres:16`, `VIGILANTE_TEST_PG_DSN=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable` |
| 셸 | Git Bash | bash |
| 외부 장비 | 없음(모의 서버·시뮬레이터) | 없음(모의 서버·시뮬레이터). 설치 시험은 `debian:12`, `rockylinux:9` 컨테이너 |

> race detector(`go test -race`)는 cgo가 필요하므로 로컬(CGO_ENABLED=0)에서는 실행하지 않았고 CI `test` 작업에서만 실행한다.

## 2.4 범위 제외와 주의

| 항목 | 내용 |
|---|---|
| 측정 이력 | 1.1판은 브랜치 `m5-load-chaos`의 `a0c000e`(PR #12 병합 전)에서 222건(최상위 210, 하위 12)을 측정했다. 본 판(1.2)은 PR #12·#13이 병합된 master `dd9a055`에서 같은 명령·같은 집계 방법(JSON의 `pass`·`fail`·`skip` 중 최상위와 하위 테스트를 모두 계수)으로 다시 측정했다. 4장과 부록은 모두 `dd9a055` 값이다 |
| `test/load` | 빌드 태그 `load`가 있어야 컴파일되므로 일반 `go test ./...`에 포함되지 않는다. 대상 2,000대 규모라 로컬에서는 실행하지 않았고, CI `load.yml` 결과로 확인했다(4.7) |
| E2E·패키징 | 저장소 밖에 쓰지 않는다는 작성 조건 때문에 로컬에서 실행하지 않았다. CI `demo`·`package` 작업 결과(PR #13 실행, master 실행 모두 통과)로 확인했다(4.8) |
| race detector | 로컬 Windows 환경은 `CGO_ENABLED=0`이라 `-race`를 실행할 수 없다. 데이터 레이스 검사는 CI `test` 작업의 결과만 있다 |

## 2.5 진입 기준

| 번호 | 기준 |
|---|---|
| E1 | `go build ./cmd/vigilante`가 성공하고 `go vet ./...`, `gofmt -l .`에 지적이 없다 |
| E2 | `api/openapi.yaml` 변경이 있으면 Spectral 린트를 통과한다 |
| E3 | 시험 환경에 PostgreSQL이 있다(임베디드 다운로드 가능 또는 `VIGILANTE_TEST_PG_DSN`). 없으면 PostgreSQL 테스트는 실패가 아닌 생략으로 처리되므로, 생략 0건을 별도로 확인한다 |
| E4 | 시험 대상 커밋을 식별했다 |

## 2.6 종료 기준 (본 단계: 자동 테스트)

| 번호 | 기준 | 판정 방법 |
|---|---|---|
| X1 | `go test ./...` 실패 0건 | JSON 출력 집계 |
| X2 | PostgreSQL 테스트 생략 0건(실제 DB로 실행) | `skip` 0건, `postgres` 하위 테스트 통과 |
| X3 | `go test -tags minimal` 대상 4개 패키지 실패 0건 | JSON 출력 집계 |
| X4 | CI 다섯 작업(`test`, `api`, `vuln`, `package`, `demo`) 성공. 특히 `-race` 데이터 레이스 0건, govulncheck 도달 가능 취약점 0건, oasdiff 비호환 변경 0건 | CI 결과 |
| X5 | 계약 테스트(`TestV2RoutesMatchSpec` 등) 통과 | JSON 출력 |

## 2.7 출시 게이트 (상용 1차 1.0.0)

`docs/05-roadmap.md` "상용 1차 범위"의 출시 게이트는 아래 조건을 모두 충족해야 한다.

| 번호 | 조건 | 측정 수단 | 현재 상태 |
|---|---|---|---|
| G1 | M8 호환성 매트릭스에서 1차 범위의 실행기·트래픽 제어기가 전부 "검증됨" (실행기 `symlink`, `container`, `openstack`, `exec`, `webhook`. 트래픽 `nginx`, `haproxy`, `f5`, `octavia`) | `vigilante lab run` 3회 연속 통과, `internal/compat`·docs/09 갱신 | 미충족. 현재 "검증됨"은 webhook 실행기와 http·access_log·log 프로브뿐 |
| G2 | 파일럿 판정 품질 목표 달성 | `vigilante pilot report` (아래 표) | 미착수 |
| G3 | 파일럿에서 실제 롤백(승인 모드 포함) 1건 이상 성공 | `pilot report`의 실제 롤백 완료 | 미착수 |
| G4 | M6-1 서명·SBOM·폐쇄망 번들 완료 | CI `package`, 릴리스 서명 키 | 파이프라인 구현됨. 릴리스 서명 키 생성은 남음(로드맵) |

파일럿 판정 품질 게이트(`docs/11-pilot.md`, `vigilante pilot report`가 자동 확인, 미달 시 종료 코드 4):

| 항목 | 기준 |
|---|---|
| 관측한 배포 | 30건 이상 (`--min-deployments`) |
| 오탐 (정상 배포에 FAIL) | 0건 |
| 미탐 (문제 배포를 FAIL하지 않음) | 0건 |
| HOLD 비율 (보류 단계 / 관측 단계) | 10% 이하 (`--max-hold-rate`) |
| 실제 롤백 완료 (드라이런 제외) | 1건 이상 |
| FAIL·HOLD 배포의 평가 | 전부 평가됨 |

## 2.8 마일스톤별 검증 기준 (요약)

| 마일스톤 | 기준 (docs/05-roadmap.md "검증 기준") | 대응 테스트 |
|---|---|---|
| 공통 | `go vet`, `go test`, Linux CI `go test -race`, govulncheck 통과. 신규 패키지마다 단위 테스트 | CI, 4장 |
| M0 | PostgreSQL로 저장소 공통 시험 통과, 롤백 중 리더 종료 후 새 리더가 `ROLLED_BACK` 도달, 역할별 허용·거부 표, `audit verify`가 1건 변조 검출 | `TestAppendAndLoad`, `TestHAFailoverFinishesInterruptedRollback`, `TestRoleAndScopeEnforcement`, `TestRoleScopeMatrix`, `TestChainDetectsTampering` |
| M3 | 린트·oasdiff·계약 테스트, 같은 Idempotency-Key 롤백 1회 실행, 웹훅 수신 서버 복구 후 순서·서명, SSE 재연결 무손실, 조회 폭주 중 롤백 응답 | `TestV2ContractLifecycle`, `TestV2WebhooksDeliverInOrderSignedAndRecover`, `TestV2EventStreamResumesAndFilters`, `TestV2RateLimits` |
| M4 | 승인 모드에서 승인 없이 롤백 안 함, 승인자 감사, ITSM 모의 서버로 티켓 게이트·인시던트, ITSM 장애에도 롤백 진행 | `TestApproveModeWaitsDrainsAndApproves`, `TestV2ApproveModeDecisions`, `TestV2ServiceNowGateIncidentsAndNotes`, `TestChangeChecks` |
| M5 | 부하 하네스로 판정 지연·메모리 측정, 카오스 시나리오 4종 통과 | `TestChaosStoreOutageDuringRollback`, `TestChaosLoadBalancerTransientErrors`, `TestChaosLoadBalancerLatency`, `TestChaosLoadBalancerDown`(로컬·CI 통과), PR #13 추가 `TestChaosStoreOutageLeaseFailMode`, `TestChaosObserverDegradedHolds`, 리더 종료 `TestHAFailoverFinishesInterruptedRollback`, 관측 불일치 `TestObserverQuorum`. 부하 결과는 4.7(CI `load` 결과) |
| M6 | 서명 검증, 폐쇄망 번들 설치, 매트릭스 반영, 실험적 플러그인 `validate` 경고 | CI `package`, `TestWarningListsExperimentalPluginsInUse`, `TestDocMatchesMatrix` |
| M7 | 모의 서버로 볼륨·이미지 부팅 prepare → rollback → verify, 멱등 재실행, Octavia 드레인·대기·409 재시도. 사내 OpenStack 실측 | `TestOpenStackVolumeSnapshotRevertAndReplay`, `TestOpenStackImageSnapshotRebuild`, `TestOctaviaDrainEnableWaitsForActive`, `TestOpenStackCanaryRollsBackThroughOctavia`. 실측은 미착수 |
| M8 | 매트릭스 "검증됨", Linux CI `-race` 통과, 파일럿 게이트, 판정 품질 보고서 | 미착수(랩·파일럿) |
| 결함 수정(PR #13, #14) | CHANGELOG `[Unreleased]` Fixed (store, security, integration, deployment, decisions) 항목마다 회귀 테스트 | 4.10 |

---

# 3. CI 파이프라인

## 3.1 ci.yml (모든 PR과 master 푸시)

| 작업 | 시간 제한 | 단계 |
|---|---|---|
| `test` | 30분 | `postgres:16` 서비스 컨테이너. 툴체인과 go.mod 일치 확인 → `gofmt -l .` 결과 없음 → `go vet ./...` → `go test -race -count=1 -timeout 20m ./...` → `CGO_ENABLED=0` 정적 교차 빌드(linux/amd64, linux/arm64, linux/ppc64le, windows/amd64) |
| `api` | 10분 | Spectral 6.15.0으로 `api/openapi.yaml` 린트(`.spectral.yaml`, 경고도 실패). PR이면 기준 브랜치 명세 대비 oasdiff v1.33.0 `breaking --fail-on ERR` |
| `vuln` | 10분 | govulncheck v1.8.0 `./...` |
| `package` | 30분 | 최소 빌드 vet(`go vet -tags minimal ./...`, `go vet -tags load ./test/load`)·테스트(`-tags minimal`, 4개 패키지) → `scripts/release.sh build`(바이너리, SBOM, 패키지, 차트) → Helm 렌더링(단일·HA, 파일 저장소 HA 거부 확인, 인증 없는 설정 거부 확인, HA·TLS 값으로 `https://$(POD_IP):8088`·`scheme: HTTPS`·`GOMEMLIMIT` `1843MiB` 확인. PR #13) → 이미지 빌드·`version` 출력 확인 → 폐쇄망 번들, 임시 키로 cosign 서명·검증 → `debian:12`, `rockylinux:9`에서 번들 `install.sh`, 계정·systemd 유닛·`validate`·권한 확인 |
| `demo` | 10분 | `bash examples/demo/run-demo.sh` |

## 3.2 load.yml (부하 시험, M5-4)

| 항목 | 내용 |
|---|---|
| 실행 조건 | 매주 월요일 03:00 UTC(`cron: "0 3 * * 1"`), 수동 실행, `test/load`·`internal/orchestrator`·`internal/probe`·`internal/decision`·`load.yml`을 바꾸는 PR |
| 실행 환경 | `ubuntu-latest`, 시간 제한 30분, 열린 파일 한도 65536 |
| 행렬 | 프로브 3개, 10개 (`fail-fast: false`) |
| 명령 | `go test -tags load ./test/load -run TestLoad -count=1 -timeout 20m -v -args -targets 2000 -deployments 100 -failing 10 -probes N -out load-N.json` |
| 판정 | 다음 중 하나면 실패: 오판 1건 이상(정상 배포가 `PROMOTED`가 아니거나 불량 배포가 `ROLLED_BACK`이 아님), 불량 배포 중 롤백이 요청되지 않은 것이 있음, 판정 지연 p99가 목표(연속 실패 3회 × 프로브 주기 2초 + 평가 주기 1초 + 5초 = 12초, `test/load/load_test.go`)를 넘음 |
| 산출물 | JSON 결과와 로그(아티팩트), 작업 요약 |

## 3.3 release.yml (SemVer 태그 `v*.*.*` 푸시)

| 순서 | 단계 |
|---|---|
| 1 | 태그에서 버전 결정(접미사가 있으면 사전 릴리스), `packaging/cosign.pub` 존재 확인 |
| 2 | 테스트: `go test -count=1 ./...` 그리고 `go test -count=1 -tags minimal ./cmd/vigilante ./internal/executor ./internal/probe ./internal/compat` |
| 3 | 바이너리(full·minimal), CycloneDX SBOM, rpm·deb, Helm 차트 |
| 4 | 다중 아키텍처 이미지(amd64, arm64, ppc64le) ghcr.io 푸시, 아키텍처별 이미지 아카이브 |
| 5 | 폐쇄망 번들과 `SHA256SUMS` |
| 6 | cosign 서명(키 기반, 투명성 로그 미사용)과 `packaging/cosign.pub`로 검증 |
| 7 | GitHub 릴리스 생성(검증 방법 안내 포함) |

## 3.4 E2E 데모 시나리오 (run-demo.sh)

| 단계 | 내용 | 기대 종료 코드 |
|---|---|---|
| 사전 | `vigilante doctor` (접근, 로그, 롤백 경로) | 0 |
| 0~1 | v1을 기준 버전으로 등록, 배포 전 기준선 10초 | - |
| 2 | 불량 v2 배포 → canary 관측이 탐지하고 v1로 자동 롤백 | 2 |
| 3 | 불량 v2 재배포, 롤백 경로 고장 → 롤백 실패, 서킷 OPEN | 3 |
| 4 | 서킷 OPEN 중 새 배포 거부 | 3 |
| 5 | 운영자 수동 조치 후 서킷 리셋 | - |
| 6 | 정상 v1.1 배포 → 20초 창 후 통과 | 0 |
| 7 | 랩 시나리오(M8): 체크포인트 → 불량 v2 주입 → 탐지 → v1.1로 롤백 | 0 |
| 8 | 판정 품질 보고서: 30건 게이트에 못 미쳐 종료 코드 4 | 4 |

> CI `demo` 작업은 PR #13 실행 38091832392와 master `dd9a055` 실행 38092384199에서 성공했다(스크립트가 단계마다 기대 종료 코드를 확인하므로 성공은 8단계 모두 기대 종료 코드라는 뜻이다). 로드맵 기록에 따르면 CI 데모에서 탐지 3초, 롤백 1초가 걸렸다(docs/05-roadmap.md M8-1 구현 결과). 로컬에서는 실행하지 않았다.

---

# 4. 테스트 결과 (실측)

## 4.1 실행 정보

| 항목 | 값 |
|---|---|
| 실행 일시 | 2026-10-11 07:43:30 ~ 07:44:42 (로컬 시각 KST, JSON 이벤트 첫·마지막 시각, 약 72초) |
| 커밋 | `dd9a055` (`git rev-parse --short HEAD`), 브랜치 master. 실행 시점 작업 트리 변경 없음(미추적 `deliverables/`만 존재) |
| Go | go1.27.2 windows/amd64 |
| OS | Windows 11 Home 10.0.26200 |
| 명령 | `go test ./... -count=1 -json` (Git Bash) |
| PostgreSQL | 임베디드 18.3.0 (자동 기동) |
| 종료 코드 | 0 |
| 표준 오류 출력 | 없음 |
| 이전 측정(1.1판) | 2026-10-11 00:29:40 ~ 00:30:36, 커밋 `a0c000e`: 패키지 41(테스트 있음 31), 최상위 210·하위 12, 합계 222건 모두 통과. 최소 빌드 46건 통과 |

## 4.2 전체 집계

| 항목 | 값 |
|---|---|
| 패키지 | 42 (1.1판 대비 `internal/observer` 추가) |
| 테스트가 있는 패키지 | 32 (모두 통과) |
| 테스트 파일이 없는 패키지 | 10 (`cmd/fakeapp`, `internal/auth/oidctest`, `internal/dockerapi`, `internal/executor/ostest`, `internal/itsm/snowtest`, `internal/journal`, `internal/model`, `internal/secrets/vaulttest`, `internal/store/pgtest`, `internal/tmpl`) |
| 최상위 테스트 | 239 (통과 239, 실패 0, 생략 0). 1.1판 대비 +29(PR #13 신규) |
| 하위 테스트 | 38 (통과 38, 실패 0, 생략 0). 1.1판 대비 +26(`TestWatchRemoteGateRefusalExitsThree` 9, 키 체인 12, `TestHSTSOverHTTPS` 5) |
| 합계 | 277 (통과 277, 실패 0, 생략 0) |
| 실제 PostgreSQL 사용 하위 테스트 | `TestAppendAndLoad/postgres`, `TestLeases/postgres`, `TestLeaseRaceHasOneWinner/postgres`, `TestChainDetectsTampering/postgres/edit`, `/postgres/delete`, `TestPruneArchivesAndKeepsState/postgres`, 그리고 PR #13의 `TestKeyedChainDetectsRecomputedTampering/postgres/keep-mac`, `/postgres/strip-mac`, `TestKeyedChainWrongKey/postgres`, `TestKeyedChainLegacyEntries/postgres`, `TestKeyedChainCoversUnkeyedPrefix/postgres`, `TestKeyedChainAcrossPrune/postgres` 모두 통과(12건, 생략 아님) |

> 테스트 파일이 없는 10개 중 `oidctest`, `ostest`, `snowtest`, `vaulttest`, `pgtest`는 다른 테스트가 쓰는 시험 도구 패키지이고 `cmd/fakeapp`은 데모용 앱이다. `internal/journal`, `internal/model`, `internal/tmpl`, `internal/dockerapi`는 자체 테스트 파일이 없고 저장소·엔진·실행기 테스트를 통해 간접적으로만 시험된다.

## 4.3 패키지별 결과

| 패키지 | 최상위 테스트 | 하위 테스트 | 결과 | 시간(초) |
|---|---|---|---|---|
| `cmd/vigilante` | 11 | 9 | 통과 | 31.33 |
| `internal/agent` | 1 | 0 | 통과 | 4.54 |
| `internal/api` | 27 | 0 | 통과 | 16.84 |
| `internal/audit` | 13 | 18 | 통과 | 31.81 |
| `internal/auth` | 3 | 0 | 통과 | 1.53 |
| `internal/cienv` | 2 | 0 | 통과 | 0.61 |
| `internal/compat` | 3 | 0 | 통과 | 2.62 |
| `internal/config` | 19 | 0 | 통과 | 0.86 |
| `internal/console` | 6 | 5 | 통과 | 2.06 |
| `internal/decision` | 8 | 0 | 통과 | 2.21 |
| `internal/doctor` | 6 | 0 | 통과 | 2.92 |
| `internal/events` | 5 | 0 | 통과 | 1.18 |
| `internal/executor` | 26 | 0 | 통과 | 3.25 |
| `internal/ha` | 2 | 0 | 통과 | 29.45 |
| `internal/itsm` | 6 | 0 | 통과 | 1.30 |
| `internal/lab` | 2 | 0 | 통과 | 3.82 |
| `internal/metrics` | 5 | 0 | 통과 | 0.82 |
| `internal/notify` | 2 | 0 | 통과 | 1.24 |
| `internal/observer` | 4 | 0 | 통과 | 1.71 |
| `internal/orchestrator` | 29 | 0 | 통과 | 49.37 |
| `internal/pilot` | 3 | 0 | 통과 | 0.49 |
| `internal/presets` | 3 | 0 | 통과 | 0.71 |
| `internal/probe` | 11 | 0 | 통과 | 28.37 |
| `internal/rules` | 7 | 0 | 통과 | 0.65 |
| `internal/safety` | 6 | 0 | 통과 | 1.84 |
| `internal/secrets` | 6 | 0 | 통과 | 0.90 |
| `internal/store` | 8 | 6 | 통과 | 29.78 |
| `internal/sudoers` | 1 | 0 | 통과 | 2.75 |
| `internal/support` | 3 | 0 | 통과 | 0.49 |
| `internal/telemetry` | 2 | 0 | 통과 | 0.42 |
| `internal/tlsconf` | 4 | 0 | 통과 | 2.28 |
| `internal/transport` | 5 | 0 | 통과 | 1.80 |
| 합계 | 239 | 38 | 32개 통과 | - |

> 패키지 시간은 `go test` JSON의 패키지 `Elapsed`이며 패키지는 병렬로 실행되므로 합계가 전체 실행 시간과 같지 않다. 시간이 긴 패키지(`orchestrator`, `probe`, `ha`, `store`, `audit`, `cmd/vigilante`)는 임베디드 PostgreSQL 기동과 HA·DB 프로브 대기 시험이 포함된 것이다. 1.1판 대비 테스트 수가 바뀐 패키지: `cmd/vigilante`(+4 최상위, +9 하위), `internal/api`(+1), `internal/audit`(+9, +12), `internal/config`(+1), `internal/console`(+1, +5), `internal/decision`(+1), `internal/itsm`(+4), `internal/observer`(신규 4), `internal/orchestrator`(+2), `internal/safety`(+1), `internal/tlsconf`(+1).

## 4.4 최소 빌드 결과

| 항목 | 값 |
|---|---|
| 실행 일시 | 2026-10-11 07:45:17 ~ 07:45:40 (로컬 시각 KST, 약 23초), 커밋 `dd9a055` |
| 명령 | `go test -tags minimal -count=1 -json ./cmd/vigilante ./internal/executor ./internal/probe ./internal/compat` |
| 종료 코드 | 0 |
| 패키지 | 4 (모두 통과) |
| 테스트 | 59 (최상위 50, 하위 9. 통과 59, 실패 0, 생략 0). 1.1판은 46(하위 테스트 없음) |

| 패키지 | 최상위 | 하위 | 결과 | 시간(초) | 전체 빌드와 차이 |
|---|---|---|---|---|---|
| `cmd/vigilante` | 12 | 9 | 통과 | 15.47 | `TestMinimalBuildRefusesMissingPlugins` 추가(최소 빌드 전용) |
| `internal/compat` | 3 | 0 | 통과 | 1.12 | 없음 |
| `internal/executor` | 24 | 0 | 통과 | 1.09 | `TestALB`, `TestVSphereSnapshotRevert` 제외(AWS SDK·govmomi는 최소 빌드에서 제외) |
| `internal/probe` | 11 | 0 | 통과 | 22.64 | 없음 |

## 4.5 종료 기준 판정

| 기준 | 결과 | 판정 |
|---|---|---|
| X1 `go test ./...` 실패 0건 | 실패 0 / 277 | 충족 |
| X2 PostgreSQL 테스트 생략 0건 | 생략 0, postgres 하위 테스트 12건 통과 | 충족 |
| X3 최소 빌드 테스트 실패 0건 | 실패 0 / 59 | 충족 |
| X4 CI 다섯 작업 성공 | PR #13 실행 38091832392(헤드 `189dfbb`)와 master `dd9a055` 실행 38092384199에서 `test`(`-race` 포함), `api`(Spectral, oasdiff), `vuln`(govulncheck), `package`, `demo` 모두 성공(4.8) | 충족 |
| X5 계약 테스트 통과 | `TestV2RoutesMatchSpec`, `TestV2ContractLifecycle`, `TestV2ApproveModeDecisions`(`approval.decided` 명세 대조) 등 통과 | 충족 |

## 4.6 주요 검증 결과 요약

| 영역 | 확인된 동작 | 근거 테스트 |
|---|---|---|
| 판정 | 조기 FAIL, 창 경과 후 PASS, warmup, 환경 요인 HOLD, 관측 쿼럼 HOLD, 판정 불가 정책, 관측 장치 저하 중 프로브 실패 위반 HOLD(PR #14부터 직접 재는 프로브만, 액세스 로그 지표는 판정) | `internal/decision` 8건, `internal/observer` 4건 |
| 롤백 | canary 실패 시 드레인 → 롤백 → 복귀(종료 코드 2), 실패 시 격리·서킷 OPEN(종료 코드 3)·재시작 후 OPEN 유지, 크래시 재개 시 완료 단계 건너뜀 | `TestCanaryFailDrainsRollsBackAndEnables`, `TestRollbackFailureIsolatesAndOpensCircuit`, `TestResumeSkipsCompletedSteps` |
| 카오스 | 롤백 중 저장소 단절에도 lease 없이 롤백 지속·대기열 기록 순서와 해시 체인 유지, `fail` 모드는 ROLLBACK_FAILED, LB API 일시 오류·지연은 재시도로 복귀, LB 완전 장애는 ROLLBACK_FAILED로 사람 호출, 관측 장치 저하 중 불량 canary는 HOLD | `TestChaos*` 6건 |
| HA | 리더 DB 단절 시 다른 노드 선출, 강등 노드 쓰기 거부, 롤백 중 리더 교체 후 마무리, 팔로워 요청 전달, TLS 전달 시 설정한 이름으로 리더 인증서 검증 | `TestFailoverOnPartition`, `TestHAFailoverFinishesInterruptedRollback`, `TestFollowerForwardsToLeader`, `TestPostgresFencing`, `TestHAClientServerName` |
| API | 명세·라우트 일치, 명세 기반 요청·응답 검증, 멱등·ETag·작업, 호출 한도와 emergency 버킷, OAuth·스코프, SSE 재개, 웹훅 순서·서명·복구, v1 게이트 필드·오류 `code`, `approval.decided` 명세 일치 | `internal/api` 27건 |
| 저장소·감사 | file·PostgreSQL 동등 동작, 리스 경합 승자 1명, 마이그레이션·다운그레이드 가드, 해시 체인 수정·삭제 검출, 키 체인으로 체인 재계산 검출, 보존 정리 후 상태·키 체인 유지, SIEM TLS 전송 | `internal/store`, `internal/audit` |
| 외부 연동(모의) | F5, ALB, Nutanix, vSphere(vcsim), KVM, OpenStack·Octavia, HAProxy, Nginx, Envoy, Docker, Vault, ServiceNow(상태 코드 기반 재시도, 중복 없는 인시던트 재시도), 알림 채널, TLS syslog 수집기 | `internal/executor`, `internal/secrets`, `internal/itsm`, `internal/notify`, `internal/audit` |
| 보안 | 역할·범위 행렬, 4-eyes(API·로컬 CLI), 로컬 CLI 권한 제한과 break-glass 감사, OIDC·CSRF, 콘솔 HTML 주입 방지·HSTS, 비밀값 로그 가림·지원 번들 제거, SSH 인증서 principal 검증, sudo 변경 명령 한정 | `internal/auth`, `internal/console`, `internal/support`, `internal/transport`, `internal/executor`, `cmd/vigilante` |
| CLI 원격 | `watch --server`의 티켓·동결 예외 전달, 게이트 거부 종료 코드 3 | `TestWatchRemoteSendsTicketAndFreezeOverride`, `TestWatchRemoteGateRefusalExitsThree` |

## 4.7 부하 시험 결과 (CI `load.yml`)

별도 워크플로 `.github/workflows/load.yml`(GitHub Actions `ubuntu-latest`)의 결과다. 각 규모에서 동시 배포 100건 중 10건을 불량으로 만들었다. 두 번의 결과를 함께 적는다.

- M5-4 기록: PR #12 시점(커밋 `a0c000e`)의 결과로 `docs/05-roadmap.md` M5-4 구현 결과와 커밋 `a0c000e` 메시지에 기록된 값이다(1.1판과 같음).
- PR #13 실행: 실행 38091832358(헤드 `189dfbb`, 2026-10-10 22:32 UTC 시작). 두 행렬 작업(`load (3)` 1분 18초, `load (10)` 1분 25초) 모두 통과했고, 아티팩트 `load-3`, `load-10`의 `load-N.json`을 내려받아 값을 옮겼다. 이 실행의 엔진은 관측 장치 가드가 기본값(켜짐)이다.

| 규모 | 출처 | 오판 | 판정 지연 p50 / p99 (목표 12초) | 프로브 요청/초 | 최대 힙 | 평균 CPU | 결과 |
|---|---|---|---|---|---|---|---|
| 대상 2,000 × 프로브 3, 동시 배포 100 | M5-4 기록 | 0 | 5.4초 / 5.5초 | 2,804 | 352 MiB | 0.45코어 | 통과 |
| 대상 2,000 × 프로브 3, 동시 배포 100 | PR #13 실행 | 0 | 5.16초 / 5.32초 | 2,821 | 388 MiB | 0.34코어 | 통과 |
| 대상 2,000 × 프로브 10, 동시 배포 100 | M5-4 기록 | 0 | 5.4초 / 5.7초 | 9,095 | 1.1 GiB | 0.72코어 | 통과 |
| 대상 2,000 × 프로브 10, 동시 배포 100 | PR #13 실행 | 0 | 5.73초 / 5.96초 | 9,100 | 1,077 MiB | 1.24코어 | 통과 |

PR #13 실행의 그 밖의 값(JSON 원본):

| 항목 | 프로브 3 | 프로브 10 |
|---|---|---|
| 측정 시간(`duration_seconds`) | 30.38초 | 31.28초 |
| 판정 지연 p90 / 최대 | 5.20초 / 5.32초 | 5.90초 / 5.96초 |
| 최대 Sys 메모리(`PeakSysMiB`) | 636 MiB | 1,728 MiB |
| 최대 고루틴 | 24,305 | 80,321 |
| CPU 시간(`CPUSeconds`) | 10.46초 | 38.77초 |
| 오판 목록(`WrongVerdicts`) | 없음(null) | 없음(null) |
| Go / OS | go1.27.2 linux/amd64 | go1.27.2 linux/amd64 |

> 판정 지연은 불량 주입부터 롤백 요청까지의 시간이다. 대상은 HTTP 시뮬레이터이므로 sshd 부하는 측정하지 않는다(세션 예산은 단위 테스트 `TestSessionBudgetKeepsRoomForRollback`). 두 실행은 GitHub Actions 공용 러너에서 한 번씩 돌린 값이라 러너 간 편차가 있을 수 있다. 프로브 10개 규모의 평균 CPU가 0.72코어에서 1.24코어로 늘었지만 이 두 표본만으로 원인(관측 장치 가드의 자체 점검, 러너 차이 등)을 가릴 수 없다. 오판 0과 판정 지연 목표는 두 실행 모두 충족했다.

## 4.8 CI 결과 (`ci.yml`)

2026-10-11에 `gh run view`로 확인한 결과다. 두 실행 모두 다섯 작업이 성공했다.

| 실행 | 이벤트·커밋 | test | api | vuln | package | demo |
|---|---|---|---|---|---|---|
| 38091832392 | pull_request(PR #13), 헤드 `189dfbb`, 2026-10-10 22:32 UTC | 성공(8분 32초) | 성공(38초) | 성공(16초) | 성공(6분 7초) | 성공(1분 53초) |
| 38092384199 | push(master), `dd9a055`, 2026-10-10 22:41 UTC | 성공(8분 32초) | 성공(14초) | 성공(23초) | 성공(6분 21초) | 성공(1분 44초) |

| 작업 | 확인된 내용(PR #13 실행의 단계 이름 기준) | 결과 |
|---|---|---|
| `test` | toolchain matches go.mod, gofmt, vet, test (race detector): `postgres:16` 서비스 컨테이너로 `go test -race -count=1 -timeout 20m ./...`, cross-build static binaries(linux/amd64, linux/arm64, linux/ppc64le, windows/amd64) | 성공 |
| `api` | Spectral 린트(`.spectral.yaml`, 경고도 실패), oasdiff `breaking --fail-on ERR` 하위호환 검사 | 성공 |
| `vuln` | govulncheck `./...` | 성공 |
| `package` | minimal build (vet and tests), build binaries, SBOMs, packages, chart, chart renders (single node, HA, TLS): 인증 없는 설정 거부·파일 저장소 HA 거부·HA TLS 값의 https advertise URL·`scheme: HTTPS`·`GOMEMLIMIT` 확인, image, bundles, signature, verification, install the deb and the rpm | 성공 |
| `demo` | end-to-end rollback demo: E2E 8단계(3.4) 모두 기대 종료 코드 | 성공 |

> PR #14(`537870c`): PR 실행 38093154231(헤드 `c4f798a`, 다섯 작업)과 `load.yml` 실행 38093154321(`load (3)`, `load (10)`), master 푸시 실행 38093673759가 모두 성공했다. 변경은 기존 테스트 `TestObserverDegradedHoldsProbeFailures` 안에 확인을 더한 것(하위 테스트 없음)이라 로컬 테스트 수(277건, 최상위 239)는 바뀌지 않는다.

> 이전 확인(1.1판): 커밋 `9eac97d`(PR #12)에서 다섯 작업 성공. PR #12 병합 커밋 `dba3dbe`의 master 푸시 실행(38088492647)도 성공이다.

## 4.9 카오스 시험 결과 (M5-4, PR #13)

`internal/orchestrator/chaos_test.go`의 6건(M5-4 4건, PR #13 추가 2건)이다. 로컬(`dd9a055`, 부록 A)과 CI `test` 작업(`-race`, PR #13 실행·master 실행) 모두에서 통과했다. 리더 강제 종료는 `TestHAFailoverFinishesInterruptedRollback`, 관측점 불일치는 `TestObserverQuorum`이 다룬다.

| 테스트 | 시나리오 | 기대 결과 | 결과 |
|---|---|---|---|
| `TestChaosStoreOutageDuringRollback` | 불량 canary 전에 상태 저장소 단절(기록과 lease 모두 실패), 롤백 후 복구 | 롤백이 저장소를 기다리지 않음. 서비스 lease 없이 진행했다는 이벤트("rolling back under this process's lock only")가 남음(PR #13). 쓰기 대기열(`engine.go` `record`·`flush`, 지표 `vigilante_store_pending_writes`)이 복구 후 순서대로 기록, 해시 체인 유지, 재기동 시 재개 대상 없음 | 통과 |
| `TestChaosStoreOutageLeaseFailMode` | 위와 같은 단절, `on_unavailable: fail`(PR #13) | 롤백하지 않고 ROLLBACK_FAILED, 사유에 "state store unreachable" | 통과 |
| `TestChaosObserverDegradedHolds` | 관측 장치 저하를 보고(스케줄링 지연)한 상태에서 불량 canary(PR #13) | 롤백하지 않고 판정 HOLD, 사유에 "observer degraded" | 통과 |
| `TestChaosLoadBalancerTransientErrors` | LB API가 한 번 오류 후 응답 | 단계 재시도로 롤백 완료, 대상 트래픽 복귀 | 통과 |
| `TestChaosLoadBalancerLatency` | LB API가 단계 제한 시간을 넘겨 지연 | 단계 제한 시간으로 끊고 재시도, 트래픽 복귀 | 통과 |
| `TestChaosLoadBalancerDown` | LB 완전 장애 | 대상은 제자리 롤백되지만 트래픽에 복귀할 수 없으므로 `ROLLBACK_FAILED`로 사람 호출(조용한 성공 아님) | 통과 |

> 저장소 장애 쓰기 대기열은 메모리에만 있으므로 장애 중 서버가 크래시하면 대기분은 유실된다(VGL-SI-05 12장).

## 4.10 결함 수정(PR #13, PR #14) 검증 결과

CHANGELOG `[Unreleased]`의 Fixed 항목별로 회귀 테스트와 결과를 정리한다. 모두 로컬(`dd9a055`)과 CI(PR #13 실행, master 실행)에서 통과했다. PR #14 행은 CI(PR #14 실행, master `537870c` 실행)로만 확인했다. Helm 항목은 Go 테스트가 아니라 CI `package` 작업의 차트 렌더링 단계로 확인한다.

| 구분 | 수정 내용 | 테스트 | 결과 |
|---|---|---|---|
| Fixed (store) | 롤백 시작 시 저장소 불통이면 lease를 `rollback_lease.wait` 동안 재시도 후 lease 없이 진행(`lease.unavailable`, 복구 후 `lease.conflict`), `fail`이면 이전 동작 | `TestGuardStoreUnreachable`, `TestChaosStoreOutageDuringRollback`, `TestChaosStoreOutageLeaseFailMode` | 통과 |
| Fixed (security) | 로컬 CLI 권한 제한(`auth.local_cli`, `--break-glass`, `breakglass.<action>`), 로컬 4-eyes | `TestLocalCLIRestricted`, `TestLocalFourEyes` | 통과 |
| Fixed (security) | SIEM syslog over TLS(`tls://`, RFC 5425, `audit.syslog.tls`) | `TestSyslogExporterTLS`, `TestSyslogExporterTLSRejectsUntrustedCollector`, `TestNewExporterTLSConfig`, `TestAuditValidation` | 통과 |
| Fixed (security) | 키 감사 체인(`audit.chain_key_ref`, `mac`, `audit verify --key`) | `TestKeyedChainDetectsRecomputedTampering`, `TestKeyedChainWrongKey`, `TestKeyedChainLegacyEntries`, `TestKeyedChainCoversUnkeyedPrefix`, `TestKeyedChainAcrossPrune`(각 file·postgres), `TestResolveChainKey` | 통과 |
| Fixed (security) | 콘솔 HSTS와 로그인 엔드포인트 보안 헤더 | `TestHSTSOverHTTPS`(하위 5) | 통과 |
| Fixed (security, deployment) | Helm: 인증 없는 설정 거부, TLS 시 https advertise URL·프로브·ServiceMonitor, 메모리 512Mi/2Gi와 `GOMEMLIMIT` | CI `package` "chart renders (single node, HA, TLS)" | 통과(CI) |
| Fixed (deployment) | `server.ha.tls`로 리더 인증서를 설정한 이름으로 검증 | `TestHAClientServerName` | 통과 |
| Fixed (integration) | `watch --server`가 `--ticket`·`--freeze-override` 전달, 게이트 거부 시 종료 코드 3 | `TestWatchRemoteSendsTicketAndFreezeOverride`, `TestWatchRemoteGateRefusalExitsThree`(하위 9) | 통과 |
| Fixed (integration) | v1 생성 본문 `change_ticket`·`freeze_override`(admin), 게이트 거부 `code` | `TestV1CreateGateFields` | 통과 |
| Fixed (integration) | OpenAPI `approval.decided` 데이터 설명 정정과 계약 테스트 | `TestV2ApproveModeDecisions` | 통과 |
| Fixed (integration) | ServiceNow 상태 코드 기반 재시도(1s·2s·4s), 백그라운드 인시던트, `result="retry"` 지표 | `TestIncidentRetriedUntilServiceNowRecovers`, `TestRetryAfterLostCreateDoesNotDuplicate`, `TestRetryIsBounded`, `TestRetryable`, `TestV2ServiceNowGateIncidentsAndNotes` | 통과 |
| Fixed (decisions) | 관측 장치 가드(`safety.observer_guard`): 저하 중 프로브 실패 기반 위반 HOLD | `TestDegradedSpansAndGrace`, `TestSpreadNeedsSeveralServices`, `TestLoopbackAndStartStop`, `TestDisabled`, `TestObserverDegradedHoldsProbeFailures`, `TestChaosObserverDegradedHolds` | 통과 |
| Fixed (decisions, PR #14) | 관측 장치 가드가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, `host`)의 지표와 모든 프로브의 `probe_error`만 HOLD. `log`·`access_log`·`docker` 프로브의 같은 이름 지표는 판정 | `TestObserverDegradedHoldsProbeFailures` 확장: 저하 중 `access.latency_ms` 위반은 FAIL, `http.latency_ms` 위반은 HOLD | 통과(CI: PR #14 실행 38093154231, master `537870c` 실행 38093673759) |

> 확인하지 않은 부분: break-glass 사용 시 critical 알림 전송, `lease.conflict`의 엔진 수준 알림(가드 콜백 호출까지만 `TestGuardStoreUnreachable`이 확인), HA TLS 전달의 팔로워 → 리더 프록시 경로(`TestHAClientServerName`은 `HAClient` 설정으로 TLS 서버에 직접 연결해 이름 검증만 확인), 관측 장치 가드의 실제 과부하 환경 동작(보고를 주입한 시험만)은 자동 테스트가 없다.

---

# 5. 미시험 항목과 잔여 위험

| 번호 | 항목 | 내용 | 영향 | 조치 계획 |
|---|---|---|---|---|
| O1 | 실장비 연동 | F5 BIG-IP, AWS ALB·NLB, vCenter 7·8, Nutanix(AOS별 Prism v2 경로), 사내 OpenStack(Nova·Cinder·Glance·Octavia), HAProxy 2.x·Nginx·Envoy 실제 버전, Docker·Podman 버전별 연동을 시험하지 않았다. 모두 모의 서버·시뮬레이터 기준 | 실제 장비의 API 차이·권한·지연으로 롤백 실패 가능. 출시 게이트 G1 미충족 | M8-1 랩, `vigilante lab run` 3회 연속 통과 후 "검증됨" 전환 |
| O2 | Cinder revert-to-snapshot | 백엔드·릴리스에 따라 사용 중 볼륨 revert를 거부할 수 있음. 모의 서버로만 확인 | OpenStack 볼륨 부팅 VM의 롤백 실패 가능 | M8 랩에서 확정 |
| O3 | 파일럿·판정 품질 | 실제 서비스에서 오탐·미탐·HOLD 비율을 측정한 적이 없음. 프리셋 임계치는 추정값 | 오판정으로 인한 불필요한 롤백 또는 미탐. 출시 게이트 G2·G3 미충족 | M8-2 파일럿 3개월(1개월 드라이런, 2개월 승인 모드) |
| O4 | 부하 시험 범위 | CI `load`는 M5-4 기록과 PR #13 실행 모두 두 규모를 통과했다(4.7). 다만 대상이 HTTP 시뮬레이터라 sshd 부하는 재지 않고, 로컬에서는 실행하지 않았다. 규모마다 공용 러너 1회 실행이라 편차를 알 수 없다. 제어 평면 RTO(30초) 목표는 별도 측정 기록이 없다. master 푸시는 `load.yml`을 실행하지 않으므로 `dd9a055` 자체의 부하 실행은 없다(PR #13 실행은 헤드 `189dfbb`) | SSH 대상 규모 성능과 RTO는 미측정 | M8 랩에서 실제 SSH 대상 규모 확인, RTO 측정 추가 |
| O5 | 데이터 레이스 | 로컬 Windows에 cgo가 없어 `-race`를 실행하지 않았다. CI `test` 작업(PR #13 실행, master `dd9a055` 실행)에서 `-race`로 통과 | 로컬 단독 보장은 없으나 CI로 확인됨 | PR마다 CI `test` 유지 |
| O6 | CI 결과 범위 | (해소) PR #12·#13 병합 후 master `dd9a055` 푸시 CI(실행 38092384199)가 다섯 작업 모두 성공했다(4.8) | - | - |
| O7 | E2E·패키징·설치 | 로컬에서 실행하지 않았다. CI `demo`, `package`(PR #13 실행, master 실행)에서 통과 | 로컬 재현 기록 없음 | CI 결과로 갈음 |
| O8 | PostgreSQL 버전 차이 | 로컬은 임베디드 18.3.0, CI는 `postgres:16`. 호환성 매트릭스는 PostgreSQL 16을 "CI에서 확인"으로 기재 | 운영 대상 버전별 차이 미확인 | 지원 버전 확정 후 매트릭스 반영 |
| O9 | 직접 테스트 없는 패키지 | `internal/journal`, `internal/model`, `internal/tmpl`, `internal/dockerapi`에 자체 테스트 파일이 없음(간접 시험만) | 경계 조건 결함을 놓칠 수 있음 | 단위 테스트 보강 검토 |
| O10 | 플랫폼 | `host` 프로브는 Linux `/proc` 전용. AIX·Solaris·HP-UX, Azure LB·Application Gateway, Citrix ADC는 미지원 | 해당 환경 사용 불가 | 고객 수요에 따라 결정 |
| O11 | 웹 콘솔 수동 점검 | M4 검증 기준의 콘솔 수동 점검(데모 배포, 승인, 롤백 흐름) 기록이 본 측정 범위에 없음 | UI 결함 미확인 | 수동 점검 기록 작성 |
| O12 | 관측 장치 과부하 오판 | 로드맵 기록: 바쁜 개발용 Windows PC에서 프로브 10개 규모를 다른 작업과 함께 돌리자 관측 쪽 프로브 시간 초과를 대상 장애로 보아 정상 배포 90건을 롤백했다. PR #13의 관측 장치 가드가 대응하지만, 시험은 저하 보고를 주입한 단위·카오스 테스트뿐이고 같은 과부하 조건을 가드를 켠 채 재현한 기록은 없다. (`dd9a055`의 가드가 지표 이름만으로 민감 지표를 골라 액세스 로그의 `latency_ms`도 HOLD하던 문제는 PR #14에서 직접 재는 프로브만 보류하도록 고쳤다, 4.10) | 과부하 시 오탐 롤백은 줄지만 실측 근거가 없음. 저하 중에는 진짜 불량도 HOLD | 과부하 재현 시험(부하 하네스를 자원 제한 환경에서 실행) 추가. 서버 크기는 4.7 표 기준으로 여유 있게 산정 |
| O13 | 브랜치 미병합 | (해소) PR #12는 `dba3dbe`, PR #13은 `dd9a055`로 master에 병합되었고 본 판은 master에서 재측정했다 | - | - |
| O14 | 저장소 장애 중 lease 없는 롤백 | `rollback_lease.on_unavailable: proceed`(기본)는 다른 프로세스와 같은 서비스를 동시에 롤백할 가능성을 감수한다. 실제 다중 프로세스 동시 롤백 시험은 없다(가드 단위 테스트의 충돌 보고만) | 동시 조작 시 대상 상태 혼선 가능(롤백 단계 멱등 전제) | `lease.conflict` 알림 운영 절차 정의, 필요 시 `fail` 사용 |
| O15 | 알림 경로 | break-glass critical 알림, `lease.unavailable`·`lease.conflict` 경고 알림의 실제 채널 전송은 자동 테스트로 확인하지 않았다 | 비상 조치가 운영자에게 전달되지 않을 수 있음 | 알림 채널 모의 서버를 쓰는 테스트 추가 검토 |

# 6. 결론

master 커밋 `dd9a055`(PR #12·#13 병합)에서 자동 테스트 277건(최상위 239, 하위 38. 카오스 6건과 PR #13 신규 최상위 29건 포함)과 최소 빌드 테스트 59건이 로컬 Windows 환경에서 모두 통과했고, PostgreSQL 통합 테스트(하위 12건)는 생략 없이 실제 데이터베이스로 실행되었다. 로컬에서는 race detector를 실행할 수 없어 데이터 레이스 검사는 CI 결과에 의존한다. CI는 PR #13 실행(헤드 `189dfbb`)과 병합 후 master `dd9a055` 실행에서 다섯 작업(`test` `-race` 포함, `api`, `vuln`, `package`, `demo`)이 모두 성공했고, 부하 시험(`load.yml`, PR #13 실행)은 대상 2,000대 × 프로브 3·10개, 동시 배포 100건에서 오판 0건, 판정 지연 p99 5.32초·5.96초(목표 12초)로 통과했다(M5-4 기록 p99 5.5초·5.7초). 문서 기준 커밋 `537870c`(PR #14 병합)은 CI(PR 실행 38093154231·38093154321, master 실행 38093673759)가 모두 통과했다. 따라서 자동 테스트 단계의 종료 기준 X1~X5를 모두 충족했다. 다만 출시 게이트(G1~G3)는 실장비 랩과 파일럿이 시작되지 않아 충족되지 않았고, 관측 장치 가드는 실제 과부하 환경에서 검증되지 않았다(O12). 1.0.0 출시 판단은 M8 랩·파일럿 결과가 나온 뒤 본 문서를 갱신해 수행한다.

---

---

# 부록 A. 테스트 케이스 목록 (전체 빌드, 2026-10-11 실측, 커밋 dd9a055)

`go test ./... -count=1 -json` 결과에서 추출한 전체 테스트(최상위 239, 하위 38)다. 시간은 JSON의 `Elapsed`(초), 목적은 테스트 코드와 이름에서 요약했다. 하위 테스트 이름의 공백은 Go가 밑줄로 바꾼 그대로 적었다.

| 번호 | 패키지 | 테스트 | 결과 | 시간(초) | 목적 |
|---|---|---|---|---|---|
| 1 | `cmd/vigilante` | `TestFeedbackAndPilotReport` | 통과 | 0.79 | `feedback` 기록과 `pilot report` 게이트 계산·종료 코드 |
| 2 | `cmd/vigilante` | `TestLocalCLIRestricted` | 통과 | 0.26 | 인증 설정 시 로컬 `circuit trip` 거부(`--break-glass` 안내), `status` 허용, `--break-glass` 리셋은 `breakglass.circuit.reset` 감사, `local_cli: full`이면 허용 |
| 3 | `cmd/vigilante` | `TestLocalFourEyes` | 통과 | 0.00 | 로컬 승인 결정의 4-eyes: 생성자·롤백 요청자 거부, 다른 사람·`--break-glass`·`four_eyes` 꺼짐은 허용 |
| 4 | `cmd/vigilante` | `TestRequireRollbackTarget` | 통과 | 0.00 | 롤백 대상 버전이 없으면 거부 |
| 5 | `cmd/vigilante` | `TestResolveInputsExistingDeploymentWins` | 통과 | 0.00 | 이미 있는 배포의 값이 자동 채움보다 우선 |
| 6 | `cmd/vigilante` | `TestResolveInputsFromCIAndJournal` | 통과 | 0.00 | CI 환경변수와 저널(마지막 성공 배포)에서 `--id`·`--version`·`--previous` 자동 채움 |
| 7 | `cmd/vigilante` | `TestResolveInputsKeepsExplicitFlags` | 통과 | 0.00 | 직접 지정한 플래그가 자동 채움보다 우선 |
| 8 | `cmd/vigilante` | `TestStoreCommand` | 통과 | 21.98 | `store status`·`migrate` CLI (실제 PostgreSQL) |
| 9 | `cmd/vigilante` | `TestSupportBundle` | 통과 | 0.07 | 지원 번들 생성과 비밀값 제거 |
| 10 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree` | 통과 | 0.02 | `watch --server` 생성 실패의 종료 코드: 게이트 거부 3, 그 밖의 실패 1 |
| 11 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/bad_request` | 통과 | 0.00 | 400 → 1 |
| 12 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/circuit` | 통과 | 0.00 | 409 `circuit_open` → 3 |
| 13 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/forbidden` | 통과 | 0.00 | 403(비admin 동결 예외) → 1 |
| 14 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/freeze` | 통과 | 0.00 | 409 `change_frozen` → 3 |
| 15 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/itsm_down` | 통과 | 0.00 | 503 `itsm_unavailable` → 3 |
| 16 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/no_ticket` | 통과 | 0.00 | 409 `change_ticket_invalid` → 3 |
| 17 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/not_leader` | 통과 | 0.00 | 503(`code` 없음, 리더 없음) → 1 |
| 18 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/older_server` | 통과 | 0.00 | `code` 없는 409(이전 서버) → 3 |
| 19 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/v2_problem` | 통과 | 0.00 | problem+json `code: change_frozen` → 3 |
| 20 | `cmd/vigilante` | `TestWatchRemoteSendsTicketAndFreezeOverride` | 통과 | 0.03 | `watch --server`가 `--ticket`(본문 `change_ticket`·`X-Change-Ticket`)과 `--freeze-override`를 전달, 플래그 없으면 보내지 않음, 서버 판정의 종료 코드 사용 |
| 21 | `internal/agent` | `TestAgentPushesAndFailsafeRollsBack` | 통과 | 0.99 | 에이전트 샘플 전송, 서버 불통 시 failsafe 롤백 |
| 22 | `internal/api` | `TestAuditTrailOverAPI` | 통과 | 0.10 | API 조작이 감사 기록에 남고 조회됨 |
| 23 | `internal/api` | `TestAuthAndLifecycle` | 통과 | 0.16 | 인증과 v1 배포 수명주기 |
| 24 | `internal/api` | `TestConsoleRefusesUsersWithoutRoles` | 통과 | 0.14 | 역할 바인딩 없는 사용자의 콘솔 로그인 거부 |
| 25 | `internal/api` | `TestConsoleSessionUsesTheAPI` | 통과 | 0.53 | 콘솔 세션 쿠키로 API 호출, CSRF 요구 |
| 26 | `internal/api` | `TestCreateFillsPreviousFromLastGood` | 통과 | 0.12 | `previous_version` 생략 시 마지막 정상 버전 사용 |
| 27 | `internal/api` | `TestFollowerForwardsToLeader` | 통과 | 0.06 | HA 팔로워가 요청을 리더로 전달 |
| 28 | `internal/api` | `TestFourEyesApproval` | 통과 | 0.10 | 4-eyes: 요청자와 같은 사람의 승인 거부 |
| 29 | `internal/api` | `TestGitHubWebhookSignature` | 통과 | 0.12 | GitHub 웹훅 HMAC 서명 검증 |
| 30 | `internal/api` | `TestMetricsEndpoint` | 통과 | 0.09 | `/metrics` 권한과 출력 |
| 31 | `internal/api` | `TestParseWebhookIgnoresNonSuccess` | 통과 | 0.00 | 성공이 아닌 CI 이벤트 무시 |
| 32 | `internal/api` | `TestRateLimitConfigValidation` | 통과 | 0.00 | 호출 한도 설정 검증 |
| 33 | `internal/api` | `TestReadyzAndRequestID` | 통과 | 0.01 | `/readyz`와 `X-Request-ID` 전파 |
| 34 | `internal/api` | `TestRoleAndScopeEnforcement` | 통과 | 0.15 | 역할·범위별 허용·거부(다른 팀 토큰은 빈 목록) |
| 35 | `internal/api` | `TestSampleIngestTagsAgentSource` | 통과 | 0.06 | 에이전트 샘플의 `source` 표시 |
| 36 | `internal/api` | `TestV1CreateGateFields` | 통과 | 0.85 | v1 생성 본문 `change_ticket`·`freeze_override`(admin만, 비admin 403), 게이트 거부 `code`(409 `change_ticket_invalid`·`change_frozen`, 503 `itsm_unavailable` + `Retry-After`), 일반 오류 400은 `code` 없음 |
| 37 | `internal/api` | `TestV2ApproveModeDecisions` | 통과 | 1.30 | 승인 모드의 승인·거절(계약 검증), `approval.decided` 데이터 필드가 명세 설명과 일치 |
| 38 | `internal/api` | `TestV2ChangeFreeze` | 통과 | 1.05 | 변경 동결 선언·종료와 `change_frozen` 응답(계약 검증) |
| 39 | `internal/api` | `TestV2ContractLifecycle` | 통과 | 1.67 | v2 수명주기 요청·응답을 OpenAPI 명세로 검증(페이지, 멱등, ETag, 작업) |
| 40 | `internal/api` | `TestV2EventStreamResumesAndFilters` | 통과 | 0.53 | SSE `Last-Event-ID` 재개와 범위 필터 |
| 41 | `internal/api` | `TestV2Feedback` | 통과 | 0.84 | 판정 평가 API와 오탐·미탐 제약 |
| 42 | `internal/api` | `TestV2OAuthClientsAndScopes` | 통과 | 0.58 | OAuth 클라이언트 등록·토큰·회전·폐기·스코프 |
| 43 | `internal/api` | `TestV2RateLimits` | 통과 | 0.35 | 429, 조회 폭주 중 emergency 버킷 롤백 허용, 레거시 토큰 무제한 |
| 44 | `internal/api` | `TestV2RoutesMatchSpec` | 통과 | 0.00 | 명세의 v2 연산과 서버 라우트가 정확히 일치 |
| 45 | `internal/api` | `TestV2Scopes` | 통과 | 0.37 | API 클라이언트 스코프 제한 |
| 46 | `internal/api` | `TestV2ServiceNowGateIncidentsAndNotes` | 통과 | 1.26 | ServiceNow 변경 게이트, 인시던트(처음 두 시도 실패 후 1건, `retry` 지표, 감사), 작업 노트(모의 서버) |
| 47 | `internal/api` | `TestV2WebhooksDeliverInOrderSignedAndRecover` | 통과 | 1.37 | 웹훅 순서 전달, 서명, 수신 서버 복구 후 재시도 |
| 48 | `internal/api` | `TestV2WebhooksNeedSigningKey` | 통과 | 0.01 | 서명 마스터 키 없으면 웹훅 거부 |
| 49 | `internal/audit` | `TestChainDetectsTampering` | 통과 | 21.95 | 감사 해시 체인이 수정·삭제를 검출(file·postgres) |
| 50 | `internal/audit` | `TestChainDetectsTampering/file/delete` | 통과 | 0.05 | file 백엔드: 기록 삭제 검출 |
| 51 | `internal/audit` | `TestChainDetectsTampering/file/edit` | 통과 | 0.07 | file 백엔드: 기록 수정 검출 |
| 52 | `internal/audit` | `TestChainDetectsTampering/postgres/delete` | 통과 | 0.76 | PostgreSQL: SQL 삭제 검출 |
| 53 | `internal/audit` | `TestChainDetectsTampering/postgres/edit` | 통과 | 21.08 | PostgreSQL: SQL 수정 검출 |
| 54 | `internal/audit` | `TestKeyedChainAcrossPrune` | 통과 | 0.79 | 키 체인: 보존 정리 후 앵커가 MAC을 이어받아 검증 통과(file·postgres) |
| 55 | `internal/audit` | `TestKeyedChainAcrossPrune/file` | 통과 | 0.06 | file 백엔드 |
| 56 | `internal/audit` | `TestKeyedChainAcrossPrune/postgres` | 통과 | 0.73 | PostgreSQL 백엔드 |
| 57 | `internal/audit` | `TestKeyedChainCoversUnkeyedPrefix` | 통과 | 1.54 | 키 체인: 키 도입 전 기록은 unkeyed로 세고 `keyed_from`부터 MAC 요구(file·postgres) |
| 58 | `internal/audit` | `TestKeyedChainCoversUnkeyedPrefix/file` | 통과 | 0.10 | file 백엔드 |
| 59 | `internal/audit` | `TestKeyedChainCoversUnkeyedPrefix/postgres` | 통과 | 1.44 | PostgreSQL 백엔드 |
| 60 | `internal/audit` | `TestKeyedChainDetectsRecomputedTampering` | 통과 | 1.90 | 키 체인: 기록을 고치고 체인을 다시 계산해도 MAC 불일치·누락으로 검출(file·postgres) |
| 61 | `internal/audit` | `TestKeyedChainDetectsRecomputedTampering/file/keep-mac` | 통과 | 0.11 | file: 옛 MAC을 둔 채 재계산 → "MAC does not match" |
| 62 | `internal/audit` | `TestKeyedChainDetectsRecomputedTampering/file/strip-mac` | 통과 | 0.09 | file: MAC을 지우고 재계산 → "has no MAC" |
| 63 | `internal/audit` | `TestKeyedChainDetectsRecomputedTampering/postgres/keep-mac` | 통과 | 0.95 | PostgreSQL: 옛 MAC을 둔 채 재계산 → "MAC does not match" |
| 64 | `internal/audit` | `TestKeyedChainDetectsRecomputedTampering/postgres/strip-mac` | 통과 | 0.75 | PostgreSQL: MAC을 지우고 재계산 → "has no MAC" |
| 65 | `internal/audit` | `TestKeyedChainLegacyEntries` | 통과 | 0.79 | 키 체인: 체인 도입 전·키 도입 전 기록과의 공존(file·postgres) |
| 66 | `internal/audit` | `TestKeyedChainLegacyEntries/file` | 통과 | 0.24 | file 백엔드 |
| 67 | `internal/audit` | `TestKeyedChainLegacyEntries/postgres` | 통과 | 0.56 | PostgreSQL 백엔드 |
| 68 | `internal/audit` | `TestKeyedChainWrongKey` | 통과 | 0.70 | 키 체인: 다른 키로 검증하면 손상 보고(file·postgres) |
| 69 | `internal/audit` | `TestKeyedChainWrongKey/file` | 통과 | 0.03 | file 백엔드 |
| 70 | `internal/audit` | `TestKeyedChainWrongKey/postgres` | 통과 | 0.67 | PostgreSQL 백엔드 |
| 71 | `internal/audit` | `TestLegacyEntriesBeforeChain` | 통과 | 0.10 | 체인 도입 전 기록 처리 |
| 72 | `internal/audit` | `TestNewExporterTLSConfig` | 통과 | 0.01 | SIEM 전송기 TLS 설정: `tls://` 기본 포트 6514, 서버 이름·최소 버전, `tls` 블록은 `tls://`에서만 |
| 73 | `internal/audit` | `TestPruneArchivesAndKeepsState` | 통과 | 0.79 | 보존 정리: 아카이브 후 앵커로 상태 유지(file·postgres) |
| 74 | `internal/audit` | `TestPruneArchivesAndKeepsState/file` | 통과 | 0.18 | file 백엔드 보존 정리 |
| 75 | `internal/audit` | `TestPruneArchivesAndKeepsState/postgres` | 통과 | 0.62 | PostgreSQL 보존 정리 |
| 76 | `internal/audit` | `TestResolveChainKey` | 통과 | 0.00 | 체인 키 참조 해석과 32바이트 미만 거부 |
| 77 | `internal/audit` | `TestSyslogExporter` | 통과 | 0.23 | SIEM syslog 전송 |
| 78 | `internal/audit` | `TestSyslogExporterTLS` | 통과 | 0.02 | TLS syslog 수집기(사설 CA)로 RFC 5425 옥텟 카운팅 프레임 전송 |
| 79 | `internal/audit` | `TestSyslogExporterTLSRejectsUntrustedCollector` | 통과 | 0.26 | 신뢰하지 않는 수집기 인증서면 전송하지 않음 |
| 80 | `internal/auth` | `TestOIDCGroupsAndUsers` | 통과 | 0.36 | OIDC JWT 검증과 그룹·사용자 역할 매핑 |
| 81 | `internal/auth` | `TestRoleScopeMatrix` | 통과 | 0.00 | 역할 × 범위 허용 행렬 |
| 82 | `internal/auth` | `TestServiceAccountsAndLegacy` | 통과 | 0.00 | 서비스 계정 토큰(해시·만료)과 레거시 토큰 |
| 83 | `internal/cienv` | `TestDeploymentID` | 통과 | 0.00 | CI별 배포 ID 추출 |
| 84 | `internal/cienv` | `TestVersion` | 통과 | 0.00 | CI별 버전 추출 |
| 85 | `internal/compat` | `TestDocMatchesMatrix` | 통과 | 0.00 | docs/09 호환성 문서와 바이너리 매트릭스 일치 |
| 86 | `internal/compat` | `TestMatrixCoversEveryPlugin` | 통과 | 0.00 | 모든 플러그인에 매트릭스 항목 |
| 87 | `internal/compat` | `TestWarningListsExperimentalPluginsInUse` | 통과 | 0.00 | 사용 중인 실험적 플러그인 경고 |
| 88 | `internal/config` | `TestAuditValidation` | 통과 | 0.00 | `audit.syslog`(tls 주소·`tls` 블록·`min_version`)와 `audit.chain_key_ref` 설정 검증 |
| 89 | `internal/config` | `TestAuthValidation` | 통과 | 0.00 | `auth` 설정 검증 |
| 90 | `internal/config` | `TestBuiltinPresetsProduceValidServices` | 통과 | 0.00 | 내장 프리셋이 유효한 서비스를 생성 |
| 91 | `internal/config` | `TestFreezeValidation` | 통과 | 0.02 | `change_freeze` 설정 검증 |
| 92 | `internal/config` | `TestNotifyAndITSMValidation` | 통과 | 0.00 | `notify`·`itsm` 설정 검증 |
| 93 | `internal/config` | `TestOpenStackValidation` | 통과 | 0.01 | OpenStack 자격증명·실행기 설정 검증 |
| 94 | `internal/config` | `TestOrganisationPresetDir` | 통과 | 0.05 | 조직 프리셋 디렉토리 |
| 95 | `internal/config` | `TestPresetErrors` | 통과 | 0.00 | 프리셋 오류 보고 |
| 96 | `internal/config` | `TestPresetOverridesAndOptionalSections` | 통과 | 0.03 | 프리셋 overrides와 선택 섹션 |
| 97 | `internal/config` | `TestReferenceConfigValidates` | 통과 | 0.02 | 참조 설정(examples/config) 검증 통과 |
| 98 | `internal/config` | `TestRemoteGrepHasNoLineCount` | 통과 | 0.00 | `remote_grep` 사용 시 `lines` 지표 규칙 거부 |
| 99 | `internal/config` | `TestRollbackModeValidationAndWarning` | 통과 | 0.02 | `rollback.mode` 검증·경고 |
| 100 | `internal/config` | `TestSecretsValidation` | 통과 | 0.00 | `secrets` 설정 검증 |
| 101 | `internal/config` | `TestServiceDefinitionsWinOverPreset` | 통과 | 0.01 | 서비스 정의가 프리셋보다 우선 |
| 102 | `internal/config` | `TestServiceWithoutRollbackRuleRejected` | 통과 | 0.00 | rollback 규칙 없는 서비스 거부 |
| 103 | `internal/config` | `TestStateAndHAValidation` | 통과 | 0.00 | `state`·`ha` 설정 검증 |
| 104 | `internal/config` | `TestUnknownFieldsRejected` | 통과 | 0.00 | 알 수 없는 설정 키 거부 |
| 105 | `internal/config` | `TestValidationCatchesBrokenReferences` | 통과 | 0.00 | 깨진 교차 참조 검출 |
| 106 | `internal/config` | `TestWeeklyFreezeWindows` | 통과 | 0.00 | 주간 동결 창 계산 |
| 107 | `internal/console` | `TestAppAvoidsHTMLSinks` | 통과 | 0.01 | 콘솔 UI가 문자열로 HTML을 만들지 않음(주입 방지) |
| 108 | `internal/console` | `TestCallbackRejections` | 통과 | 0.14 | OIDC 콜백 오류 거부 |
| 109 | `internal/console` | `TestHSTSOverHTTPS` | 통과 | 0.04 | 콘솔 HSTS: HTTPS로 제공될 때만, 로그인 엔드포인트 포함 모든 콘솔 응답에 보안 헤더 |
| 110 | `internal/console` | `TestHSTSOverHTTPS/http_redirect_url` | 통과 | 0.01 | http `redirect_url` → HSTS 없음 |
| 111 | `internal/console` | `TestHSTSOverHTTPS/https_redirect_url` | 통과 | 0.01 | https `redirect_url`(TLS 프록시 뒤) → HSTS |
| 112 | `internal/console` | `TestHSTSOverHTTPS/plain_http` | 통과 | 0.01 | 평문 HTTP → HSTS 없음 |
| 113 | `internal/console` | `TestHSTSOverHTTPS/server.tls` | 통과 | 0.00 | `server.tls` 설정 → HSTS |
| 114 | `internal/console` | `TestHSTSOverHTTPS/tls_request` | 통과 | 0.01 | TLS 요청 → HSTS |
| 115 | `internal/console` | `TestOIDCSignInSessionAndCSRF` | 통과 | 0.15 | OIDC 로그인, 세션 쿠키, CSRF |
| 116 | `internal/console` | `TestSessionKeyMustBeLongEnough` | 통과 | 0.00 | 세션 키 길이 검증 |
| 117 | `internal/console` | `TestStaticFilesAndTokenMode` | 통과 | 0.01 | 정적 파일과 토큰 로그인 모드 |
| 118 | `internal/decision` | `TestEnvironmentalHold` | 통과 | 0.15 | 대조군도 나빠지면 환경 요인으로 HOLD |
| 119 | `internal/decision` | `TestFailFast` | 통과 | 0.03 | 명확한 위반 시 조기 FAIL |
| 120 | `internal/decision` | `TestInconclusivePolicies` | 통과 | 0.45 | 판정 불가 시 정책(hold·pass·rollback) |
| 121 | `internal/decision` | `TestNotifyRuleDoesNotFail` | 통과 | 0.15 | notify 규칙은 FAIL을 만들지 않음 |
| 122 | `internal/decision` | `TestObserverDegradedHoldsProbeFailures` | 통과 | 0.22 | 관측 장치 저하 중 프로브 실패 규칙은 HOLD(action hold), 정상이면 FAIL, 액세스 로그 위반은 저하 중에도 FAIL |
| 123 | `internal/decision` | `TestObserverQuorum` | 통과 | 0.16 | 관측점 불일치 시 HOLD(관측 쿼럼) |
| 124 | `internal/decision` | `TestPassAfterWindow` | 통과 | 0.15 | 관측 창 경과 후 PASS |
| 125 | `internal/decision` | `TestWarmupIgnoresEarlyErrors` | 통과 | 0.15 | warmup 중 초기 오류 무시 |
| 126 | `internal/doctor` | `TestDoctorAccessLogFormatMismatch` | 통과 | 0.01 | doctor: 액세스 로그 형식 불일치 검출 |
| 127 | `internal/doctor` | `TestDoctorChecksSudoRules` | 통과 | 0.02 | doctor: sudo 규칙 확인 |
| 128 | `internal/doctor` | `TestDoctorChecksVaultReferences` | 통과 | 0.03 | doctor: Vault 참조 확인 |
| 129 | `internal/doctor` | `TestDoctorFindsRealProblems` | 통과 | 0.04 | doctor: 의도적 권한·경로 오류를 힌트와 함께 보고 |
| 130 | `internal/doctor` | `TestDoctorWarnsOnSSHSessionBudget` | 통과 | 0.15 | doctor: SSH 세션 예산 경고 |
| 131 | `internal/doctor` | `TestHint` | 통과 | 0.00 | doctor 조치 힌트 |
| 132 | `internal/events` | `TestEventsFollowDeploymentLifecycle` | 통과 | 0.00 | 배포 수명주기에 따른 이벤트 발행 |
| 133 | `internal/events` | `TestMatchesAndSignature` | 통과 | 0.00 | 구독 필터와 Standard Webhooks 서명 |
| 134 | `internal/events` | `TestSequenceContinuesAfterReload` | 통과 | 0.00 | 새 리더가 이벤트 순번을 이어감 |
| 135 | `internal/events` | `TestSubscribeHasNoHoles` | 통과 | 0.00 | 구독 시 누락 없음 |
| 136 | `internal/events` | `TestWebhooksSurviveReload` | 통과 | 0.00 | 재시작 후 구독·커서 유지 |
| 137 | `internal/executor` | `TestALB` | 통과 | 0.00 | AWS ALB 등록·해제·상태 대기(모의 API) |
| 138 | `internal/executor` | `TestContainerCompensatesOnStartFailure` | 통과 | 0.00 | 컨테이너 시작 실패 시 옛 컨테이너 복원 |
| 139 | `internal/executor` | `TestContainerSwitch` | 통과 | 0.03 | 컨테이너 이전 이미지로 전환(모의 Docker API) |
| 140 | `internal/executor` | `TestDryRunDoesNotMutate` | 통과 | 0.00 | 드라이런은 변경하지 않음 |
| 141 | `internal/executor` | `TestEnvoyEDS` | 통과 | 0.00 | Envoy EDS 파일 DRAINING 전환 |
| 142 | `internal/executor` | `TestF5iControl` | 통과 | 0.00 | F5 iControl 드레인·복귀(모의 서버) |
| 143 | `internal/executor` | `TestHAProxyRuntimeAPI` | 통과 | 0.00 | HAProxy runtime API 명령(모의 소켓) |
| 144 | `internal/executor` | `TestKVMSnapshotRevert` | 통과 | 0.00 | KVM virsh 스냅샷 복원(모의) |
| 145 | `internal/executor` | `TestNginxController` | 통과 | 0.00 | Nginx upstream 수정·테스트·reload |
| 146 | `internal/executor` | `TestNginxDownRewrite` | 통과 | 0.00 | Nginx `down` 표시 재작성 |
| 147 | `internal/executor` | `TestNginxSudoChangesOnly` | 통과 | 0.00 | Nginx: 변경 명령만 sudo |
| 148 | `internal/executor` | `TestNutanixRestore` | 통과 | 0.00 | Nutanix Prism v2 스냅샷 복원(모의) |
| 149 | `internal/executor` | `TestOctaviaDrainEnableWaitsForActive` | 통과 | 0.02 | Octavia 드레인·복귀, ACTIVE 대기, 409 재시도 |
| 150 | `internal/executor` | `TestOctaviaDryRun` | 통과 | 0.00 | Octavia 드라이런 |
| 151 | `internal/executor` | `TestOpenStackDiagnose` | 통과 | 0.01 | OpenStack doctor 점검 |
| 152 | `internal/executor` | `TestOpenStackDryRunAndReauth` | 통과 | 0.01 | OpenStack 드라이런과 401 재인증 |
| 153 | `internal/executor` | `TestOpenStackImageSnapshotRebuild` | 통과 | 0.02 | 이미지 부팅 서버: 스냅샷·rebuild |
| 154 | `internal/executor` | `TestOpenStackPasswordAuthAndInternalInterface` | 통과 | 0.00 | Keystone 비밀번호 인증과 internal 엔드포인트 |
| 155 | `internal/executor` | `TestOpenStackRevertRefusedLeavesClearError` | 통과 | 0.01 | Cinder revert 거부 시 명확한 오류 |
| 156 | `internal/executor` | `TestOpenStackVolumeSnapshotRevertAndReplay` | 통과 | 0.05 | 볼륨 부팅 서버: 스냅샷·revert·멱등 재실행 |
| 157 | `internal/executor` | `TestRepoOf` | 통과 | 0.00 | 이미지 참조에서 저장소 추출 |
| 158 | `internal/executor` | `TestSymlinkFailurePropagates` | 통과 | 0.00 | symlink 실패 전파 |
| 159 | `internal/executor` | `TestSymlinkRollbackAndVerify` | 통과 | 0.00 | symlink 전환·재시작·확인 |
| 160 | `internal/executor` | `TestSymlinkSudoChangesOnly` | 통과 | 0.00 | symlink: 변경 명령만 하나씩 sudo |
| 161 | `internal/executor` | `TestVSphereSnapshotRevert` | 통과 | 0.70 | vSphere 스냅샷 복원(govmomi vcsim) |
| 162 | `internal/executor` | `TestWebhookExecutor` | 통과 | 0.00 | webhook 실행기 요청·확인 |
| 163 | `internal/ha` | `TestFailoverOnPartition` | 통과 | 25.24 | 리더 DB 단절 시 다른 노드 선출, 복구 후 강등 노드 쓰기 거부(실제 PostgreSQL) |
| 164 | `internal/ha` | `TestGracefulShutdownHandsOverFast` | 통과 | 1.77 | 정상 종료 시 리스 즉시 반환·빠른 인계 |
| 165 | `internal/itsm` | `TestChangeChecks` | 통과 | 0.01 | ServiceNow 변경 승인·상태·계획 시간 확인 |
| 166 | `internal/itsm` | `TestIncidentRetriedUntilServiceNowRecovers` | 통과 | 0.01 | ServiceNow 일시 장애 동안 인시던트 생성을 재시도해 복구 후 생성 |
| 167 | `internal/itsm` | `TestIncidentsAreDeduplicatedAndNotesWritten` | 통과 | 0.00 | 인시던트 중복 방지와 작업 노트 |
| 168 | `internal/itsm` | `TestRetryAfterLostCreateDoesNotDuplicate` | 통과 | 0.02 | 응답이 유실된 생성 후 재시도 시 `correlation_id`로 찾아 중복 생성 없음 |
| 169 | `internal/itsm` | `TestRetryIsBounded` | 통과 | 0.06 | 재시도 횟수·대기(백오프) 상한 |
| 170 | `internal/itsm` | `TestRetryable` | 통과 | 0.00 | 재시도 판단: 연결 오류·429·5xx는 재시도, 그 밖의 4xx·변경 무효는 중단 |
| 171 | `internal/lab` | `TestInjectFailureStopsAndResets` | 통과 | 0.14 | 랩: 주입 실패 시 중단·리셋 |
| 172 | `internal/lab` | `TestScenarioPassesAndSummarizes` | 통과 | 0.85 | 랩 시나리오 통과·요약 표 |
| 173 | `internal/metrics` | `TestAggregations` | 통과 | 0.00 | 시계열 집계 |
| 174 | `internal/metrics` | `TestCounterAggOnEmpty` | 통과 | 0.00 | 빈 구간 카운터 집계 |
| 175 | `internal/metrics` | `TestOutOfOrderAndRetention` | 통과 | 0.00 | 순서가 바뀐 샘플과 보존 |
| 176 | `internal/metrics` | `TestSustainedHighRateStaysLinear` | 통과 | 0.42 | 고빈도 입력에서 시리즈 복사 없이 선형 유지 |
| 177 | `internal/metrics` | `TestWindowBoundsAndSource` | 통과 | 0.00 | 창 경계와 샘플 출처 |
| 178 | `internal/notify` | `TestEmail` | 통과 | 0.00 | SMTP 이메일 전송 |
| 179 | `internal/notify` | `TestRoutingDedupAndChannelFormats` | 통과 | 0.03 | 알림 라우팅, 중복 억제, 채널 형식 |
| 180 | `internal/observer` | `TestDegradedSpansAndGrace` | 통과 | 0.02 | 저하 구간: 열린 구간, 회복 후 `grace` 동안 유지, 이후 신뢰, 긴 lookback |
| 181 | `internal/observer` | `TestDisabled` | 통과 | 0.00 | `disabled`면 HOLD하지 않음 |
| 182 | `internal/observer` | `TestLoopbackAndStartStop` | 통과 | 1.20 | 루프백 에코 정상·닫힌 리스너 검출, 참조 계수 시작·정지, 유휴 프로세스는 저하 아님 |
| 183 | `internal/observer` | `TestSpreadNeedsSeveralServices` | 통과 | 0.00 | 확산 신호: 한 서비스만 시간 초과면 릴리스 문제(저하 아님), 3개 서비스에 걸치면 저하, 에이전트·오래된 샘플 제외 |
| 184 | `internal/orchestrator` | `TestAPIClientsSurviveRestart` | 통과 | 0.04 | API 클라이언트가 재시작 후 유지 |
| 185 | `internal/orchestrator` | `TestApprovalGate` | 통과 | 0.18 | 상위 전략 승인 게이트 |
| 186 | `internal/orchestrator` | `TestApprovalTimeouts` | 통과 | 0.49 | 승인 대기 시간 초과 처리 |
| 187 | `internal/orchestrator` | `TestApproveModeRejectRestoresTraffic` | 통과 | 0.13 | 승인 모드 거절 시 트래픽 복귀 |
| 188 | `internal/orchestrator` | `TestApproveModeWaitsDrainsAndApproves` | 통과 | 0.28 | 승인 모드: 대기·선드레인·승인 후 롤백 |
| 189 | `internal/orchestrator` | `TestBlastRadiusRefusesDrain` | 통과 | 0.16 | blast radius가 과도한 드레인 거부 |
| 190 | `internal/orchestrator` | `TestCanaryFailDrainsRollsBackAndEnables` | 통과 | 0.17 | canary 실패 → 드레인 → 롤백 → 복귀, 종료 코드 2 |
| 191 | `internal/orchestrator` | `TestCanaryPassPromotes` | 통과 | 1.54 | canary 통과 → 승격 |
| 192 | `internal/orchestrator` | `TestChaosLoadBalancerDown` | 통과 | 0.25 | 카오스: LB 완전 장애 — 제자리 롤백 후 ROLLBACK_FAILED(사람 호출) |
| 193 | `internal/orchestrator` | `TestChaosLoadBalancerLatency` | 통과 | 1.38 | 카오스: LB API가 단계 타임아웃을 넘겨 지연 — 재시도 후 트래픽 복귀 |
| 194 | `internal/orchestrator` | `TestChaosLoadBalancerTransientErrors` | 통과 | 0.37 | 카오스: LB API 일시 오류 — 단계 재시도 후 트래픽 복귀 |
| 195 | `internal/orchestrator` | `TestChaosObserverDegradedHolds` | 통과 | 1.56 | 카오스: 관측 장치 저하 중 불량 canary를 롤백하지 않고 HOLD, 사유에 "observer degraded" |
| 196 | `internal/orchestrator` | `TestChaosStoreOutageDuringRollback` | 통과 | 0.94 | 카오스: 불량 canary 전 저장소 단절 — lease 없이 롤백 진행(이벤트 기록), 대기열 기록 순서·해시 체인 유지, 재기동 시 재개 대상 없음 |
| 197 | `internal/orchestrator` | `TestChaosStoreOutageLeaseFailMode` | 통과 | 0.78 | 카오스: 저장소 단절 + `on_unavailable: fail` → ROLLBACK_FAILED("state store unreachable") |
| 198 | `internal/orchestrator` | `TestCompactKeepsRunningOperationsAndFreshKeys` | 통과 | 0.00 | 보존 정리가 진행 중 작업·유효 멱등 키 유지 |
| 199 | `internal/orchestrator` | `TestDeclaredFreezesPersistAndEnd` | 통과 | 0.04 | API 동결 영속·종료 |
| 200 | `internal/orchestrator` | `TestEnvironmentalProblemHolds` | 통과 | 1.54 | 환경 요인은 HOLD(롤백하지 않음) |
| 201 | `internal/orchestrator` | `TestEscalationRecovers` | 통과 | 0.15 | 상위 전략으로 복구 |
| 202 | `internal/orchestrator` | `TestFlappingGuardBlocksAndIsolates` | 통과 | 0.22 | 플래핑 가드: 자동 롤백 차단·격리 |
| 203 | `internal/orchestrator` | `TestFreezeBlocksNewPhasesUnlessOverridden` | 통과 | 1.60 | 동결 중 새 단계 거부(override 예외) |
| 204 | `internal/orchestrator` | `TestFreezeRollbackPolicy` | 통과 | 1.71 | 동결 중 롤백 허용 정책 |
| 205 | `internal/orchestrator` | `TestHAFailoverFinishesInterruptedRollback` | 통과 | 31.11 | 롤백 중 리더 종료 시 새 리더가 완료 단계를 건너뛰고 마무리(실제 PostgreSQL) |
| 206 | `internal/orchestrator` | `TestLastGoodVersionAndMarkGood` | 통과 | 0.81 | 마지막 정상 버전과 mark-good |
| 207 | `internal/orchestrator` | `TestOpenStackCanaryRollsBackThroughOctavia` | 통과 | 0.15 | M7 시나리오: 볼륨 부팅 VM canary 실패 → Octavia 드레인 → revert → 복귀(모의 OpenStack) |
| 208 | `internal/orchestrator` | `TestOperationsAndIdempotencySurviveRestart` | 통과 | 0.06 | 작업·멱등 기록이 재시작·리더 교체 후 유지 |
| 209 | `internal/orchestrator` | `TestPendingApprovalSurvivesRestart` | 통과 | 0.33 | 승인 대기가 재시작 후 유지 |
| 210 | `internal/orchestrator` | `TestResumeSkipsCompletedSteps` | 통과 | 0.11 | 크래시 후 재개 시 완료 단계 건너뜀 |
| 211 | `internal/orchestrator` | `TestRollbackFailureIsolatesAndOpensCircuit` | 통과 | 0.27 | 롤백 실패 → 격리, 2회 실패 시 서킷 OPEN·배포 거부·재시작 후 유지, 종료 코드 3 |
| 212 | `internal/orchestrator` | `TestTelemetryFollowsVerdictsAndRollbacks` | 통과 | 0.23 | 판정·롤백 지표 |
| 213 | `internal/pilot` | `TestComputeVerdictsTimingAndGate` | 통과 | 0.00 | 파일럿 판정·시간·게이트 계산 |
| 214 | `internal/pilot` | `TestGatePasses` | 통과 | 0.00 | 게이트 통과 조건 |
| 215 | `internal/pilot` | `TestHoldCause` | 통과 | 0.00 | HOLD 원인 분류 |
| 216 | `internal/presets` | `TestBuiltinsLoadAndResolve` | 통과 | 0.00 | 내장 프리셋 로드·전개 |
| 217 | `internal/presets` | `TestDuplicateAndMalformedPresets` | 통과 | 0.01 | 중복·잘못된 프리셋 거부 |
| 218 | `internal/presets` | `TestLatestAndPinnedVersions` | 통과 | 0.02 | 프리셋 최신·고정 버전 |
| 219 | `internal/probe` | `TestAccessAndAppLogLocalFollowWithRotation` | 통과 | 2.51 | 액세스·앱 로그 추적(로테이션 포함) |
| 220 | `internal/probe` | `TestCollectorRestartsFailingProbe` | 통과 | 1.50 | 실패한 프로브 재시작 |
| 221 | `internal/probe` | `TestDBProbeFullCheckCadence` | 통과 | 18.32 | DB 프로브: 커넥션 1개 재사용, 주기적 풀 점검(실제 PostgreSQL) |
| 222 | `internal/probe` | `TestDockerProbe` | 통과 | 0.12 | Docker 프로브(모의 API) |
| 223 | `internal/probe` | `TestHTTPProbeAndJSONPath` | 통과 | 0.25 | HTTP 프로브와 JSON 경로 |
| 224 | `internal/probe` | `TestHTTPTimeoutsCounted` | 통과 | 0.29 | HTTP 타임아웃 집계 |
| 225 | `internal/probe` | `TestHostParse` | 통과 | 0.00 | /proc 파싱 |
| 226 | `internal/probe` | `TestHostProbeOverRunner` | 통과 | 0.07 | host 프로브(모의 SSH 실행기) |
| 227 | `internal/probe` | `TestLogRemoteGrep` | 통과 | 3.00 | `remote_grep` 원격 필터 명령 |
| 228 | `internal/probe` | `TestParseAccessLine` | 통과 | 0.00 | 액세스 로그 줄 파싱 |
| 229 | `internal/probe` | `TestTemplateDataInURL` | 통과 | 0.00 | URL 템플릿 데이터 |
| 230 | `internal/rules` | `TestBaselineIncrease` | 통과 | 0.00 | 기준선 대비 증가율 규칙 |
| 231 | `internal/rules` | `TestCaptureAndControlBaseline` | 통과 | 0.00 | 기준선 측정과 대조군 |
| 232 | `internal/rules` | `TestCompositeAnyRule` | 통과 | 0.00 | any 복합 규칙 |
| 233 | `internal/rules` | `TestConsecutiveAndHysteresis` | 통과 | 0.00 | 연속 위반(`for`)과 히스테리시스 |
| 234 | `internal/rules` | `TestErrorRatioWeighted` | 통과 | 0.00 | 가중 오류율 |
| 235 | `internal/rules` | `TestNodeYAMLShape` | 통과 | 0.00 | 규칙 YAML 형태 |
| 236 | `internal/rules` | `TestNotAndAll` | 통과 | 0.00 | not·all 조합 |
| 237 | `internal/safety` | `TestBreakerLifecycle` | 통과 | 0.02 | 서킷 상태 전이 |
| 238 | `internal/safety` | `TestBreakerWindowPrunes` | 통과 | 0.00 | 서킷 실패 기간 정리 |
| 239 | `internal/safety` | `TestDrainBatch` | 통과 | 0.00 | 드레인 배치 계산 |
| 240 | `internal/safety` | `TestGuardLockAndFlapping` | 통과 | 0.00 | 서비스 잠금과 플래핑 |
| 241 | `internal/safety` | `TestGuardStoreUnreachable` | 통과 | 1.41 | 저장소 불통 시 lease: `fail`은 `LeaseWait` 재시도 후 거부, `proceed`는 로컬 락으로 진행·통지, 복구 후 타인 보유면 충돌 보고, 비어 있으면 lease 획득 |
| 242 | `internal/safety` | `TestManualTripNeedsReset` | 통과 | 0.00 | 수동 트립은 리셋 필요 |
| 243 | `internal/secrets` | `TestEnvAndFileRefs` | 통과 | 0.01 | `env:`·`file:` 비밀 참조 |
| 244 | `internal/secrets` | `TestKubernetesAuth` | 통과 | 0.01 | Vault Kubernetes 인증(모의) |
| 245 | `internal/secrets` | `TestRedactHandler` | 통과 | 0.02 | 로그 비밀값 가림 |
| 246 | `internal/secrets` | `TestSignSSHKey` | 통과 | 0.00 | Vault SSH CA 서명 |
| 247 | `internal/secrets` | `TestVaultAppRoleKVCacheAndRelogin` | 통과 | 0.01 | Vault AppRole, KV v2, 캐시, 403 재로그인 |
| 248 | `internal/secrets` | `TestVaultErrors` | 통과 | 0.00 | Vault 오류 처리 |
| 249 | `internal/store` | `TestAppendAndLoad` | 통과 | 1.55 | 기록 추가·재생(file·postgres) |
| 250 | `internal/store` | `TestAppendAndLoad/file` | 통과 | 0.09 | file 백엔드 |
| 251 | `internal/store` | `TestAppendAndLoad/postgres` | 통과 | 1.46 | PostgreSQL 백엔드 |
| 252 | `internal/store` | `TestDowngradeIsAllOrNothing` | 통과 | 0.48 | 다운그레이드는 전부 아니면 전무(실제 PostgreSQL) |
| 253 | `internal/store` | `TestLeaseRaceHasOneWinner` | 통과 | 0.82 | 리스 경합 시 승자 1명(file·postgres) |
| 254 | `internal/store` | `TestLeaseRaceHasOneWinner/file` | 통과 | 0.01 | file 백엔드 |
| 255 | `internal/store` | `TestLeaseRaceHasOneWinner/postgres` | 통과 | 0.81 | PostgreSQL 백엔드 |
| 256 | `internal/store` | `TestLeases` | 통과 | 1.37 | 리스 획득·갱신·만료·해제(file·postgres) |
| 257 | `internal/store` | `TestLeases/file` | 통과 | 0.47 | file 백엔드 |
| 258 | `internal/store` | `TestLeases/postgres` | 통과 | 0.90 | PostgreSQL 백엔드 |
| 259 | `internal/store` | `TestLoadMigrations` | 통과 | 0.00 | 마이그레이션 파일 규칙 |
| 260 | `internal/store` | `TestMigrateUpgradeDowngradeAndGuard` | 통과 | 22.30 | 업그레이드·auto_migrate·순차 업그레이드·다운그레이드 가드(실제 PostgreSQL) |
| 261 | `internal/store` | `TestPostgresFencing` | 통과 | 0.99 | 리스를 잃은 노드의 기록 거부(실제 PostgreSQL) |
| 262 | `internal/store` | `TestSchemaTableFromOlderRelease` | 통과 | 0.45 | 이전 릴리스 스키마 테이블 업그레이드(실제 PostgreSQL) |
| 263 | `internal/sudoers` | `TestRulesAndRender` | 통과 | 0.00 | sudoers 규칙 생성·출력 |
| 264 | `internal/support` | `TestBundleRedactsAndListsFiles` | 통과 | 0.00 | 지원 번들 비밀 제거·파일 목록 |
| 265 | `internal/support` | `TestRedactText` | 통과 | 0.00 | 텍스트 비밀값 제거 |
| 266 | `internal/support` | `TestRedactYAML` | 통과 | 0.00 | 설정 YAML 비밀값 제거 |
| 267 | `internal/telemetry` | `TestDuplicateAndLabelArityPanic` | 통과 | 0.00 | 지표 중복·레이블 수 오류 |
| 268 | `internal/telemetry` | `TestExpositionFormat` | 통과 | 0.00 | Prometheus 텍스트 형식 |
| 269 | `internal/tlsconf` | `TestHAClientServerName` | 통과 | 0.05 | HA 전달: 파드 IP로 연결해도 `server.ha.tls.server_name`으로 리더 인증서 검증 |
| 270 | `internal/tlsconf` | `TestServerAndAgentTLS` | 통과 | 0.10 | 서버·에이전트 TLS(클라이언트 인증서) |
| 271 | `internal/tlsconf` | `TestServerCertificateReloads` | 통과 | 1.14 | 인증서 파일 변경 시 재시작 없이 교체 |
| 272 | `internal/tlsconf` | `TestServerConfigErrors` | 통과 | 0.00 | TLS 설정 오류 |
| 273 | `internal/transport` | `TestLocalQuotesOnWindows` | 통과 | 0.14 | Windows 로컬 명령의 따옴표 인자 보존 |
| 274 | `internal/transport` | `TestSSHCertificateWrongPrincipalRejected` | 통과 | 0.01 | 잘못된 principal의 SSH 인증서 거부 |
| 275 | `internal/transport` | `TestSSHWithVaultCertificate` | 통과 | 0.06 | Vault 서명 인증서로 SSH 접속 |
| 276 | `internal/transport` | `TestSessionBudgetDefaults` | 통과 | 0.00 | SSH 세션 예산 기본값 |
| 277 | `internal/transport` | `TestSessionBudgetKeepsRoomForRollback` | 통과 | 0.10 | 수집이 세션을 모두 써도 롤백용 예약 세션 확보 |

# 부록 B. 최소 빌드 테스트 목록 (2026-10-11 실측, 커밋 dd9a055)

`go test -tags minimal -count=1 -json ./cmd/vigilante ./internal/executor ./internal/probe ./internal/compat` 결과 59건(최상위 50, 하위 9)이다.

| 번호 | 패키지 | 테스트 | 결과 | 시간(초) | 목적 |
|---|---|---|---|---|---|
| 1 | `cmd/vigilante` | `TestFeedbackAndPilotReport` | 통과 | 0.37 | `feedback` 기록과 `pilot report` 게이트 계산·종료 코드 |
| 2 | `cmd/vigilante` | `TestLocalCLIRestricted` | 통과 | 0.07 | 인증 설정 시 로컬 `circuit trip` 거부(`--break-glass` 안내), `status` 허용, `--break-glass` 리셋은 `breakglass.circuit.reset` 감사, `local_cli: full`이면 허용 |
| 3 | `cmd/vigilante` | `TestLocalFourEyes` | 통과 | 0.00 | 로컬 승인 결정의 4-eyes: 생성자·롤백 요청자 거부, 다른 사람·`--break-glass`·`four_eyes` 꺼짐은 허용 |
| 4 | `cmd/vigilante` | `TestMinimalBuildRefusesMissingPlugins` | 통과 | 0.00 | 최소 빌드가 vSphere·ALB·gRPC를 쓰는 설정을 이름을 들어 거부 |
| 5 | `cmd/vigilante` | `TestRequireRollbackTarget` | 통과 | 0.00 | 롤백 대상 버전이 없으면 거부 |
| 6 | `cmd/vigilante` | `TestResolveInputsExistingDeploymentWins` | 통과 | 0.00 | 이미 있는 배포의 값이 자동 채움보다 우선 |
| 7 | `cmd/vigilante` | `TestResolveInputsFromCIAndJournal` | 통과 | 0.00 | CI 환경변수와 저널(마지막 성공 배포)에서 `--id`·`--version`·`--previous` 자동 채움 |
| 8 | `cmd/vigilante` | `TestResolveInputsKeepsExplicitFlags` | 통과 | 0.00 | 직접 지정한 플래그가 자동 채움보다 우선 |
| 9 | `cmd/vigilante` | `TestStoreCommand` | 통과 | 12.31 | `store status`·`migrate` CLI (실제 PostgreSQL) |
| 10 | `cmd/vigilante` | `TestSupportBundle` | 통과 | 0.06 | 지원 번들 생성과 비밀값 제거 |
| 11 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree` | 통과 | 0.01 | `watch --server` 생성 실패의 종료 코드: 게이트 거부 3, 그 밖의 실패 1 |
| 12 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/bad_request` | 통과 | 0.00 | 400 → 1 |
| 13 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/circuit` | 통과 | 0.00 | 409 `circuit_open` → 3 |
| 14 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/forbidden` | 통과 | 0.00 | 403(비admin 동결 예외) → 1 |
| 15 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/freeze` | 통과 | 0.00 | 409 `change_frozen` → 3 |
| 16 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/itsm_down` | 통과 | 0.00 | 503 `itsm_unavailable` → 3 |
| 17 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/no_ticket` | 통과 | 0.00 | 409 `change_ticket_invalid` → 3 |
| 18 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/not_leader` | 통과 | 0.00 | 503(`code` 없음, 리더 없음) → 1 |
| 19 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/older_server` | 통과 | 0.00 | `code` 없는 409(이전 서버) → 3 |
| 20 | `cmd/vigilante` | `TestWatchRemoteGateRefusalExitsThree/v2_problem` | 통과 | 0.00 | problem+json `code: change_frozen` → 3 |
| 21 | `cmd/vigilante` | `TestWatchRemoteSendsTicketAndFreezeOverride` | 통과 | 0.02 | `watch --server`가 `--ticket`(본문 `change_ticket`·`X-Change-Ticket`)과 `--freeze-override`를 전달, 플래그 없으면 보내지 않음, 서버 판정의 종료 코드 사용 |
| 22 | `internal/compat` | `TestDocMatchesMatrix` | 통과 | 0.00 | docs/09 호환성 문서와 바이너리 매트릭스 일치 |
| 23 | `internal/compat` | `TestMatrixCoversEveryPlugin` | 통과 | 0.00 | 모든 플러그인에 매트릭스 항목 |
| 24 | `internal/compat` | `TestWarningListsExperimentalPluginsInUse` | 통과 | 0.00 | 사용 중인 실험적 플러그인 경고 |
| 25 | `internal/executor` | `TestContainerCompensatesOnStartFailure` | 통과 | 0.00 | 컨테이너 시작 실패 시 옛 컨테이너 복원 |
| 26 | `internal/executor` | `TestContainerSwitch` | 통과 | 0.03 | 컨테이너 이전 이미지로 전환(모의 Docker API) |
| 27 | `internal/executor` | `TestDryRunDoesNotMutate` | 통과 | 0.00 | 드라이런은 변경하지 않음 |
| 28 | `internal/executor` | `TestEnvoyEDS` | 통과 | 0.00 | Envoy EDS 파일 DRAINING 전환 |
| 29 | `internal/executor` | `TestF5iControl` | 통과 | 0.00 | F5 iControl 드레인·복귀(모의 서버) |
| 30 | `internal/executor` | `TestHAProxyRuntimeAPI` | 통과 | 0.00 | HAProxy runtime API 명령(모의 소켓) |
| 31 | `internal/executor` | `TestKVMSnapshotRevert` | 통과 | 0.00 | KVM virsh 스냅샷 복원(모의) |
| 32 | `internal/executor` | `TestNginxController` | 통과 | 0.00 | Nginx upstream 수정·테스트·reload |
| 33 | `internal/executor` | `TestNginxDownRewrite` | 통과 | 0.00 | Nginx `down` 표시 재작성 |
| 34 | `internal/executor` | `TestNginxSudoChangesOnly` | 통과 | 0.00 | Nginx: 변경 명령만 sudo |
| 35 | `internal/executor` | `TestNutanixRestore` | 통과 | 0.00 | Nutanix Prism v2 스냅샷 복원(모의) |
| 36 | `internal/executor` | `TestOctaviaDrainEnableWaitsForActive` | 통과 | 0.02 | Octavia 드레인·복귀, ACTIVE 대기, 409 재시도 |
| 37 | `internal/executor` | `TestOctaviaDryRun` | 통과 | 0.00 | Octavia 드라이런 |
| 38 | `internal/executor` | `TestOpenStackDiagnose` | 통과 | 0.00 | OpenStack doctor 점검 |
| 39 | `internal/executor` | `TestOpenStackDryRunAndReauth` | 통과 | 0.00 | OpenStack 드라이런과 401 재인증 |
| 40 | `internal/executor` | `TestOpenStackImageSnapshotRebuild` | 통과 | 0.01 | 이미지 부팅 서버: 스냅샷·rebuild |
| 41 | `internal/executor` | `TestOpenStackPasswordAuthAndInternalInterface` | 통과 | 0.00 | Keystone 비밀번호 인증과 internal 엔드포인트 |
| 42 | `internal/executor` | `TestOpenStackRevertRefusedLeavesClearError` | 통과 | 0.01 | Cinder revert 거부 시 명확한 오류 |
| 43 | `internal/executor` | `TestOpenStackVolumeSnapshotRevertAndReplay` | 통과 | 0.02 | 볼륨 부팅 서버: 스냅샷·revert·멱등 재실행 |
| 44 | `internal/executor` | `TestRepoOf` | 통과 | 0.00 | 이미지 참조에서 저장소 추출 |
| 45 | `internal/executor` | `TestSymlinkFailurePropagates` | 통과 | 0.00 | symlink 실패 전파 |
| 46 | `internal/executor` | `TestSymlinkRollbackAndVerify` | 통과 | 0.00 | symlink 전환·재시작·확인 |
| 47 | `internal/executor` | `TestSymlinkSudoChangesOnly` | 통과 | 0.00 | symlink: 변경 명령만 하나씩 sudo |
| 48 | `internal/executor` | `TestWebhookExecutor` | 통과 | 0.00 | webhook 실행기 요청·확인 |
| 49 | `internal/probe` | `TestAccessAndAppLogLocalFollowWithRotation` | 통과 | 2.50 | 액세스·앱 로그 추적(로테이션 포함) |
| 50 | `internal/probe` | `TestCollectorRestartsFailingProbe` | 통과 | 1.50 | 실패한 프로브 재시작 |
| 51 | `internal/probe` | `TestDBProbeFullCheckCadence` | 통과 | 13.20 | DB 프로브: 커넥션 1개 재사용, 주기적 풀 점검(실제 PostgreSQL) |
| 52 | `internal/probe` | `TestDockerProbe` | 통과 | 0.12 | Docker 프로브(모의 API) |
| 53 | `internal/probe` | `TestHTTPProbeAndJSONPath` | 통과 | 0.24 | HTTP 프로브와 JSON 경로 |
| 54 | `internal/probe` | `TestHTTPTimeoutsCounted` | 통과 | 0.28 | HTTP 타임아웃 집계 |
| 55 | `internal/probe` | `TestHostParse` | 통과 | 0.00 | /proc 파싱 |
| 56 | `internal/probe` | `TestHostProbeOverRunner` | 통과 | 0.07 | host 프로브(모의 SSH 실행기) |
| 57 | `internal/probe` | `TestLogRemoteGrep` | 통과 | 3.00 | `remote_grep` 원격 필터 명령 |
| 58 | `internal/probe` | `TestParseAccessLine` | 통과 | 0.00 | 액세스 로그 줄 파싱 |
| 59 | `internal/probe` | `TestTemplateDataInURL` | 통과 | 0.00 | URL 템플릿 데이터 |
