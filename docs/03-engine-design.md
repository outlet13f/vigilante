# 03. 핵심 엔진 설계

이 문서는 프로토타입 코드의 핵심 세 축 — **비동기 수집**, **롤백 트리거 평가**, **실행기 추상화** — 를 코드 위치와 함께 설명합니다.

## 1. 멀티 타겟 비동기 수집 (`internal/probe`, `internal/metrics`)

### 1.1 플러그인 계약

```go
// internal/probe/probe.go
type Emit func(metric string, value float64)

type Probe interface {
    Run(ctx context.Context, emit Emit) error   // ctx 취소까지 폴링 또는 스트리밍
}
type Checker interface {                        // 선택: 동기 1회 확인 (롤백 후 probe.verify)
    Check(ctx context.Context) error
}
type Factory func(spec config.Probe, env Env) (Probe, error)
func Register(typ string, f Factory)            // 각 파일의 init()에서 등록
```

새 프로브 유형은 파일 하나를 추가하고 `init()`에서 `Register("mytype", newMyProbe)`를 호출하면 끝입니다(컴파일 타임 플러그인 — 단일 정적 바이너리 유지).

### 1.2 동시성 모델

```
Collector.Run(ctx, jobs)
  └─ for each (target × probe):  go supervise(job)
         └─ loop until ctx.Done:
              p := factory(spec, env{target, runner, template data})
              err := p.Run(ctx, emit)          ← 폴링 프로브: ticker / 스트리밍: tail -F, docker /events
              emit("probe_error", 1)           ← 죽은 프로브도 신호가 된다
              backoff 1s → 2s → … → 30s (1분 이상 정상 동작 후엔 리셋)
```

- **고루틴 = (대상 × 프로브)**. 한 대상의 SSH 지연이 다른 대상 수집을 막지 않습니다.
- SSH는 `transport.Manager`가 **대상별 1개 연결을 풀링**하고 명령마다 세션(채널)만 엽니다. 50개 프로브가 한 호스트를 봐도 TCP/SSH 핸드셰이크는 1번입니다. 끊긴 연결은 keepalive 요청 실패로 감지해 재연결합니다.
- `host` 프로브는 `/proc/loadavg; /proc/stat; /proc/meminfo; /proc/diskstats; nproc`를 **한 번의 왕복**으로 가져오고, CPU·디스크 사용률은 이전 스냅샷과의 **델타**로 계산합니다.
- 로그 계열은 라인마다 샘플을 만들지 않고 **1초 버킷**(`bucketer`)으로 집계합니다. 초당 1만 라인 로그도 초당 수 개의 샘플이 됩니다. 지연시간 원시값은 초당 256개로 리저버 샘플링합니다.
- 로컬(에이전트) 모드의 로그 추적은 Go 내장 follower가 inode 변경·truncate(copytruncate)를 감지해 재오픈합니다. 원격은 `tail -n0 -F`를 SSH 세션에 스트리밍합니다.
- Docker는 SDK 없이 원시 HTTP로 Engine API를 호출하며, 다이얼러를 `Runner.Dial("unix", "/var/run/docker.sock")`로 주입해 **SSH stream-local 채널로 원격 소켓에 접근**합니다.

### 1.3 자체 시계열 저장소

```go
// internal/metrics/store.go
store.Add(model.Sample{Target, Metric, Value, Time, Source})
store.Window(target, metric, window, now, source) []Point
metrics.Aggregate(points, "p99" | "rate" | ..., window) (float64, ok)
```

- (대상, 메트릭)별 시간순 슬라이스 + 보존기간(30분)/최대 포인트(20만) 가지치기. 관측 윈도우가 분 단위라 정렬 기반 정확 백분위수(nearest-rank)로 충분합니다.
- `Source`(central / agent:NAME)를 보존해 관측점별 질의가 가능합니다 → 쿼럼 판정.
- 카운터 집계(`sum/count/rate`)는 데이터가 없으면 0, 게이지 집계(`p99/avg/last`)는 **unknown**입니다.

## 2. 동적 롤백 트리거 평가 (`internal/rules`, `internal/decision`)

### 2.1 3값 논리 규칙 평가

```go
// internal/rules/rules.go
func (e *Evaluator) Eval(rule config.Rule, sc Scope, now time.Time) Result  // State: True|False|Unknown
```

- **Leaf**: `value = Aggregate(Window(metric))` (또는 `ratio_of` 가중 비율) → 임계치/베이스라인 비교 → **연속 카운터** 갱신.
- 카운터 키 = `규칙경로|대상|관측점`. `for: N`은 연속 N회 위반, `reset_after: M`은 연속 M회 정상일 때만 리셋 → 위반·정상·위반 진동(flapping metric)도 누적되어 잡힙니다.
- **단락 평가 금지**: `any`의 첫 자식이 참이어도 나머지를 모두 평가해 카운터가 함께 전진합니다. 그렇지 않으면 `for` 의미가 깨집니다.
- 데이터 없음은 기본적으로 Unknown입니다. "응답이 없으니 정상"도 "응답이 없으니 장애"도 아닌, **증거 부족**으로 다룹니다.

