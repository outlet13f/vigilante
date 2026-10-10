# 06. 오픈 API (v2)

Vigilante 서버의 공개 계약은 [`api/openapi.yaml`](../api/openapi.yaml)(OpenAPI 3.1)입니다. 명세가 원본이고, 서버 구현은 계약 테스트(`internal/api/contract_test.go`)가 명세와 일치하는지 매번 검사합니다. CLI 원격 모드, 운영 콘솔, 에이전트도 같은 API만 씁니다.

- 공개 범위: **사내 전용**(사내 시스템·개발자). 외부·파트너 공개는 하지 않습니다.
- `/v1`은 기존 CI 연동을 위해 유지합니다. 새 연동은 `/v2`를 쓰십시오.

## 공통 규약

| 항목 | 규칙 |
|---|---|
| 버전 | URL의 메이저 버전(`/v2`). 마이너 변경은 필드·리소스·선택 파라미터 추가만. 폐기 시 `Deprecation`·`Sunset` 헤더, 최소 6개월 유지 |
| 인증 | `Authorization: Bearer <토큰>`. 아래 "인증과 권한" 참고 |
| 호출 한도 | 호출자별 토큰 버킷. 초과하면 `429` + `Retry-After`. 비상 조치는 별도 버킷 |
| 오류 | RFC 9457 `application/problem+json`. `code`로 분기하고, 문의할 때는 `request_id`를 전달 |
| 재시도 | 모든 POST·PUT에 `Idempotency-Key`. 같은 키·같은 본문이면 첫 응답을 그대로 돌려주고 `Idempotent-Replayed: true`를 붙임. 호출자별로 24시간 보관 |
| 오래 걸리는 작업 | 관측·롤백·승인·기준선 측정은 `202 Accepted` + `Operation` + `Location: /v2/operations/{id}`. `status`가 `running`이 아닐 때까지 조회 |
| 동시 수정 | 배포 조회 응답의 `ETag`를 조치 요청의 `If-Match`에 넣으면, 그 사이 배포가 바뀐 경우 `412`로 거부 |
| 목록 | `{items, next_cursor}`. `next_cursor`를 `cursor`로 넘겨 다음 페이지. `limit` 1~500(기본 50). 커서는 해석하지 말 것 |
| 형식 | 시각은 RFC 3339 UTC, ID는 의미 없는 문자열, JSON 필드는 snake_case |
| 추적 | 요청의 `X-Request-ID`(또는 W3C `traceparent`)를 응답과 서버 로그에 그대로 남김 |

## 인증과 권한

| 방식 | 토큰 | 쓰는 곳 |
|---|---|---|
| OAuth 2.0 client credentials | `POST /v2/oauth/token`으로 받은 `vat_…` (기본 1시간) | 서버 간 연동: 사내 배포 콘솔, 개발자 포털 |
| API 키 | `vgk_…` 그대로 | 단순 연동: 스크립트, SIEM 수집 |
| 서비스 계정 토큰 | `vgl_…` (설정 파일 `auth.service_accounts`) | CI, 에이전트 |
| OIDC 액세스 토큰 | 사내 IdP가 발급한 JWT | 사용자, 콘솔 |

권한은 두 단계로 판단합니다. **역할 grant**(`deployer@team=payments`처럼 역할@범위)가 어느 서비스에서 무엇을 할 수 있는지 정하고, API 클라이언트는 여기에 **스코프**가 더해져 할 수 있는 일의 종류를 좁힙니다. 둘 다 통과해야 허용됩니다.

| 스코프 | 허용하는 일 |
|---|---|
| `deployments:read` | 배포·작업·서비스·대상·서킷·지표 조회 |
| `deployments:write` | 배포 등록, 단계 관측, 중단, 기준선, 마지막 정상 버전 |
| `rollbacks:execute` | 수동 롤백 |
| `approvals:write` | 상위 전략 승인 |
| `circuit:admin` | 서킷 닫기·열기 |
| `audit:read` | 감사 기록 조회 (역할은 viewer@`*` 필요) |
| `metrics:write` | 샘플 전송 (에이전트, 외부 모니터링) |
| `config:write` | API 클라이언트·서버 설정 관리 (역할은 admin 필요) |

### API 클라이언트 관리 (admin)

```bash
# OAuth 클라이언트 등록: 응답의 secret은 이때 한 번만 보입니다(서버에는 SHA-256만 저장)
curl -X POST "$API/v2/api-clients" -H "$H" -d '{
  "name": "deploy-console", "type": "oauth",
  "scopes": ["deployments:read", "deployments:write"],
  "grants": ["deployer@team=payments"],
  "rate_limit": {"rate": 10, "burst": 20}
}'

# 토큰 받기 (RFC 6749 4.4). 표준 OAuth 라이브러리를 그대로 쓸 수 있습니다.
curl -u "$CLIENT_ID:$CLIENT_SECRET" -d grant_type=client_credentials "$API/v2/oauth/token"
```

