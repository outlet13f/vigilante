# 06. 오픈 API (v2)

Vigilante 서버의 공개 계약은 [`api/openapi.yaml`](../api/openapi.yaml)(OpenAPI 3.1)입니다. 명세가 원본이고, 서버 구현은 계약 테스트(`internal/api/contract_test.go`)가 명세와 일치하는지 매번 검사합니다. CLI 원격 모드, 운영 콘솔, 에이전트도 같은 API만 씁니다.

- 공개 범위: **사내 전용**(사내 시스템·개발자). 외부·파트너 공개는 하지 않습니다.
- `/v1`은 기존 CI 연동을 위해 유지합니다. 새 연동은 `/v2`를 쓰십시오.

## 공통 규약

| 항목 | 규칙 |
|---|---|
| 버전 | URL의 메이저 버전(`/v2`). 마이너 변경은 필드·리소스·선택 파라미터 추가만. 폐기 시 `Deprecation`·`Sunset` 헤더, 최소 6개월 유지 |
| 인증 | `Authorization: Bearer <토큰>`. 서비스 계정 토큰(`vgl_…`, API 키) 또는 OIDC 액세스 토큰. 권한은 역할 × 팀·서비스 범위(docs/02 `auth`) |
| 오류 | RFC 9457 `application/problem+json`. `code`로 분기하고, 문의할 때는 `request_id`를 전달 |
| 재시도 | 모든 POST·PUT에 `Idempotency-Key`. 같은 키·같은 본문이면 첫 응답을 그대로 돌려주고 `Idempotent-Replayed: true`를 붙임. 호출자별로 24시간 보관 |
| 오래 걸리는 작업 | 관측·롤백·승인·기준선 측정은 `202 Accepted` + `Operation` + `Location: /v2/operations/{id}`. `status`가 `running`이 아닐 때까지 조회 |
| 동시 수정 | 배포 조회 응답의 `ETag`를 조치 요청의 `If-Match`에 넣으면, 그 사이 배포가 바뀐 경우 `412`로 거부 |
| 목록 | `{items, next_cursor}`. `next_cursor`를 `cursor`로 넘겨 다음 페이지. `limit` 1~500(기본 50). 커서는 해석하지 말 것 |
| 형식 | 시각은 RFC 3339 UTC, ID는 의미 없는 문자열, JSON 필드는 snake_case |
| 추적 | 요청의 `X-Request-ID`(또는 W3C `traceparent`)를 응답과 서버 로그에 그대로 남김 |

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
| `GET /v2/audit-events` | viewer@`*` | 감사 기록, 오래된 순. `since`, `until`, `actor`, `action`, `service` |

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
호출 한도 초과. `Retry-After` 후 재시도 (M3-2에서 적용).

### internal
서버 내부 오류. `request_id`와 함께 운영자에게 알리십시오.
