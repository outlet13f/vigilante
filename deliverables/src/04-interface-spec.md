---
title: 인터페이스 정의서
doc_id: VGL-SI-04
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

본 문서는 Vigilante(Unified Rollback Orchestrator)가 외부와 주고받는 모든 인터페이스를 정의한다. 대상은 (1) 서버가 제공하는 REST API v2와 레거시 v1, (2) 이벤트 인터페이스(SSE 스트림, 아웃바운드 웹훅), (3) CI/CD 시스템이 보내는 인바운드 웹훅, (4) 명령행 인터페이스(CLI)와 종료 코드, (5) 제품이 호출하는 외부 시스템(대상 호스트, 로드밸런서, 하이퍼바이저, 클라우드, 비밀 저장소, IdP, ITSM, 알림 채널, 상태 저장소)이다.

## 1.2 기준 버전과 근거 자료

| 항목 | 내용 |
|---|---|
| 기준 소스 | 저장소 `vigilante`, master 커밋 `537870c`(PR #12 병합 `dba3dbe`, PR #13 병합 `dd9a055`, PR #14 병합 `537870c`) |
| 릴리스 상태 | 출시 전. 첫 릴리스 1.0.0은 M8 실장비 랩·파일럿 통과 후 (CHANGELOG `[Unreleased]`) |
| API 계약 원본 | `api/openapi.yaml` (OpenAPI 3.1.0, `info.version: 2.0.0`) |
| 서버 구현 | `internal/api` (v2 라우트 표 `v2Routes`, v1 라우트 `Handler`) |
| 인증·권한 | `internal/auth` |
| 이벤트·웹훅 | `internal/events`, `internal/model/event.go` |
| CLI | `cmd/vigilante/main.go` (usage 문자열과 명령 처리), `authcmd.go`(로컬 권한 제한), `watchremote.go`(`watch --server`) |
| 설정 | `internal/config/config.go`, `validate.go`(기본값·검증) |
| 지표 | `internal/telemetry/catalog.go` |
| 외부 연동 | `internal/executor`, `internal/dockerapi`, `internal/transport`, `internal/itsm`, `internal/notify`, `internal/secrets`, `internal/console`, `internal/store`, `internal/audit` |
| 참고 문서 | `docs/06-api.md`, `docs/10-security.md`, `docs/09-compatibility.md`, `docs/02-config-spec.md` |

> 명세와 서버 구현의 일치는 계약 테스트가 보장한다. `TestV2RoutesMatchSpec`는 명세의 v2 연산과 서버 라우트 목록이 정확히 같은지 확인하고, `internal/api/contract_test.go`의 테스트는 모든 v2 요청·응답을 명세로 검증한다(테스트 결과는 VGL-SI-06 참조).

## 1.3 인터페이스 식별 체계

| 접두어 | 분류 | 본 문서 절 |
|---|---|---|
| IF-API-nnn | REST API v2 및 시스템 엔드포인트 | 3, 4 |
| IF-V1-nn | REST API v1 (레거시) | 5 |
| IF-EVT-nn | 이벤트 인터페이스 (SSE, 아웃바운드 웹훅) | 6 |
| IF-INB-nn | 인바운드 웹훅 (CI/CD) | 7 |
| IF-CLI-nn | 명령행 인터페이스 | 8 |
| IF-EXT-nn | 외부 시스템 인터페이스 | 9 |
| IF-CFG-nn, IF-MET-nn | 설정 키, 운영 지표(PR #13 추가·변경분) | 10 |

## 1.4 용어

| 용어 | 정의 |
|---|---|
| 배포(Deployment) | Vigilante가 관측하고 필요 시 롤백하는 단위. 서비스·신규 버전·이전(정상) 버전으로 구성 |
| 단계(Phase) | `canary`, `rolling`, `full`. 단계별 관측 창을 가진다 |
| 판정(Verdict) | `PENDING`, `PASS`, `FAIL`, `HOLD`, `INCONCLUSIVE` |
| 작업(Operation) | 오래 걸리는 요청(관측, 롤백, 승인, 기준선)의 진행 기록. `202 Accepted`로 반환 |
| 주체(Principal) | 인증된 호출자. `user:`, `sa:`, `client:`, `token:legacy`, `webhook:` 등 접두어 ID |
| 역할 grant | `역할@범위` (예: `deployer@team=payments`). 범위는 `*`, `team=X`, `service=Y` |
| 스코프 | API 클라이언트 토큰이 가진 권한 종류 제한(`deployments:read` 등) |
| 서킷 브레이커 | 롤백 실패가 이어지면 자동 조치를 멈추는 전역 비상 정지(`CLOSED`, `OPEN`, `HALF_OPEN`) |
| 실행기(Executor) | 대상을 이전 버전으로 되돌리는 전략(symlink, container, vsphere, nutanix, kvm, openstack, exec, webhook) |
| 트래픽 제어기 | 대상을 LB 풀에서 빼고 넣는 구성요소(nginx, haproxy, envoy, f5, aws_alb, octavia) |

# 2. 인터페이스 총괄

| 분류 | 방향 | 프로토콜 | 기본 포트 | 상대 | 인증 |
|---|---|---|---|---|---|
| REST API v2 | 들어옴 | HTTP(S), JSON | 8088 | CI, 사내 시스템, 콘솔, CLI, 에이전트 | Bearer(서비스 계정·API 키·OAuth 토큰·OIDC JWT), 콘솔 세션 쿠키 |
| REST API v1 | 들어옴 | HTTP(S), JSON | 8088 | 기존 CI 연동, 에이전트, CLI 원격 모드 | Bearer |
| 이벤트 스트림 | 들어옴(구독) | SSE (`text/event-stream`) | 8088 | 콘솔, 대시보드 | Bearer |
| 아웃바운드 웹훅 | 나감 | HTTP(S) POST, CloudEvents JSON | 수신 측 | 사내 시스템(ITSM 봇, 메신저 봇) | Standard Webhooks HMAC 서명 |
| 인바운드 웹훅 | 들어옴 | HTTP(S) POST | 8088 | GitHub, GitLab, Jenkins | HMAC 서명(GitHub), 토큰(GitLab), Bearer(Jenkins) |
| 웹 콘솔 | 들어옴 | HTTP(S) | 8088 (`/console/`) | 운영자 브라우저 | OIDC 인가 코드 + PKCE 또는 API 토큰 |
| CLI | 로컬 | 프로세스 실행, 종료 코드 | - | CI 파이프라인, 운영자 | 설정 파일 또는 `VIGILANTE_TOKEN` |
| 외부 시스템 | 나감 | SSH, HTTPS REST, SOAP(vSphere), UNIX 소켓, SMTP, PostgreSQL | 22, 443, 8200, 5432, 587 등 | 9장 | 9장 |

---

# 3. REST API v2 공통 규약

## 3.1 기본 정보

| 항목 | 규약 |
|---|---|
| 기본 URL | `https://<서버>:8088` (명세 예시 `https://vigilante.example.internal:8088`). `server.listen` 기본값 `:8088` |
| 전송 보안 | `server.tls` 설정 시 서버가 직접 HTTPS 제공(TLS 1.2 이상, 선택적 클라이언트 인증서). 미설정 시 HTTP이므로 TLS 프록시·인그레스 뒤에 둔다 |
| 버전 | URL의 메이저 버전(`/v2`). 마이너 변경은 필드·리소스·선택 파라미터 추가만. 폐기 시 `Deprecation`·`Sunset` 헤더, 최소 6개월 유지 |
| 공개 범위 | 사내 전용 (docs/06-api.md) |
| 형식 | JSON, 필드명 snake_case, 시각은 RFC 3339 UTC, ID는 의미 없는 문자열 |
| HA 동작 | 어느 노드로 보내도 된다. 팔로워는 `/healthz`, `/readyz`, `/metrics`, `/console`을 제외한 모든 요청을 리더의 `ha.advertise_url`로 전달한다. 리더를 모르거나 닿지 않으면 `503` 또는 `502` + `not_leader` + `Retry-After: 2` |
| 미정의 경로 | `/v2/` 아래 명세에 없는 경로는 `404 not_found` (problem+json) |
| 확장 가능한 값 | 오류 `code`, 배포 상태, 작업 종류, 주체 종류, 스코프, 이벤트 `type`은 값이 늘어날 수 있다(`x-extensible-enum`). 모르는 값은 HTTP 상태에 맞는 일반 처리로 다룬다 |

## 3.2 인증

| 방식 | 토큰 형식 | 발급·관리 | 주 사용처 |
|---|---|---|---|
| 서비스 계정 토큰 | `vgl_` + 256비트 난수 | `vigilante token create`로 생성, 설정 `auth.service_accounts`에 SHA-256만 등록, 선택적 만료일 | CI, 에이전트 |
| API 키 | `vgk_…` | `POST /v2/api-clients` (`type: api_key`), 상태 저장소에 SHA-256만 저장 | 스크립트, SIEM 수집 |
| OAuth 2.0 client credentials | 접근 토큰 `vat_…` (기본 1시간, `api.token_ttl`), 클라이언트 비밀 `vcs_…` | `POST /v2/oauth/token` | 서버 간 연동(사내 배포 콘솔 등) |
| OIDC 토큰 | 사내 IdP가 발급한 JWT | IdP. 서버가 issuer discovery로 검증기 구성, `aud`는 `auth.oidc.audience` | 사용자 |
| 레거시 토큰 | 임의 문자열 | `server.auth_token_env` 환경변수 | 비상용(break-glass admin), 호출 한도 미적용 |
| 콘솔 세션 쿠키 | `vgl_session` (암호화·인증된 쿠키) | `/console/` 로그인 | 웹 콘솔. `Authorization` 헤더가 없을 때만 사용, 변경 요청은 `X-CSRF-Token` 필수 |

- 헤더 형식: `Authorization: Bearer <토큰>`.
- 인증 실패 시 `401` + `WWW-Authenticate: Bearer realm="vigilante"` + `code: unauthenticated`.
- 인증 설정(`auth`, `server.auth_token_env`)이 전혀 없으면 모든 호출이 익명 admin(`anonymous`)으로 처리된다(개발용, 서버가 경고). 운영에서는 반드시 인증을 켠다.

## 3.3 권한 (역할 × 스코프)

권한은 두 단계로 판단한다. 역할 grant가 어느 서비스에서 무엇을 할 수 있는지 정하고, API 클라이언트·OAuth 토큰은 스코프가 할 수 있는 일의 종류를 추가로 좁힌다. 둘 다 통과해야 허용된다. 거부된 요청은 감사 기록에 `action: denied`로 남는다.

| 역할 | 수준 | 허용 동작 (`internal/auth`의 Action) |
|---|---|---|
| viewer | 1 | 조회(read) |
| deployer | 2 | viewer + 배포 등록, 단계 관측, 중단, 기준선, 마지막 정상 버전, 판정 평가(deploy) |
| operator | 3 | deployer + 수동 롤백(rollback), 승인(approve) |
| admin | 4 | operator + 서킷 리셋·트립(circuit), API 클라이언트·동결 관리(manage) |
| agent | 별도 | 샘플 전송·하트비트(agent)만. admin도 agent 동작 가능 |

| 동작 | 최소 역할 | 필요한 스코프 |
|---|---|---|
| read | viewer | `deployments:read` |
| deploy | deployer | `deployments:write` |
| rollback | operator | `rollbacks:execute` |
| approve | operator | `approvals:write` |
| circuit | admin | `circuit:admin` |
| agent | agent | `metrics:write` |
| manage | admin | `config:write` |
| 감사 조회 | viewer@`*` | `audit:read` |

- 스코프가 없는 주체(서비스 계정, OIDC 사용자, 레거시)는 스코프 제한을 받지 않는다(`scopes: null`).
- 승인 4-eyes(`auth.four_eyes: true`): 승인자는 배포 생성자 또는 롤백 요청자와 달라야 한다. 위반 시 `403 forbidden`.

## 3.4 오류 응답 (RFC 9457)

모든 v2 오류(및 `/metrics` 오류)는 `Content-Type: application/problem+json`으로 반환한다. 예외: `POST /v2/oauth/token`은 RFC 6749 5.2 형식(`{"error", "error_description"}`)을 쓴다.

| 필드 | 형식 | 설명 |
|---|---|---|
| `type` | string (필수) | 문제 유형 URI. `docs/06-api.md#<code>` |
| `title` | string (필수) | 사람용 제목 |
| `status` | integer (필수) | HTTP 상태 |
| `code` | string (필수) | 안정적인 기계 판독 코드. 분기 기준 |
| `detail` | string | 상세 이유 |
| `request_id` | string | 문의 시 전달할 요청 ID |
| `errors[]` | `{field, message}` | 필드별 검증 오류 (`validation_failed`) |

| code | HTTP | 의미와 클라이언트 조치 |
|---|---|---|
| `bad_request` | 400 | JSON 오류, 명세에 없는 필드, 잘못된 `limit`·`cursor`. 수정 후 재전송 |
| `unauthenticated` | 401 | 토큰 없음·만료·검증 실패 |
| `forbidden` | 403 | 역할·범위 부족, 4-eyes 위반, 콘솔 세션 요청의 CSRF 토큰 누락·불일치 |
| `not_found` | 404 | 리소스 없음, 명세에 없는 경로 |
| `conflict` | 409 | 현재 상태에서 불가(이미 관측·롤백 중, 승인 대기 아님, 같은 ID의 다른 배포, 같은 이름의 활성 클라이언트) |
| `change_frozen` | 409 | 변경 동결 기간. `detail`에 동결 이름·이유·종료 시각. admin은 `freeze_override`로 진행 |
| `circuit_open` | 409 | 서킷 OPEN으로 새 단계 시작 거부. admin이 원인 조사 후 `POST /v2/circuit/reset` |
| `change_ticket_invalid` | 409 | 변경 티켓 없음·미승인·허용 상태 아님·계획 시간 밖 |
| `idempotency_key_in_flight` | 409 | 같은 키 요청이 처리 중. `Retry-After: 1` 후 재전송 |
| `precondition_failed` | 412 | `If-Match` ETag 불일치. 재조회 후 판단 |
| `validation_failed` | 422 | 형식은 맞으나 값이 유효하지 않음(없는 서비스·단계·실행기·대상, 롤백 대상 버전 없음, 빈 `reason`) |
| `idempotency_key_reused` | 422 | 같은 `Idempotency-Key`를 다른 요청(경로·본문 다름)에 사용 |
| `rate_limited` | 429 | 호출 한도 초과. `Retry-After` 후 재시도 |
| `itsm_unavailable` | 503 | ServiceNow 불통 + 변경 게이트 fail-closed. `Retry-After` 후 재시도 |
| `not_leader` | 503, 502 | HA 리더 미확정 또는 리더 불통. `Retry-After` 후 재시도 |
| `internal` | 500 | 서버 내부 오류. `request_id`와 함께 운영자에게 통보 |

## 3.5 목록 조회(커서 페이지)

| 항목 | 규약 |
|---|---|
| 응답 형식 | `{"items": [...], "next_cursor": "..." 또는 null}` |
| 다음 페이지 | `next_cursor` 값을 `cursor` 쿼리로 전달. `null`이면 끝 |
| 페이지 크기 | `limit` 1~500, 기본 50 |
| 커서 | 불투명 문자열. 해석·조작 금지 |

## 3.6 재시도와 멱등성 (Idempotency-Key)

| 항목 | 규약 |
|---|---|
| 적용 대상 | 라우트 표에서 `write`로 표시된 POST·PUT·PATCH·DELETE (4.1 표의 "멱등" 열) |
| 헤더 | `Idempotency-Key` 1~255자. 논리적 요청마다 고유값(예: UUID, `$CI_PIPELINE_ID-canary`) |
| 동작 | 같은 호출자·같은 키·같은 요청(메서드·경로·본문)이면 첫 응답을 그대로 반환하고 `Idempotent-Replayed: true`를 붙인다 |
| 기록 대상 | 성공 응답(상태 300 미만)만 기억한다. 오류 응답은 원인 수정 후 같은 키로 재시도 가능 |
| 보관 | 호출자별 24시간 (`model.IdemTTL`). 상태 저장소에 기록되어 리더 교체 후에도 유효 |
| 충돌 | 같은 키 다른 요청: `422 idempotency_key_reused`. 처리 중: `409 idempotency_key_in_flight` + `Retry-After: 1` |
| 제외 | 비밀값을 응답하는 요청(API 클라이언트 등록·비밀 회전, 웹훅 등록·비밀 회전)은 재생하지 않는다. 같은 이름의 활성 클라이언트가 있으면 `409` |

## 3.7 동시성 제어 (ETag)

| 항목 | 규약 |
|---|---|
| 발급 | 배포 조회(`GET /v2/deployments/{id}`), 배포 생성, 작업 조회 응답에 약한 ETag `W/"<SHA-256 앞 8바이트 hex>"` |
| 조건부 조회 | `If-None-Match`가 현재 ETag와 같으면 `304 Not Modified` |
| 조건부 조치 | 관측·롤백·승인 요청에 `If-Match`를 넣으면 배포가 그 사이 바뀐 경우 `412 precondition_failed` |

## 3.8 오래 걸리는 작업 (Operation)

관측, 롤백, 승인, 기준선 측정은 `202 Accepted` + `Operation` 본문 + `Location: /v2/operations/{id}`로 응답한다. 호출자는 `status`가 `running`이 아닐 때까지 `GET /v2/operations/{id}`를 조회한다.

| 필드 | 설명 |
|---|---|
| `kind` | `observation`, `rollback`, `approval`, `baseline` (확장 가능) |
| `status` | `running`, `completed`(작업이 실행됨, 롤백 실패도 결과는 `result`에), `failed`(실행 자체를 못 함, 이유는 `error`) |
| `result` | 종료 시점의 `state`, `verdict`, `exit_code`, `reason`, `baseline_samples` |

리더가 바뀌어도 작업은 상태 저장소에 남아 이어지며, 새 리더는 배포 상태를 보고 완료 여부를 판단한다.

## 3.9 호출 한도 (Rate Limit)

| 버킷 | 기본값 | 대상 | 설정 키 |
|---|---|---|---|
| default | 초당 20, 순간 40 | 모든 v2 호출 | `api.rate_limit` |
| emergency | 초당 1, 순간 10 | 롤백, 승인, 중단, 서킷 리셋·트립 | `api.emergency_rate_limit` |
| 클라이언트별 | 클라이언트의 `rate_limit` (`rate`, `burst`, `daily`) | 해당 API 클라이언트 | `POST/PATCH /v2/api-clients` |

- 호출자(사용자, 서비스 계정, API 클라이언트)마다 따로 센다. 노드 메모리에서 세지만 모든 요청이 리더로 모이므로 클러스터 기준과 같다.
- 응답 헤더 `RateLimit-Limit`, `RateLimit-Remaining`. 초과 시 `429 rate_limited` + `Retry-After`(초)와 같은 값의 `RateLimit-Reset`.
- 조회 폭주로 default 버킷이 비어도 emergency 버킷의 롤백은 받는다(`TestV2RateLimits`로 검증).
- 레거시 토큰(`server.auth_token_env`)은 한도를 적용하지 않는다.
- 토큰 발급(`POST /v2/oauth/token`)은 클라이언트별 default 버킷을 쓰며 초과 시 `429` + `error: slow_down`.

## 3.10 공통 헤더

| 헤더 | 방향 | 용도 |
|---|---|---|
| `Authorization` | 요청 | `Bearer <토큰>` |
| `Idempotency-Key` | 요청 | 3.6 |
| `If-Match`, `If-None-Match` | 요청 | 3.7 |
| `X-Request-ID` | 요청·응답 | 요청 추적. 없으면 W3C `traceparent`의 trace-id를 쓰고, 그것도 없으면 서버가 생성. 응답과 서버 로그에 남김 |
| `traceparent` | 요청 | W3C Trace Context. 3.10의 요청 ID 원천 |
| `X-Change-Ticket` | 요청 | 변경·인시던트 티켓 번호. 감사 기록 `ticket`에 남고, 배포 생성 시 변경 게이트 티켓으로도 쓰임. v1 배포 생성은 본문 `change_ticket`이 있으면 그것이 우선(5.1) |
| `X-CSRF-Token` | 요청 | 콘솔 세션 쿠키로 보내는 변경 요청 필수 |
| `Location` | 응답 | 생성된 리소스 또는 수락된 작업 URL |
| `ETag` | 응답 | 3.7 |
| `Idempotent-Replayed` | 응답 | `true`면 재생된 응답 |
| `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`(429에만), `Retry-After` | 응답 | 3.9 |
| `Deprecation`, `Sunset` | 응답 | 폐기 예정 엔드포인트 (현재 해당 없음) |

## 3.11 상태 값과 CI 종료 코드

| 배포 상태(state) | 의미 | exit_code |
|---|---|---|
| `PENDING` | 등록됨 | 1 |
| `BASELINE` | 기준선 측정 중 | 1 |
| `OBSERVING` | 단계 관측 중 | 1 |
| `PROMOTED` | 단계 통과, 다음 단계 대기 | 0 |
| `SUCCEEDED` | full 단계 통과 (종료 상태) | 0 |
| `HELD` | 판정 보류, 사람 결정 필요 | 4 |
| `ROLLING_BACK` | 롤백 진행 중 | 1 |
| `ROLLED_BACK` | 롤백 완료 (종료 상태) | 2 |
| `ROLLBACK_FAILED` | 롤백 실패, 사람 필요 (종료 상태) | 3 |
| `AWAITING_APPROVAL` | 롤백·상위 전략 승인 대기 | 3 |
| `ABORTED` | 관측 중단 (종료 상태) | 1 |

> exit_code 매핑은 `model.ExitCode`가 정한다. 0 통과, 1 오류(또는 진행 중), 2 롤백 완료, 3 롤백 실패·서킷 OPEN·승인 대기, 4 보류. 단계 시작이 게이트(서킷 OPEN, 변경 동결, 변경 티켓, ITSM 불통)로 거부된 경우의 CLI 종료 코드 3은 8.3을 본다.

---

# 4. REST API v2 엔드포인트

## 4.1 엔드포인트 목록

"역할"은 해당 서비스(또는 전체)에 필요한 최소 역할, "스코프"는 API 클라이언트에 필요한 스코프, "버킷"은 호출 한도 버킷, "멱등"은 `Idempotency-Key` 지원 여부다.

| ID | 메서드 | 경로 | operationId | 역할 | 스코프 | 버킷 | 멱등 |
|---|---|---|---|---|---|---|---|
| IF-API-001 | GET | `/v2/me` | getMe | 인증만 | - | default | - |
| IF-API-002 | GET | `/v2/deployments` | listDeployments | viewer | deployments:read | default | - |
| IF-API-003 | POST | `/v2/deployments` | createDeployment | deployer | deployments:write | default | O |
| IF-API-004 | GET | `/v2/deployments/{id}` | getDeployment | viewer | deployments:read | default | - |
| IF-API-005 | POST | `/v2/deployments/{id}/observations` | startObservation | deployer | deployments:write | default | O |
| IF-API-006 | POST | `/v2/deployments/{id}/rollbacks` | startRollback | operator | rollbacks:execute | emergency | O |
| IF-API-007 | POST | `/v2/deployments/{id}/approvals` | approveEscalation | operator | approvals:write | emergency | O |
| IF-API-008 | PUT | `/v2/deployments/{id}/feedback` | setDeploymentFeedback | deployer | deployments:write | default | O |
| IF-API-009 | POST | `/v2/deployments/{id}/abort` | abortObservation | deployer | deployments:write | emergency | O |
| IF-API-010 | GET | `/v2/operations` | listOperations | viewer | deployments:read | default | - |
| IF-API-011 | GET | `/v2/operations/{id}` | getOperation | viewer | deployments:read | default | - |
| IF-API-012 | GET | `/v2/services` | listServices | viewer | deployments:read | default | - |
| IF-API-013 | GET | `/v2/services/{name}` | getService | viewer | deployments:read | default | - |
| IF-API-014 | POST | `/v2/services/{name}/baselines` | captureBaseline | deployer | deployments:write | default | O |
| IF-API-015 | PUT | `/v2/services/{name}/last-good` | setLastGood | deployer | deployments:write | default | O |
| IF-API-016 | GET | `/v2/services/{name}/last-good` | getLastGood | viewer | deployments:read | default | - |
| IF-API-017 | GET | `/v2/targets` | listTargets | viewer | deployments:read | default | - |
| IF-API-018 | GET | `/v2/targets/{name}` | getTarget | viewer | deployments:read | default | - |
| IF-API-019 | GET | `/v2/presets` | listPresets | 인증만 | - | default | - |
| IF-API-020 | GET | `/v2/circuit` | getCircuit | viewer(어느 범위든) | deployments:read | default | - |
| IF-API-021 | POST | `/v2/circuit/reset` | resetCircuit | admin | circuit:admin | emergency | O |
| IF-API-022 | POST | `/v2/circuit/trip` | tripCircuit | admin | circuit:admin | emergency | O |
| IF-API-023 | GET | `/v2/freezes` | listFreezes | viewer(어느 범위든) | deployments:read | default | - |
| IF-API-024 | POST | `/v2/freezes` | createFreeze | admin | config:write | default | O |
| IF-API-025 | DELETE | `/v2/freezes/{id}` | endFreeze | admin | config:write | default | O |
| IF-API-026 | GET | `/v2/audit-events` | listAuditEvents | viewer@`*` | audit:read | default | - |
| IF-API-027 | POST | `/v2/oauth/token` | issueToken | 클라이언트 인증 | - | 클라이언트별 | - |
| IF-API-028 | GET | `/v2/api-clients` | listApiClients | admin | config:write | default | - |
| IF-API-029 | POST | `/v2/api-clients` | createApiClient | admin | config:write | default | 재생 안 함 |
| IF-API-030 | GET | `/v2/api-clients/{id}` | getApiClient | admin | config:write | default | - |
| IF-API-031 | PATCH | `/v2/api-clients/{id}` | updateApiClient | admin | config:write | default | O |
| IF-API-032 | DELETE | `/v2/api-clients/{id}` | revokeApiClient | admin | config:write | default | O |
| IF-API-033 | POST | `/v2/api-clients/{id}/secret` | rotateApiClientSecret | admin | config:write | default | 재생 안 함 |
| IF-API-034 | GET | `/v2/events` | streamEvents | viewer(어느 범위든) | deployments:read | default | - |
| IF-API-035 | GET | `/v2/event-types` | listEventTypes | 인증만 | - | default | - |
| IF-API-036 | GET | `/v2/webhooks` | listWebhooks | 인증(자기 구독, admin은 전체) | config:write | default | - |
| IF-API-037 | POST | `/v2/webhooks` | createWebhook | 다루는 범위의 viewer | config:write | default | 재생 안 함 |
| IF-API-038 | GET | `/v2/webhooks/{id}` | getWebhook | 소유자 또는 admin | config:write | default | - |
| IF-API-039 | PATCH | `/v2/webhooks/{id}` | updateWebhook | 소유자 또는 admin | config:write | default | O |
| IF-API-040 | DELETE | `/v2/webhooks/{id}` | deleteWebhook | 소유자 또는 admin | config:write | default | O |
| IF-API-041 | POST | `/v2/webhooks/{id}/secret` | rotateWebhookSecret | 소유자 또는 admin | config:write | default | 재생 안 함 |
| IF-API-042 | GET | `/v2/webhooks/{id}/deliveries` | listWebhookDeliveries | 소유자 또는 admin | config:write | default | - |
| IF-API-043 | POST | `/v2/webhooks/{id}/redeliveries` | redeliverEvent | 소유자 또는 admin | config:write | default | O |
| IF-API-044 | POST | `/v2/webhooks/{id}/pings` | pingWebhook | 소유자 또는 admin | config:write | default | O |
| IF-API-045 | GET | `/healthz` | health | 없음 | - | 미적용 | - |
| IF-API-046 | GET | `/readyz` | ready | 없음 | - | 미적용 | - |
| IF-API-047 | GET | `/metrics` | metrics | viewer@`*` (또는 `metrics_public`) | deployments:read | 미적용 | - |

> 역할 열은 서버 구현(`allow`, `v2Lookup`, `v2Service`, `webhookScope`, `ownWebhook`)에서 확인한 값이다. 목록 조회는 권한이 있는 서비스의 항목만 돌려준다(권한이 없으면 403이 아니라 빈 목록).

> 웹훅 구독(IF-API-036, 038, 040~044)의 스코프 `config:write`는 명세 기준이다. 구현(`internal/api/events.go`)은 등록(IF-API-037)과 변경(IF-API-039) 때 `webhookScope`로 `config:write`와 구독 범위의 viewer 권한을 확인하고, 나머지 목록·조회·삭제·비밀 회전·전달 이력·재전송·핑은 "생성자 또는 admin" 소유 확인(`ownWebhook`, 목록은 생성자 필터)만 하며 스코프를 따로 확인하지 않는다.

## 4.2 공통 요청 스키마

### 4.2.1 DeploymentCreate (IF-API-003)

| 필드 | 형식 | 필수 | 설명 |
|---|---|---|---|
| `id` | string, 최대 128 | - | 클라이언트 지정 ID(예: CI 실행 ID). 생략 시 생성 |
| `service` | string | O | 설정에 정의된 서비스 이름 |
| `version` | string | O | 신규 버전 |
| `previous_version` | string | - | 롤백 대상. 생략 시 그 서비스의 마지막 정상 버전, 없으면 422 |
| `prepare` | boolean, 기본 false | - | 지금 실행기 체크포인트(현재 릴리스, 이미지, 스냅샷)를 기록 |
| `freeze_override` | string, 최대 1000 | - | admin 전용. 동결 중 진행 사유(감사 기록) |
| `change_ticket` | string, 최대 64 | - | ITSM 변경 번호(예: `CHG0012345`). `X-Change-Ticket` 헤더도 가능 |

명세에 없는 필드는 거부한다(`additionalProperties: false`, `400 bad_request`). 이하 요청 스키마도 동일하다.

### 4.2.2 기타 요청 본문

| 스키마 | 사용 엔드포인트 | 필드 (필수 표시 O) |
|---|---|---|
| ObservationRequest | IF-API-005 | `phase` O: `canary`, `rolling`, `full` |
| RollbackRequest | IF-API-006 | `reason`(최대 1000), `executor`(주 실행기 대체), `targets[]`(대상 집합 대체) |
| ApprovalRequest | IF-API-007 | `decision`: `approve`(기본), `reject`; `comment`(최대 1000) |
| FeedbackRequest | IF-API-008 | `outcome` O: `correct`, `false_positive`, `false_negative`, `unclear`; `incident`(최대 200); `note`(최대 2000) |
| AbortRequest | IF-API-009 | `reason`(최대 1000, 감사 기록) |
| BaselineRequest | IF-API-014 | `window`: Go duration(예: `5m`). 생략 시 서비스 기준선 창 |
| LastGoodRequest | IF-API-015 | `version` O; `reason`(최대 1000) |
| CircuitRequest | IF-API-021, 022 | `reason` O (1~1000자) |
| FreezeCreate | IF-API-024 | `name` O; `ends_at` O; `reason`; `starts_at`(기본 지금); `services[]`; `teams[]`(둘 다 비면 전 서비스); `allow_rollback`(기본 true) |
| ApiClientCreate | IF-API-029 | `name` O(`^[a-z0-9][a-z0-9_-]{0,63}$`); `type` O: `oauth`, `api_key`; `scopes[]` O(1개 이상); `grants[]` O(1개 이상, `역할@범위`); `description`; `expires_at`; `rate_limit{rate, burst, daily}` |
| ApiClientUpdate | IF-API-031 | `description`, `scopes[]`, `grants[]`, `expires_at`, `rate_limit` |
| WebhookCreate | IF-API-037 | `url` O(http(s), `api.webhook_allowed_hosts` 제한); `description`; `types[]`(비면 전체); `services[]`; `teams[]` |
| WebhookUpdate | IF-API-039 | `url`, `description`, `types[]`, `services[]`, `teams[]`, `active` |
| 재전송 | IF-API-043 | `sequence` O (정수, 1 이상) |
| 토큰 요청 | IF-API-027 | form: `grant_type` O(`client_credentials`), `scope`(공백 구분, 클라이언트 스코프의 부분집합), `client_id`, `client_secret` |

## 4.3 주요 응답 스키마

| 스키마 | 주요 필드 | 상세 |
|---|---|---|
| Deployment | `id`, `service`, `version`, `previous_version`, `phase`, `state`, `verdict`, `reason`, `exit_code`, `targets`, `breaches[]`, `events[]`, `created_by`, `rollback_requested_by`, `approved_by`, `freeze_override`, `change_ticket{}`, `dry_run`, `feedback{}`, `pending_rollback{}`, `last_evaluation{}`, `created_at`, `updated_at` | VGL-SI-05 7.1 |
| Operation | `id`, `kind`, `status`, `service`, `deployment_id`, `phase`, `created_by`, `created_at`, `finished_at`, `result{}`, `error` | 3.8 |
| Service | `name`, `team`, `preset`, `targets[]`, `control_targets[]`, `phases[]`, `rules[{name, action}]`, `executor`, `traffic`, `last_good_version` | 설정에서 읽음 |
| Target | `name`, `kind`, `address`, `labels{}`, `connection`(`ssh`, `local`, `none`), `services[]` | 설정에서 읽음 |
| Preset | `name`, `version`, `ref`(예: `java-web@1`), `description`, `source`, `params[{name, required, default, description}]` | 내장·조직 프리셋 |
| Circuit | `state`(`CLOSED`, `OPEN`, `HALF_OPEN`), `reason`, `opened_at`, `recent_failures` | - |
| Freeze / FreezeWindow | Freeze: `id`, `name`, `reason`, `starts_at`, `ends_at`, `services`, `teams`, `allow_rollback`, `created_by`, `created_at`, `ended_by`, `ended_at`. FreezeWindow: `id`(`config:<name>` 또는 API ID), `source`(`config`, `api`), `active`, `until`, `weekly` 등 | - |
| AuditEvent | `seq`, `time`, `kind`, `actor`, `source`, `action`, `service`, `deployment_id`, `state`, `reason`, `ticket`, `hash`, `prev` | VGL-SI-05 6장 |
| Principal | `id`, `kind`(`user`, `service`, `client`, `legacy`, `webhook`, `anonymous`), `scopes`, `groups`, `grants[]` | - |
| ApiClient | `id`, `name`, `type`, `description`, `scopes`, `grants`, `secret_hint`(앞 8자), `rate_limit`, `expires_at`, `created_by`, `created_at`, `rotated_at`, `revoked_at`, `last_used_at`(최대 1시간 간격 갱신), `status`(`active`, `expired`, `revoked`) | 비밀값은 반환하지 않음 |
| ApiClientSecret | `client`, `secret`(`vcs_…` 또는 `vgk_…`, 1회만 표시) | - |
| Webhook | `id`, `url`, `description`, `types`, `services`, `teams`, `active`, `disabled_reason`, `cursor`, `consecutive_failures`, `dead_letters[{sequence, type, at, attempts, error}]`, `created_by`, `created_at`, `updated_at` | - |
| WebhookSecret | `webhook`, `secret`(`whsec_…`, 1회만 표시) | - |
| Delivery | `sequence`, `type`, `attempt`, `at`, `status_code`, `error`, `duration_ms`, `outcome`(`delivered`, `retrying`, `dead`) | - |
| TokenResponse | `access_token`, `token_type`(`Bearer`), `expires_in`(초), `scope` | - |
| Readiness | `ready`, `checks{store, leader}` | - |

## 4.4 엔드포인트 상세

표의 "오류"는 명세에 정의된 HTTP 상태이며, 해당 `code`는 3.4를 따른다. 모든 인증 대상 엔드포인트는 공통으로 401(`unauthenticated`)과 429(`rate_limited`)를 반환할 수 있다.

### 4.4.1 신원

#### IF-API-001 GET /v2/me

| 항목 | 내용 |
|---|---|
| 목적 | 호출자의 신원과 권한 확인 |
| 권한 | 인증만 |
| 요청 | 없음 |
| 응답 | 200 Principal |
| 오류 | 401, 429 |

### 4.4.2 배포

#### IF-API-002 GET /v2/deployments

| 항목 | 내용 |
|---|---|
| 목적 | 배포 목록, 최신순. 읽을 수 있는 서비스의 배포만 |
| 권한 | viewer (서비스별 필터) / `deployments:read` |
| 요청 | 쿼리 `limit`, `cursor`, `service`, `state`(DeploymentState) |
| 응답 | 200 DeploymentPage `{items, next_cursor}` |
| 오류 | 400, 401, 429 |

#### IF-API-003 POST /v2/deployments

| 항목 | 내용 |
|---|---|
| 목적 | 배포 등록(배포 직전 또는 직후). 롤백 대상 버전이 반드시 있어야 함 |
| 권한 | 해당 서비스 deployer / `deployments:write`. `freeze_override`는 admin |
| 요청 | 헤더 `Idempotency-Key`, `X-Change-Ticket`(선택). 본문 DeploymentCreate (4.2.1) |
| 응답 | 201 Deployment (신규) + `Location`, `ETag`. 같은 `id`·서비스·버전이 이미 있으면 200으로 그대로 반환 |
| 오류 | 400; 401; 403(역할 부족, 비admin의 `freeze_override`); 409(`change_frozen`, `change_ticket_invalid`, `conflict`); 422(`service`·`version` 누락, `id` 128자 초과, 없는 서비스, `previous_version`을 정할 수 없음); 429; 503(`itsm_unavailable`, `Retry-After: 30`) |
| 비고 | 동결 중이면 409 `change_frozen`. ServiceNow 변경 게이트 대상이면 승인·허용 상태·계획 시간 확인 |

#### IF-API-004 GET /v2/deployments/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 배포 상세(타임라인, 위반, 최근 평가, `exit_code`) |
| 권한 | viewer / `deployments:read` |
| 요청 | 경로 `id`, 헤더 `If-None-Match`(선택) |
| 응답 | 200 Deployment + `ETag`; 304 (ETag 동일) |
| 오류 | 401, 403, 404, 429 |

#### IF-API-005 POST /v2/deployments/{id}/observations

| 항목 | 내용 |
|---|---|
| 목적 | 한 단계의 관측 시작. 실패 단계는 자동 롤백(또는 `rollback.mode: approve`면 승인 대기) |
| 권한 | deployer / `deployments:write` |
| 요청 | 헤더 `Idempotency-Key`, `If-Match`. 본문 ObservationRequest |
| 응답 | 202 Operation(`kind: observation`) + `Location` |
| 오류 | 400, 401, 403, 404; 409(`circuit_open`, `change_frozen`, `conflict` 이미 관측·롤백 중); 412; 422; 429; 503(`itsm_unavailable`) |

#### IF-API-006 POST /v2/deployments/{id}/rollbacks

| 항목 | 내용 |
|---|---|
| 목적 | 운영자 결정에 의한 즉시 롤백. 진행 중인 관측을 취소하고 롤백 계획 실행 |
| 권한 | operator / `rollbacks:execute` (emergency 버킷) |
| 요청 | 헤더 `Idempotency-Key`, `If-Match`. 본문 RollbackRequest(선택) |
| 응답 | 202 Operation(`kind: rollback`) |
| 오류 | 400, 401, 403, 404, 409, 412, 422, 429 |
| 비고 | 플래핑 가드와 서킷 브레이커를 우회하지만 서비스별 잠금은 획득. 동결 중에도 수동 롤백은 항상 허용 |

#### IF-API-007 POST /v2/deployments/{id}/approvals

| 항목 | 내용 |
|---|---|
| 목적 | `AWAITING_APPROVAL` 배포의 승인 또는 거절 |
| 권한 | operator / `approvals:write` (emergency 버킷). 4-eyes 적용 시 생성자·롤백 요청자와 다른 사람 |
| 요청 | 헤더 `Idempotency-Key`, `If-Match`. 본문 ApprovalRequest |
| 응답 | 202 Operation(`kind: approval`) |
| 오류 | 400, 401, 403(권한, 4-eyes), 404, 409(승인 대기 아님), 412, 422, 429 |
| 비고 | 승인 모드의 준비된 롤백(`pending_rollback`)은 승인 시 실행, 거절 시 새 버전 유지·드레인한 대상 복귀·HOLD. `require_approval` 상위 전략(예: VM 스냅샷 복원)은 승인만 가능 |

#### IF-API-008 PUT /v2/deployments/{id}/feedback

| 항목 | 내용 |
|---|---|
| 목적 | 판정이 맞았는지 평가 기록(판정 품질 측정). 다시 남기면 교체 |
| 권한 | deployer / `deployments:write` |
| 요청 | 헤더 `Idempotency-Key`. 본문 FeedbackRequest |
| 응답 | 200 Deployment (feedback 포함) |
| 오류 | 400, 401, 403, 404, 422(`false_positive`인데 FAIL 판정 없음, `false_negative`인데 FAIL 판정 있음), 429 |

#### IF-API-009 POST /v2/deployments/{id}/abort

| 항목 | 내용 |
|---|---|
| 목적 | 진행 중인 관측 중단. 이미 실행 중인 롤백은 중단하지 않음 |
| 권한 | deployer / `deployments:write` (emergency 버킷) |
| 요청 | 헤더 `Idempotency-Key`. 본문 AbortRequest(선택) |
| 응답 | 200 Deployment |
| 오류 | 401, 403, 404, 422, 429 |

### 4.4.3 작업

#### IF-API-010 GET /v2/operations

| 항목 | 내용 |
|---|---|
| 목적 | 작업 목록, 최신순 |
| 권한 | viewer (서비스별 필터) / `deployments:read` |
| 요청 | 쿼리 `limit`, `cursor`, `deployment_id`, `status`(`running`, `completed`, `failed`) |
| 응답 | 200 OperationPage |
| 오류 | 400, 401, 429 |

#### IF-API-011 GET /v2/operations/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 작업 진행·결과 조회 (폴링 대상) |
| 권한 | 작업 서비스의 viewer / `deployments:read` |
| 요청 | 경로 `id`, 헤더 `If-None-Match` |
| 응답 | 200 Operation + `ETag`; 304 |
| 오류 | 401, 403, 404, 429 |

### 4.4.4 서비스·대상·프리셋

#### IF-API-012 GET /v2/services

| 항목 | 내용 |
|---|---|
| 목적 | 읽을 수 있는 서비스 목록(이름순) |
| 권한 | viewer / `deployments:read` |
| 요청 | 쿼리 `limit`, `cursor`, `team` |
| 응답 | 200 ServicePage |
| 오류 | 400, 401, 429 |

#### IF-API-013 GET /v2/services/{name}

| 항목 | 내용 |
|---|---|
| 목적 | 서비스 구성(대상, 단계, 규칙, 실행기, 트래픽, 마지막 정상 버전) |
| 권한 | viewer / `deployments:read` |
| 응답 | 200 Service |
| 오류 | 401, 403, 404, 429 |

#### IF-API-014 POST /v2/services/{name}/baselines

| 항목 | 내용 |
|---|---|
| 목적 | 운영 중인(이전) 버전을 관측 창 동안 측정해 비교 기준선으로 보관 |
| 권한 | deployer / `deployments:write` |
| 요청 | 헤더 `Idempotency-Key`. 본문 BaselineRequest(선택) |
| 응답 | 202 Operation(`kind: baseline`, 완료 시 `result.baseline_samples`) |
| 오류 | 400, 401, 403, 404, 422, 429 |

#### IF-API-015 PUT /v2/services/{name}/last-good

| 항목 | 내용 |
|---|---|
| 목적 | 기준(known-good) 버전 등록. `previous_version`을 비운 배포의 롤백 대상 (CLI `mark-good`) |
| 권한 | deployer / `deployments:write` |
| 요청 | 헤더 `Idempotency-Key`. 본문 LastGoodRequest |
| 응답 | 200 LastGood `{service, version, deployment_id}` |
| 오류 | 400, 401, 403, 404, 422, 429 |

#### IF-API-016 GET /v2/services/{name}/last-good

| 항목 | 내용 |
|---|---|
| 목적 | 마지막 정상 버전 조회 |
| 권한 | viewer / `deployments:read` |
| 응답 | 200 LastGood |
| 오류 | 401, 403, 404, 429 |

#### IF-API-017 GET /v2/targets

| 항목 | 내용 |
|---|---|
| 목적 | 대상 목록. 읽을 수 있는 서비스에 속한 대상만(viewer@`*`면 전체) |
| 권한 | viewer / `deployments:read` |
| 요청 | 쿼리 `limit`, `cursor` |
| 응답 | 200 TargetPage |
| 오류 | 400, 401, 429 |

#### IF-API-018 GET /v2/targets/{name}

| 항목 | 내용 |
|---|---|
| 목적 | 대상 상세 |
| 권한 | viewer / `deployments:read` |
| 응답 | 200 Target |
| 오류 | 401, 403, 404, 429 |

#### IF-API-019 GET /v2/presets

| 항목 | 내용 |
|---|---|
| 목적 | 내장·조직 규칙 프리셋과 파라미터 |
| 권한 | 인증만 |
| 응답 | 200 `{items: Preset[]}` |
| 오류 | 401, 429 |

### 4.4.5 안전장치 (서킷·동결)

#### IF-API-020 GET /v2/circuit

| 항목 | 내용 |
|---|---|
| 목적 | 서킷 브레이커 상태 |
| 권한 | 어느 범위든 viewer / `deployments:read` |
| 응답 | 200 Circuit |
| 오류 | 401, 403, 429 |

#### IF-API-021 POST /v2/circuit/reset

| 항목 | 내용 |
|---|---|
| 목적 | 서킷 닫기. 조사 후 자동 롤백과 배포 게이트 재개 |
| 권한 | admin(전체 범위) / `circuit:admin` (emergency 버킷) |
| 요청 | 헤더 `Idempotency-Key`. 본문 CircuitRequest(`reason` 필수) |
| 응답 | 200 Circuit |
| 오류 | 400, 401, 403, 422(빈 `reason`), 429 |

#### IF-API-022 POST /v2/circuit/trip

| 항목 | 내용 |
|---|---|
| 목적 | 서킷 열기(킬 스위치). 모든 자동 파괴적 조치 동결, 배포 게이트 닫음 |
| 권한 | admin / `circuit:admin` (emergency 버킷) |
| 요청 | 헤더 `Idempotency-Key`. 본문 CircuitRequest |
| 응답 | 200 Circuit |
| 오류 | 400, 401, 403, 422, 429 |

#### IF-API-023 GET /v2/freezes

| 항목 | 내용 |
|---|---|
| 목적 | 변경 동결 목록: 설정 창(`config:<name>`, 주간·절대)과 끝나지 않은 API 선언 동결 |
| 권한 | 어느 범위든 viewer / `deployments:read` |
| 응답 | 200 `{items: FreezeWindow[]}` |
| 오류 | 401, 403, 429 |

#### IF-API-024 POST /v2/freezes

| 항목 | 내용 |
|---|---|
| 목적 | 동결 선언. `ends_at`까지 대상 서비스의 새 배포·단계 시작 거부 |
| 권한 | admin / `config:write` |
| 요청 | 헤더 `Idempotency-Key`. 본문 FreezeCreate |
| 응답 | 201 Freeze + `Location` |
| 오류 | 400, 401, 403, 422, 429 |
| 비고 | 자동 롤백은 `allow_rollback: false`가 아니면 허용, 수동 롤백은 항상 허용 |

#### IF-API-025 DELETE /v2/freezes/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 선언된 동결 조기 종료(설정 창은 설정 변경으로 종료) |
| 권한 | admin / `config:write` |
| 요청 | 헤더 `Idempotency-Key` |
| 응답 | 200 Freeze |
| 오류 | 401, 403, 404, 429 |

### 4.4.6 감사

#### IF-API-026 GET /v2/audit-events

| 항목 | 내용 |
|---|---|
| 목적 | 감사 기록 조회, 오래된 순. 서비스를 가로지르므로 전체 범위 권한 필요 |
| 권한 | viewer@`*` / `audit:read` |
| 요청 | 쿼리 `limit`, `cursor`, `since`, `until`(date-time), `actor`, `action`, `service` |
| 응답 | 200 AuditEventPage |
| 오류 | 400, 401, 403, 429 |

### 4.4.7 OAuth·API 클라이언트

#### IF-API-027 POST /v2/oauth/token

| 항목 | 내용 |
|---|---|
| 목적 | OAuth 2.0 client credentials 접근 토큰 발급 (RFC 6749 4.4) |
| 권한 | 클라이언트 인증: HTTP Basic(`client_id:client_secret`) 또는 form 필드 |
| 요청 | `application/x-www-form-urlencoded`: `grant_type=client_credentials`, `scope`(선택) |
| 응답 | 200 TokenResponse (`Cache-Control: no-store`) |
| 오류 (RFC 6749 형식) | 400 `invalid_request`, `unsupported_grant_type`, `invalid_scope`; 401 `invalid_client`(없음·비밀 불일치·폐기·만료); 429 `slow_down`; 500 `server_error` |
| 비고 | 비밀 회전·폐기 시 이전 토큰 즉시 무효. 발급은 감사 `oauth.token`, 실패는 `denied` |

#### IF-API-028 GET /v2/api-clients

| 항목 | 내용 |
|---|---|
| 목적 | API 클라이언트 목록(이름순) |
| 권한 | admin / `config:write` |
| 요청 | 쿼리 `limit`, `cursor` |
| 응답 | 200 ApiClientPage |
| 오류 | 400, 401, 403, 429 |

#### IF-API-029 POST /v2/api-clients

| 항목 | 내용 |
|---|---|
| 목적 | OAuth 클라이언트 또는 API 키 등록. 비밀값은 응답에 1회만 포함, 서버는 SHA-256만 저장 |
| 권한 | admin / `config:write` |
| 요청 | 본문 ApiClientCreate. `Idempotency-Key` 재생 안 함 |
| 응답 | 201 ApiClientSecret + `Location` |
| 오류 | 400, 401, 403, 409(같은 이름의 활성 클라이언트), 422, 429 |

#### IF-API-030 GET /v2/api-clients/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 클라이언트 조회(비밀값 제외, `secret_hint`만) |
| 권한 | admin / `config:write` |
| 응답 | 200 ApiClient |
| 오류 | 401, 403, 404, 429 |

#### IF-API-031 PATCH /v2/api-clients/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 스코프·grant·만료·한도 변경. 스코프 축소는 발급된 토큰에도 즉시 적용 |
| 권한 | admin / `config:write` |
| 요청 | 헤더 `Idempotency-Key`. 본문 ApiClientUpdate |
| 응답 | 200 ApiClient |
| 오류 | 400, 401, 403, 404, 409, 422, 429 |

#### IF-API-032 DELETE /v2/api-clients/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 클라이언트 폐기. 클라이언트와 발급 토큰 즉시 무효, 기록은 감사용으로 유지 |
| 권한 | admin / `config:write` |
| 요청 | 헤더 `Idempotency-Key` |
| 응답 | 200 ApiClient(`status: revoked`) |
| 오류 | 401, 403, 404, 429 |

#### IF-API-033 POST /v2/api-clients/{id}/secret

| 항목 | 내용 |
|---|---|
| 목적 | 비밀 회전. 이전 비밀과 그 비밀로 받은 토큰 즉시 무효 |
| 권한 | admin / `config:write` |
| 응답 | 200 ApiClientSecret (새 비밀 1회 표시) |
| 오류 | 401, 403, 404, 409, 429 |

### 4.4.8 이벤트·웹훅 구독

#### IF-API-034 GET /v2/events

| 항목 | 내용 |
|---|---|
| 목적 | 이벤트 스트림(SSE). 6.3 참조 |
| 권한 | 어느 범위든 viewer / `deployments:read`. 서비스 이벤트는 그 서비스를 읽을 수 있을 때만 전달 |
| 요청 | 헤더 `Last-Event-ID`(숫자) 또는 쿼리 `after`; 쿼리 `types`(쉼표 구분), `service` |
| 응답 | 200 `text/event-stream` |
| 오류 | 400, 401, 403, 429 |

#### IF-API-035 GET /v2/event-types

| 항목 | 내용 |
|---|---|
| 목적 | 이벤트 카탈로그(유형과 의미) |
| 권한 | 인증만 |
| 응답 | 200 `{items: [{type, description}]}` |
| 오류 | 401, 429 |

#### IF-API-036 GET /v2/webhooks

| 항목 | 내용 |
|---|---|
| 목적 | 웹훅 구독 목록(오래된 순). 호출자 자신의 구독, admin은 전체 |
| 권한 | 인증 / `config:write` |
| 요청 | 쿼리 `limit`, `cursor` |
| 응답 | 200 WebhookPage |
| 오류 | 400, 401, 429 |

#### IF-API-037 POST /v2/webhooks

| 항목 | 내용 |
|---|---|
| 목적 | 이벤트 구독 등록. 다음 이벤트부터 전달 |
| 권한 | 구독이 다루는 모든 서비스·팀의 viewer(필터가 없으면 `*`) / `config:write` |
| 요청 | 본문 WebhookCreate. `Idempotency-Key` 재생 안 함 |
| 응답 | 201 WebhookSecret + `Location` (서명 비밀 `whsec_…` 1회 표시) |
| 오류 | 400; 401; 403; 422 `validation_failed`(URL·필터 검증 실패, `api.webhook_signing_key_ref` 미설정); 429; 500 |

#### IF-API-038 GET /v2/webhooks/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 구독 조회(dead-letter 목록 포함) |
| 권한 | 생성자 또는 admin / `config:write` |
| 응답 | 200 Webhook |
| 오류 | 401, 403, 404, 429 |

#### IF-API-039 PATCH /v2/webhooks/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 구독 변경 또는 재활성화. 자동 비활성 후 `active: true`면 실패 횟수 초기화, 커서부터 재전달 |
| 권한 | 생성자 또는 admin / `config:write`. 변경 후 구독 범위(서비스·팀)의 viewer 권한도 다시 확인 |
| 요청 | 헤더 `Idempotency-Key`. 본문 WebhookUpdate |
| 응답 | 200 Webhook |
| 오류 | 400, 401, 403, 404, 422, 429 |

#### IF-API-040 DELETE /v2/webhooks/{id}

| 항목 | 내용 |
|---|---|
| 목적 | 구독 해지 |
| 권한 | 생성자 또는 admin / `config:write` |
| 응답 | 204 |
| 오류 | 401, 403, 404, 429 |

#### IF-API-041 POST /v2/webhooks/{id}/secret

| 항목 | 내용 |
|---|---|
| 목적 | 서명 비밀 회전. 이후 전달은 새 비밀로 서명 |
| 권한 | 생성자 또는 admin / `config:write` |
| 응답 | 200 WebhookSecret |
| 오류 | 401, 403, 404, 429 |

#### IF-API-042 GET /v2/webhooks/{id}/deliveries

| 항목 | 내용 |
|---|---|
| 목적 | 이 노드의 최근 전달 시도 100건, 최신순 |
| 권한 | 생성자 또는 admin / `config:write` |
| 응답 | 200 `{items: Delivery[]}` |
| 오류 | 401, 403, 404, 429 |

#### IF-API-043 POST /v2/webhooks/{id}/redeliveries

| 항목 | 내용 |
|---|---|
| 목적 | 보관 중인 이벤트 재전송(주로 dead-letter 항목, 재전송 시 목록에서 제거) |
| 권한 | 생성자 또는 admin / `config:write` |
| 요청 | 헤더 `Idempotency-Key`. 본문 `{sequence}` |
| 응답 | 202 `{queued, sequence}` |
| 오류 | 400, 401, 403, 404, 422, 429 |

#### IF-API-044 POST /v2/webhooks/{id}/pings

| 항목 | 내용 |
|---|---|
| 목적 | 해당 구독에만 가는 `vigilante.ping` 테스트 이벤트 발행 |
| 권한 | 생성자 또는 admin / `config:write` |
| 요청 | 헤더 `Idempotency-Key` |
| 응답 | 202 CloudEvent |
| 오류 | 401, 403, 404, 429, 503(이 노드가 리더가 아니라 발행 불가) |

### 4.4.9 시스템

#### IF-API-045 GET /healthz

| 항목 | 내용 |
|---|---|
| 목적 | 생존 확인(liveness) |
| 권한 | 없음. 리더 전달 대상 아님 |
| 응답 | 200 `{ok, role(single, leader, follower), leader, active, circuit, dry_run, store}` |

#### IF-API-046 GET /readyz

| 항목 | 내용 |
|---|---|
| 목적 | 준비 확인(readiness) |
| 권한 | 없음 |
| 응답 | 200 Readiness; 503 Readiness(상태 저장소 불통 2초 제한, 또는 HA 리더 미확정) |

#### IF-API-047 GET /metrics

| 항목 | 내용 |
|---|---|
| 목적 | Prometheus 텍스트 형식 0.0.4 지표 |
| 권한 | viewer@`*` / `deployments:read`. `server.metrics_public: true`면 인증 없음 |
| 응답 | 200 `text/plain` |
| 오류 | 401, 403 (problem+json) |

---

# 5. REST API v1 (레거시)

`/v1`은 기존 CI 연동을 위해 유지하며 기능을 더하지 않는다(docs/08-upgrade.md: 동결, 제거는 MAJOR에서 6개월 전 예고). 예외로 PR #13에서 `POST /v1/deployments`에 v2와 같은 게이트 필드와 게이트 거부 `code`를 추가했다(5.1, 추가만이므로 하위 호환). 오류는 problem+json이 아닌 `{"error": "..."}` 형식이다. 인증과 역할·스코프 검사는 v2와 같은 로직(`authn`, `Principal.Can`)을 쓰지만, 호출 한도 버킷과 Idempotency-Key는 v2 라우트에만 적용된다.

| ID | 메서드·경로 | 용도 | 권한 | v2 대응 |
|---|---|---|---|---|
| IF-V1-01 | GET `/v1/whoami` | 호출자 확인 (CLI `whoami`) | 인증 | IF-API-001 |
| IF-V1-02 | GET `/v1/audit` | 감사 조회, `format=csv` 지원 | viewer@`*` | IF-API-026 |
| IF-V1-03 | GET `/v1/deployments` | 배포 목록 | viewer | IF-API-002 |
| IF-V1-04 | POST `/v1/deployments` | 배포 생성(+`phase`로 즉시 관측). 본문 `change_ticket`, `freeze_override`(admin), 게이트 거부 `code`(5.1) | deployer | IF-API-003 + 005 |
| IF-V1-05 | GET `/v1/deployments/{id}` | 배포와 종료 코드 | viewer | IF-API-004 |
| IF-V1-06 | POST `/v1/deployments/{id}/phases/{phase}` | 단계 관측, `wait=true`면 판정까지 롱폴링 | deployer | IF-API-005 + 011 |
| IF-V1-07 | POST `/v1/deployments/{id}/rollback` | 수동 롤백 | operator | IF-API-006 |
| IF-V1-08 | POST `/v1/deployments/{id}/approve` | 상위 전략 승인 | operator | IF-API-007 |
| IF-V1-09 | POST `/v1/deployments/{id}/abort` | 관측 중단 | deployer | IF-API-009 |
| IF-V1-10 | POST `/v1/baselines/{service}` | 기준선(응답까지 대기) | deployer | IF-API-014 |
| IF-V1-11 | GET `/v1/circuit` | 서킷 상태 | viewer | IF-API-020 |
| IF-V1-12 | POST `/v1/circuit/reset` | 서킷 닫기(`?reason=`) | admin | IF-API-021 |
| IF-V1-13 | POST `/v1/circuit/trip` | 서킷 열기 | admin | IF-API-022 |
| IF-V1-14 | POST `/v1/samples` | 에이전트 샘플 전송, 204 | 대상이 속한 서비스의 agent | 없음 |
| IF-V1-15 | GET `/v1/agents/{target}/heartbeat` | 에이전트 하트비트와 활성 배포 | agent | 없음 |
| IF-V1-16 | POST `/v1/webhooks/{provider}` | CI 웹훅 (7장) | 서명·토큰 | 없음 |
| IF-V1-17 | GET `/v1/targets/{target}/metrics` | 대상의 최근 샘플 | viewer | 없음 |

> 확인 사항: `docs/06-api.md`는 "CLI 원격 모드, 운영 콘솔, 에이전트도 같은 API만 씁니다"라고 적고 있으나, 코드상 CLI `--server` 모드의 `watch`·`circuit`·`whoami`는 v1 엔드포인트(IF-V1-01, 04, 05, 11~13)를 호출하고, v2는 `feedback`(IF-API-008)과 `support-bundle`의 서버 수집(`/healthz`, `/readyz`, `/metrics`, `GET /v2/circuit`)만 호출한다. 에이전트는 IF-V1-14, 15를 쓴다. `watch --server`는 PR #13부터 `--ticket`·`--freeze-override`를 IF-V1-04 본문으로 전달한다.

## 5.1 IF-V1-04 POST /v1/deployments 본문과 게이트 오류 (PR #13)

| 필드 | 형식 | 설명 |
|---|---|---|
| `id`, `service`, `version`, `previous_version`, `prepare`, `phase` | 기존 | 배포 생성과 즉시 관측 단계 |
| `change_ticket` | string | ITSM 변경 번호. 없으면 `X-Change-Ticket` 헤더를 쓴다 |
| `freeze_override` | string | 동결 중 진행 사유. admin(`manage` 권한)만. 그 외 주체가 보내면 `403 {"error": "forbidden: only admins may override a change freeze (…)"}` + 감사 `denied` |

`change_ticket`·`freeze_override`는 v1 생성 본문(`createV1`)에서만 읽고 수신 웹훅 본문에서는 읽지 않는다(웹훅 페이로드로 동결 예외를 줄 수 없음).

| 상황 | HTTP | 본문 |
|---|---|---|
| 서킷 OPEN | 409 | `{"error": "...", "code": "circuit_open"}` |
| 변경 동결 | 409 | `{"error": "...", "code": "change_frozen"}` |
| 변경 티켓 없음·무효 | 409 | `{"error": "...", "code": "change_ticket_invalid"}` |
| ServiceNow 불통(fail-closed) | 503 + `Retry-After: 30` | `{"error": "...", "code": "itsm_unavailable"}` |
| 그 밖의 생성 실패 | 400 | `{"error": "..."}` (`code` 없음) |

`code` 값은 v2 problem의 `code`와 같다(3.4). CLI는 이 `code`로 게이트 거부(종료 코드 3)와 오류(1)를 구분한다(8.3). 검증: `TestV1CreateGateFields`, `TestWatchRemoteSendsTicketAndFreezeOverride`, `TestWatchRemoteGateRefusalExitsThree`.

---

# 6. 이벤트 인터페이스

## 6.1 CloudEvents 형식

배포 상태 변화, 서킷, 승인, 에이전트 상실을 CloudEvents 1.0(structured JSON mode)으로 발행한다. 이벤트는 상태 저장소에 `event` 기록으로 남고, 최근 10,000건을 SSE 재개와 웹훅 따라잡기에 보관한다.

| 필드 | 형식 | 설명 |
|---|---|---|
| `specversion` | `"1.0"` | CloudEvents 버전 |
| `id` | string | `sequence`의 문자열. 클러스터 내 유일 |
| `source` | string | 발행 원천. 현재 고정값 `/vigilante` |
| `type` | string | 6.2. 종류는 늘어날 수 있으며 모르는 종류는 무시 |
| `subject` | string | 배포 ID, `circuit`, 대상 또는 웹훅 ID |
| `time` | date-time | 발생 시각 |
| `datacontenttype` | `application/json` | - |
| `sequence` | integer (확장) | 클러스터 전체 순번. SSE `id`, `Last-Event-ID`, `webhook-id`와 같은 값. 리더가 바뀌어도 이어짐 |
| `service`, `team` | string (확장) | 필터링용 서비스·소유 팀 |
| `data` | object | 배포 이벤트: `deployment_id`, `service`, `version`, `previous_version`, `phase`, `state`, `verdict`, `reason`, `exit_code`, `targets`, `breaches`. 서킷: `state`, `reason`. 승인 결정: `deployment_id`, `service`, `kind`(`rollback`, `escalation`), `decision`(`approved`, `rejected`), `decided_by`, `comment`. 에이전트 상실: `target`, `last_seen`, `services`. 웹훅 비활성: `webhook_id`, `url`, `reason`. 핑: `webhook_id`, `requested_by` |

> 승인 결정 데이터: PR #13에서 명세(`api/openapi.yaml` CloudEvent `data` 설명)를 구현에 맞춰 `deployment_id, service, kind (rollback | escalation), decision (approved | rejected), decided_by, comment`로 고쳤다(이전 명세의 `approved_by`는 구현에 없었다). `approval.decided`는 승인과 승인 모드의 거절 모두에 발행된다(이벤트 카탈로그 설명도 "approved or rejected"로 수정). 계약 테스트 `TestV2ApproveModeDecisions`가 실제 이벤트의 `data` 필드 집합을 명세 설명에서 읽은 목록과 비교해 둘이 어긋나지 않게 한다.

## 6.2 이벤트 유형

| ID | type | 의미 |
|---|---|---|
| IF-EVT-01 | `vigilante.deployment.created` | 배포 등록 |
| IF-EVT-02 | `vigilante.deployment.marked_good` | 서비스의 정상 버전 등록 |
| IF-EVT-03 | `vigilante.observation.started` | 단계 관측 시작 |
| IF-EVT-04 | `vigilante.observation.passed` | 단계 통과, 승격 가능 |
| IF-EVT-05 | `vigilante.observation.failed` | 단계 실패, 차단되지 않으면 롤백이 뒤따름 |
| IF-EVT-06 | `vigilante.observation.held` | 명확한 판정 없이 종료, 사람 결정 필요 |
| IF-EVT-07 | `vigilante.observation.aborted` | 판정 전 관측 중단 |
| IF-EVT-08 | `vigilante.rollback.started` | 롤백 시작(자동·수동) |
| IF-EVT-09 | `vigilante.rollback.completed` | 모든 대상이 이전 버전으로 복귀 |
| IF-EVT-10 | `vigilante.rollback.failed` | 롤백 실패 또는 차단. 대상이 격리되었을 수 있음, 사람 필요 |
| IF-EVT-11 | `vigilante.approval.requested` | 상위 전략(예: VM 스냅샷 복원) 승인 대기 |
| IF-EVT-12 | `vigilante.approval.decided` | 승인 결정(승인 모드의 거절 포함) |
| IF-EVT-13 | `vigilante.circuit.opened` | 서킷 OPEN, 자동 조치 동결 |
| IF-EVT-14 | `vigilante.circuit.half_opened` | 시험 조치 1회 허용 |
| IF-EVT-15 | `vigilante.circuit.closed` | 서킷 CLOSED, 자동화 재개 |
| IF-EVT-16 | `vigilante.agent.lost` | 에이전트 하트비트 끊김 |
| IF-EVT-17 | `vigilante.webhook.disabled` | 전달 실패가 이어져 구독 비활성 |
| IF-EVT-18 | `vigilante.ping` | 요청한 구독에만 가는 테스트 이벤트 |

전달 범위: 서비스 이벤트는 그 서비스를 읽을 수 있는 호출자·구독에만, 서비스가 없는 이벤트(서킷, 에이전트)는 viewer 누구에게나(웹훅은 모든 구독에) 전달한다.

## 6.3 SSE 스트림 (IF-EVT-20, `GET /v2/events`)

| 항목 | 규약 |
|---|---|
| 형식 | `text/event-stream`. 메시지마다 `id`(sequence), `event`(type), `data`(CloudEvent JSON) |
| 재개 | `Last-Event-ID` 헤더(브라우저 자동) 또는 `after` 쿼리. 보관 중인(최근 10,000건) 빠진 이벤트를 먼저 보내고 실시간으로 이어짐. 없으면 새 이벤트부터 |
| 오래 끊긴 경우 | 보관된 가장 오래된 이벤트부터 받음 |
| 필터 | `types`(쉼표 구분), `service` |
| 연결 유지 | 15초마다 keep-alive 주석 |
| 권한 | 4.4.8 IF-API-034 |

## 6.4 아웃바운드 웹훅 (IF-EVT-21)

| 항목 | 규약 |
|---|---|
| 방향·방식 | Vigilante(리더) → 구독 URL, HTTP POST, 요청당 이벤트 1건 |
| 본문 | `Content-Type: application/cloudevents+json`, CloudEvent JSON. `User-Agent: Vigilante-Webhook/2` |
| 헤더 | `webhook-id`(이벤트 sequence, 재시도에도 동일), `webhook-timestamp`(Unix 초), `webhook-signature` |
| 성공 조건 | 10초 안에 2xx 응답 (HTTP 클라이언트 타임아웃 10초) |
| 재시도 | 1초, 5초, 30초, 2분, 10분, 30분 후 재시도. 모두 실패하면 구독의 dead-letter 목록(최대 100건)에 남기고 다음 이벤트로 진행 |
| 자동 비활성 | 연속 5건 dead-letter 시 구독 비활성, `vigilante.webhook.disabled` 발행, 감사 기록·운영 알림 |
| 순서 | 구독마다 순서대로 한 건씩. 커서(`cursor`)가 마지막 처리 sequence |
| 전달 보장 | 최소 한 번(리더 교체·응답 유실 시 중복 가능). 수신 측은 `webhook-id`로 중복 제거 |
| 제약 | 리디렉션은 따라가지 않음. `api.webhook_allowed_hosts`로 수신 호스트 제한. `api.webhook_signing_key_ref`(서명 마스터 키)가 없으면 웹훅 기능 사용 불가 |

### 6.4.1 서명 (Standard Webhooks)

| 단계 | 내용 |
|---|---|
| 구독 키 | `HMAC-SHA256(마스터 키, "vigilante-webhook" + NUL + 웹훅 ID + NUL + key_version)`. 저장하지 않고 필요할 때 계산. 회전 시 `key_version` 증가 |
| 비밀 표시 | `whsec_` + base64(구독 키). 등록·회전 응답에 1회만 |
| 서명 | `v1,` + base64(`HMAC-SHA256(구독 키, webhook-id + "." + webhook-timestamp + "." + 본문)`) |
| 수신 측 검증 | `whsec_` 뒤를 base64 디코드한 키로 같은 값을 계산해 상수 시간 비교. 타임스탬프가 5분 이상 차이 나면 재전송 공격으로 보고 거부(권장) |

```text
webhook-id: 1042
webhook-timestamp: 1791676800
webhook-signature: v1,<base64 HMAC-SHA256 of "1042.1791676800.<body>">
```

---

# 7. 인바운드 웹훅 (CI/CD)

경로는 `POST /v1/webhooks/{provider}` (IF-V1-16). 본문 최대 4 MiB. 이벤트를 배포 등록(필요 시 단계 관측 시작)으로 변환한다. 성공 시 `201` + Deployment, 처리 대상이 아닌 이벤트는 `202 {"status": "ignored"}`.

| ID | provider | 인증 | 주체·권한 | 배포 ID | 서비스·버전 출처 | 처리 조건 |
|---|---|---|---|---|---|---|
| IF-INB-01 | `github` | `X-Hub-Signature-256: sha256=<HMAC-SHA256(본문, 공유 비밀)>` 상수 시간 비교 | `webhook:github`, 모든 서비스의 deployer | `gh-<deployment.id>` | `deployment.payload.service`, `version`(없으면 `deployment.ref`), `previous_version`, `phase`(기본 `canary`) | `deployment_status.state == success`만 |
| IF-INB-02 | `gitlab` | `X-Gitlab-Token` == 공유 비밀 (상수 시간 비교) | `webhook:gitlab`, 모든 서비스의 deployer | `gl-<deployment_id>` | `variables.VIGILANTE_SERVICE`, `VIGILANTE_VERSION`(없으면 `short_sha`, `ref`), `VIGILANTE_PREVIOUS_VERSION`, `VIGILANTE_PHASE`(기본 `canary`) | `status == success`만 |
| IF-INB-03 | `jenkins` 및 기타 | 일반 Bearer 토큰(3.2) | 그 토큰 주체의 grant, 서비스 deployer 필요 | 본문 `id` | 본문 JSON `{id, service, version, previous_version, prepare, phase}` | 항상 |

| 항목 | 내용 |
|---|---|
| 공유 비밀 | `server.webhook_secret_env`가 가리키는 환경변수. 없으면 GitHub·GitLab 요청은 401 |
| 서비스 지정 | 쿼리 `?service=`가 있으면 본문 값보다 우선. 서비스를 알 수 없으면 400 |
| 변경 티켓 | `X-Change-Ticket` 헤더를 변경 게이트 티켓으로 사용 |
| 오류 | 400(본문 해석 실패, 서비스 없음), 401(서명·토큰 실패), 403(deployer 아님), 409(동결·서킷·티켓 무효), 503(ITSM 불통 fail-closed, `Retry-After: 30`). 형식은 `{"error": "..."}`이며, 게이트 거부(409·503)에는 5.1과 같은 `code`가 붙는다(PR #13) |
| 감사 | `source: webhook`, `action: deployment.create` |

---

# 8. 명령행 인터페이스 (CLI)

단일 바이너리 `vigilante`가 CI 게이트, 중앙 서버, 에이전트로 동작한다. 아래는 `cmd/vigilante/main.go`의 usage 문자열 기준이다.

## 8.1 명령 목록

| ID | 명령 | 형식 | 용도 |
|---|---|---|---|
| IF-CLI-01 | `validate` | `-c FILE` | 설정 검증. 대상·서비스·실행기·트래픽 수와 경고 출력 |
| IF-CLI-02 | `prepare` | `-c FILE --service S --version V --previous P [--id ID]` | 배포 등록과 체크포인트(스냅샷 등) 기록 |
| IF-CLI-03 | `baseline` | `-c FILE --service S [--window 5m] [--out baseline.json]` | 배포 전 기준선 측정 |
| IF-CLI-04 | `watch` | `-c FILE --service S --phase canary,rolling,full [--version V] [--previous P] [--id ID] [--baseline FILE] [--dry-run] [--server URL]` | 단계 관측과 자동 롤백. 종료 코드가 CI 계약 |
| IF-CLI-05 | `mark-good` | `-c FILE --service S --version V [--reason TEXT]` | 기준(known-good) 버전 등록 |
| IF-CLI-06 | `rollback` | `-c FILE (--id ID 또는 --service S --version V --previous P [--targets a,b]) [--executor NAME] [--approve 또는 --reject] [--reason TEXT] [--dry-run] [--break-glass REASON]` | 수동 롤백, 승인 모드 대기 배포의 승인·거절, 에스컬레이션 승인. 로컬 승인·거절에는 `auth.local_cli`·`four_eyes` 적용(8.4) |
| IF-CLI-07 | `status` | `-c FILE [--id ID]` | 배포 상태 |
| IF-CLI-08 | `circuit` | `-c FILE status, reset, trip [--reason TEXT] [--server URL] [--break-glass REASON]` | 서킷 조회·리셋·트립. 로컬 reset·trip은 `auth.local_cli` 적용(8.4) |
| IF-CLI-09 | `server` | `-c FILE [--dry-run]` | 중앙 서버(REST, 웹훅, 크래시 재개, HA) |
| IF-CLI-10 | `agent` | `-c FILE --target NAME --server URL` | 대상 호스트 에이전트(샘플 전송, 하트비트, failsafe) |
| IF-CLI-11 | `doctor` | `-c FILE [--service S] [--previous P] [--json] [--junit FILE]` | 배포 전 읽기 전용 점검(접속, sudo, 로그 형식, 이전 릴리스, LB 풀) |
| IF-CLI-12 | `presets` | `[list 또는 show NAME[@V] [--set k=v]...] [--dir DIRS]` | 규칙 프리셋 조회·전개 |
| IF-CLI-13 | `token` | `create --name NAME [--role ROLE] [--scope SCOPE] [--expires YYYY-MM-DD]` | 서비스 계정 토큰 생성(토큰과 SHA-256 출력) |
| IF-CLI-14 | `whoami` | `--server URL` | `VIGILANTE_TOKEN`의 신원·권한 |
| IF-CLI-15 | `audit` | `verify [--file ARCHIVE] [--key REF]`, `export --out F`, `prune --out F (--before DATE 또는 --older-than DUR)`, `query [--actor A] [--action X] [--service S] [--since DATE]` | 감사 체인 검증·내보내기·보존 정리·조회 |
| IF-CLI-16 | `store` | `status`, `migrate [--down-to N --yes]` | PostgreSQL 스키마 상태·마이그레이션 |
| IF-CLI-17 | `feedback` | `--id ID --outcome correct, false_positive, false_negative, unclear [--incident INC] [--note T] [--server URL]` | 판정 평가 |
| IF-CLI-18 | `pilot` | `report [--since DATE] [--until DATE] [--service A,B] [--json] [--out F]` | 판정 품질 보고서와 출시 게이트 |
| IF-CLI-19 | `sudoers` | `[--target HOST] [--no-resolve] [--json]` | `sudo_scope: changes`용 sudoers 규칙 생성 |
| IF-CLI-20 | `lab` | `run --service S --label EQUIPMENT --inject CMD [--reset CMD] [--repeat 3]`, `summary FILE...` | M8 실장비 시나리오 실행·요약 |
| IF-CLI-21 | `support-bundle` | `-c FILE [--server URL] [--log FILE]... [--out F.zip]` | 진단 zip(비밀값 제거) |
| IF-CLI-22 | `plugins` | - | 플러그인과 검증 수준(호환성 매트릭스) |
| IF-CLI-23 | `version` | - | 버전, 빌드 종류(full, minimal), 커밋, 빌드 시각 |

## 8.2 공통 플래그와 환경변수

| 이름 | 종류 | 설명 |
|---|---|---|
| `-c` | 플래그 | 설정 파일(기본 `vigilante.yaml`) |
| `--service`, `--version`, `--previous`, `--id` | 플래그 | 서비스, 신규 버전, 이전 버전, 배포 ID |
| `--server` | 플래그 | 실행 중인 서버에 위임(원격 모드) |
| `--dry-run` | 플래그 | 변경 조치를 로그로만 |
| `--ticket` | 플래그 | 감사 기록에 남길 변경·인시던트 티켓. `watch --server`는 이것을 변경 게이트 티켓으로 전달(본문 `change_ticket`과 `X-Change-Ticket` 헤더, PR #13) |
| `--freeze-override` | 플래그(prepare, watch) | 동결 중 진행 사유. `watch --server`는 본문 `freeze_override`로 전달(서버에서 admin 필요). 로컬은 `auth.local_cli` 제한 시 `--break-glass` 필요 |
| `--break-glass` | 플래그(PR #13) | `auth.local_cli`가 제한일 때 로컬 권한 조작을 실행하는 사유. 감사 `breakglass.<action>`과 critical 알림. 로컬 승인 결정의 4-eyes도 면제 |
| `--key` | 플래그(`audit verify`, PR #13) | 체인 키 참조(`env:NAME`, `file:/path`, `vault:...`). `audit.chain_key_ref` 대신 사용. usage 문자열에는 없고 플래그 도움말에만 있다 |
| `VIGILANTE_TOKEN` | 환경변수 | `--server`, 에이전트용 API 토큰 |
| `VIGILANTE_LOG` | 환경변수 | `debug`, `info`, `warn` |
| `VIGILANTE_LOG_FORMAT` | 환경변수 | `json`이면 JSON 로그 |
| `VIGILANTE_DEPLOYMENT_ID`, `VIGILANTE_VERSION` | 환경변수 | `--id`, `--version` 자동 채움 원천 |
| `VIGILANTE_HA_ADVERTISE_URL`, `VIGILANTE_HA_NODE_ID` | 환경변수 | HA 설정 덮어쓰기 |

입력 자동 채움: `--id`와 `--version`은 CI 실행 정보에서 자동으로 채운다(Jenkins `BUILD_TAG`·`GIT_COMMIT`, GitLab `CI_PIPELINE_ID`·`CI_COMMIT_TAG`/`CI_COMMIT_SHORT_SHA`, GitHub `GITHUB_RUN_ID`·`GITHUB_SHA`, 또는 `VIGILANTE_DEPLOYMENT_ID`·`VIGILANTE_VERSION`, 없으면 `git describe`). `--previous`는 저널에 기록된 그 서비스의 마지막 성공 배포 버전을 쓴다. 직접 지정한 플래그가 항상 우선한다.

## 8.3 종료 코드

| 코드 | 의미 | 반환 명령 |
|---|---|---|
| 0 | 통과(단계 승격), 정상 처리 | 전 명령 |
| 1 | 오류(설정·인자·연결 오류, 감사 체인 손상 `audit verify`, doctor 실패 항목, `auth.local_cli` 제한으로 거부된 로컬 승인·서킷 조작, 로컬 4-eyes 위반) | 전 명령 |
| 2 | 판정 FAIL 후 롤백 완료 | `watch`, `rollback`, `status` |
| 3 | 롤백 실패, 서킷 OPEN, 승인 대기, 변경 동결·변경 티켓 게이트 거부, 로컬 `--freeze-override` 거부. `watch --server`는 서버 응답 `code`가 `circuit_open`·`change_frozen`·`change_ticket_invalid`·`itsm_unavailable`이거나 `code` 없는 409(이전 서버)일 때 3, 그 밖의 생성 실패는 1(PR #13 이전에는 원격 게이트 거부도 1) | `watch`, `prepare`, `rollback`, `status`, `circuit`(OPEN 상태) |
| 4 | HOLD(보류)·판정 불가 | `watch`, `status` |
| 4 | 스키마가 최신이 아님(적용 대기 또는 더 새로운 스키마) | `store status` |
| 4 | 출시 게이트 미달 | `pilot report` |
| 4 | 랩 실행 중 실패가 있음 | `lab run` |

> 종료 코드 0~4의 의미는 같은 MAJOR 안에서 고정한다(docs/08-upgrade.md 호환성 약속).

## 8.4 로컬 CLI 권한 제한 (`auth.local_cli`, PR #13)

`--server` 없이 실행하는 CLI는 저장소를 직접 다루므로 API의 역할·4-eyes를 거치지 않는다. 인증이 설정된 환경에서는 다음 명령을 서버(`--server`, operator·admin 토큰)로 실행하거나 `--break-glass REASON`을 붙여야 한다.

| `auth.local_cli` | 동작 |
|---|---|
| `auto`(기본) | `server.auth_token_env`, `auth.service_accounts`, `auth.oidc` 중 하나라도 있으면 `restricted`, 없으면 `full` |
| `restricted` | 아래 조작은 `--break-glass` 없으면 거부 |
| `full` | 제한 없음(PR #13 이전 동작) |

| 제한 대상 | 감사 action(`--break-glass` 사용 시) |
|---|---|
| `rollback --id ID --approve`·`--reject`(승인 모드 대기 배포의 결정) | `breakglass.rollback.approve`, `breakglass.rollback.reject` |
| `rollback ... --approve`(승인 필요 에스컬레이션 승인) | `breakglass.escalation.approve` |
| `circuit reset`, `circuit trip` | `breakglass.circuit.reset`, `breakglass.circuit.trip` |
| 활성 동결 중 `watch`·`prepare --freeze-override` | `breakglass.freeze.override` |

- 거부 메시지: "`<action>` refused: the API has authentication, so privileged local commands are restricted (auth.local_cli). Run it through the server with --server and an operator or admin token, or pass --break-glass REASON (audited and alerted)".
- `--break-glass` 사용 시 감사 기록(`source: cli`, `reason` = 사유)과 함께 critical 알림 "Break-glass: <작업자> ran <action> locally"를 보낸다. 원래 동작의 감사 기록(예: `circuit.reset`)도 그대로 남는다.
- `auth.four_eyes`가 켜져 있으면 로컬 승인 결정도 배포 생성자·롤백 요청자가 할 수 없다(`--break-glass`면 예외).
- `circuit status`, 조회 명령, `watch`(동결 예외 없이)는 제한하지 않는다.

---

# 9. 외부 시스템 인터페이스

## 9.1 총괄

| ID | 시스템 | 방향 | 프로토콜·포트 | 인증 | 사용 구성요소 | 검증 수준 |
|---|---|---|---|---|---|---|
| IF-EXT-01 | 대상 호스트 (SSH, sudo) | 나감 | SSH 22 (bastion 경유 가능) | 키, 키 패스프레이즈, ssh-agent, 비밀번호, Vault SSH CA 단기 인증서. 호스트 키 검증 | 프로브, symlink·exec·kvm 실행기, nginx·envoy·haproxy 제어기 | 실험적(모의 SSH 실행기) |
| IF-EXT-02 | Docker Engine API (Podman 호환) | 나감 | HTTP over UNIX 소켓(SSH 터널 가능) 또는 TCP, API `v1.41` | 소켓 접근 권한 | container 실행기, docker 프로브 | 실험적(모의 API) |
| IF-EXT-03 | F5 BIG-IP iControl REST | 나감 | HTTPS 443 | Basic 또는 토큰 로그인(`X-F5-Auth-Token`) | f5 트래픽 제어기 | 실험적(모의 iControl) |
| IF-EXT-04 | AWS ELBv2 (ALB·NLB) | 나감 | HTTPS (AWS SDK v2) | AWS 기본 자격증명 체인(리전·프로필 지정 가능) | aws_alb 트래픽 제어기 (최소 빌드 제외) | 실험적(모의 API) |
| IF-EXT-05 | VMware vSphere | 나감 | HTTPS 443, vSphere Web Services API (govmomi) | 사용자·비밀번호 | vsphere 실행기 (최소 빌드 제외) | 실험적(vcsim) |
| IF-EXT-06 | Nutanix Prism Element v2.0 | 나감 | HTTPS 9440 | Basic | nutanix 실행기 | 실험적(모의 API) |
| IF-EXT-07 | KVM libvirt (virsh) | 나감 | SSH로 하이퍼바이저 호스트에서 `virsh` | IF-EXT-01 | kvm 실행기 | 실험적(모의 virsh) |
| IF-EXT-08 | OpenStack Keystone·Nova·Cinder·Glance | 나감 | HTTPS (Keystone v3) | application credential 또는 비밀번호 → `X-Auth-Token` | openstack 실행기 | 실험적(상태 있는 모의 서버) |
| IF-EXT-09 | OpenStack Octavia v2 | 나감 | HTTPS | IF-EXT-08과 같은 Keystone 토큰 | octavia 트래픽 제어기 | 실험적(상태 있는 모의 서버) |
| IF-EXT-10 | HAProxy Runtime API | 나감 | UNIX 소켓(SSH stream-local 전달) 또는 TCP | 소켓 권한 | haproxy 트래픽 제어기 | 실험적(모의 소켓) |
| IF-EXT-11 | Nginx·Envoy 설정 파일 | 나감 | SSH로 파일 교체와 명령 실행 | IF-EXT-01 | nginx·envoy 트래픽 제어기 | 실험적 |
| IF-EXT-12 | HashiCorp Vault | 나감 | HTTPS 8200 (`/v1/...`) | token, AppRole, Kubernetes | 비밀값 해석, SSH CA 서명 | 모의 서버 단위 테스트 |
| IF-EXT-13 | OIDC IdP | 나감 | HTTPS (discovery, JWKS, 토큰 엔드포인트) | 콘솔 클라이언트 ID·비밀 | API 인증, 웹 콘솔 로그인 | 테스트용 IdP(`oidctest`) |
| IF-EXT-14 | ServiceNow Table API | 나감 | HTTPS 443 | Basic(통합 사용자) 또는 Bearer | 변경 게이트, 인시던트, 작업 노트 | 모의 서버(`snowtest`) |
| IF-EXT-15 | 알림 채널 (Teams, Slack, SMTP, PagerDuty, 범용 웹훅) | 나감 | HTTPS, SMTP 587·465 | 웹훅 URL(비밀), SMTP PLAIN, PagerDuty routing key | 알림 | 모의 서버 단위 테스트 |
| IF-EXT-16 | PostgreSQL | 나감 | PostgreSQL 프로토콜 5432 (pgx v5) | DSN(`dsn_ref`, `dsn_env`) | 상태 저장소 | 실제 PostgreSQL(로컬 임베디드, CI `postgres:16`) |
| IF-EXT-17 | SIEM (syslog) | 나감 | TCP·UDP syslog 또는 TLS syslog(RFC 5425, 기본 6514), RFC 5424(JSON 본문) 또는 CEF | 없음(TCP·UDP), 서버 인증서 검증과 선택적 클라이언트 인증서(TLS) | 감사 기록 실시간 전송 | 단위 테스트(TLS 수집기 모의 포함) |
| IF-EXT-18 | 범용 배포 웹훅 (webhook 실행기) | 나감 | HTTP(S) | Bearer 또는 Basic | webhook 실행기 | 검증됨(E2E 데모) |

> 검증 수준은 `docs/09-compatibility.md`(바이너리의 `vigilante plugins`와 동일)를 따른다. 실제 장비 검증은 M8 랩에서 수행하며 현재 "검증됨"은 http·access_log·log 프로브와 webhook 실행기뿐이다.

## 9.2 공통 실패 처리 (롤백 경로)

| 항목 | 동작 | 근거 |
|---|---|---|
| 단계 재시도 | 롤백 계획의 각 단계(드레인, 복원, 확인, 복귀)는 시도마다 `step_timeout`(기본 2분)을 두고 최대 `retry.attempts`(기본 3)회, `backoff` 2초에서 2배씩 최대 30초 간격으로 재시도 | `orchestrator/rollback.go`, `config/validate.go` |
| 상위 전략 | 주 실행기가 실패하면 `escalation` 사다리(예: 컨테이너 전환 → VM 스냅샷)로 넘어감. `require_approval` 단계는 승인 대기 | `rollback.go escalate` |
| 실패 시 격리 | 롤백이 끝내 실패하면 `traffic.enable`을 실행하지 않아 대상이 드레인(격리) 상태로 남음. 상태 `ROLLBACK_FAILED`, 이벤트 `rollback.failed` | `rollback.go` |
| 서킷 브레이커 | 롤백 실패가 기간 내 N회 쌓이면 서킷 OPEN, 새 배포·자동 조치 거부 | `safety` |
| 자동 롤백 차단 시 | 서킷 OPEN·플래핑으로 자동 롤백이 막히면 위반 대상만 blast radius 안에서 격리 | `isolateBreaches` |
| 멱등 재개 | 각 실행기의 롤백은 멱등. 크래시·리더 교체 후 완료된 단계는 건너뛰고 이어서 실행 | `TestResumeSkipsCompletedSteps`, `TestHAFailoverFinishesInterruptedRollback` |
| 드라이런 | `dry_run`이면 변경 호출 대신 로그만 남김(조회 호출은 실행) | 각 실행기 |
| SSH 세션 예산 | 대상마다 `max_sessions`(기본 8) 중 `reserved_sessions`(기본 2)는 롤백·드레인·복귀 전용 | `transport/sessions.go` |
| 비롤백 경로 | ITSM, 알림, SIEM 전송의 실패는 롤백을 막지 않음 | `itsm`, `notify`, `audit` |

## 9.3 IF-EXT-01 대상 호스트 (SSH, sudo)

| 항목 | 내용 |
|---|---|
| 연결 | 대상별 SSH 연결을 풀링해 프로브가 한 세션을 공유. 재사용 전 `keepalive@vigilante` 요청으로 생존 확인, 실패 시 재접속. `connection.bastion`이면 다른 대상을 점프 호스트로 경유. 접속 타임아웃 `connection.timeout` |
| 인증 | 우선순위대로 결합: Vault SSH CA 단기 인증서(임시 ed25519 키를 Vault가 서명, 수명 80% 경과 시 재발급) → 개인 키(`private_key_ref` 또는 `private_key_file`, 패스프레이즈) → ssh-agent(`use_ssh_agent`) → 비밀번호(`password_ref`/`password_env`) |
| 호스트 키 | `known_hosts_file`(기본 `~/.ssh/known_hosts`)로 검증. `insecure_ignore_host_key`는 시험용 |
| 권한 상승 | `sudo: true` + `sudo_scope: changes`: 변경 명령만 `sudo -n <명령>`. `sudo_scope` 생략(all): 모든 명령을 `sudo -n sh -c '<명령>'`으로 실행(사실상 root 필요) |
| 사용 명령 (읽기) | `readlink -f`, `systemctl is-active`, `cat <파일>`, `tail -n0 -F <로그>`(원격 로그 추적, `remote_grep` 시 `grep --line-buffered -E`), `/proc` 읽기, `virsh domstate` |
| 사용 명령 (변경) | symlink: `ln -sfn <릴리스> <링크>.vigilante-tmp` + `mv -Tf`(AIX·Solaris는 `ln -sfn`), `systemctl restart <unit>` 또는 `/etc/init.d/<unit> restart`. exec: 설정된 `prepare`·`rollback`·`verify` 명령 |
| 실패 처리 | 명령 실패는 종료 코드·출력과 함께 단계 오류로 보고, 9.2의 재시도 적용. 세션 예산이 차면 수집은 대기, 롤백은 예약 세션 사용 |

## 9.4 IF-EXT-02 Docker Engine API

| 항목 | 내용 |
|---|---|
| 연결 | 원시 HTTP, API 버전 `v1.41`. `socket`(예: `/var/run/docker.sock`, `/run/podman/podman.sock`)을 대상 SSH 연결로 터널링하거나 `host: tcp://h:p` |
| 사용 연산 | `GET /containers/{name}/json`, `POST /containers/{id}/stop?t=`, `POST /containers/{id}/rename?name=`, `POST /containers/create?name=`(기존 컨테이너의 설정·호스트 설정·네트워크를 복제하고 이미지만 이전 태그), `POST /containers/{id}/start`, `DELETE /containers/{id}?force=`, `GET /images/{ref}/json`, `POST /images/create?fromImage=&tag=`(`pull: true`), 이벤트 스트림(docker 프로브) |
| 인증 | 소켓 접근 권한(Docker 소켓 접근은 root와 동등. rootless Podman 권장) |
| 실패 처리 | 새 컨테이너 시작이 실패하면 옛 컨테이너 이름을 되돌리고 재시작(보상). 호스트에 컨테이너가 없는 상태를 남기지 않음 (`TestContainerCompensatesOnStartFailure`) |

## 9.5 IF-EXT-03 F5 BIG-IP iControl REST

| 항목 | 내용 |
|---|---|
| 사용 연산 | 풀 조회 `GET /mgmt/tm/ltm/pool/~<파티션>~<풀>/members`. 드레인 `PATCH .../members/~<파티션>~<멤버>` `{"session": "user-disabled"}`(`force_offline`이면 `"state": "user-down"` 추가). 복귀 `{"session": "user-enabled", "state": "user-up"}` |
| 인증 | 기본 HTTP Basic. `token_auth: true`면 `POST /mgmt/shared/authn/login`(`loginProviderName: tmos`)으로 토큰을 받아 `X-F5-Auth-Token` 사용, 15분 후 재발급 |
| 연결 | HTTP 타임아웃 30초. `tls_skip_verify`는 자체 서명 인증서 시험용 |
| 실패 처리 | 응답 상태 300 이상이면 오류(본문 포함). 멤버별로 진행하고 오류를 모아 반환 → 9.2 재시도 |
| 권장 권한 | 대상 파티션 Operator 역할(멤버 활성·비활성) |

## 9.6 IF-EXT-04 AWS ELBv2

| 항목 | 내용 |
|---|---|
| 사용 연산 | `DescribeTargetHealth`(풀·상태), `DeregisterTargets`(드레인), `RegisterTargets`(복귀). 대상 ID는 `target_id` 템플릿(기본 `{{.Labels.instance_id}}`), 포트 선택 |
| 인증 | AWS SDK 기본 자격증명 체인. 자격증명의 `region`, `profile` 지정 가능 |
| 대기 | 드레인 후 상태 `unused`까지, 복귀 후 `healthy`까지 `wait_timeout` 동안 폴링(5초 간격) |
| 실패 처리 | 드레인 대기 시간 초과는 경고 후 진행(새 요청은 이미 받지 않음). 복귀 대기 시간 초과는 오류 |
| 권장 권한 | `elasticloadbalancing:RegisterTargets`, `DeregisterTargets`(대상 그룹 한정), `DescribeTargetHealth` |

## 9.7 IF-EXT-05 VMware vSphere

| 항목 | 내용 |
|---|---|
| 사용 연산 | 로그인(govmomi), 기본 데이터센터의 VM 검색, 스냅샷 생성 `CreateSnapshot(이름, "vigilante pre-deploy checkpoint", 메모리 제외, quiesce)`, 복원 `RevertToSnapshot`, 전원 상태 조회·`PowerOn`(`power_on: true`), 로그아웃 |
| 인증 | 자격증명의 사용자·비밀번호를 URL 사용자 정보로 전달 |
| 실패 처리 | 작업(Task) 완료를 기다려 실패 시 오류. 확인 단계에서 전원이 켜지지 않았으면 오류 |
| 제약 | 최소 빌드에서 제외. doctor는 로그인·VM 조회만 확인하고 스냅샷 권한은 변경 없이 확인할 수 없음(경고) |

## 9.8 IF-EXT-06 Nutanix Prism Element v2.0

| 항목 | 내용 |
|---|---|
| 기본 경로 | `<url>/PrismGateway/services/rest/v2.0` |
| 사용 연산 | 스냅샷 생성 `POST /snapshots/`, 스냅샷 조회 `GET /snapshots/?vm_uuid=`, 복원 `POST /vms/{uuid}/restore`(`restore_network_configuration: true`), 전원 `POST /vms/{uuid}/set_power_state {"transition": "ON"}`, 작업 상태 `GET /tasks/{uuid}`(2초 폴링), VM 조회 `GET /vms/{uuid}` |
| 인증 | HTTP Basic, 타임아웃 30초 |
| 실패 처리 | 작업 `failed`·`aborted`면 오류. 확인 단계에서 `power_state`가 `on`이 아니면 오류 |
| 제약 | AOS 5.x·6.x 경로 기준. AOS 릴리스별 확인 필요(M8 랩) |

## 9.9 IF-EXT-07 KVM libvirt

| 항목 | 내용 |
|---|---|
| 사용 명령 | `virsh snapshot-create-as --domain <D> --name <S> --description "vigilante pre-deploy checkpoint" --atomic`, `virsh snapshot-revert --domain <D> --snapshotname <S> --running --force`, `virsh domstate <D>` |
| 실행 위치 | `kvm.hypervisor` 대상(SSH) |
| 인증·권한 | IF-EXT-01. `libvirt` 그룹이면 sudo 불필요 |
| 실패 처리 | 확인 단계에서 상태가 `running`이 아니면 오류 |

## 9.10 IF-EXT-08 OpenStack (Keystone·Nova·Cinder·Glance)

| 항목 | 내용 |
|---|---|
| 인증 | Keystone v3 `POST <auth_url>/auth/tokens`. application credential(id + `application_credential_secret_ref`) 또는 비밀번호(사용자·도메인·프로젝트 범위). 응답 `X-Subject-Token`과 서비스 카탈로그 캐시, 만료 5분 전 재발급, 401이면 1회 재인증 |
| 엔드포인트 | 카탈로그에서 서비스 유형(`compute`, `volumev3`/`block-storage`/`volume`, `image`, `load-balancer`)과 `interface`(기본 public)·`region`으로 선택 |
| 볼륨 부팅 서버 | 준비: Cinder `POST /snapshots`(`force: true`) 후 상태 대기. 롤백: Nova `os-stop` → Cinder `POST /volumes/{id}/action {"revert": {"snapshot_id"}}`(`OpenStack-API-Version: volume 3.40`) → 상태 대기 → 볼륨 메타데이터 `vigilante.reverted_to` 기록 → `os-start` |
| 이미지 부팅 서버 | 준비: Nova `POST /servers/{id}/action {"createImage"}`(compute 2.45) 후 Glance `GET /v2/images/{id}` 대기. 롤백: `rebuild {"imageRef"}`(IP·포트·메타데이터 유지) |
| 기타 연산 | `GET /servers/{id}`, `GET /volumes/{id}`, 오래된 스냅샷·이미지 정리(`keep_snapshots`), 쿼터 조회 `GET /os-quota-sets/{project}?usage=true`(doctor) |
| 실패 처리 | 이미 복원된 볼륨·이미지는 건너뜀(멱등). `os-stop`·`os-start`의 409는 무시. Cinder가 revert를 거부하면 마이크로버전·백엔드 지원 필요를 알리는 오류. 상태 폴링 3초 간격, `revert_timeout` 초과 시 실패. HTTP 타임아웃 60초 |
| 제약 | Cinder revert-to-snapshot은 백엔드·릴리스에 따라 사용 중 볼륨을 거부할 수 있어 M8 랩에서 확정 |

## 9.11 IF-EXT-09 OpenStack Octavia

| 항목 | 내용 |
|---|---|
| 사용 연산 | `GET /v2/lbaas/pools/{pool}/members`, `GET /v2/lbaas/pools/{pool}`(로드밸런서 확인), `GET /v2/lbaas/loadbalancers/{lb}`(ACTIVE 대기), `PUT /v2/lbaas/pools/{pool}/members/{id} {"member": {"admin_state_up": false 또는 true}}`, 멤버 `operating_status` 조회 |
| 동작 | 변경 전 로드밸런서 ACTIVE 대기, 멤버를 하나씩 변경, 복귀 시 멤버가 `ONLINE` 또는 `NO_MONITOR`가 될 때까지 대기 |
| 실패 처리 | `PENDING_*` 중 409는 대기 후 재시도. 이미 원하는 상태인 멤버는 건너뜀. 풀에 없는 멤버, `operating_status: ERROR`, `wait_timeout`(기본 5분) 초과는 오류 |

## 9.12 IF-EXT-10 HAProxy Runtime API

| 항목 | 내용 |
|---|---|
| 연결 | LB 호스트의 UNIX 소켓(`socket`, 예: `/run/haproxy/admin.sock`)을 SSH stream-local 전달로, 또는 TCP `address`. 요청마다 연결·명령 1줄·응답 읽기, 기본 10초 기한 |
| 사용 명령 | `show servers state <backend>`(풀), `set server <backend>/<server> state drain`, `drain_to_maint`면 `drain_wait` 후 `state maint`, 복귀 `state ready` |
| 실패 처리 | 빈 응답이 아니면 오류로 간주. 여러 LB 호스트에 각각 전송하고 오류를 모아 반환 |

## 9.13 IF-EXT-11 Nginx·Envoy 설정 파일

| 항목 | 내용 |
|---|---|
| Nginx | `upstream_file`을 읽어 대상 `server` 줄에 `down` 표시/해제한 새 파일을 쓴 뒤: 백업(`.vigilante-bak`) → 새 파일(`.vigilante-new`)로 교체 → `test_cmd`(기본 `nginx -t`) 실패 시 백업 복원 후 오류 → `reload_cmd`(기본 `nginx -s reload`) |
| Envoy | 파일 기반 EDS(`eds_file`) JSON에서 엔드포인트 `health_status`를 `DRAINING`으로 바꾸고 `version_info` 갱신, 임시 파일을 `mv`로 원자 교체(Envoy 파일 감시가 재시작 없이 반영) |
| 멱등 | 이미 원하는 상태면 쓰지 않음 |
| 권한 | `sudo_scope: changes`면 `tee`, `cp`, `mv`, `nginx -t`, `nginx -s reload`만 sudo |

## 9.14 IF-EXT-12 HashiCorp Vault

| 항목 | 내용 |
|---|---|
| 참조 형식 | `vault:<mount>/<path>#<key>`(KV v2), `env:NAME`, `file:/path` |
| 사용 연산 | 로그인 `POST /v1/auth/<mount>/login`(AppRole: `role_id`·`secret_id`, Kubernetes: `role`·서비스 계정 JWT), KV 읽기 `GET /v1/<mount>/data/<path>`, SSH CA 서명 `POST /v1/<mount>/sign/<role>` |
| 인증 | `token`(기본 `VAULT_TOKEN`), `approle`, `kubernetes`. `X-Vault-Token`, Enterprise는 `X-Vault-Namespace` |
| 캐시 | 해석한 값은 메모리에 `cache_ttl`(기본 5분) 보관, 디스크에 쓰지 않음. 해석된 값은 로그에서 가림 |
| 실패 처리 | 로그인 기반 방식에서 403이면 1회 재로그인 후 재시도. 그 외 오류는 그 비밀값을 쓰는 작업의 오류 |

## 9.15 IF-EXT-13 OIDC IdP

| 항목 | 내용 |
|---|---|
| API 인증 | 서버 시작 시 `auth.oidc.issuer`의 discovery 문서를 가져와 검증기 구성(서버에서 IdP 접근 필요). JWT 서명·만료·`aud`(=`audience`) 검증. 사용자는 `username_claim`(기본 `preferred_username`), 그룹은 `groups_claim`(기본 `groups`) |
| 권한 매핑 | `auth.role_bindings`의 그룹·사용자 → 역할 grant |
| 콘솔 로그인 | 인가 코드 + PKCE(S256). `/console/auth/login` → IdP → `/console/auth/callback`에서 `state` 확인·코드 교환. ID 토큰을 AES-GCM으로 암호화한 `vgl_session` 쿠키(HttpOnly, SameSite=Lax)와 CSRF 쿠키(SameSite=Strict) 발급 |
| 실패 처리 | 검증 실패는 `401 unauthenticated`. 역할 바인딩이 없는 사용자의 콘솔 로그인은 거부하고 감사 기록 |

## 9.16 IF-EXT-14 ServiceNow Table API

| 항목 | 내용 |
|---|---|
| 변경 게이트 | `GET /api/now/table/change_request?sysparm_query=number=<번호>` 후 `approval == approved`, `state`가 `allowed_states`에 포함, `check_window`(기본 켜짐)면 현재가 `start_date`~`end_date`(UTC) 안인지 확인 |
| 인시던트 | 롤백 실패(`rollback_failed`)·서킷 OPEN(`circuit_opened`) 시 `GET /api/now/table/incident?sysparm_query=correlation_id=<ID>^active=true`로 중복 확인 후 `POST /api/now/table/incident` |
| 작업 노트 | 배포 이벤트(관측 시작·통과·실패·보류, 롤백 시작·완료·실패, 승인 요청·결정)를 `PATCH /api/now/table/change_request/<sys_id> {"work_notes"}` |
| 인증 | 자격증명 `type: token`이면 Bearer, 그 외 Basic(통합 사용자). 타임아웃 15초 |
| 실패 처리 | 변경 게이트: 불통 시 `change_gate.on_error`(기본 `closed`)가 `closed`면 배포 거부(`503 itsm_unavailable`), `open`이면 진행하고 티켓을 `unverified`로 표시. 인시던트·작업 노트: 리더에서만. PR #13부터 HTTP 상태로 판단해 연결 오류·시간 초과·429·5xx만 1초·2초·4초 후 재시도(최대 4회 시도, 시도당 30초 제한), 그 밖의 4xx는 즉시 중단. 인시던트는 백그라운드로 생성하며 재시도해도 `correlation_id` 조회로 중복이 생기지 않음. 실패는 로그와 지표(`vigilante_itsm_calls_total{kind,result}`, `result`: `ok`, `error`, `retry`)만 남기고 롤백을 막지 않음. (PR #13 이전: 0초·2초·10초 3회, 오류 문자열로 4xx 판단) |

## 9.17 IF-EXT-15 알림 채널

| 채널 | 프로토콜·형식 | 인증·비밀 |
|---|---|---|
| Teams | HTTPS POST, Adaptive Card 1.4 메시지(Workflows 웹훅) | URL(`url_ref`, `url_env`, `url`) |
| Slack | HTTPS POST `{"text"}` (수준 아이콘) | URL |
| 범용 웹훅 | HTTPS POST, Message JSON(`level`, `title`, `text`, `deployment`, `service`) | URL |
| PagerDuty | HTTPS POST `https://events.pagerduty.com/v2/enqueue`, Events v2 `trigger`, `dedup_key = vigilante:<배포 ID>:<제목>` | routing key(`routing_key_ref`, `routing_key_env`) |
| 이메일 | SMTP, STARTTLS 기본(465 또는 `implicit_tls`면 암묵 TLS), TLS 1.2 이상 | 선택적 PLAIN 인증 |

| 항목 | 내용 |
|---|---|
| 라우팅 | 채널별 `min_level`(info, warning, critical), `services`, `teams`. 서비스 없는 전역 알림(서킷)은 모든 채널 |
| 중복 억제 | 같은 채널·배포·수준·제목은 10분에 한 번 |
| 실패 처리 | 최선 노력. 전송 타임아웃 10초(HTTP 클라이언트 5초), 실패는 경고 로그만 남기고 롤백을 막지 않음 |

## 9.18 IF-EXT-16 PostgreSQL (상태 저장소)

| 항목 | 내용 |
|---|---|
| 연결 | pgx v5 연결 풀. DSN은 `state.dsn_ref`(Vault 등) 또는 `state.dsn_env` 권장. `sslmode=verify-full` 권장 |
| 사용 연산 | 이벤트 추가(`INSERT`, 체인 잠금 advisory lock 하에서 직전 해시 조회), 전체 재생(`SELECT ... ORDER BY seq`), 리스 획득·갱신(`INSERT ... ON CONFLICT DO UPDATE ... WHERE owner = ... OR expires_at <= now()`), 리스 해제·조회, 보존 정리(아카이브 후 `DELETE` + 앵커 `INSERT`), 스키마 마이그레이션 |
| 일관성 | 리스 만료는 DB 시계(`now()`) 기준. 리더 쓰기는 펜싱: 리스를 잃은 노드의 `INSERT`는 0행이 되어 `ErrFenced` |
| 실패 처리 | `/readyz` 503(2초 제한). 기록 1건의 추가 제한 시간 5초. 펜싱 외 쓰기 실패는 메모리 대기열(최대 100,000건, 초과분은 버리고 `vigilante_store_errors_total{reason="dropped"}` 계수)에 순서대로 보관 후 저장소 복구 시 200밀리초~5초 백오프로 기록(`internal/orchestrator/engine.go` `record`·`flush`, 지표 `vigilante_store_pending_writes`). M5-4 커밋 `9f9470e`(PR #12, 병합 `dba3dbe`). 롤백 시작 시 서비스 lease를 못 잡으면 `safety.rollback_lease.wait`(10초) 재시도 후 `on_unavailable`(기본 `proceed`)에 따라 lease 없이 진행하거나 거부(PR #13, 10장). 상세는 VGL-SI-05 |
| 체인 키 | `audit.chain_key_ref`가 있으면 기록 본문(jsonb)에 `mac`이 들어간다. 스키마는 바뀌지 않는다(PR #13) |

## 9.19 IF-EXT-17 SIEM (syslog), IF-EXT-18 범용 배포 웹훅

| 항목 | IF-EXT-17 SIEM | IF-EXT-18 webhook 실행기 |
|---|---|---|
| 방식 | `audit.syslog.address`(`tcp://host:port`, `udp://host:port`, `tls://host[:port]`), `format` rfc5424(JSON 본문, 기본) 또는 cef. tcp·udp는 메시지마다 한 줄(`\n`), tls는 RFC 5425 옥텟 카운팅 프레임(`<길이> <메시지>`), 포트 생략 시 6514 | 템플릿 URL·본문·헤더로 HTTP 요청(`method` 기본 POST, 본문이 있으면 JSON), 확인 `GET verify_url` |
| 인증 | tcp·udp 없음. tls는 `audit.syslog.tls`: `ca_file`(수집기 사설 CA, 없으면 시스템 신뢰 저장소), `cert_file`·`key_file`(클라이언트 인증서, 둘 다 지정), `server_name`(기본 주소의 호스트), `min_version` 1.2(기본)·1.3. 연결 제한 시간 5초에 TLS 핸드셰이크 포함 | 자격증명 토큰이면 Bearer, 아니면 Basic |
| 실패 처리 | 저장 후 비동기 전송. 큐가 차면 버리고 개수 집계(`vigilante_audit_export_dropped_total`), 재연결 백오프. 신뢰할 수 없는 수집기 인증서면 연결하지 않음. 빠진 구간은 `audit export`로 보완 | 상태 300 이상이면 오류, 타임아웃 60초 → 9.2 재시도 |
| 검증 | `TestSyslogExporter`, `TestSyslogExporterTLS`, `TestSyslogExporterTLSRejectsUntrustedCollector`, `TestNewExporterTLSConfig` | `TestWebhookExecutor` |

---

# 10. 설정 키와 운영 지표 (PR #13 추가·변경분)

운영자가 설정 파일(`vigilante.yaml`)과 Prometheus로 다루는 인터페이스 중 PR #13에서 추가·변경된 것이다. 설정은 추가만 했으므로 기존 설정은 그대로 유효하다(알 수 없는 키는 거부되므로 새 키는 이 버전 이상에서만 쓸 수 있다).

## 10.1 설정 키

| ID | 키 | 형식·값 | 기본값 | 검증(`vigilante validate`)·비고 |
|---|---|---|---|---|
| IF-CFG-01 | `safety.observer_guard.disabled` | bool | `false`(켜짐) | 관측 장치 가드 끄기. 저하 중 HOLD하는 위반은 오케스트레이터가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, `host`)의 `up`·`latency_ms`·`consecutive_failures`·`consecutive_timeouts`·`timeout`과 모든 프로브의 `probe_error`에 기반한 것뿐이다. `log`·`access_log`·`docker` 프로브의 같은 이름 지표는 그대로 판정(PR #14, 설정 키 변경 없음) |
| IF-CFG-02 | `safety.observer_guard.max_lag` | duration | `1s` | 250ms 내부 타이머가 이보다 늦게 깨어나면 저하 |
| IF-CFG-03 | `safety.observer_guard.loopback_timeout` | duration | `1s` | 프로세스 내 TCP 에코 왕복 제한 |
| IF-CFG-04 | `safety.observer_guard.timeout_share` | 실수 (0, 1] | `0.5` | 범위 밖이면 거부 |
| IF-CFG-05 | `safety.observer_guard.min_services` | 정수 ≥ 1 | `3` | 1 미만이면 거부 |
| IF-CFG-06 | `safety.observer_guard.grace` | duration ≥ 0 | `1m` | 회복 후 계속 HOLD하는 시간. 음수면 거부 |
| IF-CFG-07 | `safety.rollback_lease.wait` | duration | `10s` | 저장소에 닿지 않을 때 서비스 lease 재시도 시간 |
| IF-CFG-08 | `safety.rollback_lease.on_unavailable` | `proceed`, `fail` | `proceed` | 그 밖의 값 거부. `proceed`: 프로세스 내 락으로 롤백 진행(감사 `lease.unavailable`, 경고 알림), `fail`: ROLLBACK_FAILED |
| IF-CFG-09 | `auth.local_cli` | `auto`, `full`, `restricted` | `auto` | 그 밖의 값 거부. 8.4 |
| IF-CFG-10 | `audit.chain_key_ref` | 비밀 참조(`vault:`, `env:`, `file:`) | 없음(키 체인 꺼짐) | 참조 형식 검증. 해석한 키가 32바이트 미만이면 저장소를 여는 시점에 오류. 저장소에 쓰는 모든 노드가 같은 키를 써야 하며, 설정하면 저장소를 여는 모든 명령이 키를 해석할 수 있어야 함 |
| IF-CFG-11 | `audit.syslog.address` | `tcp://host:port`, `udp://host:port`, `tls://host[:port]` | - | `tls`는 호스트 이름 필수, 포트 생략 시 6514. `format` 기본 `rfc5424` |
| IF-CFG-12 | `audit.syslog.tls` | `{ca_file, cert_file, key_file, server_name, min_version}` | 없음(시스템 신뢰 저장소, 서버 이름 = 주소 호스트, TLS 1.2) | `tls://` 주소에서만 허용, `cert_file`·`key_file`은 함께, `min_version`은 `1.2`·`1.3` |
| IF-CFG-13 | `server.ha.tls` | `{ca_file, server_name, cert_file, key_file}` | 없음(시스템 신뢰 저장소, advertise URL의 호스트로 검증) | 팔로워가 https advertise URL의 리더로 전달할 때 사용. 인증서·CA 파일 오류는 서버 시작 실패. `cert_file`·`key_file`은 리더가 `client_auth: require`일 때 |

Helm 차트 values(`deploy/helm/vigilante/values.yaml`, PR #13):

| 값 | 기본값 | 설명 |
|---|---|---|
| `auth.allowAnonymous` | `false` | `config`에 인증(서비스 계정, OIDC, `server.auth_token_env`)이 없으면 렌더링 실패. 개발용으로만 `true` |
| `tls.enabled`, `tls.secretName` | `false`, `""` | `config`의 `server.tls.cert_file`로 자동 감지(`existingConfigMap`이면 직접 설정). Secret을 `/etc/vigilante-tls`에 마운트하고 advertise URL·프로브·포트 이름·Ingress 백엔드 포트를 https로 |
| `serviceMonitor.tlsConfig` | `{}` | TLS 사용 시 Prometheus가 파드 인증서를 검증하는 방법 |
| `resources` | 요청 cpu 100m·memory 512Mi, 한도 memory 2Gi | 이전 128Mi/512Mi |
| `goMemLimit` | `""`(한도의 90%) | `off`면 설정 안 함, 그 밖의 값은 그대로 `GOMEMLIMIT` |

## 10.2 지표

| ID | 지표 | 종류·레이블 | 의미 |
|---|---|---|---|
| IF-MET-01 | `vigilante_observer_degraded` | 게이지 | 관측 장치가 저하된 동안 1 |
| IF-MET-02 | `vigilante_observer_degradations_total` | 카운터 `{signal}` | 저하 진입 횟수. `signal`은 첫 사유: `scheduling lag`, `loopback`, `spread` |
| IF-MET-03 | `vigilante_observer_holds_total` | 카운터 | 관측 장치 저하로 FAIL 대신 HOLD한 위반 수 |
| IF-MET-04 | `vigilante_itsm_calls_total` | 카운터 `{kind, result}` | ServiceNow 호출(`kind`: `incident`, `work_note`). `result`: `ok`, `error`(재시도 후 최종 실패), `retry`(반복 시도마다, PR #13 추가) |
| IF-MET-05 | `vigilante_store_pending_writes` | 게이지 | 저장소 불통 중 대기열의 기록 수(PR #12) |
| IF-MET-06 | `vigilante_store_errors_total` | 카운터 `{reason}` | `error`(대기열에 넣고 재시도), `dropped`(대기열 가득 참), `fenced`(리더 상실)(PR #12) |

## 10.3 새 감사 action (SIEM 수신 측)

| action | 출처(`source`) | 발생 |
|---|---|---|
| `lease.unavailable` | `system` | 저장소 불통으로 lease 없이 롤백 진행(`reason`에 상황 설명) |
| `lease.conflict` | `system` | 저장소 복구 후 다른 프로세스가 같은 서비스 lease를 보유 |
| `breakglass.<action>` | `cli` | 로컬 CLI의 `--break-glass` 사용. `<action>`: `rollback.approve`, `rollback.reject`, `escalation.approve`, `circuit.reset`, `circuit.trip`, `freeze.override` |

키 체인을 켜면 감사 레코드(`audit-events`, `audit export`)의 원본 기록에 `mac` 필드가 더해진다(VGL-SI-05 6.1). SIEM syslog 메시지 형식은 바뀌지 않는다.

---

# 11. 제약 사항과 미확정 사항

| 번호 | 내용 | 해소 계획 |
|---|---|---|
| 1 | IF-EXT-02~11의 실장비(F5, AWS, vCenter, Nutanix, 사내 OpenStack, HAProxy·Nginx·Envoy 실제 버전, Docker·Podman 버전별) 연동은 시험하지 않았다. 모두 모의 서버·시뮬레이터 기준 "실험적" | M8-1 랩, 3회 연속 통과 시 "검증됨" 전환 |
| 2 | Nutanix는 Prism Element v2.0 경로 기준이며 AOS 릴리스별 확인이 필요하다 | M8 랩, 필요 시 v3·v4 API 전환 |
| 3 | Cinder revert-to-snapshot의 사용 중 볼륨 동작은 백엔드·릴리스별 확인이 필요하다 | M8 랩 |
| 4 | 각 외부 시스템의 최소 역할 이름·범위는 제품 버전마다 다를 수 있다(docs/10-security.md) | M8 랩 결과 반영 |
| 5 | (해소) 상태 저장소 장애 시 쓰기 대기열(M5-4)은 PR #12로 master에 병합되었다(`dba3dbe`) | - |
| 6 | CLI 원격 모드의 v1 사용(5장 확인 사항)과 문서 서술의 차이 | 문서 또는 구현 정리 필요 |
| 7 | (해소) `vigilante.approval.decided`의 `data` 설명을 구현에 맞췄고 계약 테스트가 필드 집합을 비교한다(6.1, PR #13) | - |
| 8 | v1은 동결 정책이지만 PR #13에서 `POST /v1/deployments`에 필드와 오류 `code`를 추가했다(5.1). 추가만이라 기존 클라이언트는 영향이 없다 | docs/08 동결 정책 문구에 예외 기록 검토 |
| 9 | `audit verify --key`는 usage 문자열에 없다(플래그 도움말에만 있음) | usage 문자열 보완 |
