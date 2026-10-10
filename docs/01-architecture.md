# 01. 시스템 아키텍처 — Vigilante 통합 롤백 제어 엔진

> 외부 APM/모니터링 스택 없이, 이기종 하이브리드 인프라의 배포 상태를 **자체 측정 → 판정 → 즉시 자동 롤백**하는 단일 바이너리 오케스트레이터.

## 1. 아키텍처 결정: "Agentless 우선 + 선택형 경량 에이전트" 하이브리드

| 선택지 | 장점 | 단점 | Vigilante의 사용처 |
|---|---|---|---|
| **중앙 오케스트레이터 (SSH/API 원격 제어)** | 대상 서버 설치물 0, 레거시 유닉스·어플라이언스·클라우드 API 모두 동일 방식 | 오케스트레이터↔대상 네트워크 의존, 초고빈도 로그 수집 시 SSH 대역폭 | **기본 모드**. 모든 프로브/실행기가 SSH 세션 풀·REST API로 동작 |
| **단일 바이너리 에이전트 (Go, 정적 링크)** | 로컬 파일 tail(초당 수만 라인), 폐쇄망·SSH 금지 구간, 오케스트레이터 장애 시 자율 판단 | 배포·업그레이드 관리 필요 | **선택 모드**. 같은 바이너리를 `vigilante agent`로 실행 |

두 모드는 **같은 코드 경로**(같은 Probe 플러그인, 같은 규칙 엔진, 같은 실행기)를 공유합니다. 차이는 `transport.Runner`가 `SSH`냐 `Local`이냐 뿐입니다. 이 덕분에:

- 에이전트를 설치한 호스트에서는 **중앙(SSH)과 에이전트(로컬) 두 관측점**이 생기고, 엔진은 둘의 의견이 갈리면 롤백 대신 HOLD 합니다(관측자 장애 ≠ 서비스 장애).
- 오케스트레이터가 사라져도 에이전트가 **Dead-man's switch**로 자기 호스트를 판정·롤백할 수 있습니다.

## 2. 전체 구성도

