# 09. 호환성 매트릭스

플러그인마다 어디까지 검증했는지 적습니다. 바이너리에도 같은 표가 들어 있어 `vigilante plugins`로 볼 수 있고, `vigilante validate`와 서버 시작 로그가 쓰고 있는 실험적 플러그인을 경고합니다.

- **검증됨(verified):** 실제 시스템(장비·소프트웨어·클라우드)을 상대로 동작을 확인했고, 그 대상과 버전을 적었습니다.
- **실험적(experimental):** 자동 테스트는 있지만 모의 서버·시뮬레이터 기준입니다. 운영에 쓰기 전에 `vigilante doctor`로 대상 시스템을 점검하고, 승인 모드(`rollback.mode: approve`)로 시작하기를 권장합니다.

실장비 검증은 로드맵 M8 랩에서 진행하며, 결과가 나오는 대로 이 표와 바이너리의 표(`internal/compat`)를 함께 고칩니다. 둘이 어긋나면 테스트가 실패합니다.

## 프로브

| 플러그인 | 상태 | 검증 대상 / 현재 시험 방식 | 최소 빌드 |
|---|---|---|---|
| `http` | 검증됨 | 실제 HTTP 서비스 (CI의 E2E 데모, Linux) | 포함 |
| `access_log` | 검증됨 | 실제 combined 형식 액세스 로그, 로테이션 포함 (CI의 E2E 데모) | 포함 |
| `log` | 검증됨 | 실제 애플리케이션 로그, 로테이션 포함 (CI의 E2E 데모) | 포함 |
| `tcp` | 실험적 | 로컬 리스너 대상 단위 테스트 | 포함 |
| `grpc` | 실험적 | 단위 테스트. 실제 grpc.health.v1 서버는 M8 랩 | 제외 |
| `host` | 실험적 | /proc 샘플과 모의 SSH 실행기 | 포함 |
| `docker` | 실험적 | 모의 Docker Engine API | 포함 |
| `db` | 실험적 | 단위 테스트. PostgreSQL·MySQL 서버는 M8 랩 (MySQL 드라이버는 전체 빌드만) | 포함 |

## 실행기 (롤백 전략)

| 플러그인 | 상태 | 검증 대상 / 현재 시험 방식 | 최소 빌드 |
|---|---|---|---|
| `webhook` | 검증됨 | 실제 HTTP 배포 엔드포인트와 확인 URL (CI의 E2E 데모) | 포함 |
| `exec` | 실험적 | 단위 테스트 (로컬, 모의 SSH 실행기) | 포함 |
| `symlink` | 실험적 | 로컬 릴리스 디렉토리와 모의 systemctl | 포함 |
| `container` | 실험적 | 모의 Docker Engine API | 포함 |
| `kvm` | 실험적 | 모의 virsh | 포함 |
| `vsphere` | 실험적 | govmomi vcsim 시뮬레이터. vCenter 7·8은 M8 랩 | 제외 |
| `nutanix` | 실험적 | 모의 Prism Element v2 API. AOS 릴리스별 경로 미확인 | 포함 |
| `openstack` | 실험적 | Keystone·Nova·Cinder·Glance 상태 있는 모의 서버. 사내 OpenStack은 M8 랩 | 포함 |

## 트래픽 제어기

| 플러그인 | 상태 | 검증 대상 / 현재 시험 방식 | 최소 빌드 |
|---|---|---|---|
| `nginx` | 실험적 | 생성한 upstream 파일과 모의 nginx | 포함 |
| `haproxy` | 실험적 | 모의 runtime API 소켓 | 포함 |
| `envoy` | 실험적 | 생성한 EDS 파일 | 포함 |
| `f5` | 실험적 | 모의 iControl REST. BIG-IP VE는 M8 랩 | 포함 |
| `aws_alb` | 실험적 | 모의 ELBv2 API. AWS 테스트 계정은 M8 랩 | 제외 |
| `octavia` | 실험적 | Octavia v2 상태 있는 모의 서버. 사내 OpenStack은 M8 랩 | 포함 |

## 플랫폼

| 항목 | 지원 |
|---|---|
| 서버·CLI | Linux amd64·arm64·ppc64le, Windows amd64 (정적 바이너리, CGO 없음) |
| 패키지 | rpm (RHEL·Rocky 9 계열에서 설치 확인), deb (Debian 12에서 설치 확인), systemd |
| 컨테이너 | distroless static, linux amd64·arm64·ppc64le. Helm 차트 (Kubernetes) |
| 상태 저장소 | 파일(단일 노드), PostgreSQL 16 (CI에서 확인) |
| 대상 호스트 | 에이전트 없이 SSH(OpenSSH). 에이전트는 Linux |

## 검증 기록 방법 (M8 랩)

랩에서는 조합(서비스 하나 = 실행기 + 트래픽 제어기 + 프로브)마다 `vigilante lab run`으로 실제 시나리오를 돌립니다. 운영 경로(`vigilante watch`와 같은 엔진)를 그대로 쓰므로, 통과하면 그 조합이 실제로 동작한다는 뜻입니다.

```bash
# 랩 설정: 대상 VM·LB·자격증명을 실제 장비로. 먼저 doctor가 통과해야 시작합니다.
vigilante lab run -c lab.yaml --service order-os --label "사내 OpenStack 2024.1 (Octavia)" \
  --inject "ansible-playbook deploy-bad.yml" --reset "ansible-playbook deploy-good.yml" --repeat 3
vigilante lab summary lab-results.jsonl          # 장비·조합별 실행 수, 통과 수, 탐지·롤백 시간(중앙값), 문제
```

한 번의 실행은 체크포인트(스냅샷) → 풀 상태 기록 → 불량 버전 배포(`--inject`) → 관측·탐지 → 드레인 → 복원·확인 → 트래픽 복귀이고, 끝나면 이전에 트래픽을 받던 멤버가 모두 다시 켜졌는지 확인합니다(LB가 성공이라고 답해도 멤버가 빠져 있으면 실패). 승인 모드 서비스는 랩이 바로 승인합니다. 결과는 실행마다 한 줄씩 `lab-results.jsonl`에 쌓이며, 랩 실행은 파일럿 판정 품질 보고서에서 제외됩니다. CI의 E2E 데모도 같은 명령을 실제 프로세스에 돌립니다.

플러그인이 랩에서 3회 연속 통과하면 다음을 함께 바꿉니다.

1. `internal/compat/compat.go`의 항목: `Verified`와 검증 대상(제품·버전, 예: `F5 BIG-IP VE 17.1, iControl REST`).
2. 이 문서의 표: 상태와 검증 대상, `vigilante lab summary` 표를 아래에 붙여 근거로 남김.