| 작업 | 요청 |
|---|---|
| 목록·조회 | `GET /v2/api-clients`, `GET /v2/api-clients/{id}` (비밀값은 앞 8자 `secret_hint`만) |
| 스코프·grant·만료·한도 변경 | `PATCH /v2/api-clients/{id}`. 스코프를 줄이면 이미 발급한 토큰에도 즉시 적용 |
| 비밀 회전 | `POST /v2/api-clients/{id}/secret`. 이전 비밀과 그 비밀로 받은 토큰은 즉시 무효 |
| 폐기 | `DELETE /v2/api-clients/{id}`. 기록은 감사용으로 남음 |

- 마지막 사용 시각(`last_used_at`)은 최대 1시간 간격으로 갱신합니다.
- 클라이언트의 모든 조치는 감사 기록에 `client:<이름>`으로 남고, 토큰 발급(`oauth.token`)과 인증 실패(`denied`)도 남습니다.
- 클라이언트 등록·비밀 회전 응답은 비밀값을 담고 있어 `Idempotency-Key` 재생을 하지 않습니다. 같은 이름의 활성 클라이언트가 있으면 409입니다.

### 호출 한도

| 버킷 | 기본값 | 대상 |
|---|---|---|
| default | 초당 20, 순간 40 | 모든 v2 호출 |
| emergency | 초당 1, 순간 10 | 롤백, 승인, 중단, 서킷 닫기·열기 |

- 호출자(사용자, 서비스 계정, API 클라이언트)마다 따로 셉니다. API 클라이언트는 `rate_limit`으로 자기 한도(일일 상한 `daily` 포함)를 가질 수 있습니다.
- 응답에 `RateLimit-Limit`, `RateLimit-Remaining`이 붙고, 초과하면 `429` + `Retry-After`입니다.
- 조회가 폭주해 default 버킷이 비어도 emergency 버킷의 롤백은 그대로 받습니다.
- 비상용 legacy 토큰(`server.auth_token_env`)은 한도를 적용하지 않습니다.
- 한도는 노드 메모리에서 셉니다. 모든 요청이 리더로 모이므로 클러스터 전체 기준과 같습니다.

## 리소스

| 메서드·경로 | 권한 | 설명 |
|---|---|---|
| `GET /v2/me` | 인증만 | 호출자와 권한 |
| `GET /v2/deployments` | viewer | 읽을 수 있는 서비스의 배포, 최신순. `service`, `state` 필터 |
| `POST /v2/deployments` | deployer | 배포 등록. `previous_version`을 비우면 마지막 정상 버전, 그것도 없으면 422 |
| `GET /v2/deployments/{id}` | viewer | 배포, 타임라인, 위반, 최근 평가, `exit_code` |
| `POST /v2/deployments/{id}/observations` | deployer | 단계 관측 시작 → Operation |
| `POST /v2/deployments/{id}/rollbacks` | operator | 수동 롤백 → Operation |
| `POST /v2/deployments/{id}/approvals` | operator | 승인 대기 중인 상위 전략 승인 → Operation. 4-eyes 적용 |
| `POST /v2/deployments/{id}/abort` | deployer | 관측 중단 |
| `GET /v2/operations`, `GET /v2/operations/{id}` | viewer | 작업 진행·결과 |
| `GET /v2/services`, `GET /v2/services/{name}` | viewer | 서비스 구성(대상, 단계, 규칙, 실행기, 마지막 정상 버전) |
| `POST /v2/services/{name}/baselines` | deployer | 기준선 측정 → Operation |
| `GET`·`PUT /v2/services/{name}/last-good` | viewer·deployer | 마지막 정상 버전 조회·등록 (CLI `mark-good`) |
| `GET /v2/targets`, `GET /v2/targets/{name}` | viewer | 대상 (읽을 수 있는 서비스에 속한 것만) |
| `GET /v2/presets` | 인증만 | 규칙 프리셋 |
| `GET /v2/circuit` | viewer | 서킷 상태 |
| `POST /v2/circuit/reset`, `POST /v2/circuit/trip` | admin | 서킷 닫기·열기. `reason` 필수 |
| `GET /v2/audit-events` | viewer@`*` + `audit:read` | 감사 기록, 오래된 순. `since`, `until`, `actor`, `action`, `service` |
| `POST /v2/oauth/token` | 클라이언트 인증 | OAuth 토큰 발급 |
| `/v2/api-clients…` | admin + `config:write` | API 클라이언트 관리 |
| `GET /v2/events` | viewer | 이벤트 스트림(SSE) |
| `GET /v2/event-types` | 인증만 | 이벤트 카탈로그 |
| `/v2/webhooks…` | 다루는 범위의 viewer + `config:write` | 웹훅 구독, 재전송, 테스트, 전달 이력 |

