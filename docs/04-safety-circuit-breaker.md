# 04. 실패 복구 및 비상 정지 (Safety Circuit Breaker)

자동 롤백 시스템의 가장 위험한 순간은 **롤백 자체가 실패하거나, 잘못된 판단으로 롤백이 반복될 때**입니다. Vigilante의 원칙은 다음 네 가지입니다.

1. **Isolate, don't destroy** — 확신이 없으면 더 파괴적인 조치(재시작·스냅샷 복원) 대신 "트래픽에서 빼 두기"까지만 합니다.
2. **롤백이 장애를 키우지 않는다** — 풀 최소 가용 수(blast radius)를 깨는 드레인은 하지 않습니다.
3. **자동화가 실패하면 자동화를 멈춘다** — 롤백 실패가 누적되면 서킷을 열어 모든 자동 변경을 동결하고 배포 게이트도 닫습니다.
4. **모든 결정은 먼저 기록된다** — 저널(fsync)에 먼저 쓰고 실행하며, 재시작 시 이어서 수행합니다.

## 1. 서킷 브레이커 상태도

```
                 롤백 실패 N회 / window          open_duration 경과
     ┌────────┐  또는 수동 trip (킬 스위치)  ┌──────┐  (0s면 수동 리셋만)  ┌───────────┐
     │ CLOSED │ ─────────────────────────▶ │ OPEN │ ─────────────────▶ │ HALF_OPEN │
     └────────┘                            └──────┘                    └───────────┘
        ▲  ▲                                 ▲  │                         │      │
        │  │        시험 롤백 1건 실패         │  │ 수동 reset               │      │ 시험 롤백
        │  └─────────────────────────────────┼──┘ (토큰 필요)              │      │ 1건 성공
        │                                    └────────────────────────────┘      │
        └────────────────────────────────────────────────────────────────────────┘
```

| 상태 | 자동 롤백 | 트래픽 격리(드레인) | 새 배포 게이트 (`watch`) | 수동 롤백 (`vigilante rollback`) |
|---|---|---|---|---|
| CLOSED | 허용 | 허용 | 열림 | 허용 |
| OPEN | **금지** | 허용(blast radius 내) | **닫힘** (exit 3) | 허용 (운영자 판단) |
| HALF_OPEN | 1건만 시험 허용 | 허용 | 열림 | 허용 |

- 상태는 저널에 영속화되어 **프로세스 재시작·CI 잡 간에 유지**됩니다 (테스트: `TestRollbackFailureIsolatesAndOpensCircuit`).
- 킬 스위치: `vigilante circuit trip --reason "변경 동결"` 또는 `POST /v1/circuit/trip` — 변경 동결 기간이나 대형 장애 대응 중 자동화를 즉시 멈춥니다.
- 리셋: `vigilante circuit reset --server URL` / `POST /v1/circuit/reset` (admin 토큰 필요). 원인 조사 후에만. 인증을 켠 환경에서 서버 없이 로컬로 리셋·차단하려면 `--break-glass "이유"`가 필요하고 감사·알림이 남습니다(`auth.local_cli`).

## 2. 롤백 실패 시나리오별 대응