### 2.2 베이스라인

| 소스 | 언제 | 구현 |
|---|---|---|
| 사전 스냅샷 | `vigilante baseline`이 배포 직전 N분 측정 → JSON | `rules.Capture`, `rules.Snapshot` |
| 라이브 대조군 | 같은 시각, 구버전 대상의 같은 집계 | `rules.Control` |
| 체인 | 대조군 우선, 없으면 스냅샷 | `rules.Chain{control, snapshot}` |

`p99 > baseline × 3 AND p99 > 300ms (min_value)` 처럼 상대·절대 조건을 함께 걸어 저지연 서비스의 상대 급증 오탐을 막습니다.

### 2.3 단계 판정 (`decision.Engine.Run`)

```
start ─┬─ warmup ─┬────────────── observation window ───────────────┐
       │ (수집만)  │ eval 매 tick:                                    │ 만료:
       │          │   Fail(배포 대상만, 귀책 가능)  → 즉시 FAIL         │  Hold 남음      → HOLD
       │          │   Hold(대조군도 위반 / 관측 불일치 / action:hold)  │  증거 부족      → on_inconclusive
       │          │   Warn(action:notify)         → 알림만            │  그 외          → PASS
```

Context-aware 판정의 3가지 장치:

1. **환경 요인 분리** — 같은 규칙이 대조군(미배포 대상)에서도 참이면 `Environmental=true`, HOLD. 공유 DB 장애에 롤백해 봐야 복구되지 않고, 오히려 롤백 중 장애를 키웁니다.
2. **관측점 쿼럼** (`safety.observer_quorum`) — 중앙(SSH)과 에이전트 두 소스가 모두 데이터를 보낼 때, 한쪽만 위반이면 "관측자 장애(네트워크 분단·SSH 불가)"로 보고 HOLD.
3. **증거 충분성** — `min_samples` 미달 또는 모든 평가가 Unknown이면 PASS가 아닌 INCONCLUSIVE. 프로브가 전부 죽어서 "조용한" 배포를 통과시키지 않습니다.

## 3. 트래픽 차단 및 롤백 실행 인터페이스 (`internal/executor`, `internal/orchestrator/rollback.go`)

### 3.1 인터페이스

```go
// internal/executor/executor.go
type Executor interface {                     // 전략 A / B / D / 범용
    Rollback(ctx context.Context, rc *RunContext) error   // 반드시 멱등
    Verify(ctx context.Context, rc *RunContext) error     // 버전 사실 확인
}
type Preparer interface {                     // 선택: 배포 전 체크포인트
    Prepare(ctx context.Context, rc *RunContext) (map[string]string, error)
}
type TrafficController interface {            // 전략 C
    MemberID(m Member) (string, error)
    Pool(ctx context.Context) ([]PoolMember, error)   // blast radius 계산용 전체 풀 상태
    Drain(ctx context.Context, ms []Member) error
    Enable(ctx context.Context, ms []Member) error
}
```

`RunContext`는 대상, 템플릿 데이터(버전·체크포인트), 대상 Runner, 다른 대상 Runner 조회 함수(LB·하이퍼바이저 호스트), 자격증명, `DryRun`을 담습니다. 실행기는 **어디서 실행되는지 모릅니다** — SSH든 로컬 에이전트든 같은 코드입니다.

### 3.2 전략별 구현 요지

| 전략 | 파일 | 안전 설계 |
|---|---|---|
| A symlink | `symlink.go` | `test -d` 선확인 → `ln -sfn` 임시 링크 → `mv -Tf`(rename(2) 원자적) → 재시작. 링크가 없는 순간이 없음. `prepare`의 실제 이전 경로 우선 |
| B container | `container.go`, `dockerapi/` | 구 컨테이너 stop → 옆으로 rename → **동일 Config/HostConfig/Network**로 이전 태그 생성·기동. 기동 실패 시 원래 컨테이너 이름·상태 복원(보상 트랜잭션). 이미 이전 이미지로 실행 중이면 no-op(멱등) |
| C nginx | `traffic_sw.go` | 백업 → 새 파일 → `nginx -t` 실패 시 백업 복원 후 종료 → reload. 변경 없으면 아무것도 안 함 |
| C haproxy | `traffic_sw.go` | Runtime API `drain`(세션 유지) → 대기 → `maint`(옵션) |
| C envoy | `traffic_sw.go` | EDS `health_status: DRAINING` + 원자적 `mv` (Envoy 파일 워처) |
| C f5 | `traffic_hw.go` | `session: user-disabled`(신규 차단, 지속성 유지) / `force_offline`이면 `state: user-down`. 토큰 인증 캐시 |
| C aws_alb | `traffic_hw.go` | Deregister → deregistration_delay 완료 대기(지연은 비치명) / Register → healthy 대기 |
| D vsphere | `hypervisor.go` | govmomi `CreateSnapshot`(prepare) / `RevertToSnapshot` + 전원 확인. vcsim으로 테스트 |
| D nutanix | `hypervisor.go` | Prism v2 snapshot → `vms/{uuid}/restore` → 전원 ON, 태스크 폴링 |
| D kvm | `hypervisor.go` | `virsh snapshot-create-as --atomic` / `snapshot-revert --running` |

