---
title: 데이터 설계서
doc_id: VGL-SI-05
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

본 문서는 Vigilante의 데이터 구조를 정의한다. 범위는 상태 저장소(파일 JSONL, PostgreSQL)의 물리 설계, 스키마 마이그레이션 규칙, 저널 기록 종류와 상태 재생, 감사용 해시 체인, 주요 도메인 모델, 설정 파일 스키마 개요, 데이터 보존·정리·아카이브, 민감 데이터 처리다.

## 1.2 기준 버전과 근거 자료

| 항목 | 내용 |
|---|---|
| 기준 소스 | master 커밋 `537870c`(PR #12 병합 `dba3dbe`, PR #13 병합 `dd9a055`, PR #14 병합 `537870c`). PR #13은 저널 기록에 선택 필드 `mac`과 새 감사 action을 더했고 PostgreSQL 스키마는 바꾸지 않았다. PR #14(관측 장치 가드의 프로브 유형 구분)는 데이터 구조·설정 키를 바꾸지 않았다 |
| 저장소 인터페이스·백엔드 | `internal/store/store.go`, `file.go`, `postgres.go`, `migrate.go` |
| 스키마 | `internal/store/migrations/001_init.sql` (마이그레이션 파일은 현재 1개) |
| 저널·재생·해시 체인 | `internal/journal/journal.go` |
| 도메인 모델 | `internal/model/model.go`, `operation.go`, `apiclient.go`, `event.go`, `internal/safety/safety.go`(CircuitState) |
| 설정 | `internal/config/config.go`, `validate.go`, `docs/02-config-spec.md` |
| 정책 문서 | `docs/08-upgrade.md`(마이그레이션 정책), `docs/10-security.md`(비밀값) |

## 1.3 설계 원칙

| 원칙 | 내용 |
|---|---|
| 이벤트 소싱 | 모든 판정·조치·설정성 데이터는 추가 전용(append-only) 기록으로 남기고, 시작 시 순서대로 재생해 메모리 상태를 만든다 |
| 결정 우선 기록 | 결정은 실행 전에 기록한다. 파일 백엔드는 기록마다 fsync, PostgreSQL은 기록마다 트랜잭션 커밋 |
| 백엔드 동등성 | 파일과 PostgreSQL은 같은 기록을 같은 `State.Apply`로 재생하므로 의미가 같다 |
| 감사 일체화 | 상태 기록이 곧 감사 기록이다. 모든 기록은 해시 체인으로 묶여 변조를 검출하고, 체인 키를 설정하면 기록마다 MAC이 더해진다(7장) |
| 비밀값 비저장 | 비밀값은 참조(`*_ref`, `*_env`)로만 설정하고, 서버가 발급한 비밀은 해시만 저장하거나 저장하지 않는다 |

---

# 2. 데이터 아키텍처

## 2.1 구성

| 데이터 | 저장 위치 | 영속성 | 공유 범위 |
|---|---|---|---|
| 결정·조치·감사 기록(저널) | 상태 저장소 (`vigilante_events` 또는 JSONL 파일) | 영속 | PostgreSQL: 모든 서버 노드. 파일: 같은 파일시스템의 프로세스 |
| 리스(서비스별 롤백 잠금, HA 리더) | 상태 저장소 (`vigilante_leases` 또는 잠금 파일) | 만료형 | 위와 같음 |
| 스키마 버전 | PostgreSQL `vigilante_schema` | 영속 | 모든 노드 |
| 재생 상태(`journal.State`) | 프로세스 메모리 | 휘발(재생으로 복원) | 노드별 |
| 저장소 장애 중 쓰기 대기열(M5-4) | 프로세스 메모리 `Engine.pending` (최대 100,000건) | 휘발. 장애 중 크래시하면 유실(12장) | 노드별 |
| 감사 체인 키 | `audit.chain_key_ref`가 가리키는 비밀 저장소. 프로세스 메모리에 해석된 값 | 설정 참조만 영속 | 저장소에 쓰는 모든 노드가 같은 키 |
| 관측 장치 저하 구간 | 프로세스 메모리 `observer.Guard.spans`(최근 2시간), 프로브 시간 초과 상태(30초) | 휘발 | 노드별 |
| 프로브 샘플(시계열) | 프로세스 메모리 `metrics.Store` (보존 30분, 시리즈당 최대 200,000점) | 휘발 | 노드별 |
| 웹훅 전달 이력 | 프로세스 메모리 (구독당 최근 100건) | 휘발 | 노드별 |
| 호출 한도 버킷 | 프로세스 메모리 | 휘발 | 노드별(요청은 리더로 모임) |
| 비밀값 캐시 | 프로세스 메모리 (`secrets.cache_ttl`, 기본 5분) | 휘발 | 노드별 |
| 콘솔 세션 | 브라우저 쿠키 `vgl_session`(AES-GCM 암호화된 ID 토큰) | 쿠키 수명 | 서버에 저장하지 않음 |
| 설정 | `vigilante.yaml` (참조형 비밀값만) | 파일 | 운영자 관리 |
| 부가 산출물 | 기준선 파일(`baseline --out`), 랩 결과 `lab-results.jsonl`, 감사 아카이브 JSONL, 지원 번들 zip | 파일 | 운영자 관리 |

## 2.2 데이터 흐름

```text
API·CLI·엔진 결정 ──▶ journal.Entry 생성 ──▶ Store.Append
                                            ├─ 해시 체인 봉인(prev, hash, 체인 키가 있으면 mac)
                                            ├─ 파일: JSONL 한 줄 + fsync
                                            └─ PostgreSQL: vigilante_events INSERT (펜싱 조건)
                                                  │
기동·리더 승계 ──▶ Store.Load ──▶ State.Apply(기록 순서대로) ──▶ 메모리 상태
                                                  │
                     감사 조회·검증 ◀── Store.Scan (기록 위치와 함께 순회)
```

---

# 3. 상태 저장소 백엔드

## 3.1 백엔드 비교

| 항목 | file | postgres |
|---|---|---|
| 설정 | `server.state.backend: file`(기본), `server.journal_path`(기본 `vigilante-journal.jsonl`) | `server.state.backend: postgres`, `dsn_ref`·`dsn_env`·`dsn` |
| 용도 | 단일 노드, CI 단발 실행(실행 간 서킷·플래핑 이력 공유를 위해 공유 경로 권장) | 여러 서버 노드 공유, HA 전제(`ha.enabled`는 postgres 필수) |
| 기록 형식 | JSONL, 한 줄에 `journal.Entry` 하나 | `vigilante_events.body`(jsonb)에 `journal.Entry` |
| 위치 번호 | 줄 번호 | `seq` (bigserial) |
| 추가 직렬화 | 잠금 파일 `<journal>.lock`(배타 생성, 10초 이상 된 잠금은 크래시 잔재로 보고 제거, 10초 대기 후 실패) + 파일 꼬리에서 직전 해시 재조회 | 트랜잭션 advisory lock(`chainLock = 72105118106`) + 최신 `seq`의 해시 조회 |
| 내구성 | 기록마다 `fsync` | 기록마다 커밋 |
| 리스 | 저널과 같은 디렉토리의 `vigilante-<키>.lock` 파일(JSON `{owner, expires}`), 임시 파일 후 rename으로 원자 교체. 같은 파일시스템 공유 프로세스 간에만 유효 | `vigilante_leases` 행. 만료는 DB 시계 `now()` 기준 |
| 펜싱 | 없음(단일 작성자 전제) | 리더 리스를 가진 경우에만 INSERT 성공, 아니면 `ErrFenced` |
| 준비 확인 | 파일 존재(`os.Stat`) | `pool.Ping` |
| 보존 정리 | 파일 재작성(`<journal>.prune.tmp` 후 rename). 서버가 쓰지 않을 때 실행 | 한 트랜잭션에서 아카이브·삭제·앵커 삽입 |
| 스키마 | 없음 | 마이그레이션(5장) |
| 파일 권한 | 저널 0644, 디렉토리 0755로 생성. 패키지 설치 시 `/var/lib/vigilante` | - |

## 3.2 저장소 인터페이스 (`store.Store`)

| 연산 | 의미 |
|---|---|
| `Append(ctx, e)` | 기록 1건을 해시 체인에 봉인해 영속 기록 |
| `Load(ctx)` | 전체 기록을 재생해 새 `journal.State` 반환. 해석 불가 기록은 건너뛰고 `Corrupt`로 계수 |
| `Scan(ctx, fn)` | 위치(seq 또는 줄)와 함께 순서대로 순회. 감사 검증·조회용 |
| `Prune(ctx, before, archive)` | `before`보다 오래된 앞부분을 아카이브(JSONL)로 쓴 뒤 앵커 1건으로 대체. 삭제 건수 반환 |
| `TryLease(ctx, key, owner, ttl)` | 리스 획득 또는 갱신(같은 owner). 다른 owner가 유효 리스를 가지면 false |
| `ReleaseLease`, `LeaseHolder` | 리스 해제(owner 일치 시), 현재 유효 보유자 조회 |
| `Fence(key, owner)` | 이후 Append는 owner가 key를 보유할 때만 성공 |
| `SetChainKey(key)` | 이후 Append가 기록마다 `mac`을 붙임(nil이면 끔). `store.Open`이 `audit.chain_key_ref`를 해석해 호출(PR #13) |
| `Ping`, `Describe`, `Close` | 준비 확인, 설명 문자열(`file:<path>` 또는 `postgres://host:port/db`), 종료 |

## 3.3 리스 키

| 키 | 용도 | owner 값 |
|---|---|---|
| `leader` | HA 리더 선출. TTL `ha.lease_ttl`(기본 15초, 최소 3초), TTL/3마다 갱신 | 노드 ID와 advertise URL을 구분자로 이은 문자열(아래 주석). 팔로워가 리더 주소를 알아내는 데 사용 |
| `service:<서비스>` | 서비스별 롤백 잠금(동시에 한 롤백만). TTL 2분, TTL/3마다 갱신. 저장소 불통이면 `safety.rollback_lease.wait` 재시도 후 `proceed`면 lease 없이 진행하고 5초마다 다시 시도(PR #13) | 엔진 owner(기본 `<호스트>/<PID>`) |

> 리더 리스의 owner는 노드 ID와 advertise URL을 세로 막대 문자로 이어 붙인 값이다(`internal/ha/elector.go`의 `Owner`, 예: `node-a` + 세로 막대 + `http://node-a:8088`).

---

# 4. PostgreSQL 물리 스키마

## 4.1 테이블 목록

| 테이블 | 생성 | 설명 |
|---|---|---|
| `vigilante_events` | 마이그레이션 001 | 추가 전용 이벤트 로그. 모든 결정, 롤백 단계, 서킷 변화, 감사 기록. `seq` 순서로 재생 |
| `vigilante_leases` | 마이그레이션 001 | 서비스별 롤백 잠금과 HA 리더 선출 리스 |
| `vigilante_schema` | 마이그레이션 실행기(`lockSchema`)가 `CREATE TABLE IF NOT EXISTS` | 적용된 마이그레이션 기록 |

## 4.2 vigilante_events

| 컬럼 | 타입 | 제약·기본값 | 설명 |
|---|---|---|---|
| `seq` | bigserial | PRIMARY KEY | 저장 순서. 재생·감사 위치 번호. 보존 정리 후 앵커는 마지막으로 삭제된 `seq`를 재사용 |
| `at` | timestamptz | NOT NULL | 기록 시각(`Entry.time`) |
| `kind` | text | NOT NULL | 기록 종류(6.2) |
| `service` | text | NOT NULL DEFAULT `''` | 서비스. `Entry.service`, 없으면 `Entry.deployment.service` |
| `deployment_id` | text | NOT NULL DEFAULT `''` | 배포 ID. `Entry.deployment_id`, 없으면 `Entry.deployment.id` |
| `body` | jsonb | NOT NULL | `journal.Entry` 전체(해시 체인 필드 `prev`·`hash`, 체인 키 사용 시 `mac` 포함) |

| 인덱스 | 컬럼 | 용도 |
|---|---|---|
| `vigilante_events_pkey` | `seq` | 기본 키, 재생 순서 |
| `vigilante_events_deployment` | `deployment_id` | 배포별 조회 |
| `vigilante_events_kind_at` | `kind`, `at` | 종류·기간 조회(감사, 보존 정리) |

## 4.3 vigilante_leases

| 컬럼 | 타입 | 제약 | 설명 |
|---|---|---|---|
| `key` | text | PRIMARY KEY | 리스 키(3.3) |
| `owner` | text | NOT NULL | 보유자 |
| `expires_at` | timestamptz | NOT NULL | 만료 시각(DB 시계 기준 `now() + ttl`) |

획득·갱신은 단일 upsert로 수행한다. 행이 없거나, 만료되었거나, 이미 같은 owner일 때만 갱신하고 `RETURNING owner`가 요청 owner와 같으면 획득 성공이다.

```sql
INSERT INTO vigilante_leases (key, owner, expires_at)
VALUES ($1, $2, now() + $3 * interval '1 millisecond')
ON CONFLICT (key) DO UPDATE
  SET owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at
  WHERE vigilante_leases.owner = EXCLUDED.owner OR vigilante_leases.expires_at <= now()
RETURNING owner;
```

## 4.4 vigilante_schema

| 컬럼 | 타입 | 제약·기본값 | 설명 |
|---|---|---|---|
| `version` | int | PRIMARY KEY | 마이그레이션 번호 |
| `applied_at` | timestamptz | NOT NULL DEFAULT `now()` | 적용 시각 |
| `breaking` | boolean | NOT NULL DEFAULT false | `-- vigilante:breaking` 표시 여부 (이후 릴리스에서 `ADD COLUMN IF NOT EXISTS`로 추가) |
| `applied_by` | text | NOT NULL DEFAULT `''` | 적용한 바이너리 버전 (같은 방식으로 추가) |

## 4.5 동시성 제어 (advisory lock)

| 잠금 | 키 | 범위 | 용도 |
|---|---|---|---|
| 마이그레이션 잠금 | `72105118105` ("vigi") | 트랜잭션 (`pg_advisory_xact_lock`) | 여러 노드가 동시에 시작해도 마이그레이션은 한 번만 |
| 체인 잠금 | `72105118106` | 트랜잭션 | 추가·보존 정리를 직렬화해 모든 기록이 직전 기록에 연결되게 함 |

## 4.6 펜싱 쓰기

리더로 펜싱된 노드의 추가는 리스 보유를 같은 문장에서 DB 시계로 확인한다. 리스를 잃은 노드는 0행이 삽입되어 `ErrFenced`를 받으므로, 강등된 노드가 결정을 기록할 수 없다(`TestPostgresFencing`, `TestFailoverOnPartition`).

```sql
INSERT INTO vigilante_events (at, kind, service, deployment_id, body)
SELECT $1, $2, $3, $4, $5
WHERE $6 = '' OR EXISTS (
  SELECT 1 FROM vigilante_leases WHERE key = $6 AND owner = $7 AND expires_at > now());
```

---

# 5. 스키마 마이그레이션

## 5.1 규칙

| 항목 | 규칙 |
|---|---|
| 파일 | `internal/store/migrations/NNN_설명.sql`(바이너리에 내장). 번호는 1부터 빈틈없이. 되돌릴 수 있으면 `NNN_설명.down.sql` |
| 실행 | 적용 대기 마이그레이션을 번호순으로 한 트랜잭션에서 실행하고 `vigilante_schema`에 (`version`, `breaking`, `applied_by`) 기록. 마이그레이션 잠금 하에서 수행 |
| MINOR 정책 | 추가만(테이블, 기본값 있는 열, 인덱스). 이전 릴리스 노드가 같은 DB에서 계속 동작해야 함(순차 업그레이드) |
| 호환 불가 변경 | MAJOR에서만, 파일에 `-- vigilante:breaking` 줄. `breaking = true`로 기록 |
| 다운그레이드 가드 | DB에 이 바이너리가 모르는(번호가 더 큰) `breaking` 마이그레이션이 있으면 시작 거부(`ErrNewerSchema`). 모르는 비호환 아닌 마이그레이션은 허용 |
| 자동 적용 | `server.state.auto_migrate`(기본 true). false이고 적용 대기가 있으면 시작 거부(`ErrPendingMigrations`) → `vigilante store migrate`로 직접 적용 |
| 되돌리기 | `vigilante store migrate --down-to N --yes`. 적용된 것 중 N보다 큰 것을 최신순으로 down 파일 실행. 하나라도 down 파일이 없으면 아무것도 바꾸지 않고 실패(전부 아니면 전무). 모르는 마이그레이션이 있으면 그 릴리스 바이너리로 수행해야 함 |
| 최초 마이그레이션 | `--down-to`는 1 이상. 001(테이블 생성)은 감사 기록을 담으므로 되돌리지 않음 |
| 상태 확인 | `vigilante store status`: 적용 목록, 대기(`pending`), 모르는 것(`unknown`), 이 바이너리의 최신(`latest`). 최신이 아니면 종료 코드 4 |

## 5.2 현재 마이그레이션

| 번호 | 파일 | 내용 | breaking | down |
|---|---|---|---|---|
| 001 | `001_init.sql` | `vigilante_events`(+ 인덱스 2), `vigilante_leases` 생성 | 아님 | 없음(되돌리지 않음) |

PR #13의 키 체인 MAC은 `vigilante_events.body`(jsonb)의 필드로 저장하므로 새 마이그레이션이 없다(`pgStore.SetChainKey` 주석: "the MAC is part of the JSON body, so the schema is unchanged"). 업그레이드·다운그레이드 절차에 영향이 없고, 이전 릴리스 바이너리는 `mac` 필드를 모르는 필드로 무시한다(Go JSON 디코딩).

## 5.3 검증

| 테스트 | 확인 내용 |
|---|---|
| `TestLoadMigrations` | 파일 이름 규칙, 번호 연속성, up·down 짝 |
| `TestMigrateUpgradeDowngradeAndGuard` | 설치 → 추가형 업그레이드, `auto_migrate` 꺼짐 시 거부, 재실행 무변경, 순차 업그레이드 중 이전 릴리스 동작, 가드 |
| `TestDowngradeIsAllOrNothing` | down 파일이 없는 단계가 있으면 아무것도 되돌리지 않음 |
| `TestSchemaTableFromOlderRelease` | 새 열이 없는 이전 `vigilante_schema`가 제자리에서 업그레이드 |
| `TestStoreCommand` | `store status`·`migrate` CLI |

---

# 6. 저널 기록

## 6.1 기록 구조 (`journal.Entry`)

| 필드(JSON) | 형식 | 설명 |
|---|---|---|
| `time` | date-time | 기록 시각. 비어 있으면 추가 시 현재 시각 |
| `kind` | string | 기록 종류(6.2) |
| `service` | string | 서비스 |
| `deployment` | Deployment | 배포 전체 스냅샷(`deployment`) |
| `circuit` | CircuitState | 서킷 상태(`circuit`) |
| `operation` | Operation | 작업 스냅샷(`operation`) |
| `idempotency` | IdemRecord | 멱등 응답 기록(`idempotency`) |
| `api_client` | APIClient | API 클라이언트 스냅샷(`api-client`) |
| `access_token` | AccessToken | 발급 토큰(해시)(`access-token`) |
| `event` | CloudEvent | 발행 이벤트(`event`) |
| `webhook` | Webhook | 구독 스냅샷(`webhook`) |
| `seq` | integer | `webhook.cursor`의 마지막 처리 이벤트 순번 |
| `freeze` | Freeze | API 동결(`freeze`) |
| `deployment_id` | string | 배포 ID |
| `target` | string | 대상(`rollback.step`) |
| `step` | integer | 완료한 계획 단계 인덱스(`rollback.step`) |
| `message` | string | 보조 값(웹훅 ID, 클라이언트 ID, 앵커 설명 등) |
| `actor` | string | 감사: 주체(`user:alice`, `sa:ci`, `client:x`, `cli:bob@host`, `system`) |
| `source` | string | 감사: `api`, `ui`, `cli`, `agent`, `webhook`, `system` |
| `action` | string | 감사: 동작(6.4) |
| `reason` | string | 감사: 사유·상세 |
| `ticket` | string | 감사: 변경·인시던트 티켓(`X-Change-Ticket`, `--ticket`) |
| `prev` | string | 해시 체인: 직전 기록의 해시 |
| `hash` | string | 해시 체인: 이 기록의 해시 |
| `mac` | string, 선택 | 키 체인(PR #13): hex(HMAC-SHA256(`audit.chain_key_ref`, `hash`)). 체인 키가 설정된 동안만 붙고, `hash` 계산에는 포함하지 않는다(7.1). 앵커는 마지막 정리 기록의 `mac`을 이어받는다 |
| `compacted` | Entry[] | 앵커 전용: 정리된 구간이 만든 상태를 재현하는 기록 |

## 6.2 기록 종류와 재생 규칙

| kind | 담는 필드 | 재생(`State.Apply`) 효과 | 북키핑 |
|---|---|---|---|
| `deployment` | `deployment` | `Deployments[id]`를 스냅샷으로 교체 | O |
| `circuit` | `circuit` | `Circuit` 교체 | - |
| `rollback.start` | `service`, `time` | `Rollbacks[service]`에 시작 시각 추가(플래핑 계산) | - |
| `rollback.step` | `deployment_id`, `target`, `step` | `StepsDone[배포][대상] = max(기존, step+1)`(크래시 재개 시 완료 단계 건너뜀) | O |
| `audit` | `actor`, `source`, `action`, `service`, `deployment_id`, `reason`, `ticket` | 상태 변화 없음(감사 전용) | - |
| `anchor` | `hash`, `mac`(키 체인 사용 시), `message`, `compacted` | `compacted`의 기록을 차례로 재생 | - |
| `operation` | `operation` | `Operations[id]` 교체 | O |
| `idempotency` | `idempotency` | `Idempotency[key]` 교체 | O |
| `api-client` | `api_client` | `Clients[id]` 교체 | O |
| `access-token` | `access_token` | `Tokens[sha256]` 교체 | O |
| `api-client.used` | `message`(클라이언트 ID), `time` | 해당 클라이언트 `last_used_at` 갱신(최대 1시간 간격 기록) | O |
| `event` | `event` | `Events`에 추가. 20,000건을 넘으면 최근 10,000건(`MaxEvents`)만 유지 | O |
| `webhook` | `webhook` | `Webhooks[id]` 교체, `deleted`면 제거 | O |
| `webhook.cursor` | `message`(웹훅 ID), `seq` | 구독 `cursor`를 더 큰 값으로 전진 | O |
| `freeze` | `freeze` | `Freezes[id]` 교체 | O |

- 북키핑(O) 종류는 상태 재구성용이며 그 의미는 별도의 `audit` 기록이 담는다. 감사 조회와 SIEM 전송은 북키핑 종류를 건너뛴다(`journal.Bookkeeping`).
- 모르는 종류는 무시한다. 이전 릴리스는 새 종류를 건너뛰므로 파일 저널의 새 종류는 추가만 허용한다(docs/08-upgrade.md).
- 해석할 수 없는 줄(예: 마지막 쓰기가 잘린 경우)은 건너뛰고 `State.Corrupt`로 센다.

## 6.3 재생 상태 (`journal.State`)

| 필드 | 형식 | 설명 |
|---|---|---|
| `Deployments` | map ID → Deployment | 배포 최신 스냅샷 |
| `Circuit` | CircuitState | 서킷 상태(재시작 후에도 OPEN 유지) |
| `Rollbacks` | map 서비스 → 시각[] | 롤백 시작 시각(플래핑 가드) |
| `StepsDone` | map 배포 → 대상 → 완료 단계 수 | 롤백 재개 지점 |
| `Operations` | map ID → Operation | 작업 |
| `Idempotency` | map 키 → IdemRecord | 멱등 응답 |
| `Clients` | map ID → APIClient | API 클라이언트(폐기 포함) |
| `Tokens` | map SHA-256 → AccessToken | 발급 토큰 |
| `Events` | CloudEvent[] | 최근 이벤트(SSE 재개, 웹훅 따라잡기) |
| `Webhooks` | map ID → Webhook | 구독 |
| `Freezes` | map ID → Freeze | API 동결 |
| `Corrupt` | int | 건너뛴 손상 기록 수 |

재생 후 `ROLLING_BACK` 상태 배포는 진행 중이던 롤백(`InFlight`)으로 보고 재개한다. 롤백 단계는 멱등이며 `StepsDone`으로 완료 단계를 건너뛴다.

## 6.4 감사 동작 (action) 예

| 분류 | action |
|---|---|
| 배포 | `deployment.create`, `deployment.prepare`, `deployment.abort`, `deployment.feedback`, `phase.start`, `baseline.capture`, `mark-good`, `hold` |
| 롤백·승인 | `rollback.auto`, `rollback.manual`, `rollback.approve`, `rollback.reject`, `escalation.approve` |
| 안전장치 | `circuit.reset`, `circuit.trip`, `freeze.create`, `freeze.end`, `freeze.override`, `lease.unavailable`, `lease.conflict` |
| 신원·연동 | `oauth.token`, `api-client.create`, `api-client.update`, `api-client.rotate`, `api-client.revoke`, `console.sign_in`, `denied` |
| 비상 조치 | `breakglass.rollback.approve`, `breakglass.rollback.reject`, `breakglass.escalation.approve`, `breakglass.circuit.reset`, `breakglass.circuit.trip`, `breakglass.freeze.override` |
| 이벤트 | `webhook.create`, `webhook.update`, `webhook.delete`, `webhook.rotate`, `webhook.redeliver`, `webhook.disabled` |
| 운영 | `itsm.incident`, `audit.prune` |

> 위 목록은 소스에서 문자열 상수로 확인한 값이다. 동작 값은 늘어날 수 있다.

PR #13에서 추가한 감사 action의 기록 내용:

| action | actor / source | 서비스·배포 | reason | 함께 일어나는 일 |
|---|---|---|---|---|
| `lease.unavailable` | `system` / `system` | 서비스, 그 서비스의 가장 최근 비종료 배포(있으면) | "state store unreachable (…): rolling back under this process's lock only; another process could act on <서비스> at the same time" | 배포 이벤트(`safety`), 경고 알림 "Rollback without the service lease". 기록은 저장소가 돌아올 때까지 쓰기 대기열에 있다 |
| `lease.conflict` | `system` / `system` | 위와 같음 | "state store is back and <보유자> holds the rollback lease for <서비스>: two processes may be acting on it; check its targets" | 배포 이벤트, 경고 알림 "Concurrent rollback suspected". lease당 한 번 |
| `breakglass.<action>` | `cli:<OS 사용자>@<호스트>` / `cli` | 대상 배포의 서비스·ID(서킷 조작은 없음) | `--break-glass` 사유 | critical 알림. 원래 action(예: `circuit.reset`)의 감사 기록도 별도로 남는다. `ticket`은 `--ticket` 값 |

---

# 7. 해시 체인과 감사 필드

## 7.1 알고리즘

| 항목 | 내용 |
|---|---|
| 해시 | `hash = hex(SHA-256(prev + "\n" + JSON(entry, hash 필드를 비운 상태)))`. JSON은 구조체 필드 순서·정렬된 맵 키의 정규 형식 |
| 연결 | `prev`는 직전 기록의 `hash`. 첫 기록은 빈 문자열 |
| 봉인 시점 | `Store.Append`가 직전 해시를 읽고 같은 잠금 안에서 봉인(`Entry.Seal(prev, key)`) |
| 검출 | 기록 하나를 고치면 그 기록의 해시가 맞지 않고, 지우거나 순서를 바꾸면 다음 기록의 `prev` 연결이 끊긴다. DB 관리자가 SQL로 직접 바꿔도 검출된다 |
| 한계와 키 체인(PR #13) | 해시 체인은 키가 없으므로 저장소에 쓸 수 있는 사람은 기록을 고친 뒤 이후 체인을 다시 계산할 수 있다. `audit.chain_key_ref`를 설정하면 `mac = hex(HMAC-SHA256(키, hash))`를 기록마다 붙여, 키 없이 다시 계산한 체인은 MAC이 맞지 않거나 없어서 검출된다. MAC은 `hash` 계산에서 빠지므로(`ChainHash`가 `mac`을 비움) 해시 형식은 그대로이고 키 도입 전 기록과 같은 체인에 공존한다 |
| 키 | 32바이트 이상(`store.MinChainKey`), `vault:`·`env:`·`file:` 참조로만 설정. 저장소에 쓰는 모든 노드가 같은 키를 쓴다. 키를 설정하면 저장소를 여는 모든 명령이 키를 해석해야 한다 |

## 7.2 검증 규칙 (`journal.Verifier`)

| 경우 | 판정 |
|---|---|
| `anchor` 기록 | 체인 시작점. 이후 기록은 앵커의 `hash`에 연결되어야 함. 체인 중간의 앵커는 손상 |
| 해시 없는 기록(체인 도입 전) | 체인 시작 전이면 레거시로 계수(`Legacy`), 시작 후면 손상("체인 없이 삽입") |
| `prev` 불일치 | 손상("앞 기록이 삭제되었거나 순서가 바뀜") |
| 내용과 해시 불일치 | 손상("기록이 수정됨") |
| 키 지정 시: MAC 있는 첫 기록 전의 MAC 없는 체인 기록 | `Unkeyed`로 계수(키 도입 전 기록), 손상 아님 |
| 키 지정 시: 보호 시작(`KeyedFrom`) 뒤 MAC 없음 | 손상("키 없이 쓰거나 다시 쓴 기록") |
| 키 지정 시: MAC 불일치 | 손상. 첫 MAC부터 틀리면 "잘못된 키 또는 키 없이 다시 쓴 기록" |
| 결과 | 첫 손상 위치(seq 또는 줄)와 이유. `vigilante audit verify`는 손상 시 종료 코드 1 |

검증 보고(`audit.Report`, JSON): `entries`, `checked`, `legacy`, `head`, `ok`, `broken`, 그리고 PR #13에서 추가한 `key_checked`(키로 MAC을 확인했는지), `keyed`(MAC이 맞은 기록 수), `unkeyed`(키 도입 전 체인 기록 수), `keyed_from`(키 보호가 시작된 위치, 0이면 MAC 있는 기록 없음). CLI는 키가 없으면 "MACs not checked" 안내를, 키가 있는데 MAC이 하나도 없으면 WARNING을, 그 외에는 "chain key protects the chain from entry N on"을 출력한다.

검증 대상: `vigilante audit verify -c FILE [--key REF]`(저장소 전체), `vigilante audit verify --file ARCHIVE.jsonl [-c FILE] [--key REF]`(아카이브 단독, `-c`는 키 참조를 얻을 때만). 시험: `TestChainDetectsTampering`(file·postgres 각각 수정·삭제), `TestLegacyEntriesBeforeChain`, `TestKeyedChainDetectsRecomputedTampering`(체인을 다시 계산하고 MAC 유지·제거), `TestKeyedChainWrongKey`, `TestKeyedChainLegacyEntries`, `TestKeyedChainCoversUnkeyedPrefix`, `TestKeyedChainAcrossPrune`(각 file·postgres), `TestResolveChainKey`.

## 7.3 감사 필드 활용

| 필드 | 내용 |
|---|---|
| 작업자 | 생성·롤백 요청·승인 주체가 배포의 `created_by`, `rollback_requested_by`, `approved_by`에도 남는다. 로컬 CLI는 `cli:<OS 사용자>@<호스트>` |
| 거부 | 403 요청은 `action: denied`, `reason`에 메서드·경로·이유 |
| 티켓 | `ticket`으로 변경·인시던트와 연결 |
| 조회 | `GET /v2/audit-events`(viewer@`*` + `audit:read`), `GET /v1/audit`(CSV 가능), `vigilante audit query` |
| SIEM | 저장 후 비동기 syslog 전송(RFC 5424 JSON 또는 CEF, TCP·UDP 또는 TLS(RFC 5425 옥텟 카운팅), PR #13). 실패해도 저장소가 기록의 원본 |

---

# 8. 도메인 모델

## 8.1 Deployment (배포)

| 필드(JSON) | 형식 | 설명 |
|---|---|---|
| `id` | string | 배포 ID(클라이언트 지정 또는 생성) |
| `service` | string | 서비스 |
| `version` | string | 신규 버전 |
| `previous_version` | string | 롤백 대상(기준) 버전 |
| `phase` | Phase | 현재 단계(`baseline`, `canary`, `rolling`, `full`) |
| `state` | State | 수명주기 상태(8.2) |
| `verdict` | Verdict | `PENDING`, `PASS`, `FAIL`, `HOLD`, `INCONCLUSIVE` |
| `reason` | string | 상태·판정 사유 |
| `targets` | string[] | 신규 버전을 받은 대상 |
| `checkpoints` | map 대상 → map 키 → 값 | 실행기 체크포인트(8.3) |
| `breaches` | Breach[] | 참이 된 규칙 |
| `created_by`, `rollback_requested_by`, `approved_by` | string | 주체 ID |
| `pending_rollback` | PendingRollback | 승인 모드에서 결정을 기다리는 롤백 |
| `freeze_override` | string | 동결 중 진행 사유와 허용자 |
| `change_ticket` | ChangeTicket | 검증된 ITSM 변경 |
| `dry_run` | boolean | 드라이런으로 실행(롤백은 로그만) |
| `feedback` | Feedback | 판정 평가 |
| `events` | Event[] | 타임라인 |
| `created_at`, `updated_at` | date-time | 생성·갱신 시각 |

> API v2 응답에는 계산 필드 `exit_code`와 `last_evaluation`이 더해진다(VGL-SI-04 4.3). `checkpoints`는 내부 필드다.

## 8.2 상태·판정 열거값

| State | 종료 상태 | exit_code |
|---|---|---|
| `PENDING`, `BASELINE`, `OBSERVING`, `ROLLING_BACK` | 아님 | 1 |
| `PROMOTED` | 아님 | 0 |
| `SUCCEEDED` | 종료 | 0 |
| `HELD` | 아님 | 4 |
| `ROLLED_BACK` | 종료 | 2 |
| `ROLLBACK_FAILED` | 종료 | 3 |
| `AWAITING_APPROVAL` | 아님 | 3 |
| `ABORTED` | 종료 | 1 |

단계 순서는 `canary` → `rolling` → `full`(`model.PhaseOrder`). 판정 실패 여부(`Failed`)는 타임라인의 `verdict` 이벤트 중 `FAIL:`로 시작하는 메시지 존재로 판단한다.

## 8.3 체크포인트 키

| 실행기 | 키 |
|---|---|
| symlink | `symlink.previous` |
| container | `container.previous_image` |
| vsphere | `vsphere.snapshot` |
| nutanix | `nutanix.snapshot_name`, `nutanix.snapshot_uuid` |
| kvm | `kvm.snapshot` |
| openstack | `openstack.mode`, `openstack.server_id`, `openstack.volume_id`, `openstack.volume_snapshot_id`, `openstack.image_id` |
| exec | `exec.prepare_output` |

## 8.4 배포 하위 구조

| 구조 | 필드(JSON) | 설명 |
|---|---|---|
| Breach | `rule`, `target`, `detail`, `action`, `environmental`, `value` | 참이 된 규칙, 대상, 조치(`rollback`, `notify`, `hold`), 대조군도 나빠진 환경 요인 여부, 값 |
| Event | `time`, `kind`, `message` | 타임라인 항목 |
| PendingRollback | `reason`, `targets`, `drained`, `requested_at`, `expires_at`, `detected_at`, `escalated` | 승인 대기 롤백. `drained`는 대기 중 격리한 대상(`drain_first`), `escalated`는 시간 초과 재알림 여부 |
| Feedback | `outcome`, `incident`, `note`, `by`, `at` | `correct`, `false_positive`, `false_negative`, `unclear` |
| ChangeTicket | `number`, `sys_id`, `state`, `approval`, `checked_at`, `unverified` | ServiceNow 변경. `unverified`는 ITSM 불통·fail-open으로 미검증 |

## 8.5 Operation (작업)과 멱등 기록

| 구조 | 필드(JSON) | 설명 |
|---|---|---|
| Operation | `id`, `kind`, `status`, `service`, `deployment_id`, `phase`, `created_by`, `created_at`, `finished_at`, `result`, `error` | `kind`: `observation`, `rollback`, `approval`, `baseline`. `status`: `running`, `completed`, `failed` |
| OperationResult | `state`, `verdict`, `exit_code`, `reason`, `baseline_samples` | 작업 종료 시점의 배포 요약 |
| IdemRecord | `key`, `fingerprint`, `status`, `location`, `body`, `created_at` | `key` = SHA-256(주체 + Idempotency-Key), `fingerprint` = SHA-256(메서드·경로·본문). 성공 응답의 상태·Location·본문을 24시간 보관 |

## 8.6 API 클라이언트와 토큰

| 구조 | 필드(JSON) | 설명 |
|---|---|---|
| APIClient | `id`, `name`, `type`(`oauth`, `api_key`), `description`, `scopes`, `grants`(`역할@범위`), `secret_sha256`, `secret_hint`, `rate_limit`, `expires_at`, `created_by`, `created_at`, `rotated_at`, `revoked_at`, `last_used_at` | 비밀은 SHA-256만. 활성 = 폐기되지 않았고 만료 전 |
| RateLimit | `rate`(초당), `burst`, `daily`(UTC 일 상한, 0 = 없음) | 토큰 버킷 |
| AccessToken | `sha256`, `client_id`, `scopes`, `expires_at` | 발급한 `vat_` 토큰의 해시 |

## 8.7 이벤트·구독

| 구조 | 필드(JSON) | 설명 |
|---|---|---|
| CloudEvent | `specversion`, `id`, `source`, `type`, `subject`, `time`, `datacontenttype`, `sequence`, `service`, `team`, `data` | VGL-SI-04 6.1 |
| Webhook | `id`, `url`, `description`, `types`, `services`, `teams`, `active`, `disabled_reason`, `key_version`, `cursor`, `consecutive_failures`, `dead_letters`, `created_by`, `created_at`, `updated_at`, `deleted` | `key_version`은 서명 비밀 세대(회전 시 증가), `deleted`는 삭제 표시 |
| DeadLetter | `sequence`, `type`, `at`, `attempts`, `error` | 모든 재시도 후 실패한 이벤트. 구독당 최대 100건(`MaxDeadLetters`) |

## 8.8 Freeze (API 선언 동결)

| 필드(JSON) | 형식 | 설명 |
|---|---|---|
| `id`, `name`, `reason` | string | 식별·이름·사유 |
| `starts_at`, `ends_at` | date-time | 기간 |
| `services`, `teams` | string[] | 대상(둘 다 비면 전 서비스) |
| `allow_rollback` | boolean | 동결 중 자동 롤백 허용 |
| `created_by`, `created_at` | string, date-time | 선언자·시각 |
| `ended_by`, `ended_at` | string, date-time | 조기 종료자·시각 |

설정 파일의 동결 창(`change_freeze`)은 저장소에 기록하지 않고 설정에서 읽는다.

## 8.9 CircuitState (서킷 상태)

| 필드(JSON) | 형식 | 설명 |
|---|---|---|
| `state` | string | `CLOSED`, `OPEN`, `HALF_OPEN` |
| `failures` | date-time[] | 기간 내 롤백 실패 시각 |
| `opened_at` | date-time | 열린 시각 |
| `reason` | string | 사유 |
| `half_open_used` | boolean | HALF_OPEN의 시험 조치 사용 여부 |

전이: CLOSED → (기간 내 N회 롤백 실패 또는 수동 트립) → OPEN → (`open_duration` 경과, 0이면 수동 리셋만) → HALF_OPEN → (시험 성공) CLOSED 또는 (실패) OPEN. 저널에 남아 재시작·CI 실행 간에 유지된다.

## 8.10 Sample (프로브 샘플, 비영속)

| 필드(JSON) | 형식 | 설명 |
|---|---|---|
| `target` | string | 대상 |
| `metric` | string | `<probe-id>.<metric>` (예: `health.latency_ms`) |
| `value` | number | 값(NaN·무한대는 버림) |
| `time` | date-time | 측정 시각 |
| `source` | string | `central` 또는 `agent:<이름>` |

샘플은 메모리 시계열 저장소에만 두며(보존 30분) 상태 저장소에 기록하지 않는다. 판정 결과(위반, 판정, 타임라인)만 배포 스냅샷으로 기록된다.

---

# 9. 설정 스키마 개요

## 9.1 최상위 키

설정 파일 `vigilante.yaml`은 알 수 없는 키를 거부하고(`TestUnknownFieldsRejected`) 교차 참조를 검증한다(`vigilante validate`).

| 키 | 형식 | 내용 |
|---|---|---|
| `version` | string | 설정 형식 버전(`v1`) |
| `server` | object | `listen`, `auth_token_env`, `journal_path`, `dry_run`, `webhook_secret_env`, `state{backend, dsn, dsn_env, dsn_ref, auto_migrate}`, `ha{enabled, advertise_url, node_id, lease_ttl, tls{ca_file, server_name, cert_file, key_file}}`, `metrics_public`, `tls{cert_file, key_file, client_ca_file, client_auth, min_version}` |
| `agent` | object | `push_interval`, `heartbeat_interval`, `failsafe_after`, `failsafe`(`hold`, `rollback`), `tls` |
| `credentials` | map 이름 → 자격증명 | `type`(`ssh`, `basic`, `token`, `aws`, `openstack`), 사용자, `*_ref`·`*_env` 비밀 참조, `known_hosts_file`, `ssh_ca{mount, role, ttl, principals}`, AWS `region`·`profile`, OpenStack `auth_url`·`interface`·application credential·프로젝트·도메인·`cacert` |
| `targets` | list | `name`, `kind`(`baremetal`, `vm`, `cloud_vm`, `container_host`), `address`, `labels`, `connection{type(ssh, local, none), credential, port, bastion, sudo, sudo_scope, timeout, max_sessions, reserved_sessions}` |
| `traffic` | map 이름 → 트래픽 제어기 | `type`(`nginx`, `haproxy`, `envoy`, `f5`, `aws_alb`, `octavia`)과 유형별 블록, `drain_wait` |
| `executors` | map 이름 → 실행기 | `type`(`symlink`, `container`, `vsphere`, `nutanix`, `kvm`, `openstack`, `exec`, `webhook`)과 유형별 블록 |
| `services` | list | `name`, `team`, `preset`, `overrides`, `targets`, `control_targets`, `probes[]`, `baseline`, `rules[]`(`any`·`all`·`not` 조합 조건), `phases`(관측 창, warmup, 평가 주기, `on_inconclusive`), `rollback{executor, traffic, scope, parallelism, step_timeout, retry, plan, escalation, mode, approval}` |
| `safety` | object | `circuit_breaker{failure_threshold, window, open_duration}`, `blast_radius{min_healthy, min_healthy_percent}`, `flapping{max_rollbacks_per_hour, cooldown}`, `observer_quorum`, `rollback_lease{wait, on_unavailable}`, `observer_guard{disabled, max_lag, loopback_timeout, timeout_share, min_services, grace}` |
| `notify` | list | `type`(`webhook`, `slack`, `teams`, `email`, `pagerduty`), `url`·`url_env`·`url_ref`, `min_level`, `services`, `teams`, `smtp{...}`, `routing_key_ref`·`routing_key_env` |
| `preset_dirs` | string[] | 조직 프리셋 디렉토리 |
| `auth` | object | `oidc{issuer, audience, username_claim, groups_claim}`, `service_accounts[{name, token_sha256, expires, roles}]`, `role_bindings[{group, user, role, scope}]`, `four_eyes`, `local_cli` |
| `audit` | object | `syslog{address, format, tls{ca_file, cert_file, key_file, server_name, min_version}}`, `retention`(audit prune 기본 보존 기간), `chain_key_ref` |
| `secrets` | object | `vault{address, namespace, ca_file, auth, token_env, role_id_env, secret_id_env, k8s_role, k8s_jwt_path, auth_mount}`, `cache_ttl` |
| `api` | object | `rate_limit`, `emergency_rate_limit`, `token_ttl`, `webhook_signing_key_ref`, `webhook_allowed_hosts` |
| `change_freeze` | list | `name`, `reason`, `start`·`end`(RFC 3339) 또는 `weekly{from, to, timezone}`, `services`, `teams`, `allow_rollback` |
| `itsm` | object | `servicenow{url, credential, change_gate{enabled, services, teams, allowed_states, check_window, on_error}, incidents{enabled, on, assignment_group, caller_id, urgency, impact}, work_notes, tls_skip_verify}` |
| `console` | object | `disabled`, `redirect_url`, `client_id`, `client_secret_ref`, `session_key_ref`, `scopes` |

> `preset_dirs`는 코드(`config.Config`)에 있으나 docs/02-config-spec.md의 최상위 구조 예시에는 빠져 있다.

## 9.2 주요 기본값

| 키 | 기본값 | 근거 |
|---|---|---|
| `server.listen` | `:8088` | docs/02 |
| `server.state.backend` | `file` | docs/02 |
| `server.state.auto_migrate` | `true` | `store.Open` |
| `server.ha.lease_ttl` | `15s` | docs/02 |
| `api.rate_limit` / `api.emergency_rate_limit` | 초당 20·순간 40 / 초당 1·순간 10 | `config/validate.go` |
| `api.token_ttl` | 1시간 | `config/validate.go` |
| `services[].rollback.step_timeout` | 2분 | `config/validate.go` |
| `services[].rollback.retry` | 3회, 2초, 최대 30초 | `config/validate.go` |
| `services[].rollback.approval.timeout`, `on_timeout` | 30분, `hold` | `config/validate.go` |
| `services[].rollback.plan` | 생략 시 `traffic.drain` → `app.rollback` → `app.verify` → `traffic.enable` (트래픽 제어기가 있을 때 drain·enable 포함) | `config/validate.go` |
| `targets[].connection.max_sessions`, `reserved_sessions` | 8, 2 | `transport/sessions.go` |
| `itsm.servicenow.change_gate.on_error` | `closed` | `config/validate.go` |
| `secrets.cache_ttl` | 5분 | `secrets.New` |
| `safety.rollback_lease.wait`, `on_unavailable` | 10초, `proceed` | `config/validate.go`(PR #13) |
| `safety.observer_guard` | 켜짐, `max_lag` 1초, `loopback_timeout` 1초, `timeout_share` 0.5, `min_services` 3, `grace` 1분 | `config/validate.go`(PR #13) |
| `auth.local_cli` | 빈 값 = `auto`(인증이 설정되어 있으면 제한) | `config.LocalCLIRestricted`(PR #13) |
| `audit.syslog.format` | `rfc5424` | `config/validate.go` |
| `audit.syslog.address`의 `tls://` 포트 | 6514 | `config/validate.go`(PR #13) |
| `audit.syslog.tls.min_version` | 1.2 | `tlsconf.Syslog`(PR #13) |
| `audit.chain_key_ref` | 없음(키 체인 꺼짐) | `store.Open`(PR #13) |

PR #13 설정 검증 규칙: `auth.local_cli`는 `auto`·`full`·`restricted`, `rollback_lease.on_unavailable`은 `proceed`·`fail`, `observer_guard.timeout_share`는 (0, 1]·`min_services` ≥ 1·`grace` ≥ 0, `audit.syslog.address`는 `tcp`·`udp`·`tls`(tls는 호스트 이름 필수), `audit.syslog.tls`는 `tls://`에서만·`cert_file`과 `key_file`은 함께·`min_version`은 1.2 또는 1.3, `audit.chain_key_ref`는 비밀 참조 형식.

---

# 10. 데이터 보존·정리·아카이브

## 10.1 보존 정책

| 데이터 | 보존 | 비고 |
|---|---|---|
| 저널(감사 포함) | 운영자가 정리할 때까지 무기한. 자동 삭제 없음 | `audit.retention`(예: `8760h`)은 `audit prune --older-than`의 기본값일 뿐. 비기능 목표의 감사 보존은 기본 1년(제안값) |
| 재생 상태의 이벤트 | 최근 10,000건 (`MaxEvents`) | SSE 재개·웹훅 따라잡기 범위 |
| 멱등 기록 | 24시간 (`IdemTTL`) | |
| dead-letter | 구독당 최근 100건 | |
| 웹훅 전달 이력 | 노드 메모리, 구독당 최근 100건 | 재시작 시 소멸 |
| 프로브 샘플 | 노드 메모리 30분 | 비영속 |
| 비밀값 캐시 | 노드 메모리 `cache_ttl`(기본 5분) | 비영속 |

## 10.2 보존 정리(prune) 절차

| 단계 | 내용 |
|---|---|
| 1. 대상 결정 | `--before DATE` 또는 `--older-than DUR` 이전 시각의 앞부분 기록. PostgreSQL은 `at >= before`인 첫 `seq` 앞까지 |
| 2. 아카이브 | 정리할 기록을 그대로 JSONL로 `--out ARCHIVE.jsonl`에 쓴다(기존 파일 덮어쓰기 거부). 아카이브는 단독으로 체인 검증 가능 |
| 3. 앵커 생성 | 정리 구간이 만든 상태 중 필요한 것을 `compacted`에 담은 `anchor` 기록 1건. `hash`는 마지막 정리 기록의 해시(남은 체인이 이어짐), `mac`은 마지막 정리 기록의 MAC(키 체인 사용 시, 같은 해시의 MAC이므로 그대로 유효), `actor: system`, `action: audit.prune` |
| 4. 교체 | 파일: 앵커 + 남은 기록으로 새 파일을 쓰고 rename(서버 중지 상태에서 실행). PostgreSQL: 같은 트랜잭션에서 `DELETE seq <= 마지막` 후 앵커를 마지막 `seq`로 삽입(체인 잠금 하) |

| 앵커에 보존하는 상태(`journal.Compact`) | 조건 |
|---|---|
| 서킷 상태 | 항상 |
| 배포 | 진행 중(비종료)인 배포, 서비스별 마지막 `SUCCEEDED` 배포(다음 배포의 기본 `--previous`) |
| 롤백 단계 | 진행 중 배포의 완료 단계 |
| 롤백 시작 시각 | 기준 시각 1시간 이내(플래핑 계산) |
| 작업 | `running`인 것 |
| API 클라이언트 | 전부(폐기 포함, 기록용) |
| 접근 토큰 | 만료 전인 것 |
| 웹훅 구독 | 전부(커서 포함) |
| 이벤트 | 최근 1,000건(순번 연속성과 구독 따라잡기) |
| API 동결 | 끝나지 않았고 기준 시각 이후 종료하는 것 |
| 멱등 기록 | 24시간 이내 |

시험: `TestPruneArchivesAndKeepsState`(file·postgres), `TestCompactKeepsRunningOperationsAndFreshKeys`, `TestKeyedChainAcrossPrune`(file·postgres, 정리 후 키 체인 검증).

---

# 11. 민감 데이터 처리

## 11.1 분류와 처리

| 데이터 | 처리 | 저장 형태 |
|---|---|---|
| 서비스 계정 토큰 `vgl_…` | `vigilante token create`가 출력, 설정에는 SHA-256만 | 설정 `token_sha256` (해시) |
| API 키 `vgk_…`, OAuth 클라이언트 비밀 `vcs_…` | 등록·회전 응답에 1회만 표시 | 저널 `api_client.secret_sha256` (해시), `secret_hint`(앞 8자) |
| OAuth 접근 토큰 `vat_…` | 발급 응답만 | 저널 `access_token.sha256` (해시) |
| 웹훅 서명 비밀 `whsec_…` | 마스터 키(`api.webhook_signing_key_ref`)와 웹훅 ID·키 세대로 필요할 때 계산 | 저장하지 않음 |
| Idempotency-Key | 주체와 결합해 해시 | 저널 `idempotency.key` (SHA-256). 비밀값을 담는 응답은 멱등 기록 대상에서 제외 |
| 외부 시스템 비밀번호·토큰·SSH 키·DSN·알림 URL | 설정에는 참조(`vault:`, `env:`, `file:`)나 환경변수 이름만. 자격증명 구조에 평문 비밀번호 키가 없음 | 저장하지 않음(메모리 캐시 `cache_ttl`) |
| SSH 접속 키 (Vault SSH CA 사용 시) | 프로세스가 임시 ed25519 키를 생성하고 Vault SSH CA가 인증서를 서명, 인증서 수명의 80%에서 교체 | 디스크에 쓰지 않음 |
| 콘솔 세션 | ID 토큰을 AES-GCM으로 암호화한 HttpOnly 쿠키. HA는 `console.session_key_ref` 공유 | 서버 저장 없음 |
| 레거시 토큰 | 환경변수(`server.auth_token_env`) | 저장하지 않음 |
| 감사 체인 키 | `audit.chain_key_ref`(비밀 참조, 32바이트 이상). 해석한 값은 프로세스 메모리에만. 저장소에는 키로 만든 MAC만 남는다 | 저장하지 않음 |
| SIEM·HA 전달용 TLS 개인 키 | `audit.syslog.tls.key_file`, `server.ha.tls.key_file` 파일 경로만 설정 | 운영자가 관리하는 파일(인증서 갱신 시 다시 읽음) |
| 로그 | 해석된 비밀값은 `secrets.RedactHandler`가 로그에서 가림 | - |
| 지원 번들 | 설정 값(비밀번호·토큰·DSN·키·알림 URL·인증 헤더)과 텍스트 속 토큰·JWT·URL 비밀번호를 `REDACTED`로 바꿈 | zip |

## 11.2 저장소에 남는 운영 데이터

다음은 비밀은 아니지만 운영 정보이므로 상태 저장소 접근을 제한한다: 배포 스냅샷(버전, 대상 이름, 위반 지표 값, 체크포인트 값), 감사 기록(주체 ID, 사유, 티켓), 멱등 기록의 응답 본문(24시간), 웹훅 URL, ServiceNow 변경 번호·sys_id. PostgreSQL은 전용 DB와 소유자 계정 하나만 쓰고(docs/10-security.md), `sslmode=verify-full`을 권장한다.

---

# 12. 쓰기 신뢰성

| 항목 | 내용 | 상태 |
|---|---|---|
| 결정 선기록 | 결정을 기록한 뒤 조치. 파일은 기록마다 fsync | master 반영 |
| 펜싱 | 리더 리스를 잃은 노드의 기록 거부(`ErrFenced`) | master 반영 |
| 저장소 장애 중 롤백 lease | 롤백 시작 시 저장소에 닿지 않으면 `service:<서비스>` lease를 `safety.rollback_lease.wait`(10초) 동안 재시도. `proceed`(기본)면 lease 없이 롤백하고 감사 `lease.unavailable`(대기열 경유)을 남기며, 저장소가 돌아오면 lease를 잡거나 타인 보유 시 `lease.conflict`. `fail`이면 ROLLBACK_FAILED | PR #13(`f696bc9`). `TestGuardStoreUnreachable`, `TestChaosStoreOutageDuringRollback`, `TestChaosStoreOutageLeaseFailMode` |
| 키 체인 MAC | 기록 본문의 `mac`. 스키마 변경 없음, 키 도입 전 기록과 공존 | PR #13(`c9e39f1`). `TestKeyedChain*` 5건(file·postgres 각각), `TestResolveChainKey` |
| 저장소 장애 대기열 | `internal/orchestrator/engine.go`의 `record`·`flush`(write-behind). 펜싱 외 쓰기 실패(기록 1건 제한 시간 5초)를 메모리에 순서대로 보관하고, 대기분이 있으면 새 기록도 그 뒤에 줄 세운다. 저장소가 응답하면 200밀리초에서 2배씩 최대 5초 백오프로 순서대로 기록. 해시 체인 순서 유지. 이벤트 버스·SIEM 훅은 저장된 뒤 실행. 대기열은 최대 100,000건이며 넘치면 버리고 `vigilante_store_errors_total{reason="dropped"}`로 센다. 종료 시 최대 10초 대기. 지표 `vigilante_store_pending_writes`(게이지, `internal/telemetry/catalog.go`). 다른 노드가 리더가 되면(`ErrFenced`) 대기분은 기록하지 않고 버린다. **대기열은 메모리에만 있으므로 장애 중 크래시하면 대기분은 유실된다**(복구 후 기록되는 배포 스냅샷이 상태를 다시 담음) | M5-4. 커밋 `9f9470e`에서 도입, PR #12로 master 병합(`dba3dbe`). 카오스 테스트 `TestChaosStoreOutageDuringRollback`로 검증(로컬 `dd9a055` 통과, CI PR #13 실행 38091832392 통과) |