| # | 시나리오 | 탐지 | 자동 대응 | 최종 상태 / 사람이 할 일 |
|---|---|---|---|---|
| S1 | 롤백 명령 일시 실패 (SSH 끊김, API 5xx) | 단계 오류 | 단계별 `retry`(지수 백오프, 상한 `max_backoff`), 단계마다 `timeout` | 재시도 성공 시 정상 진행 |
| S2 | 1차 전략 영구 실패 (이전 릴리스 디렉토리 삭제됨, 이전 이미지 pull 불가) | 재시도 소진 | **에스컬레이션 래더**: 다음 실행기(예: Ansible → VM 스냅샷) 순차 시도. 각 단계 후 `Verify` + `probe.verify` | 성공 시 ROLLED_BACK |
| S3 | 에스컬레이션이 파괴적 (VM 스냅샷 복원 = 데이터 손실 가능) | `require_approval: true` | 실행하지 않고 **AWAITING_APPROVAL**, 대상은 드레인 상태 유지, critical 알림. 서킷 실패로 세지 않음 | `POST /v1/deployments/{id}/approve` 또는 `vigilante rollback --id X --approve` |
| S4 | 모든 전략 실패 | 래더 소진 | `traffic.enable` 미실행 → **실패 대상은 풀에서 빠진 채 격리**, `Breaker.Failure()`, critical 알림 | ROLLBACK_FAILED (exit 3). 수동 복구 후 enable |
| S5 | 롤백 실패 누적 (예: 1시간 내 2회) | 서킷 카운터 | **서킷 OPEN**: 자동 롤백 동결 + 배포 게이트 폐쇄 | 원인 조사 → `circuit reset` |
| S6 | 컨테이너 전환 중 새 컨테이너 기동 실패 (포트 충돌 등) | Start API 오류 | **보상 트랜잭션**: 새 컨테이너 삭제, 원래 컨테이너 이름 복구·재기동 → "이전보다 나빠지지 않음" 보장 | S2/S4로 이어짐 |
| S7 | Nginx 설정 편집 후 `nginx -t` 실패 | 명령 종료코드 | 백업 파일 즉시 복원, reload 안 함 → LB는 기존 설정으로 계속 서비스 | 드레인 실패로 처리되어 제자리 롤백 계속 |
| S8 | 드레인하면 풀이 비게 됨 (2대 중 1대 이미 점검 중) | `Pool()` + `DrainBatch` | 드레인 **거부**, 1대씩 **제자리 롤백**(트래픽 유지). 부분 허용이면 허용치만큼 배치 처리 | 이벤트 `blast-radius` 기록 |
| S9 | LB 풀 상태 조회 불가 | `Pool()` 오류 | 보수적으로 배치 크기 1 | — |
| S10 | 롤백 → 재배포 → 롤백 반복 (이전 버전도 불량, 또는 오탐) | 서비스별 롤백 이력 | `max_rollbacks_per_hour` 초과 또는 `cooldown` 이내면 자동 롤백 **차단** → 실패 대상만 격리 → 사람에게 | ROLLBACK_FAILED("flapping guard") |
| S11 | 같은 서비스에 동시 롤백 (CI 잡 2개, 서버+CLI) | 프로세스 내 락 + 저널 디렉토리의 **락 파일**(O_EXCL, 30분 stale 회수) | 두 번째 요청은 즉시 거부 | — |
| S12 | 오케스트레이터가 롤백 도중 사망 또는 DB에서 끊김 | 단일 노드: 재시작 시 저널 재생. HA: 리더 리스 만료 | 완료된 단계(`rollback.step`)는 건너뛰고 나머지 수행. 모든 단계 멱등. HA에서는 다른 노드가 리더가 되어 이어받고, 물러난 노드의 기록은 DB가 거부(펜싱) | 테스트: `TestResumeSkipsCompletedSteps`, `TestHAFailoverFinishesInterruptedRollback` |
| S12a | 롤백 시작 시점에 상태 저장소(PostgreSQL) 장애 | 서비스 lease 획득 오류 | `rollback_lease.wait`(10s) 동안 재시도 후, 기본(`proceed`)은 프로세스 안 잠금만으로 롤백 진행 + 이벤트·감사·경고 알림. 기록은 대기열에 쌓였다가 복구 시 순서대로 기록. 복구 후 다른 프로세스가 lease를 잡고 있으면 `lease.conflict` 알림. `fail`이면 롤백하지 않음 | 테스트: `TestChaosStoreOutageDuringRollback`, `TestChaosStoreOutageLeaseFailMode` |
| S13 | 오케스트레이터가 카나리 관측 중 사망 / 네트워크 분단 | 에이전트 하트비트 실패 `failsafe_after` | 에이전트가 **로컬 규칙 평가**. `failsafe: rollback`이면 자기 호스트만 롤백(트래픽 단계 제외), `hold`면 기록·알림만 | 테스트: `TestAgentPushesAndFailsafeRollsBack` |
| S14 | 관측자 실명 (오케스트레이터→대상 SSH만 불가, 서비스는 정상) | 중앙=위반, 에이전트=정상 | **HOLD** — 롤백하지 않음 (`observer_quorum`) | 네트워크 점검 |
| S14a | 관측 장치 과부하 (오케스트레이터 CPU 부족, 소켓 고갈, 관측 쪽 네트워크 장애) — 대상은 정상 | 내부 타이머 지연 > `max_lag`, 루프백 왕복 > `loopback_timeout`, 또는 3개 이상 서비스에 걸친 대상 절반 이상의 프로브 시간 초과 | 프로브 실패 지표 기반 위반을 **HOLD** — 회복 후 `grace`(1m)까지 유지. 로그·액세스 로그 위반은 그대로 판정 (`observer_guard`) | 서버 자원·네트워크 점검. `vigilante_observer_degraded` 지표. 테스트: `TestChaosObserverDegradedHolds` |
| S15 | 공유 의존성 장애 (DB 다운) — 신·구 버전 모두 에러 | 대조군도 같은 규칙 위반 | **HOLD** (Environmental) — 롤백해도 복구되지 않으므로 하지 않음 | 의존성 복구 |
| S16 | 프로브 전체 침묵 (수집 불가) | 샘플 수 < `min_samples`, 전부 Unknown | PASS 금지 → INCONCLUSIVE → `on_inconclusive` 정책 | 기본 hold (exit 4) |
| S17 | 설정 오류(오타, 없는 실행기, 롤백 규칙 없는 서비스) 또는 환경 문제(sudo 비밀번호 요구, 이전 릴리스 삭제, LB 풀에 없는 대상, 로그 형식 불일치) | `validate`(엄격 디코딩·교차 참조) + `doctor`(읽기 전용 사전 점검, JUnit 리포트) | 파이프라인 첫 단계에서 실패하고 조치 방법 출력 | 롤백 순간에 발견되지 않도록 CI에서 강제 |
| S18 | 운영 중 리허설 필요 | — | `--dry-run` / `server.dry_run`: 읽기 명령·체크포인트만 실행, 변경은 로그 | 실제 플랜 검증 |