```
                         ┌──────────────────────── 배포 시스템 ────────────────────────┐
                         │  Jenkins     GitLab CI     GitHub Actions     사내 배포 콘솔   │
                         └────┬───────────────┬──────────────┬──────────────────┬──────┘
             CLI 게이트 (exit code)       REST API      Webhook(HMAC/토큰)    REST API
           vigilante watch/prepare       /v1/...      /v1/webhooks/{p}     /v1/deployments
                              │               │              │                  │
┌─────────────────────────────▼───────────────▼──────────────▼──────────────────▼──────────────┐
│                         VIGILANTE ORCHESTRATOR  (단일 Go 바이너리, CGO 없음)                   │
│                                                                                              │
│  ┌─────────────┐   Sample    ┌──────────────┐  query   ┌────────────────────────────────┐    │
│  │  Collector  │────chan────▶│ Metrics Store│◀────────│  Decision Engine               │    │
│  │ goroutine × │             │ (링버퍼 TSDB, │         │  · Rule Evaluator (tri-state)  │    │
│  │ (target ×   │◀──push──┐   │  p95/p99,     │         │  · for / reset_after 히스테리시스│    │
│  │  probe)     │         │   │  rate, ratio) │         │  · Baseline / Control group    │    │
│  └──────┬──────┘         │   └──────────────┘         │  · Observer quorum             │    │
│         │ Probe plugins  │                             │  · Warmup / Window / Evidence  │    │
│         │ http grpc tcp  │                             └───────────────┬────────────────┘    │
│         │ host docker    │                                     Verdict │ PASS/FAIL/HOLD     │
│         │ log access_log │                                             ▼                    │
│         │ db             │   ┌──────────────────────────────────────────────────────────┐   │
│         │                │   │ Rollback Orchestrator                                    │   │
│         │                │   │  plan: traffic.drain → app.rollback → app.verify         │   │
│         │                │   │        → probe.verify → traffic.enable                   │   │
│         │                │   │  retry/backoff · escalation ladder · approval gate        │   │
│         │                │   └───────┬───────────────────────────┬──────────────────────┘   │
│         │                │           │ Executor (A/B/D)          │ TrafficController (C)    │
│         │                │           │ symlink container vsphere │ nginx haproxy envoy      │
│         │                │           │ nutanix kvm exec webhook  │ f5 aws_alb               │
│         │                │           │ openstack                 │ octavia                  │
│  ┌──────┴────────────────┴───────────┴───────────────────────────┴───────────────────────┐   │
│  │ SAFETY: Circuit Breaker · Service Lock(파일 락) · Flapping/Cooldown · Blast Radius     │   │
│  ├───────────────────────────────────────────────────────────────────────────────────────┤   │
│  │ JOURNAL: append-only JSONL WAL (fsync) — 감사 로그 + 크래시 복구 + 서킷 상태 영속화      │   │
│  ├───────────────────────────────────────────────────────────────────────────────────────┤   │
│  │ TRANSPORT: SSH 세션 풀 · Bastion 점프 · sudo · SSH stream-local(unix socket 터널) · HTTP│   │
│  └───────┬──────────────────────────┬──────────────────────────┬─────────────────────────┘   │
└──────────┼──────────────────────────┼──────────────────────────┼─────────────────────────────┘
           │ SSH (22, bastion 경유)    │ HTTPS REST/SOAP           │ HTTPS (AWS SigV4 SDK)
           ▼                          ▼                          ▼
 ┌───────────────────────┐ ┌──────────────────────────┐ ┌───────────────────────────────┐
 │ 1. 베어메탈/온프레미스   │ │ 2. 프라이빗 가상화         │ │ 3. 퍼블릭 클라우드 IaaS          │
 │  Linux/Unix 서버        │ │  OpenStack Nova/Cinder   │ │  EC2 / Azure VM (SSH)          │
 │  /proc, systemd, 로그   │ │   (Keystone v3)          │ │  ALB/NLB Target Group API      │
 │  /opt/app/current ──▶  │ │  vCenter(SOAP/govmomi)   │ │                               │
 │                         │ │  Nutanix Prism REST      │ │                               │
 │                         │ │  KVM: virsh over SSH     │ │                               │
 ├───────────────────────┤ ├──────────────────────────┤ ├───────────────────────────────┤
 │ 4. 컨테이너 과도기       │ │ 5. 트래픽 제어 계층        │ │  (선택) vigilante agent         │
 │  VM 내 Docker/Podman    │ │  Nginx upstream + reload │ │  로컬 tail · push /v1/samples   │
 │  docker.sock ⇐ SSH 터널 │ │  HAProxy Runtime API     │ │  heartbeat · failsafe 판정      │
 │  K8s Ingress 하단 VM     │ │  Envoy file-EDS          │ │                               │
 │                         │ │  F5 BIG-IP iControl REST │ │                               │
 │                         │ │  OpenStack Octavia       │ │                               │
 └───────────────────────┘ └──────────────────────────┘ └───────────────────────────────┘
```

OpenStack은 사내 CMP가 관리하는 주력 프라이빗 클라우드이므로 **상용 1차 출시의 필수 지원 대상**입니다. 실행기 `openstack`(전략 D: Cinder 볼륨 스냅샷 revert 또는 Nova rebuild)과 트래픽 제어기 `octavia`(전략 C)를 M7에서 구현했습니다. 실장비 검증(M8) 전까지는 "실험적"입니다. 설정은 docs/02 "OpenStack" 절을 참고하십시오.

핵심 포인트