`Operation.result`에는 작업이 끝났을 때의 배포 상태, 판정, CI 종료 코드(`exit_code`)가 들어 있습니다. `status`는 작업이 실행됐으면 `completed`(롤백이 실패했어도 결과는 `result`에), 실행 자체를 못 했으면 `failed`(`error`에 이유)입니다. 리더가 바뀌어도 작업은 이어지고, 새 리더는 배포 상태를 보고 작업 완료 여부를 판단합니다.

## 예: CI에서 canary 관측

```bash
H='Authorization: Bearer '"$VIGILANTE_TOKEN"
API=https://vigilante.example.internal:8088

curl -sf -X POST "$API/v2/deployments" -H "$H" -H "Idempotency-Key: $CI_PIPELINE_ID-create" \
  -d '{"id":"'"$CI_PIPELINE_ID"'","service":"order-api","version":"'"$CI_COMMIT_TAG"'"}'

OP=$(curl -sf -X POST "$API/v2/deployments/$CI_PIPELINE_ID/observations" -H "$H" \
  -H "Idempotency-Key: $CI_PIPELINE_ID-canary" -d '{"phase":"canary"}' | jq -r .id)

while :; do
  R=$(curl -sf "$API/v2/operations/$OP" -H "$H")
  [ "$(jq -r .status <<<"$R")" != running ] && break
  sleep 5
done
exit "$(jq -r '.result.exit_code // 1' <<<"$R")"   # 0 통과, 2 롤백됨, 3 사람 필요, 4 보류
```

재시도할 때 같은 `Idempotency-Key`를 쓰면 관측이 두 번 시작되지 않습니다.

## 이벤트