## 3. Blast Radius 계산

```
minRequired = max(min_healthy, ceil(poolSize × min_healthy_percent / 100))
allowed     = enabledNow − minRequired
batch       = allowed ≤ 0 ? 드레인 거부(제자리, 1대씩) : min(allowed, 롤백 대상 수, parallelism)
```

`Pool()`은 Vigilante가 관리하지 않는 멤버(다른 팀 서버, 점검 중 서버)도 포함해 반환하므로 실제 풀 기준으로 계산됩니다.

## 4. 종료 코드 계약 (CI 연동)

| 코드 | 의미 | 파이프라인 권장 동작 |
|---|---|---|
| 0 | PASS — 다음 단계 진행 | 계속 |
| 2 | FAIL → 자동 롤백 완료 | 실패 처리, 알림 (서비스는 정상 복구됨) |
| 3 | 롤백 실패 / 서킷 OPEN / 승인 대기 | **온콜 호출**, 파이프라인 중단 |
| 4 | HOLD / INCONCLUSIVE | 수동 승인 단계(input) |
| 1 | 엔진 오류 (설정, 접근) | 실패 처리 |

## 5. 운영 런북 요약

1. **exit 3 알림 수신** → `vigilante status -c ... --id <ID>`로 이벤트 타임라인 확인 (어느 대상의 어느 단계가 실패했는지).
2. 격리된 대상은 LB에서 빠져 있음 → 서비스 영향 최소. 원인 수정 후 `vigilante rollback --id <ID> [--executor <다른 전략>] [--approve]`로 재시도.
3. 서킷 OPEN이면 원인(롤백 경로 자체 고장: 레지스트리 장애, 스냅샷 누락, 자격증명 만료 등) 해결 후 `vigilante circuit reset`.
4. 플래핑 차단이면 이전 버전 자체를 의심 — 새 ID로 더 이전 버전을 지정해 수동 롤백: `vigilante rollback --id <NEW> --service S --version <불량> --previous <더 이전 버전>`.
5. 감사: 모든 판정·단계·서킷 전이와 사람·CI의 조치(작업자, 티켓, 거부된 요청 포함)가 해시 체인으로 묶여 저장됩니다. 사고 조사 전에 `vigilante audit verify`로 기록이 변조되지 않았는지 먼저 확인하고, `vigilante audit query --since <시각>`으로 타임라인을 뽑으십시오.

## 6. 알려진 한계

- `state.backend: file`(기본)은 단일 노드용입니다. 리스(락)는 같은 파일시스템을 공유하는 프로세스 사이에서만 배타적입니다. 여러 서버 노드는 `postgres` + `ha`를 쓰십시오.
- HA 펜싱은 상태 기록을 막습니다. 리더 자리를 잃는 순간 이미 대상에 보낸 명령(진행 중이던 한 단계)은 되돌리지 못하므로, 새 리더가 그 단계를 한 번 더 실행할 수 있습니다. 모든 롤백 단계가 멱등이어야 하는 이유입니다.
- 에이전트 failsafe 롤백은 LB에 접근하지 않습니다(트래픽 단계 제외). LB 격리가 필요하면 오케스트레이터 복구 후 처리됩니다.