- **docker.sock / HAProxy admin socket도 SSH 채널로 터널링**(`direct-streamlocal@openssh.com`)하므로 Docker TCP 포트를 열거나 에이전트를 깔 필요가 없습니다.
- 모든 상태 변경은 **Journal에 먼저 fsync** 된 뒤 실행됩니다. 오케스트레이터가 중간에 죽어도 재시작 시 `Resume()`이 완료된 단계는 건너뛰고 이어서 수행합니다.
- **Safety 계층은 실행기 앞단의 게이트**입니다. 롤백 자체가 실패하면 자동화를 멈추고(서킷 OPEN) 사람에게 넘깁니다.

## 3. 데이터 흐름 (한 번의 카나리 판정)

```
 t=0 ─ CI: vigilante prepare ──▶ Executor.Prepare(): 현재 symlink 대상/이미지 태그/VM 스냅샷 생성
                                   └─▶ Journal{deployment.checkpoints}
 t=1 ─ CI: vigilante baseline ──▶ 5분간 구버전 측정 → baseline.json  (p99 등 기준점)
 t=2 ─ CI: (자체 배포 도구로 canary 1대 배포)
 t=3 ─ CI: vigilante watch --phase canary
        │
        ├─▶ Collector: (canary + control 대상) × probes 고루틴 기동
        │      http  ─ 2s 간격 ─▶ health.up / latency_ms / consecutive_timeouts
        │      access_log ─ tail -F (SSH) ─ 1초 버킷 ─▶ requests / count_5xx / latency_ms
        │      log   ─ tail -F ─ regex ─▶ match.oom / match.exception / match.pool_exhausted
        │      docker ─ inspect + /events ─▶ restarts / oom_events / running
        │      host  ─ /proc 1회 왕복 ─▶ load_per_cpu / mem_available_pct / disk_util_pct
        │      db    ─ pool_size개 동시 확보 + SELECT 1 ─▶ pool_acquire_ms / up
        │                     │
        │                     ▼   Sample{target, "probe.metric", value, time, source}
        │              Metrics Store (target×metric 링버퍼, 30분 보존)
        │                     │
        ├─▶ Decision Engine: eval_interval(5s)마다
        │      warmup 동안은 판정 보류(수집은 계속)
        │      rule tree 평가 (any/all/not, 모든 leaf 평가해 카운터 동기 전진)
        │      ├─ control 대상도 같은 규칙 위반? → Environmental → HOLD
        │      ├─ 중앙 vs 에이전트 관측 불일치? → Observer disagreement → HOLD
        │      └─ 배포 대상만 위반 & for N회 연속 → FAIL (즉시 종료)
        │      window 만료: 증거 부족 → INCONCLUSIVE(on_inconclusive 정책) / 위반 없음 → PASS
        │
        ├─▶ FAIL → Safety: flapping/cooldown 검사 → Breaker.Allow() → Service Lock
        │      ├─ 차단됨 → 실패 대상만 트래픽 격리(blast-radius 허용 범위) → ROLLBACK_FAILED(exit 3)
        │      └─ 허용 → Rollback plan (대상별, blast-radius 배치 단위 병렬)
        │             traffic.drain ─▶ app.rollback ─▶ app.verify ─▶ probe.verify ─▶ traffic.enable
        │             (각 단계 timeout·retry·backoff, 완료마다 Journal{rollback.step})
        │             app.* 실패 → escalation ladder (예: container → VM snapshot[승인 필요])
        │             전부 실패 → 대상은 drain 상태로 격리 유지, Breaker.Failure()
        │
        └─▶ 결과: Journal + Notify(Slack/Webhook) + exit code
               0 PASS · 2 롤백 완료 · 3 롤백 실패/서킷 OPEN/승인 대기 · 4 HOLD · 1 오류
```

## 4. 배포 수명주기 상태 머신