배포 상태 변화, 서킷, 승인, 에이전트 상실을 [CloudEvents 1.0](https://cloudevents.io) 형식으로 내보냅니다. 받는 방법은 두 가지입니다.

| 방법 | 쓰는 곳 | 특징 |
|---|---|---|
| `GET /v2/events` (SSE) | 콘솔, 대시보드, 실시간 도구 | 연결을 유지하는 동안 받음. 끊기면 `Last-Event-ID`로 빠진 구간부터 다시 받음 |
| 웹훅 구독 `POST /v2/webhooks` | 사내 시스템(ITSM, 메신저 봇, 배포 콘솔) | 서버가 등록된 URL로 POST. 재시도, 순서 보장, 서명 |

| 이벤트 | 뜻 |
|---|---|
| `vigilante.deployment.created`, `.marked_good` | 배포 등록, 정상 버전 등록 |
| `vigilante.observation.started`, `.passed`, `.failed`, `.held`, `.aborted` | 단계 관측 시작과 판정 |
| `vigilante.rollback.started`, `.completed`, `.failed` | 롤백 시작·완료·실패(사람 필요) |
| `vigilante.approval.requested`, `.decided` | 상위 전략 승인 요청·승인 |
| `vigilante.circuit.opened`, `.half_opened`, `.closed` | 서킷 상태 |
| `vigilante.agent.lost` | 에이전트 하트비트 끊김 |
| `vigilante.webhook.disabled` | 실패가 이어져 웹훅 구독이 꺼짐 |
| `vigilante.ping` | 테스트 이벤트 (해당 구독에만) |

- **순번:** 이벤트마다 클러스터 전체에서 증가하는 `sequence`가 있고, `id`·SSE `id`·`webhook-id`가 모두 이 값입니다. 리더가 바뀌어도 이어집니다.
- **범위:** 서비스 이벤트는 그 서비스를 읽을 수 있는 호출자에게만, 서비스가 없는 이벤트(서킷, 에이전트)는 viewer 누구에게나 갑니다.
- **보관:** 최근 10,000건을 보관합니다. 더 오래 끊겨 있었다면 보관된 가장 오래된 이벤트부터 받습니다.

### 웹훅

```bash
curl -X POST "$API/v2/webhooks" -H "$H" -d '{
  "url": "https://incident-bot.example.internal/vigilante",
  "types": ["vigilante.rollback.failed", "vigilante.circuit.opened"],
  "teams": ["payments"]
}'
# 응답의 secret(whsec_…)은 이때 한 번만 보입니다. 서버는 저장하지 않고 필요할 때 계산합니다.
```

- **전달:** 구독마다 순서대로 한 건씩 보냅니다. 2xx(10초 안)면 성공입니다. 실패하면 1초, 5초, 30초, 2분, 10분, 30분 뒤 재시도하고, 그래도 실패하면 dead-letter 목록에 남기고 다음 이벤트로 넘어갑니다. 연속 5건이 dead-letter가 되면 구독을 끄고 운영 알림을 보냅니다. `PATCH {"active": true}`로 다시 켜면 남은 이벤트부터 이어서 보냅니다.
- **최소 한 번:** 리더 교체나 응답 유실로 같은 이벤트가 두 번 갈 수 있습니다. `webhook-id`로 중복을 거르십시오.
- **재전송·점검:** `POST …/redeliveries {"sequence": N}`(dead-letter 재전송), `POST …/pings`(테스트 이벤트), `GET …/deliveries`(최근 시도 100건).
- **권한:** 구독이 다루는 서비스·팀 전부에 viewer가 있어야 합니다(필터가 없으면 `*`). 자기가 만든 구독만 보고 바꿀 수 있고, admin은 전부 다룹니다.
- **서버 설정:** `api.webhook_signing_key_ref`(서명 마스터 키)가 있어야 웹훅을 쓸 수 있습니다. `api.webhook_allowed_hosts`로 보낼 수 있는 호스트를 제한할 수 있습니다. 리디렉션은 따라가지 않습니다.

서명 검증([Standard Webhooks](https://www.standardwebhooks.com) 형식). 표준 라이브러리로 충분합니다.

```python
import base64, hashlib, hmac, time

def verify(secret: str, headers: dict, body: bytes) -> bool:
    msg_id, ts = headers["webhook-id"], headers["webhook-timestamp"]
    if abs(time.time() - int(ts)) > 300:        # 5분 넘은 요청은 재전송 공격으로 보고 거부
        return False
    key = base64.b64decode(secret.removeprefix("whsec_"))
    mac = hmac.new(key, f"{msg_id}.{ts}.".encode() + body, hashlib.sha256).digest()
    expected = "v1," + base64.b64encode(mac).decode()
    return any(hmac.compare_digest(expected, s) for s in headers["webhook-signature"].split())
```

## v1 → v2 대응

| v1 | v2 |
|---|---|
| `POST /v1/deployments` (+ `phase`) | `POST /v2/deployments` 후 `POST …/observations` |
| `POST /v1/deployments/{id}/phases/{phase}?wait=true` | `POST …/observations` → `GET /v2/operations/{id}` 조회 |
| `POST /v1/deployments/{id}/rollback` | `POST …/rollbacks` |
| `POST /v1/deployments/{id}/approve` | `POST …/approvals` |
| `POST /v1/baselines/{service}` (응답까지 대기) | `POST /v2/services/{name}/baselines` → Operation |
| `POST /v1/circuit/reset?reason=` | `POST /v2/circuit/reset` 본문 `{"reason"}` (필수) |
| `GET /v1/audit` | `GET /v2/audit-events` (커서 페이지) |
| 오류 `{"error": "..."}` | problem+json |

## 오류 코드

`type`은 이 절의 각 코드를 가리킵니다.

### bad_request
요청 형식이 잘못됨: JSON 오류, 명세에 없는 필드, 잘못된 `limit`·`cursor`. 고쳐서 다시 보내십시오.

### validation_failed
형식은 맞지만 값이 유효하지 않음: 없는 서비스·단계·실행기·대상, 롤백 대상 버전 없음, 빈 `reason`. `errors[]`에 필드별 이유가 있습니다.

### unauthenticated
토큰이 없거나, 만료됐거나, 검증에 실패함.

### forbidden
역할 또는 범위가 부족함, 또는 4-eyes 규칙 위반. 거부된 요청도 감사 기록에 남습니다.

### not_found
리소스가 없거나, 명세에 없는 경로.

### conflict
현재 상태에서 할 수 없음: 이미 관측·롤백 중, 승인 대기가 아님, 같은 ID의 다른 배포.

### precondition_failed
`If-Match`의 ETag가 현재 배포와 다름. 다시 조회한 뒤 판단하십시오.

### idempotency_key_reused
같은 `Idempotency-Key`를 다른 요청(경로나 본문이 다름)에 썼음. 새 키를 쓰십시오.

### idempotency_key_in_flight
같은 키의 요청이 아직 처리 중. `Retry-After` 후 다시 보내면 첫 응답을 받습니다.

### not_leader
HA 팔로워가 리더를 아직 모르거나 리더에 닿지 못함. `Retry-After` 후 재시도.

### rate_limited
호출 한도 초과. `Retry-After` 후 재시도하십시오. 롤백 같은 비상 조치는 별도 한도를 씁니다.

### internal
서버 내부 오류. `request_id`와 함께 운영자에게 알리십시오.
