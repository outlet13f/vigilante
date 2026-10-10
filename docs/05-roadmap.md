# 05. 로드맵 — 엔터프라이즈 제품화 (2단계)

## 진행 현황 (2026-10-10 기준)

| 마일스톤 | 상태 | 비고 |
|---|---|---|
| 1단계 프로토타입 | 완료 | 커밋 `d1e6411`. 설계는 docs/01~04 |
| **M0 기반** | **완료** | 결정 확정: PostgreSQL, OIDC + 서비스 계정 토큰, Vault. M0-1 상태 저장소·HA, M0-2 인증·권한, M0-4 감사, M0-3 비밀관리, M0-5 자체 관측성 완료. 커밋 `5df283c`(M0-1), `6e29a18`(M0-2), `dc88133`(M0-4), `31d5c34`(M0-3), `9fd9e74`(M0-5), [PR #3](https://github.com/outlet13f/vigilante/pull/3)로 master에 병합. 범위 차이는 M0 절의 "구현 결과" 참고 |
| **M1 입력 간소화** | **완료** | 커밋 `2a2ab09`(M1-1), `c6ab1d6`(M1-2), `50806e1`(M1-3), [PR #1](https://github.com/outlet13f/vigilante/pull/1)로 master에 병합(`f877c88`). 범위 차이는 M1 절 참고 |
| M3 오픈 API | **진행 중** | 결정 확정: 사내 전용, OAuth2 client credentials + API 키. **M3-1 명세·v2 공통 규약**, **M3-2 인증·스코프·호출 한도**, **M3-3 이벤트 구독** 완료 |
| M2, M4 ~ M6 | 미착수 | 일정은 모두 추정 |

선행 수정: rollback 규칙이 없는 서비스·단계를 검증에서 거부 (커밋 `3969470`).

저장소: https://github.com/outlet13f/vigilante

## Context
1단계 프로토타입은 단일 노드에서 핵심 흐름(수집 → 판정 → 롤백 → 안전장치)이 동작함을 증명했다. 대규모 조직의 운영 시스템으로 쓰기에는 다음이 부족하다.

| 영역 | 현재 한계 |
|---|---|
| 가용성 | 상태가 단일 노드의 JSONL 파일이다. 오케스트레이터가 죽으면 판정과 롤백이 멈춘다 |
| 보안 | 단일 Bearer 토큰, 비밀값은 환경변수, 에이전트 통신이 평문 HTTP일 수 있다 |
| 감사·통제 | 작업자 식별 없음, 변조 방지 없음, 변경 승인·동결 기간 연동 없음 |
| 사용성 | 서비스당 약 80줄 YAML, 배포마다 수동 인자, 권한 문제가 롤백 순간에야 드러남, 규칙이 없으면 무조건 PASS |
| 규모 | 한 프로세스가 모든 대상을 수집한다. SSH 동시 세션 한도(기본 10), 연결 단위 sudo, DB 프로브의 커넥션 생성 부담 |
| 신뢰 | 실제 F5·vCenter·Nutanix·AWS 장비 검증 없음, 패키징·서명·업그레이드 절차 없음 |

목표: 수천 대 규모의 하이브리드 환경에서 **고가용성으로 동작하고, 사내 인증·비밀관리·변경관리 체계에 연결되며, 모든 결정과 조치가 감사 가능하고, 관리자가 적은 입력으로 안전하게 운영할 수 있는 제품**으로 만든다.

## 설계 원칙
1. **롤백 경로는 의존성이 가장 적어야 한다.** 웹 콘솔, ITSM, SSO, Vault 같은 외부 연동이 장애여도 판정과 롤백은 계속 동작한다. 외부 연동 실패는 HOLD나 알림으로 처리하고 롤백을 막지 않는다. 예외는 명시적으로 승인 게이트를 설정한 경우다.
2. **플러그형 백엔드.** 상태 저장소, 인증, 비밀관리, ITSM, 알림은 인터페이스로 두고 개발용 기본 구현(file, 토큰, env)을 유지한다. 단일 바이너리 데모는 계속 동작해야 한다.
3. **선언형 설정을 코드로 관리(GitOps).** 설정의 원본은 Git이고, UI 편집은 PR 생성 또는 검증된 기록으로 남긴다.
4. **기존 1단계 인터페이스는 하위 호환을 유지한다.** CLI 종료 코드, REST v1, 설정 v1은 변경 시 v2를 신설하고 마이그레이션 도구를 제공한다.

## 비기능 목표 (제안값, 확정 필요)
| 항목 | 목표 |
|---|---|
| 규모 | 클러스터당 대상 2,000대, 동시 관측 배포 100건, 대상당 프로브 10개 |
| 판정 지연 | 위반 발생부터 롤백 시작까지 `eval_interval × for + 5초` 이내 |
| 제어 평면 가용성 | 99.9%, 리더 장애 시 30초 이내 인계(RTO), 결정 기록 유실 0(RPO 0, 동기 커밋) |
| 감사 | 모든 결정·조치에 작업자와 근거 기록, 변조 검출 가능, 보존 기간 설정(기본 1년) |
| 보안 | 모든 내부 통신 TLS 1.2 이상(에이전트 mTLS), 비밀값 디스크 평문 저장 금지, 최소 권한 |

## 마일스톤 개요
| 마일스톤 | 내용 | 추정(1인 기준, 실측 아님) |
|---|---|---|
| **M0 기반** | 상태 저장소·HA, 인증·RBAC, 비밀관리, 감사, 자체 관측성 | 5~6주 |
| **M1 입력 간소화** | 배포 입력 자동화, 프리셋, doctor (기존 P1) | 1.5~2주 |
| **M2 온보딩·거버넌스** | 설정 분리·멀티테넌시, GitOps·정책, init·discover·인벤토리·suggest (기존 P2 확장) | 4~5주 |
| **M3 오픈 API 플랫폼** | OpenAPI 3.1 명세 우선 설계, v2 공통 규약(멱등 키·오류·페이지), OAuth2·스코프·레이트 리밋, 이벤트 구독 웹훅, 외부 메트릭·실행기 API, SDK 4종, 개발자 문서·샌드박스 | 4~5주 |
| **M4 운영 콘솔·변경관리** | 웹 콘솔, 승인 워크플로우, 변경 동결, ITSM·알림 연동 (기존 P3 확장) | 5~6주 |
| **M5 규모·신뢰성** | 수집 샤딩, 에이전트 관리, SSH·sudo 개선, 부하·카오스 테스트 | 4~5주 |
| **M6 출시·지원** | 패키징·서명·SBOM, 업그레이드, 실장비 검증, 문서·지원 도구 | 3~4주 |

합계 약 7개월(1인)이며, 3인 팀이면 M0 이후 나머지를 병렬로 진행해 약 3~3.5개월이다.

의존 관계:
- **M0(상태 저장소·인증)이 M3·M4·M5의 선행**이다. 이벤트 순번, 멱등 키 보관, OAuth2 클라이언트가 M0 위에 올라간다.
- **M3 OpenAPI 명세가 M4 콘솔과 CLI 원격 모드의 선행**이다. 콘솔과 CLI는 비공개 엔드포인트 없이 공개 API만 쓴다(API-first). 명세 초안(M3-1)은 M0와 병행해 먼저 확정한다.
- **M1 프리셋이 M2 init·discover·suggest의 선행**이다.
- **M2 설정 분리가 M4 설정 편집의 선행**이다.

## M0. 기반 (HA·보안·감사)

### M0-1. 상태 저장소 추상화 + HA
- 신규 `internal/store`: `StateStore` 인터페이스를 둔다.
  - 저널 이벤트 추가: append-only, 순번 부여.
  - 배포 스냅샷 조회·갱신: 낙관적 잠금(version 필드).
  - 서킷 상태, 롤백 단계 진행, 서비스 락 lease, 리더 lease.
- 백엔드 두 가지:
  - `file`: 현재 JSONL. 개발·단일 노드용으로 유지한다.
  - `postgres`: 운영용. pgx는 이미 의존성에 있다. 테이블은 `events`(append-only), `deployments`, `leases`, `circuit`이고, 스키마는 `internal/store/migrations`의 번호 매긴 SQL로 관리한다.
- 리더 선출:
  - PostgreSQL lease 행(`leader`, TTL 15초, 5초마다 갱신)으로 단일 활성 리더를 정한다.
  - 리더만 판정·롤백을 실행한다. 팔로워는 API 조회와 에이전트 수신만 처리하고, 변경 요청은 리더로 전달한다.
  - 리더 교체 시 새 리더가 `Resume()`을 실행한다. 이미 구현된 멱등 단계 재개를 쓴다.
- 서비스 락을 파일 락에서 저장소 lease로 바꾼다. 여러 서버와 여러 CI 잡이 같은 저장소를 쓰므로 노드 간 배타성이 생긴다.
- 기존 `internal/journal`은 `store/file`로 옮기고, `orchestrator`는 `StateStore`만 의존하게 바꾼다.

### M0-2. 인증·권한(RBAC)
- 신규 `internal/auth`. 인증 제공자:
  - OIDC: Keycloak, Azure AD, Okta 등. 사용자 로그인과 그룹 클레임에 쓴다.
  - LDAP/AD: OIDC가 없는 경우의 대안.
  - 서비스 계정 토큰: CI·에이전트용. 해시로 저장하고 만료와 폐기를 지원한다.
- 역할 4종을 정의하고, 권한 범위를 팀 또는 서비스 단위로 지정한다.

  | 역할 | 권한 |
  |---|---|
  | viewer | 조회만 |
  | deployer | 배포 생성, watch, 중단 |
  | operator | deployer 권한 + 수동 롤백, 승인 |
  | admin | operator 권한 + 서킷 리셋·차단, 설정·정책 변경, 사용자·토큰 관리 |

- 모든 API 핸들러는 `authorize(actor, action, scope)`를 거친다. 역할 매핑은 설정으로 지정한다(`auth.role_bindings: [{group: sre-prod, role: operator, scope: "team=payments"}]`).
- **4-eyes 원칙(선택):** 승인이 필요한 조치는 요청자와 승인자가 달라야 한다.

### M0-3. 비밀관리
- 신규 `internal/secrets`. `Provider` 인터페이스:
  - `env`: 현재 방식.
  - `file`: K8s Secret 마운트 등.
  - `vault`: HashiCorp Vault KV v2. AppRole 또는 K8s auth로 인증한다.
  - `cyberark`: CyberArk CCP REST.
- 설정에서는 `password_env: X` 대신 `password: {secret: "vault://kv/prod/f5#password"}`로 지정한다. 기존 `*_env` 형식도 계속 지원한다.
- **SSH 인증서 인증:** Vault SSH CA로 단기 인증서를 발급받아 접속하도록 지원한다. 장기 개인키 배포를 없앨 수 있다.
- 비밀값은 메모리에만 두고 TTL 캐시를 쓴다. 로그, 저널, support bundle에서는 자동으로 가린다.

> **구현 결과(M0-3):** 별도 `password: {secret: ...}` 객체 대신 기존 키 옆에 `*_ref`(`password_ref`, `token_ref`, `private_key_ref`, `dsn_ref` 등)를 두었다. 형식은 `vault:<mount>/<path>#<key>`, `env:NAME`, `file:/path`이고 `*_env`보다 우선한다. Vault는 token·AppRole·Kubernetes 로그인, 네임스페이스, 사설 CA, 403 시 재로그인을 지원한다. `ssh_ca`는 메모리의 일회용 ed25519 키를 Vault SSH CA로 서명받아 접속하고, 수명의 80%가 지나면 다시 발급한다. 해석한 값은 로그에서 `[REDACTED]`로 가린다. `doctor`가 모든 참조를 실제로 해석하고 SSH CA 서명 권한을 점검한다. CyberArk는 수요 확인 후 같은 참조 형식(`cyberark:`)으로 추가한다.

### M0-4. 감사·컴플라이언스
- 저널 이벤트에 `actor`, `source`(cli/api/ui/agent/system), `reason`, `ticket`을 추가한다.
- **해시 체인:** 각 이벤트가 직전 이벤트 해시를 포함하게 해 변조를 검출한다. `vigilante audit verify`로 체인을 검증한다.
- SIEM 내보내기: syslog RFC5424 또는 CEF, 그리고 선택적으로 Kafka. 이벤트 유형별 필터를 둔다.
- 보존 정책: 기간이 지난 이벤트는 아카이브 파일(서명 포함)로 옮긴 뒤 삭제한다.
- 감사 조회: `GET /v1/audit?from&to&actor&service`, CSV 내보내기.

### M0-5. 자체 관측성
- `/metrics`를 Prometheus 텍스트 형식으로 노출한다. 외부 스택에 의존하지 않고 내보내기만 한다.
  - 지표: 판정 지연, 프로브 오류율, 수집 샘플 수, 롤백 성공·실패, 서킷 상태, 리더 여부, 저장소 지연, SSH 세션 수.
- 로그는 JSON 구조화 형식을 선택할 수 있게 하고, `deployment_id`, `trace_id`를 포함한다.
- `/healthz`(생존)와 `/readyz`(저장소 연결, 리더 여부)를 분리한다. OpenTelemetry 트레이스는 선택 사항이다.

> **구현 결과(M0-5):** 클라이언트 라이브러리 없이 Prometheus 텍스트 형식을 직접 출력한다(의존성 0). 지표 목록은 docs/02 "자체 관측성". 판정 지연은 "실패 판정 → 롤백 시작 기록"(`vigilante_rollback_trigger_seconds`)과 평가 1회 시간(`vigilante_evaluation_seconds`)으로 나눠 잰다. 위반이 `for` 횟수를 채우기까지의 시간은 규칙 설정이 정하므로 따로 재지 않는다. `/metrics`는 기본으로 `viewer@*` 토큰이 필요하다(`server.metrics_public`으로 해제). 로그는 `VIGILANTE_LOG_FORMAT=json`, 요청 로그에 `request_id`(`X-Request-ID` 또는 `traceparent`)를 남긴다. 기존 로그 키 `deployment`는 그대로 두었다. OpenTelemetry 트레이스 내보내기는 M5(수집 샤딩, 노드 간 gRPC)와 함께 검토한다.

## M1. 입력 간소화 — 완료

**구현 현황.** 아래 계획 중 실제로 구현한 것과 남은 것입니다.

| 항목 | 구현됨 | 남음 (다른 마일스톤과 함께) |
|---|---|---|
| M1-1 입력 자동 채우기 | CI 환경변수에서 ID·버전, 저널에서 이전 버전, `mark-good`, 출처 출력·기록, 롤백 대상 미상 시 관측 전 거부, REST API 동일 동작 | `mark-good` 권한 제한(M0 RBAC) |
| M1-2 규칙 프리셋 | 내장 4종, `preset_dirs` 조직 프리셋, `name@version` 고정, 항목별 병합, `presets` / `presets show` | 사내 프리셋 저장소 배포 방식(M2 GitOps) |
| M1-3 doctor | 자격증명(Vault 참조·SSH CA 서명 권한 포함)·접속·sudo·프로브·로그 형식·실행기·LB 풀·용량 점검, 조치 힌트, `--json`·`--junit` | 서버 정기 실행·지표(M4·M0-5), 실제 sshd `MaxSessions` 조회(현재는 기본값 10과 비교) |

### 원래 계획

### M1-1. 배포 입력 자동 채우기
- 신규 `internal/cienv`:
  - `--id`: Jenkins `BUILD_TAG`, GitLab `CI_PIPELINE_ID`, GitHub `GITHUB_RUN_ID`·`ATTEMPT`, `VIGILANTE_DEPLOYMENT_ID` 순서로 찾는다.
  - `--version`: `VIGILANTE_VERSION`, `CI_COMMIT_TAG`, 커밋 SHA, `git describe` 순서로 찾는다.
- `--previous`: 상태 저장소의 서비스별 "마지막 성공 버전"을 쓴다. 처음 쓸 때는 `vigilante mark-good`으로 등록한다. 등록은 deployer 권한이 필요하고 감사 기록에 남는다.
- 자동으로 채운 값과 그 출처를 출력하고 감사 기록에도 남긴다.

### M1-2. 규칙 프리셋
- 내장 프리셋 4종(`java-web`, `container-api`, `static-web`, `worker`)을 `go:embed`로 둔다. `overrides`로 params만 바꿀 수 있다.
- **조직 프리셋 저장소:** 사내에서 표준 프리셋을 버전 관리하며 배포한다. 서비스는 `preset: corp/java-web@2`처럼 버전을 고정하고, 프리셋을 바꿔도 기존 서비스의 판정 기준이 몰래 바뀌지 않게 한다.
- 검증 추가: 서비스마다 rollback 규칙이 1개 이상 있어야 한다(무조건 PASS 문제 해결). 기존 테스트 설정도 보강한다.
- CLI: `vigilante presets`, `vigilante presets show NAME [--set k=v]`.

### M1-3. `vigilante doctor`
- 신규 `internal/doctor`. 점검 대상과 조치 힌트는 기존 계획과 같다.
  - SSH·bastion·호스트키·sudo
  - 릴리스 경로·이전 릴리스
  - 컨테이너·이전 이미지
  - 하이퍼바이저 로그인·VM·스냅샷 권한
  - LB 풀에 대상이 있는지, 쓰기 권한
  - 로그 형식 파싱률
- 엔터프라이즈 보강:
  - **비밀값 조회 점검:** Vault 경로 접근 가능 여부.
  - **SSH 세션 여유:** 대상별 필요한 동시 세션 수와 sshd `MaxSessions` 비교.
  - **DB 프로브 부담 계산:** 초당 새 커넥션 수 경고.
  - **정기 실행 모드:** 서버가 주기적으로 doctor를 돌려 결과를 콘솔과 지표로 노출한다. 권한 만료나 인증서 만료를 롤백 전에 알 수 있다.
- 출력 형식: 사람용 표, `--json`, JUnit XML(CI 리포트용).

## M2. 온보딩·거버넌스 (기존 P2 확장)

### M2-1. 설정 분리 + 멀티테넌시
- `include`로 `infra.yaml`(플랫폼팀)과 `teams/<team>/services/*.yaml`(앱팀)을 나눈다.
- **팀 단위 네임스페이스:**
  - 서비스는 `team` 속성을 가진다. RBAC 범위, 알림 대상, 정책 적용 범위가 팀 단위로 결정된다.
  - 팀은 자기 서비스만 정의할 수 있고, 공용 대상·LB·자격증명은 플랫폼팀 파일에서 참조만 한다. 참조 허용 목록은 `allowed_teams`로 지정한다.

### M2-2. GitOps + 정책(policy-as-code)
- 설정의 원본은 Git 저장소다. 서버는 지정 브랜치를 주기적으로 pull하거나 웹훅을 받아 반영하고, 반영한 커밋 해시를 감사 기록에 남긴다.
- PR 검증용 `vigilante validate --policy policy.yaml`. 내장 정책 엔진으로 YAML 규칙을 검사한다. 예:
  - prod 서비스는 canary `observation_window` 10분 이상.
  - 전략 D(스냅샷 복원)는 `require_approval: true` 필수.
  - prod는 `dry_run` 금지.
  - 모든 서비스에 doctor 통과 기록 필요.
- 정책이 커지면 OPA/Rego 통합을 검토한다. 바이너리 크기와 의존성 부담을 고려해 처음에는 내장 엔진으로 한다.

### M2-3. `init`·`discover`·인벤토리·`suggest`
- `vigilante init`:
  - 대화형 마법사로 팀 디렉토리 구조에 맞춰 파일을 생성하고 validate·policy·doctor를 자동 실행한다.
  - `--answers`로 비대화형 실행도 지원한다.
- `vigilante discover`: SSH 1회 왕복으로 다음을 찾아 프리셋을 포함한 초안을 만든다. 확신이 낮은 값은 `TODO`로 표시한다.
  - systemd 유닛, `current` symlink
  - 컨테이너, 리스닝 포트, nginx upstream, 로그 경로
- `vigilante inventory import`:
  - 원천: Ansible, vCenter(폴더·태그), AWS·Azure 태그, **CMDB**(ServiceNow CMDB REST).
  - 가져온 결과는 정적 파일로 남겨 PR로 리뷰한다.
  - 서비스 대상은 `selector: role=order,env=prod`로 지정할 수 있게 한다.
- `vigilante suggest`:
  - 1분 롤업만 누적해 장시간 관측에도 메모리를 일정하게 유지한다.
  - 임계치를 추천하고 근거도 출력한다. 결과는 `overrides` 형식으로 PR에 바로 쓸 수 있다.

## M3. 오픈 API 플랫폼
목표는 사내 배포 콘솔, 개발자 포털, 파트너·고객 시스템 같은 외부 시스템이 Vigilante를 프로그램으로 안전하게 통합하게 하는 것이다. CLI, 웹 콘솔, 에이전트도 같은 공개 API만 쓴다(API-first). 1단계의 REST v1은 실제 구현을 문서화하는 데 그치고, 공개 규약은 v2로 신설한다.

### M3-1. API 설계 기준 (OpenAPI 3.1 명세가 원본)
- `api/openapi.yaml`을 원본(spec-first)으로 둔다. 서버의 요청 검증·라우팅 골격은 명세에서 생성하고(oapi-codegen), 생성하지 않는 부분은 계약 테스트로 명세와 구현이 일치하는지 강제한다.
- 리소스: `deployments`, `observations`(단계 관측), `rollbacks`, `approvals`, `services`, `targets`, `presets`, `policies`, `circuit`, `audit-events`, `agents`, `webhooks`(구독), `api-clients`, `operations`(비동기 작업).
- 공통 규약:
  - **버전:** URL에 메이저 버전(`/v2`). 마이너는 하위호환 추가만 한다. 폐기는 `Deprecation`·`Sunset` 헤더로 알리고 최소 6개월 유지한다.
  - **오류:** RFC 9457 `application/problem+json`(type, title, detail, 기계용 `code`, `trace_id`).
  - **페이지:** 커서 기반(`limit`, `cursor`). 필터와 정렬 문법은 전 리소스에 같게 적용한다.
  - **멱등 키:** 모든 변경 요청에 `Idempotency-Key` 헤더를 지원한다. CI 재시도로 롤백이 두 번 시작되는 사고를 막는다. 키와 응답은 상태 저장소에 24시간 보관한다.
  - **동시 수정 방지:** 설정·서비스 수정에 `ETag`/`If-Match`.
  - **비동기 작업:** 오래 걸리는 작업(관측, 롤백, baseline)은 `202` + `Location: /v2/operations/{id}`로 응답하고, 폴링이나 이벤트로 완료를 알린다. v1의 `?wait=true` 롱폴링은 호환용으로만 유지한다.
  - 시각은 RFC 3339 UTC, ID는 형식에 의미를 두지 않는 문자열.
- 1단계 v1 엔드포인트(배포 생성, 단계 시작, 롤백, 승인, 서킷, 샘플 수신, 수신 웹훅)도 명세에 기록해 v2와 함께 문서화한다.

> **구현 결과(M3-1):** `api/openapi.yaml`(OpenAPI 3.1)과 v2 리소스 22개(배포·관측·롤백·승인·작업·서비스·대상·프리셋·서킷·감사·마지막 정상 버전)를 구현했다. 오류는 problem+json(코드는 docs/06-api.md), 커서 페이지, `Idempotency-Key`(호출자별, 응답을 상태 저장소에 먼저 기록한 뒤 전송, 24시간), 배포 `ETag`/`If-Match`, 202 + `Operation`. 작업과 멱등 기록은 상태 저장소에 남아 리더가 바뀌어도 유지된다. 코드 생성(oapi-codegen) 대신 손으로 쓴 핸들러를 계약 테스트로 묶었다: 테스트의 모든 v2 요청·응답을 명세로 검증하고(libopenapi-validator, 테스트 전용), 명세의 연산과 서버 라우트 목록이 정확히 같은지 확인한다. `policies`·`agents`·`webhooks`·`api-clients`는 해당 마일스톤(M2·M5·M3-3·M3-2)에서 명세에 추가한다.

### M3-2. 인증·권한·보호
- 인증 방식:
  - 서버 간 통합: **OAuth 2.0 client credentials**.
  - 사용자 위임: OIDC 액세스 토큰(M0-2 재사용).
  - 단순 연동: API 키(해시 저장, 만료·회전).
- 스코프를 두고 M0-2의 팀·서비스 범위와 결합한다: `deployments:read`, `deployments:write`, `rollbacks:execute`, `approvals:write`, `config:write`, `circuit:admin`, `audit:read`, `metrics:write`.
- **레이트 리밋·쿼터:**
  - 클라이언트별 토큰 버킷(초당·일일). 초과하면 `429` + `Retry-After`를 돌려주고, `RateLimit-*` 헤더를 붙인다.
  - **롤백·승인·서킷 같은 비상 조치 엔드포인트는 별도 버킷**을 쓴다. 조회 요청이 폭주해도 비상 조치는 막히지 않는다.
- API 클라이언트 관리: 발급, 스코프 변경, 비밀 회전, 폐기, 마지막 사용 시각을 API와 콘솔에서 제공한다. 모든 호출은 클라이언트 ID와 함께 감사 기록(M0-4)에 남긴다.
- API 게이트웨이(Kong, Apigee, AWS API Gateway, 사내 APIM) 뒤에 둘 수 있게 한다: 신뢰 프록시 헤더 처리, 게이트웨이에서 mTLS를 끝내는 구성 지원.

> **구현 결과(M3-2):** OAuth 2.0 client credentials(`POST /v2/oauth/token`, RFC 6749 오류 형식)와 API 키를 `/v2/api-clients`로 등록·변경·회전·폐기한다. 클라이언트와 토큰은 상태 저장소에 SHA-256으로만 남고, 토큰은 불투명 문자열(`vat_…`)이다. 스코프 8종이 역할 grant와 함께 판단되며, 클라이언트 스코프를 줄이면 발급된 토큰에도 즉시 적용된다. 호출 한도는 호출자별 토큰 버킷(초당·일일)이고, 롤백·승인·중단·서킷은 별도 버킷이다. 게이트웨이 뒤 배치(신뢰 프록시 헤더, 게이트웨이 mTLS 종료)는 수요가 생기면 추가한다.

### M3-3. 이벤트 구독 (아웃바운드 웹훅·스트림)
- 이벤트 카탈로그를 CloudEvents 1.0 형식으로 정의하고, OpenAPI `webhooks` 섹션에 스키마를 둔다.
  - `deployment.created`, `observation.started`, `observation.passed`, `observation.failed`, `observation.held`
  - `rollback.started`, `rollback.completed`, `rollback.failed`
  - `approval.requested`, `approval.decided`
  - `circuit.opened`, `circuit.closed`, `agent.lost`, `doctor.failed`
- 구독 API `POST /v2/webhooks`: URL, 이벤트 필터, 서비스·팀 범위를 지정한다.
  - HMAC-SHA256 서명에 타임스탬프를 포함해 재전송 공격을 막는다.
  - 지수 백오프로 재시도하고, 실패가 누적되면 구독을 자동 비활성화하고 알린다.
  - 전달 이력 조회와 수동 재전송 API, 최종 실패 보관함(DLQ)을 제공한다.
- 스트림 `GET /v2/events`(SSE): 콘솔도 같은 스트림을 쓴다. `Last-Event-ID`로 재연결하면 누락 없이 이어 받는다. 이벤트 순번은 M0-1 저장소의 순번을 쓴다.
- 기존 수신 웹훅(GitHub, GitLab, Jenkins)은 유지하고 명세에 포함한다.

> **구현 결과(M3-3):** 엔진이 기록하는 항목(배포 상태 변화, 서킷, 승인)에서 CloudEvents 1.0 이벤트 18종을 만든다. 이벤트마다 클러스터 전체 순번을 붙여 상태 저장소에 남기므로, 리더가 바뀌어도 순번과 웹훅 진행 위치가 이어진다. `GET /v2/events`(SSE)는 `Last-Event-ID`로 끊긴 구간을 다시 받는다(최근 10,000건). 웹훅은 구독별로 순서를 지켜 최소 한 번 전달하고, Standard Webhooks 형식으로 서명한다. 서명 비밀은 서버 마스터 키(`api.webhook_signing_key_ref`)에서 구독별로 파생해 어디에도 저장하지 않는다. 재시도는 1초~30분 6회, 최종 실패는 dead-letter 목록, 연속 5건 실패 시 자동 비활성화와 운영 알림. 재전송·테스트 이벤트·전달 이력 API를 둔다. 계획의 `doctor.failed`는 서버 정기 doctor(M4)와 함께 추가한다.

### M3-4. 확장 API (외부 수집·외부 실행)
- **외부 지표 수신 `POST /v2/metrics`:**
  - 사내에 이미 있는 모니터링·APM 값을 규칙에 쓸 수 있게 한다.
  - 배치 전송, 대상·메트릭 이름 검증, 출처 태그를 지원한다.
  - 에이전트 push와 같은 경로를 쓰며, 관측 쿼럼에서 독립 관측점으로 취급할 수 있다.
- **외부 실행기 프로토콜(`type: remote`):**
  - 기존 webhook 실행기를 정식 계약으로 만든다.
  - Vigilante가 표준 요청(대상, 목표 버전, 체크포인트, 멱등 키)을 보내면, 사내 배포 콘솔이 롤백을 수행하고 결과를 비동기 콜백(`POST /v2/operations/{id}/result`)으로 보고한다.
  - 콜백 제한 시간을 넘기면 실패로 보고 에스컬레이션한다.
- **외부 판정 훅(선택):** 단계 종료 직전에 외부 시스템에 승인 여부를 묻는다. 예를 들어 주문 건수 같은 비즈니스 지표를 확인한다. 응답이 없으면 정책에 따라 HOLD나 PASS로 처리하고, 롤백 결정은 막지 않는다.

### M3-5. SDK·개발자 지원
- 명세에서 SDK를 생성한다: **Go, Python, Java, TypeScript**.
  - 멱등 키 자동 생성, 재시도, 페이지 순회, 웹훅 서명 검증 도우미를 포함한다.
  - 사내 패키지 저장소(Nexus, Artifactory)에 배포한다.
- CLI의 `--server` 모드를 Go SDK 기반으로 바꾼다. CLI 자체가 SDK 사용 예가 된다.
- 개발자 문서:
  - 서버에 Redoc을 정적으로 내장해 `/docs`로 제공한다(폐쇄망 대응).
  - 시작 가이드, 인증 가이드, 이벤트 레퍼런스.
  - 연동 예제: Jenkins 공유 라이브러리, GitHub Action, GitLab CI 컴포넌트. Terraform provider는 후속 검토.
- **샌드박스:** `vigilante server --sandbox`. fakeapp과 mock 실행기를 내장해 실제 인프라 없이 API 통합을 시험할 수 있다.

### M3-6. 품질 보증
- 명세 린트(Spectral)와 **하위호환 깨짐 자동 검출**(oasdiff)을 CI 차단 조건으로 둔다. v2에 호환을 깨는 변경이 있으면 PR을 막는다.
- 계약 테스트: 모든 엔드포인트의 정상·오류 응답이 명세와 일치하는지 검증한다.
- SDK 통합 테스트: 언어별 SDK로 샌드박스에 대해 배포 → 실패 → 롤백 → 이벤트 수신 시나리오를 실행한다.
- 보안 시험: 스코프 우회, 다른 팀 리소스 접근, 멱등 키 재사용·충돌, 웹훅 서명 위조·재전송, 레이트 리밋 우회.

## M4. 운영 콘솔·변경관리 (기존 P3 확장)

### M4-1. 웹 콘솔
- `go:embed` 정적 UI로 만든다. 외부 CDN 없이 폐쇄망에서도 동작한다. **데이터는 M3 공개 API(v2)로만 가져온다.**
- 로그인: OIDC로 로그인하고, 서버 세션은 HttpOnly 쿠키와 CSRF 토큰으로 보호한다.
- 화면:
  - 전체 현황: 진행 중 배포, 서킷 상태, 최근 롤백, doctor 경고.
  - 배포 상세: 단계 타임라인, 위반 규칙과 그 시점의 지표 그래프, 롤백 단계 로그.
  - 서비스·대상 목록: 프리셋 버전, doctor 결과, 최근 배포 이력.
  - 감사 조회.
- 조작 버튼(역할에 따라 노출): 수동 롤백, 승인·거절, 중단, 서킷 리셋·차단. 모든 조작은 사유 입력이 필수다.
- 설정 편집:
  - 프리셋 선택과 params 입력 폼을 제공하고, 결과는 **Git PR 생성**(GitOps 모드) 또는 검증 후 저장(단독 모드)으로 처리한다.
  - 직접 수정은 admin만 할 수 있다.
- 실시간 갱신: M3-3의 SSE `GET /v2/events`.

### M4-2. 승인 워크플로우·변경 동결
- 승인 정책을 선언한다. 예:
  - prod의 전략 D는 operator 2인 승인.
  - 업무 시간 외 자동 롤백은 허용하되 사후 승인 기록을 남긴다.
- 승인 요청은 콘솔·Slack/Teams 버튼·API로 처리하고, 만료 시간을 둔다. 만료되면 HOLD를 유지하고 상위 호출한다.
- **변경 동결 기간(change freeze):**
  - 달력 설정 또는 ITSM에서 동기화한다.
  - 동결 중에는 새 배포 게이트를 닫는다.
  - **자동 롤백은 허용**한다. 장애 복구는 동결 대상이 아니다. 단, 정책으로 바꿀 수 있다.

### M4-3. ITSM·알림 연동
- ITSM 인터페이스: ServiceNow, Jira Service Management, 일반 웹훅.
  - **배포 게이트:** 유효한 변경 티켓(승인 상태, 작업 시간대 일치)이 없으면 `watch` 시작을 거부한다. 정책으로 켜고 끈다.
  - **사고 자동 생성:** `ROLLBACK_FAILED`나 서킷 OPEN이면 인시던트를 만들고, 롤백 완료 시 변경 티켓에 결과를 기록한다.
  - ITSM 장애 시 게이트 동작은 정책으로 정한다(fail-open 또는 fail-closed). 롤백 자체는 항상 진행한다.
- 알림 채널 확장: Slack, MS Teams, 이메일(SMTP), PagerDuty, Opsgenie.
  - 팀별 라우팅과 심각도별 채널을 지원한다.
  - 중복 억제: 같은 배포의 같은 이벤트는 한 번만 보낸다.

## M5. 규모·신뢰성

### M5-1. 수집 샤딩
- 리더가 대상을 **수집 워커 노드**에 일관 해싱으로 배정한다. 팔로워 노드도 수집 워커로 쓴다.
- 워커는 샘플을 리더로 스트리밍한다. 내부 gRPC 스트림이며 mTLS를 쓴다.
- 워커가 장애나면 30초 안에 대상을 재배정한다.
- 판정은 리더가 한다. 수집량이 많으면 워커가 1초 롤업을 먼저 계산해 보낸다.

### M5-2. 에이전트 관리
- 등록 절차:
  - 1회용 등록 토큰으로 가입하고 mTLS 클라이언트 인증서를 발급받는다.
  - 인증서는 자동으로 갱신하고, 서버에서 폐기할 수 있다.
- 콘솔에서 에이전트 상태를 본다: 버전, 마지막 하트비트, 수집 상태.
- 원격 업그레이드는 서명 검증 후 교체하고, 실패 시 이전 바이너리로 되돌린다.
- 패키지: rpm, deb, Windows 서비스, systemd 유닛 파일.

### M5-3. SSH·대상 부하 개선 (1단계 프로토타입에서 발견한 문제)
- **동시 세션 관리:** 대상별 세션 세마포어를 두고 한도는 기본 8로 설정한다. 롤백 명령용 세션 2개를 따로 예약해, 수집이 세션을 다 써도 롤백이 막히지 않게 한다.
- **sudo 분리:** 읽기 명령은 sudo 없이 실행하고, 변경 명령만 sudo를 쓴다. 실행기별로 필요한 sudo 명령 목록을 문서화해 sudoers 최소 권한 예시를 제공한다.
- **원격 로그 스트리밍 부담:** 대용량 로그는 원격에서 `grep -E`로 먼저 걸러 보내는 모드, 또는 에이전트 권장 경고(doctor)로 대응한다.
- **DB 프로브:** 기본은 커넥션 재사용 모드로 바꾸고, 풀 전체 확보 점검은 주기를 따로 두어(기본 1분) 실행한다.

### M5-4. 부하·카오스 테스트
- 부하 테스트 하네스(`test/load`):
  - 가상 대상 2,000대(SSH 서버 시뮬레이터 + HTTP 목업)와 동시 배포 100건으로 판정 지연, 메모리, CPU를 측정한다.
  - CI에서 주기적으로 실행하고 결과를 기록한다.
- 카오스 시나리오:
  - 롤백 도중 리더 강제 종료 → 새 리더가 재개하는지.
  - 저장소 일시 단절 → 판정 보류, 데이터 유실 없음.
  - 워커·에이전트 네트워크 분단 → 관측자 장애로 판정해 HOLD.
  - LB API 지연·오류 → 재시도, 격리.
- **Race detector:** cgo가 있는 Linux CI 러너에서 `go test -race`를 상시 실행한다. 이번 개발 환경에서는 실행하지 못했다.

## M6. 출시·지원

### M6-1. 빌드·배포 산출물
- 배포 형태:
  - 정적 바이너리: linux amd64/arm64/ppc64le, windows.
  - rpm/deb 패키지, 컨테이너 이미지(distroless), 서버용 Helm 차트.
  - **폐쇄망 설치 번들:** 바이너리, 이미지, 차트, 문서를 묶는다.
- **공급망 보안:**
  - 바이너리·이미지 서명(cosign 또는 GPG), SBOM(CycloneDX) 생성.
  - 의존성 취약점 스캔(govulncheck)을 CI 차단 조건으로 둔다.
- 바이너리 크기: 빌드 태그로 플러그인을 분리하고, 최소 빌드와 전체 빌드를 제공한다.

### M6-2. 버전·업그레이드
- 유의적 버전(SemVer). REST·설정·CLI 종료 코드의 호환성 정책을 문서화한다.
- 상태 저장소 스키마는 자동 마이그레이션하며 되돌릴 수 있게 한다.
- `vigilante config migrate v1→v2`. 무중단 업그레이드 절차는 팔로워 → 리더 순서로 진행한다.

### M6-3. 실장비 연동 검증 (호환성 매트릭스)
- 통합 테스트 랩 구성 대상:
  - F5 BIG-IP(VE 평가판)
  - vCenter 7·8
  - Nutanix CE: Prism v2 경로를 실제로 확인하고, 필요하면 v3·v4 API로 전환한다.
  - AWS 테스트 계정(ALB·NLB)
  - HAProxy 2.x, Nginx, Envoy
  - Docker·Podman 버전별
- 검증 결과로 지원 버전 매트릭스를 공개한다.
- 미지원 영역의 우선순위는 고객 수요로 정한다: Azure LB·Application Gateway, Citrix ADC, AIX·Solaris 호스트 프로브, OpenStack(인스턴스 스냅샷·LBaaS).

### M6-4. 문서·지원 체계
- 문서: 설치 가이드(단독·HA·폐쇄망), 관리자 가이드, 프리셋·정책 작성 가이드, 운영 런북(기존 docs/04 확장), API 레퍼런스(M3 OpenAPI 명세와 개발자 포털), 보안 가이드(최소 권한 sudoers, IAM·F5·vCenter 역할 예시).
- `vigilante support-bundle`: 설정(비밀값 가림), 최근 로그, doctor 결과, 저장소 상태 요약을 하나로 묶는다.
- 교육 자료: 데모 환경을 확장한 실습 시나리오.

## 결정 필요 사항 (권장안)
| 항목 | 권장안 | 대안 |
|---|---|---|
| 상태 저장소 | PostgreSQL (이미 pgx 의존, 운영 친숙도 높음) | etcd |
| 인증 | OIDC (사내 SSO) + 서비스 계정 토큰 | LDAP/AD 직접 연동 |
| 비밀관리 | HashiCorp Vault | CyberArk, 없음(env) |
| ITSM | ServiceNow | Jira SM, 사내 시스템(웹훅) |
| 규모 목표 | 대상 2,000대 / 동시 배포 100건 | 조직 규모에 맞게 조정 |
| 규제 요건 | 감사 보존 1년, 4-eyes 승인 선택 적용 | 금융권 등 규제 대상이면 보존 기간·승인 의무 상향 |
| 오픈 API 공개 범위 | 사내 전용(사내 시스템·개발자) | 파트너 공개, 외부 공개(약관·쿼터·지원 체계 추가 필요) |
| API 인증 | OAuth 2.0 client credentials + API 키(단순 연동) | API 키만, mTLS 클라이언트 인증서 |
| SDK 우선순위 | Go(CLI 공용) → Python → Java → TypeScript | 사내 주력 언어에 맞춰 조정 |
| API 게이트웨이 | 직접 노출 + 내장 레이트 리밋 | 사내 APIM(Kong 등) 뒤 배치 |

## 검증 기준
- **마일스톤 공통:** `go vet`, `go test`, Linux CI에서 `go test -race`, `govulncheck` 통과. 신규 패키지마다 단위 테스트.
- **M0:**
  - PostgreSQL 백엔드로 기존 orchestrator 테스트 전부 통과(저장소 공통 테스트 스위트를 file·postgres 양쪽에 실행).
  - 2노드 서버에서 롤백 중 리더를 종료했을 때 새 리더가 재개해 `ROLLED_BACK`에 도달.
  - 역할별 API 허용·거부 표 테스트.
  - `vigilante audit verify`가 이벤트 1건 변조를 검출.
- **M1:** 데모를 프리셋 설정으로 바꾸고, `watch --service --phase`만으로 기존 5개 시나리오가 같은 종료 코드로 끝나는지 확인. doctor가 의도적 권한 오류를 조치 힌트와 함께 보고하는지 확인.
- **M2:** 분리 설정과 단일 설정이 같은 결과로 해석되는지, 정책 위반 PR이 validate에서 거부되는지 확인.
- **M3:**
  - Spectral 린트, oasdiff 호환성 검사, 전 엔드포인트 계약 테스트가 CI에서 통과.
  - 같은 `Idempotency-Key`로 롤백을 2번 요청해도 롤백은 1번만 실행되고, 두 응답이 같다.
  - 웹훅 수신 서버를 내렸다 올리면 재시도로 모든 이벤트가 순서대로 도착하고, 서명 검증을 통과한다.
  - SSE를 끊었다가 `Last-Event-ID`로 재연결하면 누락이 없다.
  - 조회 요청으로 레이트 리밋을 소진한 상태에서도 롤백 엔드포인트는 응답한다.
  - Python SDK로 샌드박스에 대해 배포 → 실패 → 롤백 → 이벤트 수신 E2E 통과.
- **M4:**
  - 콘솔에서 서비스 추가(PR 생성), 데모 배포, 승인 요청·승인, 롤백 흐름을 수동 점검.
  - ITSM 모의 서버로 티켓 게이트와 인시던트 생성 확인.
  - ITSM이 장애여도 롤백은 진행되는지 확인.
- **M5:** 부하 하네스로 비기능 목표(판정 지연, 메모리)를 측정해 보고. 카오스 시나리오 4종 통과.
- **M6:** 서명 검증, 폐쇄망 번들 설치, 실장비 랩 호환성 매트릭스 완성.