```
                    prepare/create
                          │
                          ▼
   ┌──────────────── PENDING ─────────────────┐
   │ watch(phase)         │ circuit OPEN       │
   ▼                      ▼                    │
 OBSERVING ──PASS──▶ PROMOTED ──watch(next)──▶ OBSERVING ──PASS(full)──▶ SUCCEEDED
   │  │                                                       
   │  └──HOLD/INCONCLUSIVE──▶ HELD (사람 판단, exit 4)              
   │                                                        
   └──FAIL──▶ ROLLING_BACK ──모두 복구──▶ ROLLED_BACK (exit 2)
                   │  │
                   │  └──승인 필요 escalation만 남음──▶ AWAITING_APPROVAL ──approve──▶ ROLLING_BACK
                   │
                   └──실패/차단──▶ ROLLBACK_FAILED (exit 3, 서킷 실패 카운트 +1)
```

## 5. 배포(운영) 형태

| 형태 | 명령 | 적합한 상황 |
|---|---|---|
| **CI 게이트 (단발 실행)** | `vigilante watch ...` | 파이프라인 러너가 대상 네트워크에 접근 가능. 서버 운영 불필요. 서킷·플래핑 이력은 공유 저널 파일로 유지 |
| **중앙 서버** | `vigilante server` | 여러 파이프라인/콘솔이 공유. REST·웹훅, 에이전트 수신, 크래시 복구(Resume) |
| **서버 + CLI 위임** | `vigilante watch --server URL` | CI 러너는 대상망 접근 불가, 오케스트레이터만 접근 가능 (DMZ/폐쇄망 분리) |
| **에이전트** | `vigilante agent --server URL --target NAME` | SSH 금지 구간, 초고빈도 로그, 오케스트레이터 장애 대비 자율 롤백 |

권장 프로덕션 토폴로지

```
   [CI Runner] ──HTTPS──▶ [vigilante server 노드 A: 리더] ──SSH/API──▶ 대상들
          │                 │  ▲ 리더 리스 갱신(TTL/3), 기록은 리더만(펜싱)
          │                 ▼  │
          └──HTTPS──▶ [노드 B: 팔로워] ──요청 전달──▶ 노드 A
                            │
                     [PostgreSQL: 이벤트 로그 · 리스]   ← 두 노드가 공유
   [대상 호스트 일부] ── vigilante agent ── push ─▶ 아무 노드 (리더로 전달)
```

- 상태(판정·롤백 단계·서킷·락)는 PostgreSQL의 append-only 이벤트 로그와 리스 테이블에 있습니다. 리스 만료는 DB 시계 기준이라 노드 간 시계 차이의 영향을 받지 않습니다.
- 리더가 죽거나 DB에서 끊기면 리스가 만료된 뒤 다른 노드가 리더가 됩니다. 새 리더는 상태를 다시 읽고 중단된 롤백을 이어서 끝냅니다. 실측: TTL 3초 설정에서 리더 강제 종료 후 4.1초 만에 전환.
- 리더 자리를 잃은 노드의 기록은 DB가 거부하고(펜싱), 그 노드는 즉시 새 판정·롤백 단계를 멈춥니다.
- 단일 노드·CI 단발 실행은 기존처럼 `state.backend: file`(JSONL 저널)을 씁니다.

## 6. 패키지 지도 (코드 위치)

| 관심사 | 패키지 |
|---|---|
| CLI / 진입점 | `cmd/vigilante` |
| 설정 스키마·검증 | `internal/config` |
| 템플릿(`{{.Address}}`, `{{.PreviousVersion}}`) | `internal/tmpl` |
| SSH 풀·bastion·소켓 터널·dry-run | `internal/transport` |
| 프로브 플러그인 + Collector | `internal/probe` |
| 자체 시계열 저장소 | `internal/metrics` |
| 규칙 평가 / 베이스라인 | `internal/rules` |
| 단계 판정(윈도우·warmup·환경요인·쿼럼) | `internal/decision` |
| 실행기(A/B/D) + 트래픽(C) | `internal/executor`, `internal/dockerapi` |
| 서킷·락·플래핑·blast radius | `internal/safety` |
| WAL 저널 | `internal/journal` |
| 오케스트레이션·롤백 플랜 | `internal/orchestrator` |
| REST/웹훅 | `internal/api` |
| 에이전트 | `internal/agent` |
| 알림 | `internal/notify` |
