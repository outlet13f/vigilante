# 11. 파일럿 운영 가이드 (판정 품질 측정)

로드맵 M8-2의 파일럿을 운영하는 방법입니다. 목표는 실제 서비스에서 Vigilante의 판정이 맞는지 측정해 **출시 게이트**를 확인하고, 프리셋 임계치를 보정하는 것입니다.

## 출시 게이트

`vigilante pilot report`가 아래를 자동으로 확인합니다(기본값, 플래그로 조정 가능).

| 항목 | 기준 |
|---|---|
| 관측한 배포 | 30건 이상 (`--min-deployments`) |
| 오탐 (정상 배포에 FAIL) | 0건 |
| 미탐 (문제 배포를 FAIL하지 않음) | 0건 |
| HOLD 비율 (보류된 단계 / 관측한 단계) | 10% 이하 (`--max-hold-rate`) |
| 실제 롤백 완료 (드라이런 제외) | 1건 이상 |
| FAIL·HOLD 배포의 평가 | 전부 평가됨 |

오탐·미탐은 사람이 평가해야만 셀 수 있습니다. 평가하지 않은 FAIL·HOLD 배포가 남아 있으면 게이트는 통과하지 않습니다.

## 진행 순서 (3개월)

1. **대상 고르기:** 사내 서비스 1~2개. OpenStack VM + Octavia 서비스 1개와 베어메탈 또는 컨테이너 서비스 1개를 권장합니다. 실제 배포 파이프라인(CI)에 `vigilante watch`를 붙입니다.
2. **1개월차: 드라이런.** 서버 설정 `server.dry_run: true`(CI 단발 실행은 `--dry-run`)이면 판정은 그대로 하고 롤백·드레인은 로그로만 남습니다. 서버 전체에 적용되므로 파일럿 전용 서버(또는 CI 잡)로 돌리십시오. 배포 기록에 `dry_run`이 표시되며, 보고서는 이를 "롤백했을 것"(dry run)으로 따로 셉니다.
3. **2~3개월차: 승인 모드.** `dry_run`을 끄고 `rollback.mode: approve`로 바꿉니다. FAIL이면 롤백 계획을 만들어 승인을 기다리고, 운영자가 콘솔·API·CLI로 승인하거나 거절합니다. 실제 롤백 완료 1건이 게이트 조건입니다.
4. **매주:** 보고서를 뽑아 평가가 빠진 배포를 채우고, HOLD 원인과 오탐을 검토합니다.
5. **끝:** 게이트를 통과하면 보고서를 판정 품질 보고서로 확정하고, 보정한 임계치를 프리셋 v2로 냅니다. 통과하지 못하면 임계치를 고친 뒤 1개월 단위로 연장합니다.

## 평가 기록 (feedback)

배포마다 "판정이 맞았는가"를 남깁니다. FAIL·HOLD로 끝난 배포는 반드시, 통과한 배포도 장애가 났다면 반드시 기록합니다.

| 평가 | 뜻 | 쓰는 때 |
|---|---|---|
| `correct` | 판정이 맞음 | FAIL·HOLD가 실제 문제였거나, 통과한 배포가 실제로 정상 |
| `false_positive` | 오탐: FAIL이었지만 배포는 정상 | 롤백(또는 승인 요청)이 필요 없었음. FAIL 판정이 있던 배포만 |
| `false_negative` | 미탐: 문제 배포였는데 FAIL하지 않음 | 사후 인시던트로 드러남. 인시던트 번호를 함께. FAIL 판정이 없던 배포만 |
| `unclear` | 판단 불가 | 원인 조사 중이거나 자료 부족 |

```bash
# 콘솔: 배포 상세 > 판정 평가.  CLI:
vigilante feedback -c vigilante.yaml --id order-api-1234 --outcome false_positive --note "방화벽 변경으로 프로브 포트가 막힘"
vigilante feedback --server https://vigilante:8088 --id order-api-1240 --outcome false_negative --incident INC0012345 --note "p99 악화를 늦게 발견"
# API: PUT /v2/deployments/{id}/feedback {"outcome": "...", "incident": "...", "note": "..."}
```

평가는 deployer 권한(그 서비스)이 있으면 남길 수 있고, 다시 남기면 바뀝니다. 모든 평가는 감사 기록(`deployment.feedback`)에 남습니다.

**미탐 찾기:** 파일럿 대상 서비스의 인시던트를 매주 훑어, 원인이 배포인데 Vigilante가 통과시킨 건을 `false_negative`로 기록합니다. ServiceNow 연동을 켜 두었다면 변경 티켓과 인시던트를 함께 보면 빠릅니다.

## 보고서

```bash
vigilante pilot report -c vigilante.yaml --since 2026-11-01                  # Markdown, 종료 코드 4 = 게이트 미달
vigilante pilot report -c vigilante.yaml --service order-api,billing-api --json --out pilot.json
```

보고서 내용:

- **게이트 표:** 항목별 기준·실제 값·통과 여부.
- **판정:** 관측한 단계별 PASS·FAIL·HOLD·중단·진행 중, 서비스별 표.
- **HOLD 원인:** `environmental`(대조군도 같이 나빠짐), `observer_disagreement`(관측점끼리 불일치), `insufficient_evidence`(표본 부족), `hold_rule`(hold 조치 규칙), `other`.
- **평가:** 평가한 수, 맞음·오탐·미탐·판단 불가.
- **롤백과 시간:** 완료·실패·드라이런·수동, 승인 모드의 승인·거절·대기. "관측 시작 → FAIL 판정", "롤백 시작 → 완료", "승인 대기"의 중앙값·P90·최대(초).
- **발견 사항:** 오탐·미탐·롤백 실패 배포 목록. **평가 필요:** 평가하지 않은 FAIL·HOLD 배포 목록.

지표는 상태 저장소의 배포 기록에서 계산합니다(보고서는 아무것도 바꾸지 않습니다). 프로브 상태와 판정 지연의 세부 분포는 `/metrics`의 `vigilante_probe_samples_total`·`vigilante_probe_restarts_total`(프로브), `vigilante_rollback_trigger_seconds`(위반 감지부터 롤백 시작까지), `vigilante_evaluation_seconds`, `vigilante_verdicts_total`을 함께 보십시오.

## 보정

- 오탐이 나온 규칙은 임계치, `for`(연속 위반 횟수), `warmup`, 관측 창을 조정하고 근거(그때의 지표)를 보고서 발견 사항에 붙입니다.
- HOLD가 많으면 원인별로: `insufficient_evidence`는 관측 창·프로브 주기, `environmental`은 대조군 구성, `observer_disagreement`는 에이전트·중앙 프로브의 경로 차이를 봅니다.
- 보정한 값은 서비스의 `overrides`에서 시험한 뒤 프리셋 v2에 반영합니다. 이미 쓰는 서비스는 프리셋 버전을 고정하므로(`preset: java-web@1`) 몰래 바뀌지 않습니다.
