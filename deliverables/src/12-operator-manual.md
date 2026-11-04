---
title: 운영자 매뉴얼
doc_id: VGL-OP-02
version: 1.2
date: 2026-10-11
author: Vigilante 개발팀
status: 검토본
history:
  - 1.0 | 2026-10-11 | 최초 작성
  - 1.1 | 2026-10-11 | 코드 대조 검증 반영
  - 1.2 | 2026-10-11 | 결함 수정(PR #13) 반영, PR #12 병합 반영, PR #14 반영
---

# 1. 문서 개요

## 1.1 목적

이 문서는 Vigilante로 배포를 관측하고 판정 결과에 따라 조치하는 배포 담당자(deployer)와 운영자(operator)를 위한 매뉴얼입니다. 기본 개념, CI 파이프라인 연동, 웹 콘솔 화면별 사용법, CLI 작업, 승인 모드와 변경 동결 업무 절차, 판정 평가 기록, 파일럿 보고서 읽는 법을 다룹니다.

설치와 설정은 관리자 매뉴얼(VGL-OP-01)을, 장애 상황별 대응은 장애 대응 런북(VGL-OP-03)을 참고하십시오.

## 1.2 전제

- 관리자가 서버와 설정(`vigilante.yaml`)을 준비해 두었고, 운영자는 자신의 역할(5.2절)에 맞는 계정 또는 토큰을 받았습니다.
- 이 문서의 명령 예에서 설정 파일은 `/etc/vigilante/vigilante.yaml`, 서버 주소는 `https://vigilante.example.internal:8088`입니다.
- 원격 서버를 호출하는 명령은 환경변수 `VIGILANTE_TOKEN`의 토큰을 씁니다.
- 기준 코드는 master 브랜치의 병합 커밋 `537870c`(PR #12 저장소 장애 대기열, PR #13 결함 수정, PR #14 관측 장치 과부하 보류 대상 수정 포함)입니다.

---

# 2. 기본 개념

## 2.1 용어

| 용어 | 쉬운 설명 |
|---|---|
| 배포(deployment) | 한 서비스를 새 버전으로 바꾸는 한 번의 작업. 배포 ID(보통 CI 실행 번호), 서비스, 새 버전, 이전 버전(되돌아갈 버전)을 가집니다 |
| 단계(phase) | 새 버전을 넓혀 가는 순서. `canary`(일부 대상) → `rolling`(절반 등) → `full`(전체). 단계마다 관측합니다 |
| 관측(observation) | 정해진 시간(관측 창) 동안 헬스 체크·로그·자원 지표를 모아 규칙에 비추어 보는 일 |
| 판정(verdict) | 관측 결과. PASS(통과), FAIL(실패), HOLD(보류), INCONCLUSIVE(증거 부족) |
| 롤백(rollback) | FAIL이면 이전 버전으로 되돌리는 일. 보통 트래픽에서 빼기 → 되돌리기 → 확인 → 트래픽 복귀 순서입니다 |
| 보류(hold) | 자동으로 결론을 내릴 수 없어 사람의 판단을 기다리는 상태. 환경 문제, 관측점 불일치, 증거 부족 등이 원인입니다 |
| 승인 모드(approve) | FAIL이어도 바로 롤백하지 않고 사람의 승인을 기다리는 서비스 설정 |
| 서킷(circuit) | 자동화의 비상 정지 장치. 롤백 실패가 쌓이거나 관리자가 차단하면 열려(OPEN) 자동 롤백과 새 배포를 멈춥니다 |
| 변경 동결(change freeze) | 새 배포를 받지 않는 기간(주말, 결산 기간, 장애 대응 중) |
| 정상 버전(known-good) | 마지막으로 전체 단계를 통과한 버전. 다음 배포의 "이전 버전" 기본값이 됩니다 |
| 판정 평가(feedback) | 판정이 맞았는지 사람이 남기는 기록(맞음·오탐·미탐·판단 불가) |

## 2.2 배포 상태

콘솔에는 상태가 한국어 배지로 표시됩니다.

| 상태 | 콘솔 표시 | 뜻 | CI 종료 코드 |
|---|---|---|---|
| `PENDING` | 대기 | 등록됨, 관측 전 | 1 |
| `BASELINE` | 기준선 | 배포 전 기준선 측정 중 | 1 |
| `OBSERVING` | 관측 중 | 단계 관측 중 | 판정 후 결정 |
| `PROMOTED` | 단계 통과 | 단계 통과, 다음 단계 대기 | 0 |
| `SUCCEEDED` | 완료 | full 단계 통과 | 0 |
| `HELD` | 보류 | 사람 판단 필요 | 4 |
| `AWAITING_APPROVAL` | 승인 대기 | 롤백이 승인을 기다림 | 3 |
| `ROLLING_BACK` | 롤백 중 | 롤백 진행 중 | 롤백 후 결정 |
| `ROLLED_BACK` | 롤백됨 | 롤백 완료, 서비스 복구됨 | 2 |
| `ROLLBACK_FAILED` | 롤백 실패 | 롤백 실패 또는 안전장치가 차단 | 3 |
| `ABORTED` | 중단 | 사람이 관측을 중단 | 1 |

## 2.3 종료 코드 계약

`vigilante watch`와 `vigilante rollback`의 종료 코드는 다음과 같으며, 같은 MAJOR 버전 안에서 바뀌지 않습니다.

| 코드 | 의미 | 파이프라인 권장 동작 |
|---|---|---|
| 0 | PASS. 다음 단계 진행 | 계속 |
| 2 | FAIL 후 자동 롤백 완료 | 실패 처리, 알림(서비스는 복구됨) |
| 3 | 롤백 실패, 서킷 OPEN, 승인 대기, 변경 동결·티켓 게이트 거부(원격 모드에서 서버가 거부한 경우 포함) | 온콜 호출, 파이프라인 중단 |
| 4 | HOLD 또는 INCONCLUSIVE | 수동 승인 단계 |
| 1 | 엔진 오류(설정, 접근, 입력 누락), 로컬 CLI 제한 거부(5.11절) | 실패 처리 |

## 2.4 서킷 상태

| 상태 | 콘솔 표시 | 자동 롤백 | 새 배포 | 수동 롤백 |
|---|---|---|---|---|
| CLOSED | 정상 (CLOSED) | 허용 | 허용 | 허용 |
| OPEN | 차단 (OPEN) | 금지(트래픽 격리만) | 거부(종료 코드 3) | 허용 |
| HALF_OPEN | 시험 (HALF_OPEN) | 1건만 시험 | 허용 | 허용 |

## 2.5 안전장치가 남기는 표시

다음은 자동화가 스스로 판단을 미루거나 평소와 다른 방식으로 동작했음을 알리는 표시입니다. 대응 절차는 런북(VGL-OP-03)의 해당 장을 따릅니다.

| 상황 | 운영자가 보는 것 | 뜻과 할 일 |
|---|---|---|
| 관측 장치 과부하로 보류 | 배포 `HELD`(보류, 종료 코드 4), 이유 끝에 `— observer degraded (<신호>), not attributed to the release`, warning 알림 `<서비스> <단계> HELD — human decision needed` | Vigilante 서버 자신이 과부하(CPU 부족, 소켓 고갈, 서버 쪽 네트워크 장애)여서 프로브 시간 초과를 대상 장애로 믿을 수 없다는 뜻입니다. 롤백하지 않고 보류했습니다. 보류 대상은 서버가 직접 재는 프로브(HTTP, TCP, gRPC, DB, SSH로 읽는 호스트)의 실패·지연 지표와 `probe_error`뿐이며, 로그·액세스 로그·컨테이너 지표 위반은 이때도 그대로 판정합니다. 관리자에게 서버 자원 확인을 요청하고, 대상 상태를 다른 경로로 확인해 재관측 또는 롤백을 결정합니다(런북 19장) |
| 저장소 장애 중 롤백 | warning 알림 `<서비스>: Rollback without the service lease`, 배포 타임라인의 `safety` 이벤트 | 상태 저장소에 닿지 않아 서비스 잠금 없이 롤백했습니다. 같은 서비스에 다른 롤백(다른 CI 잡 등)을 시작하지 마십시오(런북 20장) |
| lease 충돌 | warning 알림 `<서비스>: Concurrent rollback suspected` | 저장소가 돌아왔을 때 다른 프로세스가 같은 서비스의 잠금을 잡고 있었습니다. 두 프로세스가 같은 대상에 조치했을 수 있으므로 대상의 실제 버전과 LB 풀 상태를 확인합니다(런북 20장) |
| 로컬 CLI 비상 실행 | critical 알림 `Break-glass: cli:<사용자>@<호스트> ran <작업> locally`, 감사 기록 `breakglass.<작업>` | 누군가 서버를 거치지 않고 권한이 큰 명령(승인, 서킷 리셋 등)을 실행했습니다. 예정된 조치인지 확인합니다(5.11절, 런북 21장) |

---

# 3. CI 파이프라인 연동

## 3.1 기본 흐름

1. 최초 1회, 지금 운영 중인 버전을 정상 버전으로 등록합니다.
   ```bash
   vigilante mark-good -c /etc/vigilante/vigilante.yaml --service order-api --version v41
   ```
2. 파이프라인 첫 단계에서 설정을 검증하고 사전 점검합니다.
   ```bash
   vigilante validate -c /etc/vigilante/vigilante.yaml
   vigilante doctor   -c /etc/vigilante/vigilante.yaml --service order-api --junit doctor.xml
   ```
3. 체크포인트(스냅샷 등)를 만들고 배포 전 기준선을 측정합니다.
   ```bash
   vigilante prepare  -c /etc/vigilante/vigilante.yaml --service order-api
   vigilante baseline -c /etc/vigilante/vigilante.yaml --service order-api --window 5m --out baseline.json
   ```
4. 자체 배포 도구로 canary 대상에 새 버전을 배포합니다.
5. 단계를 관측합니다. 종료 코드에 따라 다음 단계로 가거나 멈춥니다.
   ```bash
   vigilante watch -c /etc/vigilante/vigilante.yaml --service order-api --phase canary --baseline baseline.json
   ```
6. `rolling`, `full` 단계도 4~5단계를 반복합니다. full 단계를 통과한 버전이 다음 배포의 이전 버전 기본값이 됩니다.

## 3.2 입력값 자동 채움

`--id`와 `--version`을 생략하면 CI 실행 정보에서 채웁니다. `VIGILANTE_DEPLOYMENT_ID`·`VIGILANTE_VERSION`이 있으면 CI 변수보다 먼저 씁니다.

| CI | 배포 ID | 버전 |
|---|---|---|
| (공통, 최우선) | `VIGILANTE_DEPLOYMENT_ID` | `VIGILANTE_VERSION` |
| Jenkins | `BUILD_TAG` | `GIT_COMMIT`(짧게 줄임) |
| GitLab CI | `gl-<CI_PIPELINE_ID>` | `CI_COMMIT_TAG` 또는 `CI_COMMIT_SHORT_SHA` |
| GitHub Actions | `gh-<GITHUB_RUN_ID>-<GITHUB_RUN_ATTEMPT>` | 태그 실행이면 `GITHUB_REF_NAME`, 아니면 `GITHUB_SHA`(짧게 줄임) |
| 그 밖 | — | 작업 디렉토리의 `git describe --tags --always` |

`--previous`를 생략하면 저널에 기록된 그 서비스의 마지막 성공 배포 버전을 씁니다. 채운 값과 출처는 표준 오류에 `vigilante: auto-filled id=... (출처)`로 출력되고 배포 기록에도 남습니다. 직접 지정한 플래그가 항상 우선합니다.

이전 버전을 알 수 없으면 다음 오류로 종료 코드 1을 돌려줍니다. 이 경우 `--previous`를 주거나 `prepare`를 먼저 실행하거나 `mark-good`으로 정상 버전을 등록하십시오.

```
previous version unknown for order-api: pass --previous, run `vigilante prepare` first, or register a known-good version with `vigilante mark-good --service order-api --version <v>`
```

## 3.3 로컬 모드와 원격 모드

| 모드 | 명령 | 쓰는 경우 |
|---|---|---|
| 로컬(단발 실행) | `vigilante watch -c FILE --service S --phase P` | CI 러너가 대상망에 접근할 수 있을 때. 서킷·플래핑 이력은 공유 저널 파일로 유지 |
| 원격(서버 위임) | `vigilante watch --server URL --service S --phase P` | CI 러너는 대상망에 접근할 수 없고 서버만 접근 가능할 때 |

원격 모드는 서버에 배포를 등록하고 2초마다 상태를 조회하다가 판정이 끝나면 같은 종료 코드로 끝납니다. 서버가 잠시 응답하지 않으면 `status poll failed`를 출력하고 계속 재시도합니다.

원격 모드는 `--ticket`(변경 티켓)과 `--freeze-override`(동결 예외 사유)를 서버로 보냅니다. 서버는 등록 시 서킷, 변경 동결, 변경 티켓 게이트를 적용하며, 게이트가 닫혀 거부하면(`circuit_open`, `change_frozen`, `change_ticket_invalid`, `itsm_unavailable`) CLI는 로컬 모드와 같은 **종료 코드 3**으로 끝납니다. `--freeze-override`는 서버에서 admin만 쓸 수 있으므로 `VIGILANTE_TOKEN`이 admin 토큰이어야 합니다.

> `--server`를 실제로 쓰는 명령은 `watch`, `circuit`, `feedback`, `whoami`, `support-bundle`, `agent`뿐입니다. `rollback`, `mark-good`, `status`, `prepare`, `baseline`은 `--server`를 받아도 무시하고 설정 파일의 상태 저장소로 로컬 실행합니다.

## 3.4 파이프라인 예 (저장소 `examples/ci/`)

Jenkins 선언형 파이프라인에서는 종료 코드별로 분기합니다.

```groovy
def gate(String phase) {
  def rc = sh(returnStatus: true, script:
    "${VIG} watch --service ${SVC} --phase ${phase} --baseline baseline.json")
  switch (rc) {
    case 0: echo "${phase}: PASS"; break
    case 2: error("${phase}: FAILED — vigilante rolled back to the last known-good version")
    case 3: error("${phase}: ROLLBACK FAILED or circuit OPEN — page on-call")
    case 4: input message: "${phase} is HELD (environmental / inconclusive). Promote anyway?"; break
    default: error("vigilante error (${rc})")
  }
}
```

GitLab CI에서는 서버에 위임합니다. `VIGILANTE_TOKEN`은 마스킹된 CI/CD 변수로 둡니다.

```yaml
.gate: &gate
  script:
    - deploy-pay.sh "$DEPLOY_HOSTS" "$CI_COMMIT_SHORT_SHA"
    - |
      set +e
      vigilante watch --server "$VIGILANTE_SERVER" --service "$SVC" --phase "$PHASE"
      rc=$?
      case $rc in
        0) echo "PASS";;
        2) echo "rolled back automatically"; exit 1;;
        3) echo "ROLLBACK FAILED / circuit open - paging on-call"; exit 1;;
        4) echo "HELD - needs a human"; exit 1;;
        *) exit $rc;;
      esac
```

GitHub Actions에서는 단계마다 `watch`를 실행하고, 실패하면 배포 상세를 조회해 원인을 남깁니다.

```yaml
- name: Canary (web-ec2-a)
  run: |
    ./deploy.sh web-ec2-a "$NEW"
    vigilante watch --server "$VIG_SERVER" --service "$SVC" --phase canary
- name: Explain failure
  if: failure()
  run: |
    curl -fsS -H "Authorization: Bearer $VIGILANTE_TOKEN" \
      "$VIG_SERVER/v1/deployments/gh-${{ github.run_id }}-${{ github.run_attempt }}" | jq '.deployment | {state, reason, breaches}'
```

`doctor --junit doctor.xml`의 결과를 CI 테스트 보고서로 게시하면 설정·환경 문제를 롤백 순간이 아니라 파이프라인 첫 단계에서 발견할 수 있습니다.

## 3.5 v2 API로 연동

사내 배포 콘솔처럼 HTTP로 연동할 때는 v2 API를 씁니다. 같은 `Idempotency-Key`로 재시도하면 관측이 두 번 시작되지 않습니다. 아래 예처럼 `previous_version`을 생략하려면 그 서비스의 정상 버전이 등록되어 있어야 하며, 없으면 `422 validation_failed`로 거부됩니다.

```bash
curl -sf -X POST "$API/v2/deployments" -H "$H" -H "Idempotency-Key: $CI_PIPELINE_ID-create" \
  -d '{"id":"'"$CI_PIPELINE_ID"'","service":"order-api","version":"'"$CI_COMMIT_TAG"'"}'
OP=$(curl -sf -X POST "$API/v2/deployments/$CI_PIPELINE_ID/observations" -H "$H" \
  -H "Idempotency-Key: $CI_PIPELINE_ID-canary" -d '{"phase":"canary"}' | jq -r .id)
while :; do
  R=$(curl -sf "$API/v2/operations/$OP" -H "$H")
  [ "$(jq -r .status <<<"$R")" != running ] && break
  sleep 5
done
exit "$(jq -r '.result.exit_code // 1' <<<"$R")"
```

---

# 4. 웹 콘솔

## 4.1 접속과 로그인

1. 브라우저에서 `https://<서버>:8088/console/`에 접속합니다.
2. 로그인 화면은 서버 설정에 따라 다릅니다.
   - SSO가 설정된 경우: "회사 계정으로 로그인합니다." 안내와 **회사 계정으로 로그인** 버튼이 나옵니다. 누르면 사내 IdP 로그인으로 이동합니다.
   - SSO가 없는 경우: "이 서버에는 SSO가 설정되지 않았습니다." 안내와 **API 토큰** 입력란이 나옵니다. 서비스 계정 토큰(`vgl_…`), API 키(`vgk_…`), OAuth 토큰(`vat_…`)을 넣고 **로그인**을 누릅니다. 토큰은 그 브라우저 탭에만 보관됩니다.
3. 로그인하면 상단 오른쪽에 신원(예: `user:alice`)이 표시됩니다. 그 위에 마우스를 올리면 권한(예: `operator@team=payments`)이 보입니다.

> 역할 바인딩이 없는 SSO 사용자는 로그인이 거부됩니다. 관리자에게 역할 부여를 요청하십시오.

## 4.2 공통 화면 요소

| 요소 | 설명 |
|---|---|
| 상단 메뉴 | **현황**, **배포**, **서비스**, **변경 동결**, **감사 기록** |
| 실시간 표시(●) | 이벤트 스트림 연결 상태. 연결되면 "실시간 이벤트 수신 중", 끊기면 "실시간 연결 끊김 (재시도 중)"이며 3초마다 다시 연결합니다 |
| **로그아웃** | 세션 또는 탭에 저장한 토큰을 지웁니다 |
| 알림 메시지 | 화면 오른쪽 아래. 요청 결과("... 요청을 보냈습니다", "... 실패: 이유")와 중요 이벤트(승인 요청, 롤백 실패, 서킷 열림) |
| 사유 입력 대화상자 | 모든 조작은 사유를 받습니다. "사유 (필수, 감사 기록에 남습니다)". **취소** 또는 확인 버튼 |

버튼은 사용자의 역할과 범위에 맞는 것만 보입니다. 화면에 버튼이 보여도 서버가 다시 권한을 확인합니다.

| 버튼 | 필요한 권한 |
|---|---|
| **관측 중단** | 그 서비스의 deployer |
| **롤백** | 그 서비스의 operator |
| **승인 (롤백 실행)**, **거절 (새 버전 유지)** | 그 서비스의 operator |
| 판정 평가 **저장**, **고치기** | 그 서비스의 deployer |
| **비상 정지 (서킷 열기)**, **서킷 닫기** | admin(전체 범위) |
| 변경 동결 **선언**, **종료** | admin(전체 범위) |
| 감사 기록 화면 조회 | viewer 전체 범위(`viewer@*`) |

## 4.3 현황 화면

메뉴의 **현황**(첫 화면)은 조치가 필요한 배포와 진행 중인 관측·롤백을 보여 주며, 이벤트가 오면 자동으로 갱신됩니다.

| 영역 | 내용 |
|---|---|
| 요약 카드 | **서킷**(상태 배지), **조치 필요**(건수), **진행 중**(건수), **변경 동결**(진행 중인 동결 이름 또는 "없음") |
| 서킷브레이커 패널 | 상태 배지와 사유. 서킷이 OPEN이면 빨간 테두리. admin에게는 CLOSED일 때 **비상 정지 (서킷 열기)**, 그 밖에는 **서킷 닫기** 버튼 |
| 조치 필요 (승인 대기 · 롤백 실패 · 보류) | `AWAITING_APPROVAL`, `ROLLBACK_FAILED`, `HELD` 상태 배포 |
| 진행 중 | `OBSERVING`, `ROLLING_BACK` 상태 배포 |
| 최근 배포 | 최근 20건 |
| 최근 이벤트 | 화면을 연 뒤 들어온 이벤트(최대 30건). 배포 ID를 누르면 상세로 이동 |

배포 표의 열은 **배포**(ID, 누르면 상세), **서비스**, **버전**(`이전 → 새`), **상태**, **이유**(140자까지), **갱신**(몇 분 전)입니다.

비상 정지 절차는 다음과 같습니다(admin).

1. **비상 정지 (서킷 열기)**를 누릅니다. "모든 자동 롤백과 새 배포가 멈춥니다." 안내가 나옵니다.
2. 사유를 입력하고 **서킷 열기**를 누릅니다(대화상자 제목은 "서킷 열기 (비상 정지)").
3. 서킷 배지가 **차단 (OPEN)**으로 바뀌는지 확인합니다.

서킷을 닫을 때는 **서킷 닫기**를 누르고, "원인 조사가 끝났는지 확인하십시오. 자동화가 다시 동작합니다." 안내를 확인한 뒤 사유를 넣습니다.

## 4.4 배포 목록 화면

메뉴의 **배포**는 읽을 수 있는 서비스의 배포를 최신순으로 50건씩 보여 줍니다.

1. **서비스** 입력란에 서비스 이름을 넣거나 **상태** 목록에서 상태를 고릅니다.
2. **조회**를 누릅니다.
3. 다음 페이지는 아래의 **다음** 버튼으로 이동합니다.
4. 배포 ID를 누르면 상세 화면으로 이동합니다.

## 4.5 배포 상세 화면

상세 화면은 위에서 아래로 다음 영역으로 구성됩니다.

| 영역 | 내용 |
|---|---|
| 머리글 | 배포 ID, 상태 배지, `exit N`(CI 종료 코드), 현재 이유 |
| 승인 패널 | `AWAITING_APPROVAL`일 때만 표시(4.5.2절) |
| 정보 패널 | **서비스**(팀), **버전**, **단계**, **대상**, **등록**(시각·작성자), **롤백 요청**, **승인**, **변경 티켓**("미검증" 표시 포함), **동결 예외**, **드라이런**, **최근 평가**(위반·보류·경고 수와 판정 가능 비율) |
| 조작 버튼 | **관측 중단**, **롤백** |
| 판정 평가 | 판정이 끝난 배포에 표시(4.5.3절) |
| 규칙 위반 | **규칙**, **대상**, **조치**(환경 요인이면 "(환경 요인)"), **내용** |
| 작업 | 이 배포에 대한 API 작업(관측, 롤백, 승인)의 **작업**, **종류**, **상태**, **결과**, **요청** |
| 타임라인 | 배포 이벤트를 최신순으로(시각, 종류, 메시지) |

### 4.5.1 관측 중단과 수동 롤백

- **관측 중단**은 `OBSERVING` 상태에서 보입니다. "판정 없이 관측을 멈춥니다. 롤백은 하지 않습니다." 사유를 넣고 **중단**을 누르면 배포는 `ABORTED`가 됩니다.
- **롤백**은 `ROLLING_BACK`, `ROLLED_BACK`, `AWAITING_APPROVAL`이 아닌 상태에서 보입니다. "`<서비스>`를 `<이전 버전>`(으)로 되돌립니다." 사유를 넣고 **롤백**을 누릅니다. 수동 롤백은 서킷이 OPEN이어도, 변경 동결 중에도 실행됩니다.

조작 후에는 알림 메시지에 "롤백 요청을 보냈습니다 (작업 <작업 ID>)"처럼 작업 ID가 표시되고, **작업** 표와 **타임라인**에서 진행을 확인할 수 있습니다.

### 4.5.2 승인 패널

승인 모드 서비스가 FAIL이면 패널 제목이 **롤백이 승인을 기다립니다**로 표시되고 다음 정보를 보여 줍니다.

| 항목 | 뜻 |
|---|---|
| 대상 | 롤백할 대상 |
| 격리됨 | 기다리는 동안 트래픽에서 뺀 대상(`drain_first`) |
| 이유 | FAIL 판정 내용 |
| 만료 | 승인 기한. 지나면 "(만료됨, 상위 호출함)" 표시 |

에스컬레이션 단계(예: VM 스냅샷 복원)가 승인을 기다리는 경우에는 제목이 **상위 복구 단계가 승인을 기다립니다**이며 **승인 (롤백 실행)** 버튼만 있습니다(거절은 API에서도 409로 거부되며, 멈추려면 관측 중단 또는 수동 롤백). 결정 절차는 6장을 따르십시오.

### 4.5.3 판정 평가 패널

판정이 끝난 배포(`SUCCEEDED`, `PROMOTED`, `HELD`, `ROLLED_BACK`, `ROLLBACK_FAILED`, `ABORTED`)에 표시됩니다. 아직 평가하지 않았으면 "아직 평가하지 않았습니다. 판정 품질(오탐·미탐) 측정에 쓰입니다."가 강조 표시됩니다.

| 입력 | 설명 |
|---|---|
| **평가** | **판정이 맞음**, **오탐 (정상인데 FAIL)**(FAIL 판정이 있던 배포만) 또는 **미탐 (문제가 있었는데 통과)**(FAIL 판정이 없던 배포만), **판단 불가** |
| **인시던트** | 근거 인시던트 번호(예: `INC0012345`) |
| **메모** | 확인한 내용 |

입력 후 **저장**(이미 평가했으면 **고치기**)을 누릅니다. 평가 결과는 "평가 · 작성자 · 시각 · 인시던트 · 메모" 형식으로 표시됩니다.

## 4.6 서비스 화면

메뉴의 **서비스**는 서비스별 구성을 보여 줍니다. 열은 **서비스**(누르면 그 서비스의 배포 목록), **팀**, **정상 버전**, **단계**(예: `canary → rolling → full`), **대상**(수), **실행기 / 트래픽**, **프리셋**입니다. 정상 버전이 `-`이면 `mark-good` 등록이 필요합니다.

## 4.7 변경 동결 화면

메뉴의 **변경 동결**은 설정 파일의 동결 창과 실행 중 선언된 동결을 함께 보여 줍니다. "동결 중에는 새 배포와 단계 시작을 받지 않습니다. 자동 롤백은 기간 설정에 따릅니다."

| 열 | 내용 |
|---|---|
| **이름** | 동결 이름과 사유 |
| **상태** | **동결 중 (~종료 시각)** 또는 **예정** |
| **기간** | 주간 반복 창 또는 시작~종료 |
| **범위** | 서비스, 팀, 또는 **전체** |
| **자동 롤백** | **허용** 또는 **금지** |
| 마지막 열 | API로 선언한 동결은 admin에게 **종료** 버튼, 설정 파일의 동결은 "설정 파일" |

admin에게는 **동결 선언** 패널이 보입니다. 사용법은 7장을 참고하십시오.

## 4.8 감사 기록 화면

메뉴의 **감사 기록**은 누가 무엇을 했는지 보여 줍니다. 전체 범위 viewer(`viewer@*`) 권한이 없으면 "감사 기록은 전체 범위(viewer@*) 권한이 있어야 볼 수 있습니다."가 표시됩니다.

1. 조회 조건을 넣습니다: **작업자**(예: `user:alice`), **동작**(예: `rollback.manual`), **서비스**, **시작 (RFC 3339)**. 시작을 비우면 최근 24시간을 보여 줍니다.
2. **조회**를 누릅니다. 기록은 오래된 순으로 100건씩 나오며 **다음**으로 넘깁니다.
3. 표의 열은 **시각**, **작업자**, **동작**, **서비스 / 배포**, **사유**, **티켓**입니다.

자주 보는 동작 값은 다음과 같습니다.

| 동작 | 뜻 |
|---|---|
| `phase.start`, `deployment.prepare` | 단계 관측 시작, 체크포인트 준비(CLI) |
| `rollback.manual` | 수동 롤백 |
| `deployment.create`, `deployment.abort` | 배포 등록(API·웹훅), 관측 중단 |
| `rollback.approve`, `rollback.reject`, `escalation.approve` | 승인·거절(콘솔·API·CLI) |
| `rollback.auto` | 자동 롤백(작업자 `system`) |
| `circuit.trip`, `circuit.reset` | 서킷 열기·닫기 |
| `freeze.create`, `freeze.end`, `freeze.override` | 동결 선언·종료(API·콘솔), 동결 예외 배포 |
| `deployment.feedback` | 판정 평가 |
| `mark-good` | 정상 버전 등록 |
| `denied` | 권한 거부 |
| `console.sign_in` | 콘솔 로그인 |
| `itsm.incident` | ServiceNow 인시던트 생성 |
| `breakglass.<작업>` | 로컬 CLI 비상 실행(예: `breakglass.circuit.reset`, `breakglass.rollback.approve`). 사유 칸에 입력한 이유 |
| `lease.unavailable`, `lease.conflict` | 저장소 장애로 lease 없이 롤백, 저장소 복구 후 lease 충돌(작업자 `system`) |

---

# 5. CLI 작업

## 5.1 배포 상태 확인

```bash
vigilante status -c /etc/vigilante/vigilante.yaml                 # 서킷 상태와 전체 배포 목록
vigilante status -c /etc/vigilante/vigilante.yaml --id order-api-1234   # 한 배포의 전체 기록(JSON), 종료 코드는 그 배포의 코드
```

`status`는 상태 저장소를 직접 읽습니다. 서버의 상태 저장소에 접근할 수 없는 곳에서는 `GET /v2/deployments/{id}`를 쓰십시오.

## 5.2 수동 롤백

| 상황 | 명령 |
|---|---|
| 이 파이프라인의 배포를 되돌림 | `vigilante rollback -c FILE` (CI 환경에서 배포 ID 자동 채움) |
| 배포 ID로 되돌림 | `vigilante rollback -c FILE --id order-api-1234 --reason "p99 악화"` |
| 다른 실행기로 재시도 | `vigilante rollback -c FILE --id order-api-1234 --executor legacy-kvm-snapshot` |
| 승인이 필요한 에스컬레이션 단계까지 허용 | `vigilante rollback -c FILE --id order-api-1234 --approve`(인증 환경에서는 `--break-glass "이유"` 필요, 5.11절) |
| 일부 대상만 | `vigilante rollback -c FILE --id order-api-1234 --targets order-bm-01,order-bm-02` |
| 기록에 없는 배포를 새로 만들어 되돌림 | `vigilante rollback -c FILE --id NEW-ID --service order-api --version v43 --previous v41` |
| 변경을 하지 않고 계획만 확인 | `vigilante rollback -c FILE --id order-api-1234 --dry-run` |

수동 롤백은 사람의 결정이므로 서킷, 플래핑 제한, 변경 동결을 거치지 않습니다(서비스별 잠금은 적용). 감사 기록에 `rollback.manual`(또는 `escalation.approve`)로 남으며, `--ticket CHG-123`으로 변경 티켓을 함께 남길 수 있습니다.

> `rollback` 명령은 `--server`를 지원하지 않으며 항상 로컬에서 실행합니다. 로컬 CLI는 역할을 검사하지 않고 작업자를 `cli:OS사용자@호스트`로 기록합니다. `--approve`·`--reject`가 없는 수동 롤백은 제한 없이 실행되지만, `--approve`·`--reject`는 인증 환경에서 `--break-glass`가 필요합니다(5.11절). 중앙 서버가 운영 중이면 콘솔의 **롤백** 버튼이나 `POST /v2/deployments/{id}/rollbacks`(`{"executor": "...", "targets": [...], "reason": "..."}`)를 쓰십시오. 에스컬레이션 단계에 승인이 필요해지면 배포가 `AWAITING_APPROVAL`이 되므로 5.3절대로 승인합니다. 서버가 실행 중일 때 같은 상태 저장소에 대해 로컬 CLI로 롤백·승인하는 경우의 동작은 확인 필요입니다(HA에서는 로컬 엔진이 리더가 아니면 기록이 펜싱될 수 있음).

## 5.3 승인과 거절

서버 운영 환경에서는 콘솔(6.2절) 또는 operator 토큰으로 API를 씁니다.

```bash
curl -X POST "$API/v2/deployments/order-api-1234/approvals" -H "Authorization: Bearer $VIGILANTE_TOKEN" \
  -H "Content-Type: application/json" -d '{"decision":"approve","comment":"운영자 확인: 5xx 증가 재현"}'
```

`decision`은 `approve` 또는 `reject`입니다. 에스컬레이션 단계 승인 대기는 승인만 할 수 있습니다(거절은 `409`).

서버를 쓸 수 없을 때(서버 장애)는 로컬 CLI로 결정합니다. 인증이 설정된 환경에서는 `--break-glass "이유"`가 필요하며, 감사 기록과 critical 알림이 남습니다(5.11절).

```bash
vigilante rollback -c FILE --id order-api-1234 --approve --reason "운영자 확인: 5xx 증가 재현" --break-glass "서버 장애 INC0012345"
vigilante rollback -c FILE --id order-api-1234 --reject  --reason "외부 결제사 장애로 판단, 새 버전 유지" --break-glass "서버 장애 INC0012345"
```

- 승인 대기 중인 배포가 아닌데 `--reject`를 주면 `deployment ... has no rollback waiting for approval` 오류입니다.
- `--approve`와 `--reject`는 함께 쓸 수 없습니다.
- 로컬 CLI 승인·거절에도 `auth.four_eyes`가 적용됩니다(break-glass 제외). 비교 대상은 CLI 작업자 이름(`cli:OS사용자@호스트`)입니다.

## 5.4 정상 버전 등록 (mark-good)

```bash
vigilante mark-good -c FILE --service order-api --version v41 --reason "현재 운영 버전"
```

성공하면 `order-api v41 recorded as known-good (...); later deployments default --previous to it`가 출력됩니다. 서버 API로는 `PUT /v2/services/{name}/last-good`입니다.

## 5.5 서킷 조회·차단·리셋

서버 운영 환경에서는 서버에 위임합니다. 조회는 viewer, 차단·리셋은 admin 토큰이 필요합니다.

```bash
export VIGILANTE_TOKEN=<토큰>
vigilante circuit status --server https://vigilante.example.internal:8088
vigilante circuit trip   --server https://vigilante.example.internal:8088 --reason "대형 장애 대응: 자동화 정지"
vigilante circuit reset  --server https://vigilante.example.internal:8088
```

서버를 쓸 수 없을 때는 상태 저장소에 직접 실행합니다. 조회(`status`)는 제한이 없지만, 인증이 설정된 환경에서 차단·리셋은 `--break-glass "이유"`가 필요합니다(5.11절).

```bash
vigilante circuit status -c FILE
vigilante circuit trip  -c FILE --reason "대형 장애 대응: 자동화 정지" --ticket INC0012345 --break-glass "서버 장애 중 자동화 정지"
vigilante circuit reset -c FILE --ticket CHG-123 --break-glass "서버 장애 중 원인 조치 완료"
```

동작(`status`·`trip`·`reset`)은 플래그보다 **앞에** 쓰십시오. `circuit -c FILE trip --reason ...`처럼 동작 뒤에 쓴 플래그는 해석되지 않아 사유가 기본값("manual kill switch (CLI)")으로 기록되고 `--ticket`·`--break-glass`도 빠집니다(cmd/vigilante/main.go `cmdCircuit`).

로컬 실행에서는 서킷이 OPEN이면 종료 코드 3, 아니면 0입니다. `--server`로 위임하면 서킷 상태와 관계없이 성공 시 0이므로 출력의 `state`를 확인하십시오. 원인 조사가 끝난 뒤에만 리셋하십시오.

## 5.6 동결 중 긴급 배포 (freeze override)

```bash
# 서버에 위임(admin 토큰)
vigilante watch --server https://vigilante.example.internal:8088 --service order-api --phase canary --freeze-override "INC0012345 긴급 보안 패치"
# 로컬 실행(인증 환경에서는 동결 중이면 --break-glass 필요)
vigilante watch -c FILE --service order-api --phase canary --freeze-override "INC0012345 긴급 보안 패치" --break-glass "INC0012345 긴급 배포"
vigilante prepare -c FILE --service order-api --freeze-override "INC0012345 긴급 보안 패치" --break-glass "INC0012345 긴급 배포"
```

배포의 `freeze_override`와 감사 기록(`freeze.override`)에 누가 왜 했는지 남습니다. API(v1·v2)와 `watch --server`는 admin만 `freeze_override`를 쓸 수 있습니다. 로컬 CLI의 `--freeze-override`는 인증이 설정된 환경에서 실제로 동결 중일 때 `--break-glass`가 있어야 하며, 없으면 종료 코드 3으로 거부됩니다(5.11절).

## 5.7 판정 평가

```bash
vigilante feedback -c FILE --id order-api-1234 --outcome false_positive --note "방화벽 변경으로 프로브 포트가 막힘"
vigilante feedback --server https://vigilante.example.internal:8088 --id order-api-1240 --outcome false_negative --incident INC0012345 --note "p99 악화를 늦게 발견"
```

`--outcome`은 `correct`, `false_positive`, `false_negative`, `unclear` 중 하나입니다.

## 5.8 파일럿 보고서

```bash
vigilante pilot report -c FILE --since 2026-11-01                                  # Markdown
vigilante pilot report -c FILE --service order-api,billing-api --json --out pilot.json
vigilante pilot report -c FILE --since 2026-11-01 --min-deployments 30 --max-hold-rate 0.1
```

출시 게이트를 통과하면 종료 코드 0, 미달이면 4입니다. 보고서는 아무것도 바꾸지 않습니다. 해석은 9장을 참고하십시오.

## 5.9 사전 점검 (doctor)

```bash
vigilante doctor -c FILE --service order-api --previous v41
vigilante doctor -c FILE --json
vigilante doctor -c FILE --junit doctor.xml --timeout 15s
```

결과 줄은 `[OK]`, `[WARN]`, `[FAIL]`, `[SKIP]`으로 시작하고, 문제가 있으면 다음 줄에 `→` 조치 안내가 나옵니다. 마지막 줄은 "점검 N건: 통과 a, 경고 b, 실패 c, 생략 d"입니다. 실패가 하나라도 있으면 종료 코드 1입니다. 점검 범위는 자격증명, 대상(SSH 접속, sudo), 프로브, 실행기(이전 릴리스·이미지 존재), 트래픽(풀 조회, 대상이 풀에 있는지), sudo 규칙, 용량(SSH 세션 수, DB 커넥션)입니다. doctor는 읽기 전용이며 아무것도 바꾸지 않습니다.

## 5.10 기타

| 명령 | 용도 |
|---|---|
| `vigilante whoami --server URL` | 내 토큰의 신원과 권한 |
| `vigilante presets show java-web` | 서비스 판정 기준(프리셋) 확인 |
| `vigilante plugins` | 이 바이너리에 든 플러그인 전체와 검증 수준(최소 빌드에 없는 것은 "not in this build") |

## 5.11 로컬 CLI 제한과 break-glass

`--server` 없이 실행한 CLI는 서버 API를 거치지 않고 상태 저장소를 직접 다루므로 역할 검사를 받지 않습니다. 그래서 인증이 설정된 환경(관리자 설정 `auth.local_cli`, 기본 `auto`)에서는 다음 로컬 명령을 거부합니다.

| 로컬 명령 | 평소 쓰는 경로 |
|---|---|
| `rollback --approve`·`--reject`(승인 대기 결정, 에스컬레이션 허용) | 콘솔 승인 패널, `POST /v2/deployments/{id}/approvals`(operator) |
| `circuit reset`·`trip` | 콘솔 서킷 패널, `circuit reset --server URL`(admin) |
| 동결 중 `watch`·`prepare --freeze-override` | `watch --server URL --freeze-override`(admin) |

- 거부되면 `<작업> refused: the API has authentication, so privileged local commands are restricted (auth.local_cli). Run it through the server with --server and an operator or admin token, or pass --break-glass REASON (audited and alerted)`가 나옵니다.
- 서버 장애처럼 서버를 쓸 수 없을 때만 `--break-glass "이유"`를 붙여 실행합니다. 감사 기록 `breakglass.<작업>`과 critical 알림 `Break-glass: cli:<사용자>@<호스트> ran <작업> locally`가 남으므로, 이유에 인시던트 번호를 넣고 `--ticket`도 함께 씁니다.
- 수동 롤백(`--approve` 없는 `rollback`), `mark-good`, `status`, `prepare`, `baseline`, `feedback`은 제한하지 않습니다.

---

# 6. 승인 모드 업무 절차

## 6.1 흐름

1. 승인 모드(`rollback.mode: approve`) 서비스의 단계가 FAIL이면 롤백 계획(대상, 이유, 만료 시각)이 만들어지고 배포가 `AWAITING_APPROVAL`이 됩니다. CI는 종료 코드 3으로 멈춥니다.
2. `drain_first: true`이면 위반한 대상이 트래픽에서 빠진 채로 기다립니다.
3. critical 알림 "`<서비스> <단계> failed — rollback to <이전 버전> awaits approval`"과 콘솔 알림 "승인 요청"이 갑니다. 이벤트는 `vigilante.approval.requested`입니다.

## 6.2 결정 절차 (operator)

1. 콘솔 **현황**의 **조치 필요**에서 배포를 엽니다.
2. 승인 패널의 **대상**, **격리됨**, **이유**, **만료**를 확인합니다.
3. **규칙 위반** 표와 **타임라인**에서 근거를 확인합니다. 필요하면 서비스 대시보드, 로그를 함께 봅니다.
4. 결정합니다.
   - 실제 문제라고 판단하면 **승인 (롤백 실행)**을 누르고 사유를 넣습니다. 준비된 계획대로 롤백하며, 승인자는 `approved_by`에 남습니다. 서킷과 플래핑 제한을 거치지 않습니다.
   - 오탐이라고 판단하면 **거절 (새 버전 유지)**을 누르고 사유를 넣습니다. 새 버전을 유지하고, 격리했던 대상을 트래픽에 다시 넣은 뒤 `HELD`로 둡니다.
5. 결정 후 상태가 `ROLLING_BACK` → `ROLLED_BACK`(승인) 또는 `HELD`(거절)로 바뀌는지 확인합니다.
6. 판정 평가를 기록합니다(8장). 거절한 경우는 보통 `false_positive`입니다.

> `auth.four_eyes: true`이면 배포를 만든 사람이나 롤백을 요청한 사람은 콘솔·API로 승인·거절할 수 없습니다(`403 forbidden`). 다른 운영자에게 요청하십시오. 로컬 CLI(`vigilante rollback --approve`)에도 적용되지만 비교 대상은 CLI 작업자 이름(`cli:OS사용자@호스트`)이고, `--break-glass`를 쓰면 적용되지 않습니다(5.11절).

## 6.3 시간 초과

| `on_timeout` | 기한(`approval.timeout`, 기본 30m)이 지나면 |
|---|---|
| `hold`(기본) | 계속 기다리며 "ESCALATION: `<서비스>` rollback still awaits approval" 알림을 한 번 더 보냅니다. 콘솔 만료 칸에 "(만료됨, 상위 호출함)"이 붙고, 그 뒤에도 승인할 수 있습니다 |
| `rollback` | 자동 롤백으로 넘어갑니다. 이때는 서킷·플래핑 제한이 적용됩니다 |

시간 초과 처리는 서버 리더가 15초마다 합니다. 서버 없이 CI 단발 실행만 쓰는 경우에는 시간 초과 처리가 없습니다.

---

# 7. 변경 동결 업무 절차

## 7.1 동결 확인

1. 콘솔 **현황**의 **변경 동결** 카드 또는 **변경 동결** 화면에서 진행 중인 동결을 확인합니다.
2. 동결 중 배포를 시도하면 CLI는 종료 코드 3, API는 `409 change_frozen`으로 거부되며, 응답에 동결 이름, 이유, 끝나는 시각이 있습니다.

## 7.2 장애 대응 동결 선언 (admin)

1. **변경 동결** 화면의 **동결 선언** 패널에서 **이름**(예: `incident-123`), **사유**, **종료 시각**, **팀**(쉼표로 구분, 비우면 전체)을 넣습니다.
2. 자동 롤백을 허용하려면 **자동 롤백 허용**을 선택된 상태로 둡니다(기본). 해제하면 동결 중 FAIL에서 자동 롤백 대신 실패 대상을 격리하고 사람에게 넘깁니다.
3. **선언**을 누릅니다. 이름, 사유, 종료 시각이 없으면 "이름, 사유, 종료 시각을 넣으십시오"가 표시됩니다.
4. 목록에 **동결 중** 배지로 표시되는지 확인합니다.
5. 일찍 끝내려면 그 행의 **종료**를 누르고 사유를 넣습니다. 설정 파일의 동결은 콘솔에서 끝낼 수 없습니다.

## 7.3 동결 중 긴급 배포

1. 긴급 배포가 필요한 근거(인시던트·변경 번호)를 확보합니다.
2. admin 권한자가 예외 사유를 넣어 배포를 등록합니다(`watch --server URL --freeze-override "사유"`와 admin 토큰, 또는 API `freeze_override`). 서버를 쓸 수 없으면 로컬 CLI에 `--break-glass "이유"`를 함께 붙입니다(5.6절, 5.11절).
3. 콘솔 배포 상세의 **동결 예외** 칸에 사유가 표시되는지 확인합니다.
4. 감사 기록에서 동작 `freeze.override`로 기록을 확인합니다.

---

# 8. 판정 평가 기록

판정 평가는 판정 품질(오탐·미탐)을 측정하는 유일한 근거입니다. FAIL·HOLD로 끝난 배포는 반드시, 통과한 배포도 장애가 났다면 반드시 기록하십시오.

| 평가 | 뜻 | 쓰는 때 |
|---|---|---|
| `correct` | 판정이 맞음 | FAIL·HOLD가 실제 문제였거나, 통과한 배포가 실제로 정상 |
| `false_positive` | 오탐: FAIL이었지만 배포는 정상 | 롤백(또는 승인 요청)이 필요 없었음. FAIL 판정이 있던 배포만 |
| `false_negative` | 미탐: 문제 배포를 FAIL하지 않음 | 사후 인시던트로 드러남. 인시던트 번호를 함께 |
| `unclear` | 판단 불가 | 원인 조사 중이거나 자료 부족 |

기록 절차는 다음과 같습니다.

1. 배포 상세의 **판정 평가** 패널을 엽니다(또는 `vigilante feedback`).
2. 평가, 인시던트 번호, 확인한 내용을 넣습니다.
3. **저장**을 누릅니다. 다시 저장하면 이전 평가를 바꿉니다.
4. 평가는 감사 기록 `deployment.feedback`에 남습니다.

미탐은 판정이 놓친 것이므로 스스로 드러나지 않습니다. 매주 대상 서비스의 인시던트를 훑어, 원인이 배포인데 Vigilante가 통과시킨 건을 `false_negative`로 기록하십시오.

---

# 9. 파일럿 보고서 읽기

## 9.1 출시 게이트

`vigilante pilot report`의 Markdown 출력은 제목("Decision quality report") 다음에 "Release gate: **MET**" 또는 "**NOT MET**"과 게이트 표가 나옵니다.

| 보고서 항목 | 기준(기본값) | 뜻 |
|---|---|---|
| `observed deployments` | 30 이상(`--min-deployments`) | 관측한 배포 수 |
| `false positives` | 0 이하 | 오탐 평가 수 |
| `false negatives` | 0 이하 | 미탐 평가 수 |
| `hold rate (held / observed phases)` | 10% 이하(`--max-hold-rate`) | 보류된 단계 비율 |
| `real rollbacks completed` | 1 이상 | 드라이런을 제외한 실제 롤백 완료 수 |
| `failed or held deployments assessed` | all | 평가하지 않은 FAIL·HOLD 배포가 없어야 함 |

## 9.2 나머지 절

| 절 | 내용과 읽는 법 |
|---|---|
| Verdicts | 관측한 배포·단계 수, 전체와 서비스별 Pass·Fail·Hold·Aborted·In progress. `Hold causes`에 보류 원인별 수 |
| Assessment | 평가한 수와 correct·false positive·false negative·unclear 수 |
| Rollbacks and timing | 완료·실패·드라이런(롤백했을 것)·수동 롤백 수, 승인 모드의 승인·거절·대기 수. "observation start -> FAIL", "rollback start -> done", "approval wait"의 중앙값·P90·최대(초) |
| Findings | 오탐·미탐·롤백 실패 배포 목록(해당 건이 없으면 절이 생략됨) |
| Needs assessment | 평가하지 않은 FAIL·HOLD 배포 목록(없으면 절이 생략됨). 이 목록이 비어야 게이트를 통과합니다 |

보류 원인 값의 뜻은 다음과 같습니다.

| 값 | 뜻 | 개선 방향 |
|---|---|---|
| `environmental` | 대조군도 같이 나빠짐 | 대조군 구성 점검 |
| `observer_disagreement` | 관측점(중앙·에이전트)끼리 불일치 | 에이전트·중앙 프로브의 경로 차이 점검 |
| `insufficient_evidence` | 표본 부족 | 관측 창·프로브 주기·`min_samples` 조정 |
| `hold_rule` | `action: hold` 규칙이 위반됨. 관측 장치 과부하로 보류한 배포(이유에 `observer degraded`)도 여기로 집계됨 | 해당 규칙 검토. `observer degraded`면 서버 자원 점검(런북 19장) |
| `other` | 그 밖(관측 중단 등) | 타임라인 확인 |

## 9.3 주간 점검 절차

1. 매주 보고서를 뽑습니다: `vigilante pilot report -c FILE --since <파일럿 시작일> --out pilot.md`
2. **Needs assessment** 목록의 배포마다 판정 평가를 기록합니다.
3. **Findings**의 오탐은 규칙의 임계치, `for`, `warmup`, 관측 창을 조정할 근거로 관리자에게 전달합니다.
4. HOLD 비율이 높으면 원인별로 위 표의 개선 방향을 검토합니다.
5. 종료 코드 0(게이트 통과)이 나오면 보고서를 판정 품질 보고서로 확정합니다.

---

# 부록 A. 이벤트 종류

콘솔 **최근 이벤트**와 웹훅으로 받는 이벤트입니다.

| 이벤트 | 콘솔 표시 |
|---|---|
| `vigilante.deployment.created`, `vigilante.deployment.marked_good` | 배포 등록, 정상 버전 등록 |
| `vigilante.observation.started`, `.passed`, `.failed`, `.held`, `.aborted` | 관측 시작, 단계 통과, 단계 실패, 보류, 관측 중단 |
| `vigilante.rollback.started`, `.completed`, `.failed` | 롤백 시작, 롤백 완료, 롤백 실패 |
| `vigilante.approval.requested`, `.decided` | 승인 요청, 승인 결정 |
| `vigilante.circuit.opened`, `.half_opened`, `.closed` | 서킷 열림, 서킷 시험, 서킷 닫힘 |
| `vigilante.agent.lost` | 에이전트 끊김 |
| `vigilante.webhook.disabled` | 웹훅 중지 |
| `vigilante.ping` | 테스트 |
