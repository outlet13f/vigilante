---
title: 장애 대응 런북
doc_id: VGL-OP-03
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

이 런북은 Vigilante 운영 중 발생할 수 있는 장애 상황별로 증상, 확인 방법, 조치 절차, 복구 확인, 예방 방법을 정리한 문서입니다. 온콜 운영자와 시스템 관리자가 알림을 받은 직후 바로 따라 할 수 있도록 작성했습니다.

기준 자료는 개발 문서의 실패 복구 및 비상 정지 설계(docs/04-safety-circuit-breaker.md)와 저장소의 구현 코드(master 병합 커밋 `537870c`, PR #12·#13·#14 포함)입니다. 설정 키 설명은 관리자 매뉴얼(VGL-OP-01), 콘솔 사용법은 운영자 매뉴얼(VGL-OP-02)을 참고하십시오.

## 1.2 대응 원칙

Vigilante의 안전 설계는 다음 네 원칙을 따르며, 장애 대응도 같은 원칙을 지킵니다.

1. **파괴하지 말고 격리한다.** 확신이 없으면 재시작·스냅샷 복원 대신 대상을 트래픽에서 빼 둔 상태로 사람에게 넘깁니다.
2. **롤백이 장애를 키우지 않는다.** 풀의 최소 가용 수(blast radius)를 깨는 드레인은 하지 않습니다.
3. **자동화가 실패하면 자동화를 멈춘다.** 롤백 실패가 누적되면 서킷을 열어 자동 변경을 동결합니다.
4. **모든 결정은 먼저 기록된다.** 기록 후 실행하고, 재시작 시 이어서 수행합니다.

> 대응 중 서킷 리셋, 수동 롤백 같은 조치는 모두 감사 기록에 남습니다. 변경·인시던트 번호를 `--ticket`(CLI) 또는 `X-Change-Ticket` 헤더(API)로 함께 남기십시오.
>
> 권한이 큰 조치(승인·거절, 에스컬레이션 승인, 서킷 리셋·차단, 동결 예외)는 콘솔, API, 또는 `--server URL`과 operator·admin 토큰(`VIGILANTE_TOKEN`)으로 서버를 통해 실행하는 것이 기본입니다. 인증이 설정된 환경에서 `--server` 없이 로컬 CLI로 실행하려면 `--break-glass "이유"`가 필요하며, 서버 장애처럼 서버를 쓸 수 없을 때만 씁니다(21장).

## 1.3 공통 확인 명령

장애 유형과 관계없이 먼저 아래를 확인합니다.

```bash
# 서버 생존·역할·서킷·저장소
curl -fsS https://vigilante.example.internal:8088/healthz
# 준비 상태 (저장소 연결, HA 리더)
curl -sS https://vigilante.example.internal:8088/readyz
# 서킷과 배포 목록 (상태 저장소 직접 조회)
vigilante status -c /etc/vigilante/vigilante.yaml
# 한 배포의 전체 기록: 상태, 이유, 위반, 타임라인
vigilante status -c /etc/vigilante/vigilante.yaml --id <배포 ID>
# 최근 조치 기록
vigilante audit query -c /etc/vigilante/vigilante.yaml --since <YYYY-MM-DD>
# 서버 로그 (systemd)
journalctl -u vigilante-server --since "30 min ago"
```

서버를 통해 조회할 때는 `GET /v2/deployments/{id}`(viewer)를 씁니다. 응답에 `state`, `reason`, `breaches`, `events`, `pending_rollback`, `exit_code`가 있습니다.

## 1.4 종료 코드와 알림 요약

| CI 종료 코드 | 상황 | 이 런북의 절 |
|---|---|---|
| 2 | 자동 롤백 완료(서비스 복구됨) | 대응 불필요, 판정 평가만 |
| 3 | 롤백 실패, 서킷 OPEN, 승인 대기, 동결·티켓 게이트 거부(`watch --server`에서 서버가 거부한 경우 포함) | 2장, 3장, 5장, 13장 |
| 4 | HOLD 또는 INCONCLUSIVE | 4장, 19장 |
| 1 | 설정·접근 오류, 로컬 CLI 제한 거부 | 9장~12장, 21장 |

| 알림 제목(영문 원문) | 수준 | 뜻 |
|---|---|---|
| `ROLLBACK FAILED <서비스> — human intervention required` | critical | 롤백 실패 |
| `<서비스>: rollback blocked by safety guard` | critical | 서킷 OPEN·플래핑·동결로 자동 롤백 차단 |
| `<서비스> <단계> failed — rollback to <버전> awaits approval` | critical | 승인 모드 승인 요청 |
| `<서비스> rollback needs approval` | critical | 승인 필요 에스컬레이션 단계만 남음 |
| `ESCALATION: <서비스> rollback still awaits approval` | critical | 승인 기한 초과 |
| `<서비스> <단계> HELD — human decision needed` | warning | 보류(4장). 이유에 `observer degraded`가 있으면 19장 |
| `<서비스>: Rollback without the service lease` | warning | 저장소 장애로 lease 없이 롤백 진행(20장) |
| `<서비스>: Concurrent rollback suspected` | warning | lease 충돌: 다른 프로세스가 같은 서비스에 조치했을 수 있음(20장) |
| `Break-glass: cli:<사용자>@<호스트> ran <작업> locally` | critical | 로컬 CLI 비상 실행(21장) |
| `Webhook subscription disabled: <URL>` | critical | 웹훅 구독 자동 중지 |

## 1.5 지원 번들

원인을 찾지 못해 개발팀에 넘길 때는 지원 번들을 만들어 전달합니다. 비밀값은 가려지지만 보내기 전에 내용을 확인하십시오.

```bash
VIGILANTE_TOKEN=vgl_... vigilante support-bundle -c /etc/vigilante/vigilante.yaml --server https://127.0.0.1:8088
```

---

# 2. 롤백 실패 (ROLLBACK_FAILED)

## 2.1 증상

- CI가 종료 코드 3으로 끝나고 "ROLLBACK FAILED or circuit OPEN" 단계 메시지를 남깁니다.
- critical 알림 `ROLLBACK FAILED <서비스> — human intervention required`, 이벤트 `vigilante.rollback.failed`. 콘솔 알림 "롤백 실패".
- 콘솔 **현황**의 **조치 필요**에 **롤백 실패** 배지로 표시됩니다.
- 배포 이유(reason) 예:
  - `<대상>: <오류>; ... — failed targets are left drained (isolated); circuit: CLOSED` (모든 전략 실패)
  - `automatic rollback blocked: flapping guard: too many automatic rollbacks for this service: 3 in the last hour (max 3); isolation: ...`
  - `automatic rollback blocked: rollback cooldown active for this service: ...`
  - `automatic rollback blocked: circuit breaker OPEN: automated rollback actions are frozen (...)`
  - `rollback not started: another rollback for this service is in progress (held by ...)`
  - `rollback not started: state store unreachable: cannot take the service rollback lease: ...` (`safety.rollback_lease.on_unavailable: fail`일 때 저장소 장애, 20장)
- 지표: `vigilante_rollbacks_total{result="failed"}` 또는 `{result="blocked"}` 증가. ServiceNow 연동 시 인시던트 자동 생성.

## 2.2 확인

1. 배포의 이유와 타임라인을 봅니다. 어느 대상의 어느 단계(`traffic.drain`, `app.rollback`, `app.verify`, `probe.verify`, `traffic.enable`)가 실패했는지 확인합니다.
   ```bash
   vigilante status -c /etc/vigilante/vigilante.yaml --id <배포 ID>
   ```
2. 서킷 상태를 봅니다. 이유 끝의 `circuit: OPEN`이면 3장도 함께 진행합니다.
   ```bash
   vigilante circuit status -c /etc/vigilante/vigilante.yaml
   ```
3. 실패 원인 유형을 가립니다.

| 이유에 나타나는 문구 | 원인 유형 |
|---|---|
| `flapping guard`, `rollback cooldown` | 자동 롤백 반복 차단(S10) |
| `circuit breaker OPEN` | 서킷이 열려 있어 자동 롤백 금지(3장) |
| `change freeze ... forbids automatic rollback` | `allow_rollback: false` 동결 기간 |
| `another rollback for this service is in progress` | 같은 서비스에 동시 롤백 시도(S11) |
| `state store unreachable: cannot take the service rollback lease` | 저장소 장애 중 롤백 시작, `on_unavailable: fail`(20장) |
| 그 밖의 실행기·트래픽 오류 | 롤백 경로 고장(이전 릴리스 삭제, 이미지 pull 불가, 자격증명 만료, sudo 거부 등) |

4. 롤백 경로를 읽기 전용으로 점검합니다. 이전 릴리스·이미지 존재, SSH·sudo, LB 풀 상태를 확인합니다.
   ```bash
   vigilante doctor -c /etc/vigilante/vigilante.yaml --service <서비스> --previous <이전 버전>
   ```

## 2.3 조치

1. 서비스 영향부터 확인합니다. 트래픽 제어기가 있는 서비스는 실패한 대상이 풀에서 빠진 채 격리되어 있으므로(`traffic.enable` 미실행), 남은 대상으로 서비스가 유지되는지 확인합니다.
2. 원인 유형별로 조치합니다.
   - **롤백 경로 고장:** doctor가 알려 준 원인(이전 릴리스 디렉토리 복구, 레지스트리 복구, 자격증명 갱신, sudoers 추가)을 고친 뒤 수동 롤백을 다시 실행합니다. 서버 운영 중이면 콘솔 배포 상세의 **롤백** 버튼 또는 v2 API를 씁니다.
     ```bash
     vigilante rollback -c /etc/vigilante/vigilante.yaml --id <배포 ID> --reason "원인 수정 후 재시도" --ticket <티켓>
     ```
   - **1차 전략을 쓸 수 없음:** 다른 실행기로 재시도합니다. 서버 운영 중이면 operator 토큰으로 API를 씁니다. 승인이 필요한 에스컬레이션 단계(VM 스냅샷 등)에 이르면 배포가 `AWAITING_APPROVAL`이 되고 `<서비스> rollback needs approval` 알림이 가므로, 콘솔 **승인 (롤백 실행)** 또는 승인 API로 허용합니다(`auth.four_eyes: true`이면 롤백을 요청한 사람이 아닌 다른 operator).
     ```bash
     export VIGILANTE_TOKEN=<operator 토큰>
     curl -X POST "$API/v2/deployments/<배포 ID>/rollbacks" -H "Authorization: Bearer $VIGILANTE_TOKEN" \
       -H "Content-Type: application/json" -d '{"executor":"<다른 실행기>","reason":"1차 전략 불가, 재시도"}'
     curl -X POST "$API/v2/deployments/<배포 ID>/approvals" -H "Authorization: Bearer $VIGILANTE_TOKEN" \
       -H "Content-Type: application/json" -d '{"decision":"approve","comment":"스냅샷 복원 허용"}'
     ```
     서버를 쓸 수 없을 때(서버 장애)는 로컬 CLI로 에스컬레이션까지 한 번에 허용합니다. 인증이 설정된 환경에서는 `--break-glass`가 필요하며 감사 기록과 critical 알림이 남습니다(21장).
     ```bash
     vigilante rollback -c /etc/vigilante/vigilante.yaml --id <배포 ID> --executor <다른 실행기> --approve \
       --break-glass "서버 장애 중 스냅샷 복원 허용" --ticket <티켓>
     ```
   - **플래핑 차단:** 이전 버전 자체가 불량일 수 있습니다. 더 이전 버전으로 새 배포 ID를 만들어 수동 롤백합니다.
     ```bash
     vigilante rollback -c /etc/vigilante/vigilante.yaml --id <새 ID> --service <서비스> --version <불량 버전> --previous <더 이전 버전>
     ```
   - **동시 롤백:** 먼저 실행 중인 롤백(다른 CI 잡, 서버)이 끝날 때까지 기다린 뒤 상태를 다시 봅니다. 서비스별 롤백 락은 상태 저장소의 리스로 관리되며, 보유 프로세스가 비정상 종료하면 리스 유효 시간(2분)이 지난 뒤 다른 프로세스가 가져갈 수 있습니다. 오류의 `held by` 뒤에 보유자(호스트/PID)가 표시됩니다.
3. 대상을 수동으로 복구했다면(예: 직접 이전 버전 배포) LB 풀에 다시 넣습니다. Vigilante가 격리한 대상은 자동으로 복귀하지 않습니다.
4. 서킷이 OPEN이면 3장 절차로 원인을 정리한 뒤 리셋합니다.

## 2.4 복구 확인

- 배포 상태가 `ROLLED_BACK`이고 `vigilante status --id`의 종료 코드가 2인지 확인합니다.
- 격리했던 대상이 LB 풀에서 활성 상태인지 확인합니다(`vigilante doctor`의 트래픽 항목 "풀 조회": 멤버 수 중 활성 수).
- 서비스 헬스 체크와 오류율이 정상인지 확인합니다.
- 판정 평가를 기록합니다(대부분 `correct`).

## 2.5 예방

- 파이프라인 첫 단계에서 `vigilante doctor --service <서비스> --previous <버전>`을 강제합니다. 이전 릴리스 삭제, sudo 비밀번호 요구, 풀에 없는 대상을 롤백 순간이 아니라 배포 전에 발견합니다.
- 1차 전략 뒤에 에스컬레이션(`rollback.escalation`)을 둡니다. 파괴적인 전략에는 `require_approval: true`를 둡니다.
- 릴리스 디렉토리·이전 이미지를 최소 1개 버전 이상 보관하도록 배포 도구를 설정합니다.
- 정기적으로 `--dry-run`으로 롤백 플랜을 리허설합니다.

---

# 3. 서킷 브레이커 OPEN

## 3.1 증상

- 새 배포의 `watch`가 종료 코드 3으로 끝나고 `deployment gate closed: circuit breaker OPEN: automated rollback actions are frozen (<사유>); reset with "vigilante circuit reset" after investigation` 오류를 남깁니다. API(v1 배포 등록·v2 관측 시작)는 `409 circuit_open`입니다. 원격 모드(`watch --server`)도 서버의 게이트 거부를 받아 종료 코드 3으로 끝납니다.
- 알림·이벤트 `vigilante.circuit.opened`(콘솔 "서킷 열림"). ServiceNow 연동 시 인시던트 생성.
- 콘솔 **현황**의 서킷 카드가 **차단 (OPEN)**, 서킷브레이커 패널이 빨간 테두리로 표시됩니다.
- 지표 `vigilante_circuit_state{state="open"} 1`(레이블 값은 `closed`, `open`, `half_open`). `GET /healthz`의 `circuit`이 `OPEN`.
- 자동 롤백이 필요한 배포는 `automatic rollback blocked: circuit breaker OPEN ...`으로 `ROLLBACK_FAILED`가 되고, 실패 대상은 blast radius 범위 안에서 격리됩니다.

## 3.2 확인

1. 서킷 상태와 열린 사유를 확인합니다.
   ```bash
   vigilante circuit status --server https://vigilante.example.internal:8088   # VIGILANTE_TOKEN(viewer 이상)
   vigilante circuit status -c /etc/vigilante/vigilante.yaml                    # 상태 저장소 직접 조회
   ```
2. 사유가 자동(롤백 실패 누적)인지 수동(`circuit trip`)인지 감사 기록에서 확인합니다. 로컬 CLI의 비상 실행은 `breakglass.circuit.trip`으로 남습니다.
   ```bash
   vigilante audit query -c /etc/vigilante/vigilante.yaml --action circuit.trip --since <날짜>
   vigilante audit query -c /etc/vigilante/vigilante.yaml --action breakglass.circuit.trip --since <날짜>
   vigilante audit query -c /etc/vigilante/vigilante.yaml --action rollback.auto --since <날짜>
   ```
3. 자동으로 열렸다면 `safety.circuit_breaker.window`(기본 1h) 안의 롤백 실패 배포들을 확인합니다. 콘솔 배포 목록에서 상태 **롤백 실패**로 거르면 됩니다.
4. 실패들이 같은 원인(레지스트리 장애, 스냅샷 누락, 자격증명 만료, Vault 장애 등)인지 판단합니다.

## 3.3 조치

1. 서킷이 열려 있어도 수동 롤백과 트래픽 격리는 가능하므로, 먼저 2장 절차로 실패한 배포의 서비스 영향을 정리합니다.
2. 롤백 경로를 고장 낸 공통 원인을 해결합니다.
3. 원인이 해결되었는지 `vigilante doctor`로 확인합니다.
4. admin이 서킷을 닫습니다. 콘솔에서는 **서킷 닫기**를 누르고 사유를 입력합니다. CLI로는 admin 토큰으로 서버에 위임합니다.
   ```bash
   export VIGILANTE_TOKEN=<admin 토큰>
   vigilante circuit reset --server https://vigilante.example.internal:8088
   ```
   서버가 응답하지 않아 위임할 수 없을 때만 상태 저장소에 직접 리셋합니다. 인증이 설정된 환경에서는 `--break-glass`가 필요하며, 감사 기록 `breakglass.circuit.reset`과 critical 알림이 남습니다(21장).
   ```bash
   vigilante circuit reset -c /etc/vigilante/vigilante.yaml --break-glass "서버 장애 중 원인 조치 후 리셋" --ticket <티켓>
   ```
   동작(`reset`·`trip`·`status`)은 플래그보다 앞에 씁니다. `circuit -c FILE reset --ticket ...`처럼 동작 뒤에 쓴 플래그는 해석되지 않아 티켓·사유·`--break-glass`가 적용되지 않습니다(cmd/vigilante/main.go `cmdCircuit`). 서버 위임 시 종료 코드는 서킷 상태와 관계없이 0이므로 출력의 `state`를 확인합니다. 로컬 리셋은 상태 저장소만 바꾸므로, 서버가 다시 뜨면 `vigilante circuit status --server URL`로 서버가 보는 상태도 확인합니다.
5. `open_duration`이 양수로 설정되어 있으면 그 시간 뒤 자동으로 HALF_OPEN이 되어 롤백 1건만 시험으로 허용합니다. 시험 롤백이 성공하면 CLOSED, 실패하면 다시 OPEN입니다. 기본값 `0s`는 수동 리셋만 허용합니다.

> 대형 장애 대응이나 변경 동결 중에 자동화를 즉시 멈추려면 반대로 서킷을 엽니다: 콘솔 **비상 정지 (서킷 열기)** 또는 `vigilante circuit trip --server URL --reason "<사유>"`(admin 토큰). 서버를 쓸 수 없으면 `vigilante circuit trip -c FILE --reason "<사유>" --break-glass "<이유>"`.

## 3.4 복구 확인

- `vigilante circuit status --server URL`(또는 `-c FILE`)의 출력이 `CLOSED`인지 확인합니다(로컬 실행은 OPEN일 때만 종료 코드 3이므로 HALF_OPEN도 0입니다).
- 지표 `vigilante_circuit_state{state="closed"} 1`, 이벤트 `vigilante.circuit.closed`.
- 보류했던 배포 파이프라인을 다시 실행하여 `watch`가 정상 진행하는지 확인합니다.

## 3.5 예방

- 롤백 실패를 2장 예방 항목으로 줄입니다.
- 서킷 상태를 경보로 감시합니다(`vigilante_circuit_state{state="open"} == 1`).
- CI 단발 실행만 쓰는 경우에도 서킷 이력이 공유되도록 `server.journal_path`를 공유 경로로 둡니다.

---

# 4. 보류된 배포 (HELD)

## 4.1 증상

- CI가 종료 코드 4로 끝납니다. Jenkins 예시는 "is HELD (environmental / inconclusive). Promote anyway?" 입력 단계로 멈춥니다.
- 이벤트 `vigilante.observation.held`(콘솔 "보류"). 콘솔 **조치 필요**에 **보류** 배지.
- 배포 이유가 아래 원인 중 하나입니다.

| 원인 | 이유(reason) 문구 | 보고서 분류 |
|---|---|---|
| 환경 요인 | `unresolved hold condition at end of window: [규칙@대상] ... — also firing on control target <대조군> (environmental)` | `environmental` |
| 관측점 불일치 | `unresolved hold condition at end of window: [규칙@대상] observer disagreement: breached from [...], healthy from [...]` | `observer_disagreement` |
| 증거 부족 | `insufficient evidence (N samples, k/m rule evaluations known)` | `insufficient_evidence` |
| hold 규칙 | `unresolved hold condition at end of window: ...`(`action: hold` 규칙) | `hold_rule` |
| 관측 장치 과부하 | `unresolved hold condition at end of window: [규칙@대상] ... — observer degraded (<신호>), not attributed to the release` | `hold_rule`(별도 분류 없음). 19장 |
| 승인 거절 | `rollback rejected by <작업자>: ...` | 6장 참고 |

- 콘솔 배포 상세의 **규칙 위반** 표에서 환경 요인은 조치 칸에 "hold (환경 요인)"으로 표시됩니다.

## 4.2 확인

1. 배포 상세(콘솔 또는 `vigilante status --id`)에서 이유와 **최근 평가**(위반·보류·경고 수, 판정 가능 비율)를 확인합니다.
2. 원인별로 추가 확인합니다.
   - **환경 요인:** 새 버전을 받지 않은 대조군에서도 같은 규칙이 위반되었습니다. 공유 의존성(DB, 상위 서비스, 네트워크) 장애 여부를 확인합니다. 이 경우 롤백해도 복구되지 않습니다.
   - **관측점 불일치:** 중앙(SSH)과 에이전트 중 한쪽만 위반을 봤습니다. 오케스트레이터→대상 네트워크, SSH 접속, 에이전트 상태를 확인합니다. `vigilante_agents_connected`, 이벤트 `vigilante.agent.lost`도 봅니다.
   - **증거 부족:** 샘플 수가 `min_samples` 미달이거나 모든 평가가 unknown입니다. 프로브 오류(`<프로브>.probe_error`, `vigilante_probe_restarts_total` 증가), 트래픽 부족(비율 규칙은 트래픽 0이면 unknown), SSH 세션 대기(8장)를 확인합니다.
3. 프로브 경로를 점검합니다.
   ```bash
   vigilante doctor -c /etc/vigilante/vigilante.yaml --service <서비스>
   ```

## 4.3 조치

1. 원인을 해소합니다.
   - 환경 요인: 공유 의존성을 복구합니다. 새 버전은 그대로 둡니다.
   - 관측점 불일치: 네트워크·SSH·에이전트를 복구합니다.
   - 증거 부족: 프로브 오류를 고치거나 트래픽이 있는 시간대에 다시 관측합니다.
2. 결정합니다.
   - 새 버전이 정상이라고 판단하면 파이프라인의 수동 승인 단계에서 진행하거나, 같은 단계를 다시 관측합니다.
     ```bash
     vigilante watch -c /etc/vigilante/vigilante.yaml --service <서비스> --phase <단계> --id <같은 배포 ID>
     ```
   - 새 버전에 문제가 있다고 판단하면 수동 롤백합니다(콘솔 **롤백** 또는 `vigilante rollback --id <배포 ID>`).
3. 판정 평가를 기록합니다. 보류가 타당했으면 `correct`, 판단이 어려우면 `unclear`입니다.

## 4.4 복구 확인

- 재관측한 단계가 PASS(종료 코드 0)이거나, 롤백한 경우 `ROLLED_BACK`(종료 코드 2)인지 확인합니다.
- 공유 의존성·관측 경로가 정상으로 돌아왔는지 확인합니다.

## 4.5 예방

- `environmental`이 잦으면 대조군(`control_targets: [auto]`) 구성을 점검합니다.
- `insufficient_evidence`가 잦으면 관측 창, 프로브 주기, `min_samples`를 조정합니다. 트래픽이 적은 서비스는 `on_inconclusive` 정책을 명시합니다.
- `observer_disagreement`가 잦으면 에이전트와 중앙 프로브의 경로 차이를 점검합니다.
- 파일럿 보고서의 `Hold causes`로 추세를 매주 확인합니다.

> **관측 서버 과부하:** M5-4 부하 시험(PR #12) 중 다른 작업으로 바쁜 개발용 PC에서 프로브 2만 개 규모를 돌리자 프로브가 시간 초과로 실패했고, 이를 대상 장애로 보아 **정상 배포 90건을 롤백**했습니다. PR #13부터는 관측 장치 과부하 판별(`safety.observer_guard`, 기본 켬)이 이런 경우 직접 재는 프로브의 실패 기반 위반을 롤백 대신 보류합니다(19장). 다만 다음은 여전히 지킵니다.
> - 판별은 서버가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, SSH로 읽는 `host`)의 실패 지표(`up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts`, `timeout`)와 모든 프로브의 `probe_error`를 쓰는 규칙에만 적용됩니다. 액세스 로그의 `latency_ms`처럼 대상이 보고한 같은 이름의 지표는 판정합니다(PR #14). 판별 기준(지연·루프백 1초, 3개 이상 서비스)에 걸리지 않는 과부하에서 여러 서비스가 동시에 FAIL·롤백되고 사유가 프로브 시간 초과·`probe_error`라면, 대상보다 관측 서버의 CPU·메모리·네트워크를 먼저 확인하고 필요하면 서킷을 엽니다(3장).
> - 서버 크기는 PR #12 측정값을 기준으로 여유 있게 잡습니다: GitHub Actions ubuntu-latest에서 대상 2,000 × 프로브 10(동시 배포 100)에 최대 힙 1.1 GiB·평균 0.72코어, 프로브 3개면 352 MiB·0.45코어(대상은 HTTP 시뮬레이터라 SSH 부하는 미포함). Helm 기본 메모리(요청 512Mi·한도 2Gi)는 프로브 약 2만 개까지를 기준으로 하므로 그보다 크면 올립니다.
> - CI 러너에서 `watch`를 직접 돌릴 때도 러너가 과부하되지 않게 합니다. 판별은 `watch`를 실행하는 프로세스 자신을 기준으로 합니다.

---

# 5. 승인 대기 시간 초과

## 5.1 증상

- 승인 모드 서비스의 배포가 `AWAITING_APPROVAL`(종료 코드 3)로 오래 머뭅니다.
- `approval.timeout`(기본 30m)이 지나면:
  - `on_timeout: hold`(기본): critical 알림 `ESCALATION: <서비스> rollback still awaits approval` ("No decision since <시각>. The failing release is still running ..."). 타임라인에 `no decision before <시각>; still waiting (on_timeout: hold)`. 콘솔 만료 칸에 "(만료됨, 상위 호출함)".
  - `on_timeout: rollback`: 타임라인에 `no decision before <시각>: rolling back automatically (on_timeout: rollback)` 후 자동 롤백.
- 지표 `vigilante_rollback_approvals_total{decision="expired"}` 증가.

## 5.2 확인

1. 콘솔 배포 상세의 승인 패널에서 대상, 격리됨, 이유, 만료를 확인합니다.
2. 승인권자(operator)가 결정할 수 있는 상태인지 확인합니다. `auth.four_eyes: true`이면 배포 생성자·롤백 요청자는 결정할 수 없습니다.
3. 서버 리더가 정상인지 확인합니다. 시간 초과 처리는 리더가 15초마다 하므로, 리더가 없으면(`GET /readyz`의 `checks.leader`가 `none`) 상위 호출도 자동 롤백도 일어나지 않습니다. CI 단발 실행만 쓰는 구성에는 시간 초과 처리가 없습니다.

## 5.3 조치

1. 판단 근거(규칙 위반, 타임라인, 서비스 지표)를 확인하고 승인 또는 거절합니다. 결정은 기한이 지난 뒤에도 가능합니다.
   - 콘솔: **승인 (롤백 실행)** 또는 **거절 (새 버전 유지)**
   - API: `POST /v2/deployments/{id}/approvals` `{"decision": "approve", "comment": "..."}` 또는 `"reject"`(operator 토큰)
   - 서버를 쓸 수 없을 때만 로컬 CLI로 결정합니다. 인증 환경에서는 `--break-glass`가 필요하고(21장), break-glass가 아니면 `auth.four_eyes`도 적용됩니다. 승인 기한 처리는 서버 리더가 하므로 서버가 없으면 상위 호출도 일어나지 않습니다.
     ```bash
     vigilante rollback -c /etc/vigilante/vigilante.yaml --id <배포 ID> --approve --reason "<근거>" --break-glass "서버 장애 중 승인"
     ```
2. 승인권자가 없으면 온콜 체계에 따라 다른 operator를 호출합니다.
3. 결정하지 못한 채 서비스 영향이 커지면 operator가 수동 롤백을 선택합니다.

## 5.4 복구 확인

- 승인 후 `ROLLED_BACK`, 거절 후 `HELD`로 바뀌었는지 확인합니다. 거절한 경우 `drain_first`로 빼 두었던 대상이 다시 트래픽에 들어갔는지(배포 이유 끝의 `re-enabled [...]`) 확인합니다. `re-enabling ... failed: ... (still drained)`가 보이면 LB에서 직접 복귀시킵니다.
- 판정 평가를 기록합니다.

## 5.5 예방

- 승인 요청 알림(critical)을 온콜 채널·PagerDuty로 라우팅합니다.
- 승인권자 그룹(`operator` 역할 바인딩)에 충분한 인원을 둡니다.
- 서비스 특성에 맞게 `approval.timeout`과 `on_timeout`을 정합니다.

---

# 6. 상태 저장소 연결 불가

> **적용 범위:** 아래 "쓰기 대기열"(write-behind)은 M5-4(PR #12), 롤백 시작 시점의 lease 처리는 PR #13의 동작이며, 둘 다 master(`537870c`, 1.0.0 후보)에 포함되어 있습니다.

## 6.1 증상

- 로그: `state write failed; queued until the store is back`, `readiness: state store unreachable`.
- `GET /readyz`가 503이고 `checks.store`가 `unreachable`. LB·Kubernetes가 그 노드로 트래픽을 보내지 않습니다(파일 저장소는 17장 참고).
- 지표: `vigilante_store_errors_total{reason="error"}` 증가, `vigilante_store_pending_writes`가 0보다 큼. 대기열이 100,000건에 차면 그 뒤 기록은 버려지며 `{reason="dropped"}`가 증가합니다.
- 장애 중 롤백이 시작되면 warning 알림 `<서비스>: Rollback without the service lease`와 배포 이벤트(`safety`)가 나옵니다(20장).
- HA 구성에서 장애가 `server.ha.lease_ttl`(기본 15s)보다 길면 로그 `leader lease renewal failed`, `lost leadership`이 나오고 `vigilante_engine_active`가 0이 됩니다.

## 6.2 동작 이해

- 저장소가 쓰기를 거부하면 기록을 메모리 대기열에 순서대로 쌓고(write-behind), 저장소가 돌아오면 같은 순서로 기록합니다. 재시도 간격은 200ms에서 시작해 두 배씩 늘어 최대 5초입니다. 대기열이 비면 로그 `state store is back: queued writes stored`가 나옵니다.
- 이미 진행 중인 판정과 롤백은 저장소를 기다리지 않고 계속됩니다(internal/orchestrator/chaos_test.go `TestChaosStoreOutageDuringRollback`: 롤백 중 저장소 단절 → 롤백 완료, 기록 무손실, 해시 체인 유지).
- 새로 시작하는 롤백은 서비스별 롤백 잠금(lease)을 `safety.rollback_lease.wait`(기본 10초) 동안 다시 시도합니다. 그래도 닿지 않으면 기본(`on_unavailable: proceed`)은 프로세스 안 잠금만으로 롤백하고 이벤트·감사(`lease.unavailable`)·warning 알림을 남깁니다. `fail`이면 `rollback not started: state store unreachable: ...`로 `ROLLBACK_FAILED`가 됩니다(시험 `TestChaosStoreOutageLeaseFailMode`). 자세한 대응은 20장입니다.
- **대기열은 메모리에만 있습니다.** 장애 중에 프로세스가 죽으면 대기열의 기록(판정, 롤백 단계, 감사 기록)은 사라집니다. 복구 후 기록되는 배포 스냅샷이 상태를 다시 담지만, 그 사이의 개별 기록은 남지 않습니다.
- 대기열 상한은 100,000건이며 넘친 기록은 버려집니다(`{reason="dropped"}`).
- 서버 종료 시 대기열을 최대 10초 동안 비우려 시도하고, 남으면 `state writes still queued at shutdown are lost`를 남깁니다.
- HA에서 장애가 리스 TTL보다 길면 리더가 물러나 어느 노드도 판정·롤백을 하지 않습니다. 저장소가 돌아온 뒤 같은 노드가 다시 리더가 되면 대기열을 기록하고, 다른 노드가 리더가 되면 대기열은 펜싱되어 버려집니다(`vigilante_store_errors_total{reason="fenced"}`).

## 6.3 확인

1. 저장소 종류를 확인합니다: `GET /healthz`의 `store`(`file:<경로>` 또는 PostgreSQL).
2. PostgreSQL이면 네트워크·DB 상태·계정·인증서를 확인합니다. `store status`는 PostgreSQL 전용입니다.
   ```bash
   vigilante store status -c /etc/vigilante/vigilante.yaml
   ```
3. 파일 저장소이면 디스크·마운트 상태를 확인합니다(17장).
4. 대기열 규모를 확인합니다: `vigilante_store_pending_writes`.
5. 장애 중 시작된 롤백이 있는지 알림(`Rollback without the service lease`)과 배포 이벤트(`safety`)로 확인합니다(20장).

## 6.4 조치

1. **서버를 재시작하지 마십시오.** 메모리 대기열이 사라지고, 저장소에 닿지 않으면 시작 시 상태를 읽지 못합니다. 업그레이드·설정 변경도 저장소가 복구될 때까지 미룹니다.
2. 저장소를 복구합니다(DB 재기동, 네트워크 복구, 디스크 확보).
3. 장애가 길어지면 진행 중인 배포를 늘리지 않도록 CI에서 새 배포를 멈춥니다. 필요하면 서킷을 열어 새 배포를 막습니다(콘솔 또는 `circuit trip --server URL`. 서킷 상태 기록도 저장소 쓰기이므로 대기열에 들어갑니다).
4. 기록이 버려졌거나(`dropped`) 장애 중 프로세스가 죽었다면, 복구 후 각 진행 중 배포의 상태를 확인하고 실제 대상 상태(현재 버전, LB 풀)와 비교하여 차이를 수동으로 정리합니다.
5. lease 없이 진행한 롤백이 있었으면 20장 절차로 같은 서비스에 다른 프로세스가 조치하지 않았는지 확인합니다.

## 6.5 복구 확인

- 로그 `state store is back: queued writes stored`.
- `vigilante_store_pending_writes`가 0, `GET /readyz`가 200.
- `vigilante audit verify -c /etc/vigilante/vigilante.yaml`이 성공(체인 순서 유지 확인).
- HA이면 `vigilante_engine_active`가 리더에서 1.

## 6.6 예방

- PostgreSQL을 고가용 구성으로 운영하고 연결 수·지연을 감시합니다.
- `vigilante_store_errors_total` 증가, `vigilante_store_pending_writes > 0`, `{reason="dropped"}`를 경보로 둡니다.
- 파일 저장소는 전용 볼륨과 디스크 사용률 경보를 둡니다.

---

# 7. HA 리더 교체

## 7.1 증상

- 로그(이전 리더): `leader lease renewal failed`, `lost leadership`, `leadership lost: state writes are fenced, standing down`, 진행 중이던 롤백은 `rollback handed over to the new leader`.
- 로그(새 리더): `elected leader`, `resumed rollbacks left in flight by the previous leader`.
- 지표: `vigilante_leader`가 노드 간에 바뀜, `vigilante_rollbacks_total{result="handed_over"}`, `vigilante_store_errors_total{reason="fenced"}` 증가.
- 전환 중(대략 리스 TTL 이내) API 요청이 `503 not_leader` "no leader available yet; retry shortly"(`Retry-After: 2`)로 응답합니다.

## 7.2 확인

1. 모든 노드의 `GET /healthz`에서 `role`과 `leader`가 일치하는지 확인합니다. 리더는 하나여야 합니다.
2. `GET /readyz`가 모든 노드에서 200이고 `checks.leader`가 같은 노드를 가리키는지 확인합니다.
3. 교체 원인을 확인합니다: 이전 리더 프로세스 종료, 호스트 장애, DB 연결 단절(6장), 업그레이드.

## 7.3 조치

1. 정상적인 교체(업그레이드, 노드 재시작)라면 별도 조치가 필요 없습니다. 새 리더가 공유 상태를 다시 읽고 중단된 롤백을 완료된 단계부터 이어서 끝냅니다.
2. 진행 중이던 롤백 배포를 확인합니다. 리더가 물러나는 순간 대상에 이미 보낸 명령은 되돌릴 수 없으므로 새 리더가 그 단계를 한 번 더 실행할 수 있습니다(내장 롤백 단계는 멱등으로 설계됨. `exec`·`webhook`의 사용자 명령은 멱등하게 작성해야 함, docs/04 S12).
   ```bash
   curl -fsS "$API/v2/deployments?state=ROLLING_BACK" -H "Authorization: Bearer $VIGILANTE_TOKEN"
   ```
3. 교체가 반복되면(플래핑) DB 지연과 `server.ha.lease_ttl`(기본 15s)을 점검합니다. TTL이 너무 짧으면 DB 순간 지연에도 리더가 바뀝니다.
4. CI의 `watch --server`는 상태 조회 실패 시 재시도하므로 보통 그대로 진행됩니다. API 연동은 `Retry-After` 후 재시도하게 합니다.

## 7.4 복구 확인

- 리더가 하나이고 `vigilante_engine_active`가 그 노드에서 1입니다.
- `ROLLING_BACK` 상태로 남은 배포가 없습니다(모두 `ROLLED_BACK` 또는 `ROLLBACK_FAILED`).
- `vigilante audit verify` 성공.

## 7.5 예방

- 노드를 서로 다른 장애 영역에 둡니다.
- 업그레이드는 팔로워 먼저, 리더 마지막 순서로 합니다(관리자 매뉴얼 17장).
- `max(vigilante_engine_active) == 0`이 1분 이상 지속되면 경보를 울립니다.

---

# 8. 팔로워가 리더에 닿지 못함

## 8.1 증상

- 팔로워로 들어온 API 요청이 `502`, 오류 코드 `not_leader`, 내용 `leader <노드> unreachable: <오류>`(`Retry-After: 2`)로 실패합니다.
- 리더가 잘못된 주소를 알리면 `leader <노드> advertises an invalid URL "<주소>"`.
- CI `watch --server`가 특정 노드를 거칠 때만 `status poll failed`를 반복합니다.
- 주의: 팔로워의 `/readyz`는 리더의 이름만 알면 200을 돌려주므로, LB 헬스 체크로는 이 장애가 드러나지 않습니다. 지표 `vigilante_api_requests_total{route="forwarded",code="502"}`로 확인합니다.

## 8.2 확인

1. 리더의 `advertise_url`을 확인합니다. 환경변수 `VIGILANTE_HA_ADVERTISE_URL`이 있으면 설정 파일의 `server.ha.advertise_url`보다 우선합니다.
2. 팔로워 호스트에서 그 주소로 직접 접속해 봅니다.
   ```bash
   curl -v https://vigilante-1.internal:8088/healthz
   ```
3. TLS 오류(`x509: certificate signed by unknown authority`, `x509: certificate is valid for ..., not ...` 등)이면 팔로워의 `server.ha.tls`(`ca_file`, `server_name`)를 확인합니다. `server.ha.tls`가 없으면 팔로워의 시스템 신뢰 저장소에 사설 CA가 있는지, 인증서에 `advertise_url`의 호스트가 들어 있는지 확인합니다.
4. Kubernetes(Helm)에서는 차트가 `VIGILANTE_HA_ADVERTISE_URL=<scheme>://$(POD_IP):8088`로 알립니다. `config`에 `server.tls.cert_file`이 있으면(또는 `tls.enabled: true`) scheme이 https입니다. 파드 IP는 보통 인증서에 없으므로 `server.ha.tls`에 `ca_file`(예: `/etc/vigilante-tls/ca.crt`)과 인증서에 들어 있는 이름(`server_name: vigilante.<네임스페이스>.svc`)을 지정해야 합니다.

## 8.3 조치

1. 즉시 완화: LB에서 문제 팔로워를 빼거나, 클라이언트(CI)가 리더 주소로 직접 호출하게 합니다.
2. 원인을 고칩니다.
   - 주소 오류: 리더의 `advertise_url`(또는 환경변수)을 다른 노드가 닿는 주소로 고치고 리더를 재시작합니다(실행 중 설정을 다시 읽는 기능은 없음). 리더 재시작 시 다른 노드가 리더가 되므로 7장을 함께 확인합니다.
   - 방화벽: 노드 간 8088 포트를 엽니다.
   - 인증서 신뢰: `server.ha.tls`에 CA와 검증할 이름을 넣고 팔로워를 재시작합니다(`ca_file`은 시작할 때 읽음). `server.ha.tls`를 쓰지 않으면 사설 CA를 `update-ca-trust` 또는 `update-ca-certificates`로 넣고 팔로워를 재시작합니다.
3. 팔로워를 LB에 다시 넣습니다.

## 8.4 복구 확인

- 팔로워를 통해 `GET /v2/me`가 200으로 응답합니다.
- `vigilante_api_requests_total{route="forwarded",code="502"}` 증가가 멈춥니다.

## 8.5 예방

- 설치 직후 각 팔로워를 통해 API를 한 번씩 호출하는 점검을 절차에 넣습니다.
- 노드 간 통신에 쓰는 인증서의 CA를 `server.ha.tls.ca_file` 또는 모든 노드의 신뢰 저장소에 배포합니다.

---

# 9. SSH 세션 고갈

## 9.1 증상

- 지표 `vigilante_ssh_session_waits_total{priority="normal"}`이 꾸준히 증가합니다. 수집 명령이 세션을 기다리느라 수집 간격이 늘어납니다.
- 심하면 로그·호스트 프로브 샘플이 줄어 `insufficient evidence`로 보류(4장)되거나 판정이 늦어집니다.
- `{priority="urgent"}`가 증가하면 롤백·트래픽 명령까지 세션을 기다린 것입니다. 롤백 지연(`vigilante_rollback_duration_seconds` 증가)으로 이어질 수 있습니다.
- 대상의 sshd 한도를 넘으면 명령이 실패하고 `vigilante_ssh_dials_total{result="error"}`가 늘 수 있습니다.

## 9.2 확인

1. doctor의 용량 점검을 봅니다.
   ```bash
   vigilante doctor -c /etc/vigilante/vigilante.yaml --service <서비스>
   ```
   `용량` 항목의 "SSH 세션 수" 줄에 "상시 로그 스트림 N + 주기 명령 M / 수집 몫 K (세션 한도 X 중 롤백 예약 Y)"가 나옵니다. 로그 스트림이 수집 몫 이상이면 `[FAIL]`, 합계가 넘으면 `[WARN]`입니다.
2. 대상별 설정 `connection.max_sessions`(기본 8), `reserved_sessions`(기본 2)와 대상 sshd의 `MaxSessions`(OpenSSH 기본 10)를 확인합니다.
3. 같은 대상에 붙은 서비스·프로브 수(특히 `log`, `access_log`)를 셉니다.

## 9.3 조치

1. 대상 sshd의 `MaxSessions`를 올리고 sshd를 다시 읽힙니다.
2. `connection.max_sessions`를 sshd 한도보다 작게 올립니다. 롤백 예약분 `reserved_sessions`는 줄이지 마십시오.
3. 또는 로그가 많은 대상은 에이전트 모드로 수집하거나, `log.remote_grep`으로 대상에서 먼저 걸러 냅니다.
4. 설정 검증 후 서버를 재시작합니다.

## 9.4 복구 확인

- `vigilante doctor`의 "SSH 세션 수"가 `[OK]`.
- `vigilante_ssh_session_waits_total` 증가율이 0에 가까워집니다.

## 9.5 예방

- 서비스·프로브를 추가할 때마다 doctor 용량 점검을 CI에서 실행합니다.
- `increase(vigilante_ssh_session_waits_total{priority="urgent"}[10m]) > 0` 경보를 둡니다.

---

# 10. sudo 규칙 누락

## 10.1 증상

- 롤백 단계가 실패하고 오류에 `sudo: a password is required` 또는 `sudo: a terminal is required`가 나옵니다. 결과적으로 2장의 `ROLLBACK_FAILED`가 됩니다.
- `vigilante doctor`의 `sudo` 항목이 `[FAIL]`이고 "sudo가 허용하지 않습니다: <이유>"와 "`vigilante sudoers --target <호스트>`가 만든 규칙을 /etc/sudoers.d에 설치하십시오" 안내가 나옵니다.
- `sudo_scope`가 없는 대상(`sudo: true`만)은 doctor `sudo 범위` `[WARN]`과 `validate` 경고가 나옵니다.

## 10.2 확인

1. 어떤 명령이 거부되었는지 doctor로 확인합니다. doctor는 `sudo -n -l`로 확인만 하고 실행하지 않습니다.
   ```bash
   vigilante doctor -c /etc/vigilante/vigilante.yaml --service <서비스>
   ```
2. 필요한 규칙을 생성하여 현재 대상의 `/etc/sudoers.d/vigilante`와 비교합니다.
   ```bash
   vigilante sudoers -c /etc/vigilante/vigilante.yaml --target <호스트>
   ```
3. 최근 실행기 설정 변경(경로, 유닛 이름, 릴리스 디렉토리)이나 대상 OS 변경(명령 경로 변경)이 있었는지 확인합니다.

## 10.3 조치

1. 규칙을 생성합니다.
   ```bash
   vigilante sudoers -c /etc/vigilante/vigilante.yaml --target <호스트> > vigilante.sudoers
   ```
2. 대상 호스트에서 문법을 검사하고 설치합니다.
   ```bash
   visudo -cf vigilante.sudoers && install -m 0440 vigilante.sudoers /etc/sudoers.d/vigilante
   ```
3. 직접 쓴 명령(`exec` 실행기, `restart_cmd`, `reload_cmd`)은 생성기에 포함되지 않으므로 규칙을 직접 추가합니다.
4. 실패했던 배포는 2장 절차로 수동 롤백을 다시 실행합니다.

## 10.4 복구 확인

- `vigilante doctor`의 sudo 항목이 모두 `[OK]`.
- 재시도한 롤백이 `ROLLED_BACK`.

## 10.5 예방

- 실행기·대상 설정을 바꿀 때마다 `vigilante sudoers`로 규칙을 다시 만들고 배포합니다.
- 파이프라인 첫 단계의 doctor를 필수로 둡니다.
- `sudo_scope: changes`를 모든 sudo 대상에 적용합니다.

---

# 11. Vault 연결 불가

## 11.1 증상

- 서버 시작이나 명령 실행 시 `secret vault:...: ...` 오류, `vault login (approle): ...`, `ssh_ca sign (<mount>/<role>): ...`.
- `cache_ttl`(기본 5m)이 지난 비밀값을 쓰는 프로브·실행기만 실패합니다. 예: SSH 접속 실패(`vigilante_ssh_dials_total{result="error"}` 증가), LB·하이퍼바이저 API 인증 실패, 프로브 오류로 인한 보류.
- 롤백 중이면 실행기 단계가 실패하여 `ROLLBACK_FAILED`가 될 수 있습니다.
- Vault에 의존하는 설정 값(`console.session_key_ref`, `server.state.dsn_ref` 등)은 서버 시작 시 필요하므로, Vault 장애 중에는 서버가 시작하지 못할 수 있습니다.

## 11.2 확인

1. Vault 서버 상태와 네트워크(8200 포트), CA 인증서(`secrets.vault.ca_file`)를 확인합니다.
2. 인증 정보가 유효한지 확인합니다. AppRole `secret_id` 만료, Kubernetes 서비스 계정 토큰, `VAULT_TOKEN` 만료.
3. 어떤 참조가 실패하는지 doctor로 확인합니다. doctor는 모든 `*_ref`를 실제로 해석하고 `ssh_ca`는 서명까지 받아 봅니다.
   ```bash
   vigilante doctor -c /etc/vigilante/vigilante.yaml
   ```

## 11.3 조치

1. **서버를 재시작하지 마십시오.** 메모리 캐시의 비밀값을 잃고, 시작 시 필요한 비밀값을 가져오지 못하면 서버가 뜨지 않습니다.
2. Vault를 복구하거나 인증 정보를 갱신합니다(새 `secret_id` 발급 후 `/etc/vigilante/vigilante.env` 갱신은 재시작이 필요하므로 Vault 복구 뒤에 합니다).
3. `ssh_ca`를 쓰는 자격증명은 Vault 서명이 실패하면 `private_key_file`이 함께 있어도 그 키로 대신 접속하지 **않습니다**(internal/transport/ssh.go `clientConfig`는 서명 오류에서 바로 실패). 이미 발급받은 단기 인증서가 유효한 동안은 계속 접속됩니다. 롤백이 급하면 대상 호스트에서 수동으로 복구합니다.
4. 실패한 롤백은 Vault 복구 후 2장 절차로 다시 실행합니다.

## 11.4 복구 확인

- `vigilante doctor`의 자격증명 항목("자격증명 값")이 모두 `[OK]`.
- SSH 접속 오류 증가가 멈추고 프로브 샘플이 정상 수집됩니다.

## 11.5 예방

- `secrets.cache_ttl`을 관측 창보다 길게 두어 롤백 경로가 Vault 순간 장애에 의존하지 않게 합니다.
- Vault를 고가용 구성으로 운영합니다.
- AppRole `secret_id`의 만료를 관리하고 갱신 일정을 둡니다.

---

# 12. OIDC IdP 장애

## 12.1 증상

- 콘솔에서 **회사 계정으로 로그인**을 누르면 IdP 화면이 열리지 않거나 오류가 납니다.
- 서버를 재시작하면 시작에 실패합니다: `oidc discovery for <issuer>: ...` 또는 `console oidc discovery: ...`. 서버는 시작할 때 IdP의 discovery 문서를 가져오기 때문입니다.
- 서비스 계정 토큰(`vgl_…`), API 키(`vgk_…`), OAuth 토큰(`vat_…`)을 쓰는 CI·에이전트·연동은 영향을 받지 않습니다.

## 12.2 확인

1. 서버 호스트에서 issuer의 discovery 주소에 접근해 봅니다.
   ```bash
   curl -fsS https://sso.example.internal/realms/ops/.well-known/openid-configuration
   ```
2. 서버가 실행 중인지 확인합니다(`GET /healthz`). 실행 중이면 재시작하지 않습니다.
3. 이미 로그인한 콘솔 세션이 계속 동작하는지 확인합니다. 세션은 ID 토큰 만료 시각까지 유효하지만, IdP 장애 중 토큰 서명 키 검증이 계속 되는지는 확인 필요입니다.

## 12.3 조치 (토큰 로그인 대체 경로)

1. **서버를 재시작하지 마십시오.** `auth.oidc`가 설정된 상태에서는 IdP가 복구될 때까지 서버가 시작하지 못합니다.
2. 긴급 조치는 CLI·API로 수행합니다. 운영자는 미리 발급받은 서비스 계정 토큰 또는 API 키를 씁니다.
   ```bash
   export VIGILANTE_TOKEN=vgl_...
   vigilante circuit --server https://vigilante.example.internal:8088 status
   curl -X POST "$API/v2/deployments/<id>/approvals" -H "Authorization: Bearer $VIGILANTE_TOKEN" \
     -H "Content-Type: application/json" -d '{"decision":"approve","comment":"IdP 장애 중 토큰으로 승인"}'
   ```
3. 비상용 토큰(`server.auth_token_env`)이 설정되어 있으면 admin 작업에 쓸 수 있습니다. 사용 후 감사 기록(`token:legacy`)을 검토하고 토큰을 교체합니다.
4. 콘솔을 토큰 로그인으로 바꿔야 할 만큼 장애가 길어지면 다음을 검토합니다. 콘솔은 `auth.oidc`와 `console.redirect_url`이 모두 있을 때만 SSO를 쓰고, 그렇지 않으면 토큰 입력 화면을 보여 줍니다.
   - `auth.oidc`가 남아 있는 한 재시작이 실패하므로, `auth.oidc` 절을 잠시 제거(주석 처리)하고 재시작해야 합니다.
   - 이 경우 OIDC 사용자 인증이 모두 꺼지고 서비스 계정·API 키만 동작합니다. 콘솔 로그인 화면에 토큰 입력란이 나옵니다.
   - IdP 복구 후 원래 설정으로 되돌리고 재시작합니다.

## 12.4 복구 확인

- discovery 주소가 응답하고, 콘솔 SSO 로그인이 성공합니다.
- 감사 기록에 `console.sign_in`이 다시 남습니다.

## 12.5 예방

- 운영자마다 최소 권한의 서비스 계정 토큰 또는 API 키를 비상용으로 미리 발급해 금고에 보관합니다(만료일 설정).
- IdP 장애 중에는 서버 재시작·업그레이드를 금지하는 운영 규칙을 둡니다.

---

# 13. ServiceNow 장애

## 13.1 증상

- 변경 티켓 게이트가 적용되는 서비스의 새 배포가 거부되거나(`on_error: closed`) 미검증으로 진행됩니다(`on_error: open`).
  - `closed`: API `503 itsm_unavailable`(Retry-After), CLI 종료 코드 3, 오류에 `ITSM unavailable: could not verify <티켓>: ... (change_gate.on_error: closed)`.
  - `open`: 로그 `change ticket not verified; proceeding (change_gate.on_error: open)`, 배포 기록에 `change <번호> NOT verified (ITSM unreachable, on_error: open)`, 콘솔 **변경 티켓** 칸에 "(미검증)".
- 인시던트 생성·작업 노트 일시 실패: `vigilante_itsm_calls_total{result="retry"}`가 늘어납니다. 일시 오류(접속 오류, 시간 초과, 429, 5xx)는 1초, 2초, 4초 뒤 최대 세 번 다시 시도합니다.
- 재시도 후에도 실패: 로그 `servicenow call failed`(kind `incident` 또는 `work_note`, `attempts` 시도 횟수), 지표 `vigilante_itsm_calls_total{result="error"}`. 인시던트가 만들어지지 않았으므로 감사 기록 `itsm.incident`와 배포 타임라인의 `incident <번호> opened`가 없습니다.
- 롤백은 영향을 받지 않습니다. 롤백은 ServiceNow를 기다리지 않으며, 인시던트는 백그라운드로 만들어 다른 처리(작업 노트, 이벤트)를 늦추지 않습니다.

## 13.2 확인

1. ServiceNow 인스턴스 상태와 네트워크(443)를 확인합니다.
2. 통합 사용자 자격증명(비밀번호·OAuth 토큰) 만료 여부와 권한(`incident` 읽기·생성)을 확인합니다. 429·5xx 외의 4xx 응답(인증 실패, 필드 거부)은 재시도하지 않으므로 `attempts=1`로 바로 실패합니다.
3. 장애 기간 동안 생성되지 않은 인시던트가 있는지 확인합니다. 롤백 실패·서킷 열림 시점의 로그에 `servicenow call failed ... kind=incident`가 있거나, 그 배포에 감사 기록 `itsm.incident`가 없으면 인시던트가 없습니다.
   ```bash
   vigilante audit query -c /etc/vigilante/vigilante.yaml --action itsm.incident --since <날짜>
   journalctl -u vigilante-server --since "<시각>" | grep 'servicenow call failed'
   ```
4. 인시던트와 작업 노트는 서버 리더가 처리합니다. CI 단발 실행(`vigilante watch`, 서버 없음)만 쓰는 구성에서는 원래 인시던트가 만들어지지 않습니다.

## 13.3 조치

1. 긴급 배포가 필요한데 `closed`로 막혀 있으면, 조직의 변경 관리 절차에 따라 승인을 받은 뒤 관리자가 해당 서비스를 게이트 대상에서 잠시 제외하거나 `on_error: open`으로 바꾸고 재시작합니다. 이 변경은 감사·변경 기록에 남겨야 합니다.
2. 인시던트 생성은 최대 4회(즉시, 1초, 2초, 4초 뒤) 시도 후 포기하며 나중에 다시 만들지 않습니다. 장애 중 발생한 롤백 실패·서킷 열림은 운영자가 ServiceNow에 인시던트를 수동으로 등록합니다. 같은 사건의 인시던트가 이미 있는지 먼저 확인합니다(응답을 못 받은 생성이 실제로는 저장됐을 수 있음).
3. 작업 노트도 다시 보내지 않으므로, 필요하면 배포 타임라인(`vigilante status --id`)을 변경 티켓에 수동으로 옮깁니다.
4. 자격증명 문제였다면 갱신합니다. Vault 참조(`*_ref`)는 `secrets.cache_ttl`이 지나면 새 값을 읽고, 환경변수(`*_env`)로 넣은 값은 서버를 재시작해야 반영됩니다.
5. 복구 후 임시로 바꾼 게이트 설정을 원래대로 되돌립니다.

## 13.4 복구 확인

- 시험 배포 등록 시 티켓이 검증되고, 콘솔에 "(미검증)" 표시가 없습니다.
- `vigilante_itsm_calls_total{result="ok"}`가 다시 증가하고 `{result="error"}`가 멈춥니다.

## 13.5 예방

- 서비스 중요도에 따라 `on_error`를 의도적으로 정합니다(감사 요구가 강하면 `closed`).
- `vigilante_itsm_calls_total{result="error"}` 증가를 경보로 둡니다. `{result="retry"}`가 꾸준히 늘면 ServiceNow 응답 지연·호출 한도를 점검합니다.
- 인시던트를 꼭 받아야 하는 사건(롤백 실패, 서킷 열림)은 `notify`의 PagerDuty·메일 채널로도 받습니다.
- 통합 사용자 자격증명 만료를 관리합니다.

---

# 14. 웹훅 전달 실패와 구독 비활성화

## 14.1 증상

- 사내 수신 시스템(인시던트 봇 등)에 이벤트가 오지 않거나 늦게 옵니다.
- 구독이 꺼지면 이벤트 `vigilante.webhook.disabled`(콘솔 "웹훅 중지"), 운영 알림 `Webhook subscription disabled: <URL>`, 로그 `webhook disabled`, 감사 기록 `webhook.disabled`가 남습니다.
- 구독 조회 결과에 `active: false`, `disabled_reason: "5 events in a row could not be delivered; last error: ..."`가 있습니다.

## 14.2 동작 이해

- 구독마다 이벤트를 순서대로 한 건씩 보내며, 10초 안의 2xx 응답이면 성공입니다. 리디렉션은 따라가지 않습니다.
- 실패하면 1초, 5초, 30초, 2분, 10분, 30분 뒤 재시도하고, 그래도 실패하면 dead-letter로 남기고 다음 이벤트로 넘어갑니다.
- 연속 5건이 dead-letter가 되면 구독을 끕니다.

## 14.3 확인

1. 구독 상태와 dead-letter 목록을 봅니다.
   ```bash
   curl -fsS "$API/v2/webhooks/<id>" -H "Authorization: Bearer $TOKEN"              # active, consecutive_failures, dead_letters, disabled_reason
   curl -fsS "$API/v2/webhooks/<id>/deliveries" -H "Authorization: Bearer $TOKEN"   # 최근 시도 100건 (상태 코드, 오류, 결과)
   ```
2. 실패 원인을 가립니다: 수신 측 장애, 10초 초과 응답, 3xx 리디렉션, TLS 오류, `api.webhook_allowed_hosts`에 없는 호스트, 수신 측 서명 검증 실패(마스터 키 교체 후 비밀 미전달).

## 14.4 조치

1. 수신 측 문제를 해결합니다. 마스터 키를 바꾼 뒤라면 새 비밀을 받아 수신 측에 전달합니다.
   ```bash
   curl -X POST "$API/v2/webhooks/<id>/secret" -H "Authorization: Bearer $TOKEN"
   ```
2. 테스트 이벤트로 전달을 확인합니다.
   ```bash
   curl -X POST "$API/v2/webhooks/<id>/pings" -H "Authorization: Bearer $TOKEN"
   ```
3. 구독을 다시 켭니다. 다시 켜면 실패 수가 초기화되고 남은 이벤트부터 이어서 보냅니다.
   ```bash
   curl -X PATCH "$API/v2/webhooks/<id>" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"active": true}'
   ```
4. dead-letter로 남은 중요한 이벤트를 다시 보냅니다.
   ```bash
   curl -X POST "$API/v2/webhooks/<id>/redeliveries" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"sequence": <N>}'
   ```

## 14.5 복구 확인

- 구독의 `active: true`, `consecutive_failures: 0`.
- `deliveries`의 최근 결과가 `delivered`.

## 14.6 예방

- 수신 측은 빠르게 2xx를 돌려주고 처리는 비동기로 합니다. 같은 이벤트가 두 번 올 수 있으므로 `webhook-id`로 중복을 거릅니다.
- 웹훅 대신 꼭 받아야 하는 알림(롤백 실패, 서킷 열림)은 `notify`의 PagerDuty·메일 채널로도 받습니다.

---

# 15. 감사 체인 검증 실패

## 15.1 증상

- `vigilante audit verify -c FILE`이 종료 코드 1로 끝나고 `audit chain of <저장소> is broken: <위치와 원인>`을 출력합니다. JSON 보고서의 `ok: false`, `broken`에 위치가 있습니다.
- 체인 키(`audit.chain_key_ref`)를 쓰는 경우 원인이 다음처럼 나올 수 있습니다.

| 원인 문구 | 뜻 |
|---|---|
| `MAC does not match (wrong chain key, or the entry was rewritten without it)` | 첫 MAC부터 맞지 않음. 다른 키로 검증했거나(키 교체, `--key` 오지정), 키 없이 기록을 고침 |
| `MAC does not match (the entry was rewritten without the chain key)` | 키가 보호하기 시작한 뒤의 기록이 키 없이 고쳐짐 |
| `entry has no MAC although the chain key protects the chain from entry N on (written or rewritten without the key)` | 키 보호 구간에 MAC 없는 기록이 있음(키 없이 쓰거나 고침, 키 설정이 빠진 노드가 기록) |

- 오류는 아니지만 확인이 필요한 출력(표준 오류)도 있습니다. `WARNING: no entry carries a MAC: ...`는 키를 설정했는데 MAC이 하나도 없다는 뜻(아직 키로 쓴 기록이 없거나 MAC이 모두 지워짐)이고, `chain key protects the chain from entry <N> on`의 N이 처음 기록해 둔 `keyed_from`과 다르면 앞부분이 바뀌었을 수 있습니다.
- 키를 해석하지 못하면 `audit.chain_key_ref: ...` 오류(32바이트 미만이면 `the key must be at least 32 bytes`)로 검증을 시작하지 못합니다. 키가 설정된 환경에서는 서버와 상태 저장소를 여는 모든 CLI 명령이 같은 오류로 시작하지 않습니다.
- 파일 저장소에서 읽을 수 없는 줄이 있으면 `line N: unreadable entry: ...` 오류가 납니다.
- 지원 번들의 `state/audit-verify.json`에 같은 결과가 있습니다.

## 15.2 확인

1. 검증 결과를 보존합니다.
   ```bash
   vigilante audit verify -c /etc/vigilante/vigilante.yaml > verify-$(date +%Y%m%d%H%M).json
   ```
2. `broken`이 가리키는 위치의 기록을 확인합니다. 원인은 수정 또는 삭제로 보고됩니다.
3. 우발적 원인을 먼저 배제합니다.
   - 파일 저장소에서 디스크가 가득 차 줄이 잘렸는지(16장).
   - 백업 복원이나 수동 파일 편집이 있었는지.
   - `audit prune` 후의 앵커 처리 중 오류가 있었는지.
   - MAC 오류이면: 검증에 쓴 키가 맞는지(`audit.chain_key_ref`가 가리키는 Vault 경로·버전, `--key` 값), 최근 키를 바꾸었는지, 키 설정이 빠진 노드·CLI가 기록했는지 확인합니다. 키를 바꾸었다면 이전 키로 쓴 기록(prune 앵커 포함)은 MAC 불일치로 보고되는 것이 정상입니다(키 교체 미지원). 이전 키로 따로 검증합니다.
     ```bash
     vigilante audit verify -c /etc/vigilante/vigilante.yaml --key vault:secret/prod/vigilante#audit_chain_key_old
     ```
4. 우발적 원인이 없으면 변조 가능성으로 보고 보안 사고 절차를 시작합니다. 체인 키가 없으면 저장소 쓰기 권한자가 변조 지점 이후 체인을 모두 다시 계산했을 때 `audit verify`로는 검출되지 않으므로, 외부에 보관한 이전 검증 결과의 `head`와 SIEM 사본으로 대조하십시오. 체인 키가 있으면 키 없이 고친 기록은 MAC 오류로 검출되며, 키 보호 구간의 시작(`keyed_from`)은 처음 기록해 둔 값과 대조합니다.

## 15.3 조치

1. 현재 저장소를 그대로 보존합니다(파일 복사, PostgreSQL 덤프). 증거를 바꾸지 마십시오.
2. 보안 담당자에게 알리고, SIEM에 전송된 사본(`audit.syslog`)과 비교하여 바뀐 내용을 확인합니다.
3. 이전 아카이브(`audit prune`, `audit export`, 백업)가 있으면 따로 검증합니다. 체인 키를 쓰면 `-c` 또는 `--key`를 주어 MAC도 검증합니다.
   ```bash
   vigilante audit verify --file <아카이브.jsonl>
   vigilante audit verify --file <아카이브.jsonl> --key <그 기록을 쓴 때의 키 참조>
   ```
4. 저장소를 어떻게 복구할지(백업 복원, 손상 줄 처리)는 개발팀과 보안 담당자가 함께 결정합니다. 손상된 줄을 직접 지우거나 고치는 절차는 제품에서 정의되어 있지 않습니다(확인 필요).

## 15.4 복구 확인

- 결정된 복구 후 `vigilante audit verify`가 `intact`로 끝납니다.
- 사고 보고서에 끊어진 위치, 원인, 조치를 기록합니다.

## 15.5 예방

- `audit verify`를 매일 실행하고 종료 코드와 `keyed_from` 위치를 감시합니다.
- `audit.chain_key_ref`를 설정하고, 키는 저장소 쓰기 권한자가 읽을 수 없는 곳(Vault)에 둡니다. 처음 설정한 `keyed_from`을 SIEM·변경 티켓에 남깁니다.
- `audit.syslog`로 SIEM에 실시간 사본을 남깁니다(`tls://` 권장, 22장).
- 상태 저장소(파일·DB)에 대한 직접 접근 권한을 최소화합니다.

---

# 16. 다운그레이드 가드로 서버 시작 거부

## 16.1 증상

- 이전 버전으로 되돌린 서버가 시작하지 않고 다음과 같은 오류를 남깁니다.
  ```
  state store schema is newer than this binary: migration 7 (applied by vigilante 2.0.0) is not known
  to this binary (1.4.2, knows up to 6) and older releases cannot use it; run vigilante 2.0.0 or newer,
  or revert with `vigilante store migrate --down-to 6` from that release
  ```
- `vigilante store status`가 `newer than this binary: N migration(s)`를 출력하고 종료 코드 4로 끝납니다(서버 시작 거부는 그중 호환되지 않는(breaking) 마이그레이션이 있을 때만입니다).
- `server.state.auto_migrate: false`인데 적용 대기 마이그레이션이 있으면 시작을 거부하며, `store status`에 `pending: [...]`가 나옵니다.

## 16.2 확인

1. 현재 바이너리 버전과 스키마 상태를 봅니다.
   ```bash
   vigilante version
   vigilante store status -c /etc/vigilante/vigilante.yaml
   ```
2. 다른 노드가 아직 새 버전으로 실행 중인지 확인합니다(HA).

## 16.3 조치

정말 이전 버전으로 돌아가야 하는지 먼저 판단합니다. 새 버전을 다시 설치하는 것이 가장 빠른 복구입니다. 되돌려야 한다면 다음 순서를 따릅니다.

1. 모든 서버를 멈춥니다.
2. PostgreSQL을 백업합니다.
3. **새 버전 바이너리로** 스키마를 되돌립니다. 되돌릴 수 없는 마이그레이션이 있으면 아무것도 바꾸지 않고 실패합니다.
   ```bash
   vigilante store migrate -c /etc/vigilante/vigilante.yaml --down-to 6 --yes
   ```
   `--yes` 없이 실행하면 "stop every vigilante server, take a backup, then repeat with --yes" 안내와 함께 거부합니다.
4. 이전 버전을 설치하고 서버를 시작합니다.

적용 대기 마이그레이션 때문에 시작하지 않는 경우(`auto_migrate: false`)에는 DBA 승인 후 `vigilante store migrate -c FILE`을 실행합니다.

## 16.4 복구 확인

- `vigilante store status`가 종료 코드 0.
- `GET /readyz`가 200, `vigilante audit verify` 성공.

## 16.5 예방

- 업그레이드 전에 항상 DB를 백업하고 `store status`를 기록합니다.
- 릴리스 노트의 Upgrade notes에서 호환되지 않는 스키마 변경 여부를 확인합니다.

---

# 17. 파일 저장소 디스크 가득 참

## 17.1 증상

- 로그 `state write failed; queued until the store is back`, 오류에 `no space left on device`. 지표 `vigilante_store_errors_total{reason="error"}`와 `vigilante_store_pending_writes` 증가. 기록은 메모리 대기열에 쌓입니다(6장).
- 파일 저장소의 `/readyz` 검사는 저널 파일이 있는지만 보므로 **200을 유지할 수 있습니다.** 지표로 발견해야 합니다.
- 기록 중 공간이 떨어지면 저널 마지막 줄이 잘릴 수 있습니다. 잘린 줄은 상태 재생 때 건너뛰지만(지원 번들 `unreadable_entries`), `audit verify`는 `line N: unreadable entry`로 실패할 수 있습니다.
- 기록마다 만드는 락 파일(`<journal_path>.lock`)을 만들 수 없으면 그 오류가 그대로 나옵니다. `journal ... is locked by another process`는 다른 프로세스가 락 파일을 10초 넘게 쥐고 있을 때의 오류입니다.

## 17.2 확인

```bash
df -h /var/lib/vigilante
du -sh /var/lib/vigilante/*
ls -l /var/lib/vigilante/
```

저널(`journal_path`)의 크기, 같은 디스크를 쓰는 다른 파일(로그, 아카이브, 지원 번들)을 확인합니다.

## 17.3 조치

1. **서버를 재시작하지 마십시오.** 메모리 대기열(6장)이 사라집니다.
2. 같은 디스크의 다른 파일(오래된 로그, 지원 번들, 다른 디스크로 옮길 수 있는 아카이브)을 정리하거나 볼륨을 확장하여 공간을 확보합니다. 저널 파일 자체는 지우거나 편집하지 마십시오.
3. 공간이 생기면 대기열을 자동으로 기록합니다(로그 `state store is back: queued writes stored`). 대기열이 넘쳐 기록이 버려졌으면(`{reason="dropped"}`) 6.4의 4번처럼 진행 중 배포 상태를 실제와 대조합니다.
4. 저널이 커서 공간이 부족했다면, 다음 점검 시간에 서버를 멈추고 보존 기간이 지난 기록을 다른 디스크로 아카이브합니다.
   ```bash
   systemctl stop vigilante-server
   vigilante audit prune -c /etc/vigilante/vigilante.yaml --out /archive/vigilante-$(date +%Y%m%d).jsonl --older-than 8760h
   systemctl start vigilante-server
   ```
5. `audit verify`가 잘린 줄 때문에 실패하면 15장 절차로 처리합니다. 잘린 줄 처리 방법은 확인 필요입니다.

## 17.4 복구 확인

- 디스크 여유 공간 확보, `vigilante_store_errors_total` 증가가 멈추고 `vigilante_store_pending_writes`가 0.
- `vigilante audit verify -c /etc/vigilante/vigilante.yaml` 성공.

## 17.5 예방

- `/var/lib/vigilante`를 전용 볼륨으로 두고 사용률 경보를 둡니다.
- `audit.retention`(기본값 없음. 설정하지 않으면 `audit prune`에 `--before` 또는 `--older-than`이 필요)과 정기 `audit prune`(서버 정지 시간에)을 운영 일정에 넣습니다.
- `vigilante_store_errors_total`과 `vigilante_store_pending_writes`를 경보로 둡니다.

---

# 18. 인증서 만료

## 18.1 증상

인증서 종류별로 증상이 다릅니다. Go의 TLS 오류 문구는 대개 `x509: certificate has expired or is not yet valid`입니다.

| 만료된 인증서 | 증상 |
|---|---|
| 서버 인증서(`server.tls.cert_file`) | CI `watch --server`, 에이전트, 브라우저 접속이 TLS 오류로 실패. 에이전트 샘플 중단(`vigilante_agents_connected` 감소, 이벤트 `vigilante.agent.lost`) |
| HA 노드 간 인증서 | 팔로워 요청이 `502 not_leader` "leader ... unreachable: ... x509 ..."(8장) |
| SIEM 수집기 인증서·클라이언트 인증서(`audit.syslog.tls`) | 감사 기록 SIEM 전송 중단, 로그 `SIEM unreachable; audit entries queue up`(22장) |
| 에이전트 클라이언트 인증서(`agent.tls.cert_file`) | `client_auth: optional`·`require`에서 에이전트 연결 거부 |
| Vault CA·서버 인증서 | 비밀값 해석 실패(11장) |
| PostgreSQL 서버 인증서(`sslmode=verify-full`) | 상태 저장소 연결 불가(6장) |
| 대상 API(F5, vCenter, Prism, OpenStack, ServiceNow, IdP) | 해당 실행기·트래픽 제어기·연동 실패 |
| SSH CA 인증서(`ssh_ca`) | 단기 인증서는 수명의 80%에서 자동 재발급되므로 보통 문제 없음. Vault 서명 실패 시 11장 |

## 18.2 확인

```bash
openssl s_client -connect vigilante.example.internal:8088 -servername vigilante.example.internal </dev/null 2>/dev/null | openssl x509 -noout -enddate
openssl x509 -noout -enddate -in /etc/vigilante/tls/server.crt
openssl x509 -noout -enddate -in /etc/vigilante/tls/agent.crt
```

Vigilante는 인증서 만료일 지표를 제공하지 않습니다. 외부 감시 도구로 확인하십시오.

## 18.3 조치

1. **서버 인증서:** 같은 경로의 `cert_file`·`key_file`을 새 파일로 바꿉니다. 서버는 파일 수정 시각이 바뀌면 재시작 없이 새 인증서를 씁니다(최대 1초 간격으로 확인). 새 파일을 다른 이름으로 쓴 뒤 `mv`로 바꾸면 반쯤 쓴 파일을 읽는 일을 피할 수 있습니다.
2. **에이전트 인증서:** 에이전트의 `agent.tls.cert_file`·`key_file`을 바꿉니다. 파일이 바뀌면 다시 읽습니다.
3. **CA 교체:** `server.tls.client_ca_file`, `agent.tls.ca_file`, `server.ha.tls.ca_file`, `audit.syslog.tls.ca_file`은 시작할 때만 읽으므로 교체 후 서버·에이전트를 재시작합니다. `server.ha.tls`를 쓰지 않는 HA 노드 간 사설 CA는 시스템 신뢰 저장소를 갱신하고 재시작합니다.
4. **외부 시스템 인증서:** 해당 시스템 담당자에게 갱신을 요청합니다. Vault(`secrets.vault.ca_file`), OpenStack(`cacert`)처럼 Vigilante에 CA 파일을 지정한 경우 파일을 갱신하고 재시작합니다.
5. 인증서 문제로 실패한 롤백·배포는 복구 후 2장 또는 파이프라인 재실행으로 정리합니다.

## 18.4 복구 확인

- `openssl s_client`로 새 만료일이 보입니다.
- CI `watch --server`와 에이전트 연결이 정상(`vigilante_agents_connected`가 원래 수준).
- 팔로워를 통한 API 호출이 성공합니다.

## 18.5 예방

- cert-manager 등 자동 갱신을 쓰고, 갱신된 파일이 같은 경로에 놓이게 합니다(서버는 재시작 없이 반영).
- 모든 인증서의 만료일을 외부 감시 도구로 30일 전에 경보합니다.
- `tls_skip_verify`는 시험용으로만 쓰고 운영에서는 CA를 지정합니다.

---

# 19. 관측 장치 과부하로 보류 (observer degraded)

관측 장치 과부하 판별(`safety.observer_guard`, 기본 켬)은 Vigilante 서버(또는 `watch`를 실행하는 CI 러너) 자신의 측정을 믿을 수 없을 때 서버가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, SSH로 읽는 `host`)의 실패에 기반한 규칙 위반과 `probe_error` 위반을 롤백 대신 보류합니다(docs/04 S14a). `log`·`access_log`·`docker` 프로브의 지표는 이름이 같아도(예: 액세스 로그의 `latency_ms`) 대상이 보고한 값이라 그대로 판정합니다(PR #14). 대상은 정상인데 관측하는 쪽이 느려 프로브가 시간 초과로 실패하는 상황에서 정상 배포를 롤백하지 않기 위한 장치입니다.

## 19.1 증상

- 배포가 `HELD`(CI 종료 코드 4)로 끝나고 이유에 `— observer degraded (<신호>), not attributed to the release`가 붙습니다. 신호 예: `scheduling lag: woke 4s late`, `loopback: no echo within 1s`, `spread: probes time out on N of M targets across K services`.
- warning 알림 `<서비스> <단계> HELD — human decision needed`. 여러 서비스의 배포가 같은 시각에 보류되는 경우가 많습니다.
- 서버 로그 `observer degraded: probe failures will hold instead of rolling back`(`reason` 포함), 회복 시 `observer recovered`(`degraded_for`).
- 지표 `vigilante_observer_degraded` 1, `vigilante_observer_degradations_total{signal}`·`vigilante_observer_holds_total` 증가.
- 파일럿 보고서의 보류 원인(`Hold causes`)에는 별도 분류 없이 `hold_rule`로 집계됩니다.

## 19.2 확인

1. 보류 이유와 지표로 관측 장치 과부하인지 확인합니다. 보류된 위반이 직접 재는 프로브의 지표(`<프로브>.up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts`, `timeout`) 또는 `<프로브>.probe_error`이고 이유에 `observer degraded`가 있으면 이 장입니다. 같은 시각에 액세스 로그·로그·컨테이너 지표 위반으로 FAIL·롤백된 배포는 이 판별의 대상이 아니므로 일반 롤백 절차로 봅니다.
   ```bash
   vigilante status -c /etc/vigilante/vigilante.yaml --id <배포 ID>
   curl -fsS -H "Authorization: Bearer $VIGILANTE_TOKEN" https://vigilante.example.internal:8088/metrics | grep vigilante_observer_
   ```
2. 서버의 자원을 확인합니다. 신호별로 의심할 곳이 다릅니다.

| 신호(`signal`) | 뜻 | 확인 |
|---|---|---|
| `scheduling lag` | 내부 250ms 타이머가 `max_lag`(1s)보다 늦게 깸 | CPU 사용률·스로틀링(컨테이너 CPU 한도), 메모리 부족에 따른 GC, VM 일시 정지 |
| `loopback` | 프로세스 안 TCP 에코 왕복이 `loopback_timeout`(1s) 초과 | 소켓·파일 디스크립터 고갈, 네트워크 스택 과부하 |
| `spread` | 3개 이상 서비스에 걸쳐 관측 대상 50% 이상의 프로브가 시간 초과 | 서버 쪽 네트워크 장애(방화벽, 경로, DNS), 동시에 관측하는 프로브 수 |

   ```bash
   top -b -n 1 | head -20                       # CPU·메모리
   ss -s                                        # 소켓 수
   kubectl -n vigilante top pod                 # Kubernetes
   ```
3. 대상이 실제로 정상인지 다른 경로(서비스 대시보드, 대상 호스트에서 직접 헬스 체크, 에이전트 지표)로 확인합니다. 로그·액세스 로그·호스트·컨테이너 지표의 위반은 과부하 중에도 그대로 판정되므로, 그쪽 위반이 함께 있으면 실제 불량일 가능성이 높습니다.

## 19.3 조치

1. 서버 자원을 확보합니다: 같은 호스트의 다른 부하를 옮기거나, CPU·메모리 한도를 올리거나(Helm `resources`), 동시에 관측하는 배포를 줄입니다. 네트워크 장애면 복구합니다.
2. 보류된 배포를 결정합니다(4.3절과 같음).
   - 대상이 정상이면 회복 후(`vigilante_observer_degraded`가 0이 되고 `grace`(1m)와 규칙의 윈도우가 지난 뒤) 같은 단계를 다시 관측하거나 파이프라인 수동 승인 단계에서 진행합니다.
     ```bash
     vigilante watch -c /etc/vigilante/vigilante.yaml --service <서비스> --phase <단계> --id <같은 배포 ID>
     ```
   - 새 버전에 문제가 있다고 판단하면 수동 롤백합니다(콘솔 **롤백** 또는 `POST /v2/deployments/{id}/rollbacks`).
3. 판정 평가를 기록합니다. 과부하로 보류된 정상 배포는 `correct`(보류가 타당) 또는 `unclear`로, 메모에 "observer degraded"를 남깁니다.

## 19.4 복구 확인

- `vigilante_observer_degraded`가 0이고 로그에 `observer recovered`가 남습니다.
- 재관측한 단계가 PASS(종료 코드 0)이거나, 롤백한 경우 `ROLLED_BACK`(종료 코드 2)입니다.

## 19.5 예방

- 서버 크기를 부하 시험 결과(프로브 1천 개당 약 60 MiB, 프로브 2만 개에 힙 약 1.1 GiB)에 맞춰 여유 있게 잡고, 다른 부하와 함께 두지 않습니다.
- `vigilante_observer_degraded == 1`(1분 이상)과 `increase(vigilante_observer_holds_total[10m]) > 0`을 경보로 둡니다.
- 판별을 끄는 것(`observer_guard.disabled: true`)은 권장하지 않습니다. 기준값(`max_lag`, `loopback_timeout`, `timeout_share`, `min_services`, `grace`)은 오탐·보류 추세를 보고 관리자가 조정합니다.

---

# 20. 저장소 장애 중 롤백 (lease 없음, lease 충돌)

롤백은 같은 서비스에 두 프로세스가 동시에 조치하지 않도록 상태 저장소의 서비스 lease를 잡습니다. 롤백을 시작하는 순간 저장소에 닿지 않으면 `safety.rollback_lease.wait`(10초) 동안 다시 시도한 뒤, 기본(`on_unavailable: proceed`)은 프로세스 안 잠금만으로 롤백합니다(docs/04 S12a).

## 20.1 증상

- **lease 없이 진행:** warning 알림 `<서비스>: Rollback without the service lease`, 배포 이벤트(`safety`)와 감사 기록 `lease.unavailable`에 `state store unreachable (...): rolling back under this process's lock only; another process could act on <서비스> at the same time`.
- **lease 충돌:** 롤백 중 저장소가 돌아왔는데 다른 프로세스가 이미 lease를 잡고 있으면 warning 알림 `<서비스>: Concurrent rollback suspected`, 감사 기록 `lease.conflict`에 `state store is back and <보유자> holds the rollback lease for <서비스>: two processes may be acting on it; check its targets`.
- **`fail` 설정:** `on_unavailable: fail`이면 롤백하지 않고 `rollback not started: state store unreachable: cannot take the service rollback lease: ...`로 `ROLLBACK_FAILED`가 됩니다.
- 6장의 저장소 장애 증상이 함께 나타납니다.

## 20.2 확인

1. 6장 절차로 저장소 상태를 확인합니다.
2. lease 없이 진행한 롤백의 결과(`ROLLED_BACK` 또는 `ROLLBACK_FAILED`)를 확인합니다. 저장소 장애 중 기록은 대기열에 있다가 복구 후 기록됩니다.
3. `lease.conflict`가 나왔으면 보유자(호스트/PID)를 확인하고, 그 프로세스(다른 CI 잡, 다른 서버)가 같은 서비스에 무엇을 했는지 확인합니다.
   ```bash
   vigilante audit query -c /etc/vigilante/vigilante.yaml --action lease.conflict --since <날짜>
   vigilante audit query -c /etc/vigilante/vigilante.yaml --service <서비스> --since <날짜>
   ```
4. 대상의 실제 상태(현재 버전, LB 풀 멤버의 활성 여부)를 확인합니다. 두 프로세스가 서로 다른 버전으로 되돌렸거나, 한쪽이 드레인한 대상을 다른 쪽이 복귀시켰을 수 있습니다.
   ```bash
   vigilante doctor -c /etc/vigilante/vigilante.yaml --service <서비스>
   ```

## 20.3 조치

1. 대상 상태가 기대(이전 버전, 모두 풀에 활성)와 다르면, 한 프로세스의 롤백이 끝난 뒤 수동 롤백을 한 번 더 실행해 상태를 맞춥니다(2.3절. 롤백 단계는 멱등). 격리된 채 남은 대상은 LB 풀에 다시 넣습니다.
2. `fail` 설정으로 롤백이 시작되지 않았으면, 저장소를 복구한 뒤 수동 롤백을 실행합니다. 저장소 복구가 늦어지고 서비스 영향이 크면 대상 호스트에서 수동으로 복구합니다.
3. 동시 조치가 규제상 허용되지 않는 서비스는 관리자가 `safety.rollback_lease.on_unavailable: fail`로 바꾸는 것을 검토합니다.

## 20.4 복구 확인

- 배포가 `ROLLED_BACK`이고, 대상이 모두 이전 버전이며 LB 풀에서 활성입니다.
- `vigilante audit verify` 성공, `vigilante_store_pending_writes`가 0.

## 20.5 예방

- 6.6절의 저장소 가용성 대책을 따릅니다.
- 같은 서비스를 여러 경로(CI 단발 실행과 서버)에서 동시에 롤백하지 않도록 운영 경로를 하나로 정합니다.

---

# 21. 로컬 CLI 비상 실행 (break-glass)

인증이 설정된 환경(`auth.local_cli: auto` 기본)에서는 `--server` 없이 실행하는 권한이 큰 CLI 명령을 거부합니다. 로컬 CLI는 상태 저장소를 직접 다루어 역할 검사를 받지 않기 때문입니다.

| 로컬 명령 | 평소 경로 | 감사 동작(break-glass) |
|---|---|---|
| `rollback --approve`·`--reject`(승인 대기 결정) | 콘솔, `POST /v2/deployments/{id}/approvals`(operator) | `breakglass.rollback.approve`, `breakglass.rollback.reject` |
| `rollback --approve`(에스컬레이션 허용) | `POST /v2/deployments/{id}/rollbacks` 후 승인 API(operator) | `breakglass.escalation.approve` |
| `circuit reset`·`trip` | 콘솔, `vigilante circuit reset --server URL`(admin) | `breakglass.circuit.reset`, `breakglass.circuit.trip` |
| 동결 중 `watch`·`prepare --freeze-override` | `watch --server URL --freeze-override`, API `freeze_override`(admin) | `breakglass.freeze.override` |

## 21.1 증상

- 로컬 명령이 `<작업> refused: the API has authentication, so privileged local commands are restricted (auth.local_cli). Run it through the server with --server and an operator or admin token, or pass --break-glass REASON (audited and alerted)`로 거부됩니다(종료 코드 1, 동결 예외는 3).
- 로컬 승인·거절이 `four-eyes: cli:<사용자>@<호스트> created this deployment or requested its rollback, so another operator must decide (or --break-glass REASON)`로 거부됩니다.
- 누군가 break-glass를 쓰면 critical 알림 `Break-glass: cli:<사용자>@<호스트> ran <작업> locally`(본문은 입력한 이유)와 감사 기록 `breakglass.<작업>`이 남습니다.

## 21.2 확인

1. 서버를 쓸 수 있는지 먼저 확인합니다. 서버가 동작하면 break-glass는 쓰지 않습니다.
   ```bash
   curl -fsS https://vigilante.example.internal:8088/readyz
   VIGILANTE_TOKEN=<토큰> vigilante whoami --server https://vigilante.example.internal:8088   # 권한 확인
   ```
2. break-glass 알림을 받았으면 누가, 언제, 왜 썼는지 감사 기록으로 확인합니다.
   ```bash
   vigilante audit query -c /etc/vigilante/vigilante.yaml --action breakglass.circuit.reset --since <날짜>
   vigilante audit query -c /etc/vigilante/vigilante.yaml --actor cli:<사용자>@<호스트> --since <날짜>
   ```

## 21.3 조치

1. **평소:** 콘솔·API, 또는 `--server URL`과 operator·admin 토큰(`VIGILANTE_TOKEN`)으로 실행합니다(위 표).
2. **서버 장애 등으로 서버를 쓸 수 없을 때:** 승인된 운영자가 이유와 티켓을 붙여 로컬로 실행합니다. 플래그는 동작 뒤에 씁니다(`circuit reset ...`).
   ```bash
   vigilante circuit reset -c /etc/vigilante/vigilante.yaml --break-glass "서버 장애 INC0012345, 원인 조치 완료" --ticket INC0012345
   vigilante rollback -c /etc/vigilante/vigilante.yaml --id <배포 ID> --approve --reason "<근거>" --break-glass "서버 장애 INC0012345"
   ```
3. 서버가 복구되면 서버가 보는 상태(`circuit status --server URL`, 배포 상태)가 로컬에서 바꾼 상태와 같은지 확인합니다.
4. 예정되지 않은 break-glass 알림이면 보안 사고로 보고, 그 계정의 상태 저장소 접근 권한(DB 계정, 저널 파일 권한)을 점검합니다.

## 21.4 복구 확인

- 의도한 조치(서킷 CLOSED, 롤백 결정 반영)가 상태에 나타납니다.
- 감사 기록의 `breakglass.*` 건마다 사유와 티켓이 있고, 사후 검토가 기록됩니다.

## 21.5 예방

- 운영자마다 최소 권한의 토큰을 미리 발급해 두어 서버가 살아 있으면 break-glass가 필요 없게 합니다.
- 상태 저장소 계정(PostgreSQL 사용자, 저널 파일 권한)은 운영자에게만 줍니다. 이 권한이 있으면 로컬 CLI로 수동 롤백·`mark-good` 등 제한되지 않는 명령을 실행할 수 있습니다.
- break-glass 알림(critical)을 보안 담당자 채널로도 보냅니다.

---

# 22. SIEM TLS 전송 실패

감사 기록은 상태 저장소에 먼저 기록된 뒤 비동기로 SIEM에 보냅니다. SIEM 전송이 실패해도 판정·롤백과 감사 기록 자체에는 영향이 없습니다.

## 22.1 증상

- 서버 로그 `SIEM unreachable; audit entries queue up`(`addr`, `err`, `queued`)가 반복됩니다. TLS 문제면 `err`에 `x509: certificate signed by unknown authority`, `x509: certificate is valid for ..., not ...`, `tls: ... handshake failure`, `remote error: tls: certificate required` 같은 문구가 있습니다.
- `vigilante_audit_exported_total` 증가가 멈춥니다. 큐(10,000건)가 차면 `vigilante_audit_export_dropped_total`이 증가합니다.
- SIEM 쪽에서 Vigilante 기록이 들어오지 않습니다.
- 설정 오류는 서버 시작이나 `validate`에서 드러납니다: `audit.syslog.tls is only used with a tls:// address`, `audit.syslog.tls: cert_file and key_file go together`, `audit.syslog.address: tls needs a collector host name ...`, `audit.syslog.tls.min_version must be 1.2 or 1.3`.

## 22.2 확인

1. 서버 호스트에서 수집기 인증서와 이름을 확인합니다.
   ```bash
   openssl s_client -connect siem.example.internal:6514 -servername siem.example.internal \
     -CAfile /etc/vigilante/siem-ca.pem </dev/null 2>/dev/null | openssl x509 -noout -subject -ext subjectAltName -enddate
   ```
2. 원인을 가립니다.

| 오류 | 원인 | 확인할 설정 |
|---|---|---|
| `certificate signed by unknown authority` | 수집기 인증서의 CA를 신뢰하지 않음 | `audit.syslog.tls.ca_file`(없으면 시스템 루트 CA) |
| `certificate is valid for ..., not ...` | 인증서 이름과 검증 이름 불일치 | `audit.syslog.tls.server_name`(없으면 `address`의 호스트) |
| `certificate has expired` | 수집기 또는 클라이언트 인증서 만료 | 18장 |
| `certificate required`, `bad certificate` | 수집기가 클라이언트 인증서를 요구하거나 거부 | `cert_file`·`key_file` |
| `protocol version` | TLS 버전 불일치 | `min_version`(1.2 또는 1.3) |
| 접속 거부·시간 초과 | 수집기 장애, 방화벽(6514) | 네트워크 |

3. 수집기가 RFC 5425 형식(`길이 공백 메시지`, octet counting)을 받도록 설정되어 있는지 확인합니다. 줄 단위 TLS 수신만 받는 수집기는 메시지를 잘못 나눌 수 있습니다(확인 필요: 수집기 제품별 설정).

## 22.3 조치

1. 원인에 맞게 설정이나 인증서를 고칩니다. `audit.syslog.tls`의 CA 파일은 시작할 때 읽으므로 바꾼 뒤 서버를 재시작합니다(재시작 전에 6장·11장 상황이 아닌지 확인). 클라이언트 인증서 파일은 같은 경로에서 바뀌면 다시 읽습니다.
2. 수집기가 복구되면 큐에 쌓인 기록부터 다시 보냅니다(재접속 간격은 최대 1분).
3. 큐가 넘쳐 빠진 구간(`vigilante_audit_export_dropped_total` 증가)은 `audit export`로 내보내 SIEM에 따로 넣습니다.
   ```bash
   vigilante audit export -c /etc/vigilante/vigilante.yaml --out /tmp/vigilante-audit-$(date +%Y%m%d).jsonl
   ```

## 22.4 복구 확인

- 로그 `SIEM unreachable`가 멈추고 `vigilante_audit_exported_total`이 다시 증가합니다.
- SIEM에서 최근 기록이 조회됩니다.

## 22.5 예방

- `increase(vigilante_audit_export_dropped_total[15m]) > 0`을 경보로 둡니다.
- 수집기 인증서와 클라이언트 인증서의 만료일을 외부 감시 도구로 경보합니다.
- 평문 `tcp://`·`udp://`는 신뢰 망 안에서만 씁니다.

---

# 부록 A. 시나리오별 빠른 참조

| 장 | 시나리오 | 첫 확인 | 첫 조치 |
|---|---|---|---|
| 2 | 롤백 실패 | `vigilante status --id`, `doctor` | 원인 수정 후 수동 롤백 |
| 3 | 서킷 OPEN | `circuit status`, `audit query --action circuit.trip` | 원인 해결 후 `circuit reset --server URL`(admin) |
| 4 | 보류 | 배포 이유의 원인 문구 | 의존성·관측 경로 복구 후 재관측 또는 롤백 |
| 5 | 승인 시간 초과 | 승인 패널, 리더 상태 | 승인 또는 거절 |
| 6 | 저장소 연결 불가 | `/readyz`, `vigilante_store_errors_total`, `vigilante_store_pending_writes` | 재시작 금지, 저장소 복구 |
| 7 | HA 리더 교체 | 모든 노드 `/healthz` | 진행 중 롤백 확인 |
| 8 | 팔로워 전달 실패 | 리더 `advertise_url` 접속 | LB에서 제외, 주소·CA 수정 |
| 9 | SSH 세션 고갈 | doctor "SSH 세션 수" | `MaxSessions`·`max_sessions` 상향, 에이전트 |
| 10 | sudo 규칙 누락 | doctor sudo 항목 | `vigilante sudoers` 규칙 설치 |
| 11 | Vault 장애 | doctor 자격증명 항목 | 재시작 금지, Vault 복구 |
| 12 | IdP 장애 | discovery 주소 | 재시작 금지, 토큰으로 CLI·API 작업 |
| 13 | ServiceNow 장애·인시던트 미생성 | `vigilante_itsm_calls_total{result="error"}`, 로그 `servicenow call failed` | 인시던트 수동 등록 |
| 14 | 웹훅 실패 | `GET /v2/webhooks/{id}` | 수신 측 복구, `active: true`, 재전송 |
| 15 | 감사 체인 실패·MAC 불일치 | `audit verify` 결과 보존, 검증 키 확인 | 증거 보존, 보안 절차 |
| 16 | 다운그레이드 가드 | `store status` | 새 버전 재설치 또는 `migrate --down-to` |
| 17 | 디스크 가득 참 | `df -h` | 재시작 금지, 공간 확보 |
| 18 | 인증서 만료 | `openssl x509 -enddate` | 같은 경로에 새 파일 배치 |
| 19 | 관측 장치 과부하 보류 | 이유의 `observer degraded`, `vigilante_observer_degraded` | 서버 자원 확보 후 재관측 또는 롤백 |
| 20 | lease 없는 롤백·lease 충돌 | `lease.unavailable`·`lease.conflict` 알림, 대상 실제 상태 | 저장소 복구, 대상 상태 맞춤 |
| 21 | 로컬 CLI 거부·break-glass | 서버 상태, `audit query --action breakglass.*` | `--server`와 토큰, 서버 장애 시만 `--break-glass` |
| 22 | SIEM TLS 전송 실패 | 로그 `SIEM unreachable`의 `err` | CA·이름·클라이언트 인증서 수정, 빠진 구간 `audit export` |