### 3.3 롤백 플랜 실행기

```
Rollback(dep)
  ├─ (자동일 때) Guard.Check(flapping/cooldown) → Breaker.Allow()   ─실패→ blocked(): 격리만 하고 사람에게
  ├─ Guard.Acquire(service)   ← 프로세스 내 + 락 파일(동시 CI 잡 간 배타)
  ├─ Journal{rollback.start}, state=ROLLING_BACK, 알림
  ├─ TrafficController.Pool() → DrainBatch(blast radius) → 배치 크기 / 드레인 금지 결정
  └─ 배치별 병렬: target(tn)
        for i, step in plan:
           if i < journal.stepsDone[tn]: skip          ← 크래시 후 재개
           step(): timeout + retry(지수 백오프)
           ├─ ok → Journal{rollback.step i}
           ├─ traffic.drain 실패 → 제자리 롤백 계속
           ├─ app.* 실패 → escalate(): 다음 실행기 Rollback+Verify+probe.verify
           │                 (require_approval이면 AWAITING_APPROVAL로 중단)
           │                 전부 실패 → return err  (traffic.enable 미실행 = 격리 유지)
           └─ traffic.enable 실패 → err
  결과: 모두 성공 → ROLLED_BACK, Breaker.Success()
        승인 대기뿐 → AWAITING_APPROVAL (실패로 세지 않음)
        그 외 → ROLLBACK_FAILED, Breaker.Failure()
```

## 4. 확장 가이드

| 하고 싶은 것 | 방법 |
|---|---|
| 새 수집기 (예: Oracle/Tibero 풀, JMX) | `internal/probe/xxx.go`에 Factory 구현 + `Register`. `config.Probe`에 설정 블록 추가, `validateProbe`에 필수 키 검사 추가 |
| 새 롤백 전략 (예: Proxmox) | `Executor`(+필요 시 `Preparer`) 구현 + `executor.Register` |
| 새 LB (예: Citrix ADC, Azure LB, A10) | `TrafficController` 구현 + `RegisterTraffic`. `Pool()`은 관리 외 멤버도 반환해야 blast radius가 정확 |
| 사내 배포 콘솔 연동 | 코드 없이 `webhook`/`exec` 실행기로 가능 |

## 5. 테스트 전략 (구현됨)

| 범위 | 테스트 | 방식 |
|---|---|---|
| 시계열 | `metrics/store_test.go` | 백분위·rate·윈도우 경계·소스 필터·보존 |
| 규칙 | `rules/rules_test.go` | any/all/not, 가중 비율, 연속·히스테리시스, baseline+min_value, 대조군 |
| 판정 | `decision/decision_test.go` | 즉시 FAIL, PASS, warmup, 환경 HOLD, 관측 쿼럼, inconclusive 정책 3종, notify |
| 프로브 | `probe/probe_test.go` | HTTP/JSON path, 타임아웃 연속, 로그 로테이션, access log 파싱, /proc 파싱, Docker 이벤트, 재시작 |
| 실행기 | `executor/*_test.go` | Mock Runner 명령 시퀀스, dry-run 무변경, nginx/haproxy/envoy/F5/ALB, 컨테이너 보상, **vcsim vSphere**, Nutanix, KVM, webhook |
| 안전장치 | `safety/safety_test.go` | 서킷 CLOSED→OPEN→HALF_OPEN→CLOSED, 창 밖 실패 제외, 수동 trip, 프로세스 간 락, 플래핑/쿨다운, DrainBatch |
| 오케스트레이션 | `orchestrator/orchestrator_test.go` | 카나리 PASS / FAIL→drain→rollback→verify→enable, 에스컬레이션 복구, 롤백 실패→격리→서킷 OPEN→게이트 폐쇄→재시작 후 유지, 승인 게이트, **크래시 재개**, blast radius 드레인 거부, 플래핑 차단+격리, 환경 HOLD |
| API | `api/server_test.go` | 인증, 수명주기, 샘플 수집, GitHub HMAC |
| 에이전트 | `agent/agent_test.go` | push, 오케스트레이터 소실 후 failsafe 로컬 롤백 |
| E2E | `examples/demo/run-demo.sh` | 실제 프로세스: baseline → 불량 배포 자동 롤백 → 롤백 경로 고장 → 서킷 OPEN → 게이트 폐쇄 → 리셋 → 정상 배포 PASS |
