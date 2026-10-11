---
title: 호환성·검증 현황
doc_id: VGL-BD-03
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

이 문서는 Vigilante의 플러그인(프로브·실행기·트래픽 제어기)과 실행 플랫폼이 지금 어디까지 검증되었는지, 남은 검증을 어떻게 할지, 출시 게이트가 무엇인지를 정리합니다. 도입 검토자가 "이 조합을 우리 환경에서 써도 되는가"를 판단하는 근거로 씁니다.

- 기준일: 2026-10-11 (제품 저장소 master `537870c` 기준, 정식 릴리스 없음). 로드맵 M5-4(부하·장애 주입 시험, PR #12), 검토 결함 수정(PR #13), 관측 장치 보호의 보류 대상을 서버가 직접 재는 프로브로 한정한 수정(PR #14)이 이 커밋에 병합되어 있습니다.
- 원본: 바이너리의 호환성 표 internal/compat/compat.go와 공개 문서 docs/09-compatibility.md. 두 원본은 테스트(internal/compat/compat_test.go의 `TestDocMatchesMatrix`)로 서로 일치하도록 묶여 있으며, 이 문서는 그 내용을 옮기고 시험 방식의 세부를 덧붙인 것입니다.
- 설치된 바이너리에서는 `vigilante plugins`로 같은 표를 볼 수 있습니다.

## 1.2 검증 수준의 정의

| 수준 | 뜻 | 운영 사용 시 권장 |
|---|---|---|
| **검증됨 (verified)** | 실제 시스템(장비·소프트웨어·클라우드)을 상대로 동작을 확인했고 그 대상을 기록함 | 일반 절차 |
| **실험적 (experimental)** | 자동 테스트는 있지만 모의 서버·시뮬레이터 기준 | `vigilante doctor`로 대상 점검 후 승인 모드(`rollback.mode: approve`)로 시작 |

`vigilante validate`와 서버 시작 로그는 설정에서 쓰는 실험적 플러그인을 이름으로 나열해 경고합니다(internal/compat/compat.go의 `Warning`). 표에 없는 플러그인(사내에서 추가한 플러그인 등)은 실험적으로 취급합니다.

## 1.3 요약

| 구분 | 전체 | 검증됨 | 실험적 |
|---|---|---|---|
| 프로브 | 8 | 3 (`http`, `access_log`, `log`) | 5 |
| 실행기 | 8 | 1 (`webhook`) | 7 |
| 트래픽 제어기 | 6 | 0 | 6 |
| 합계 | 22 | 4 | 18 |

> **요점:** "검증됨" 4개의 근거는 모두 CI의 E2E 데모(실제 프로세스, Linux)입니다. 상용 장비·하이퍼바이저·클라우드·로드밸런서를 상대로 한 검증은 아직 한 건도 없으며, M8 실장비 랩에서 진행합니다.

# 2. 플러그인 매트릭스

"최소 빌드" 열은 `-tags minimal`로 만든 최소 빌드 바이너리에 그 플러그인이 들어 있는지입니다. 최소 빌드에 없는 플러그인을 설정에서 쓰면 시작할 때 이름을 들어 거부합니다(docs/07-install.md "배포 형태").

## 2.1 프로브

| 플러그인 | 수준 | 현재 시험 방식 (검증 대상) | 최소 빌드 |
|---|---|---|---|
| `http` | 검증됨 | 실제 HTTP 서비스 (CI의 E2E 데모, Linux) | 포함 |
| `access_log` | 검증됨 | 실제 combined 형식 액세스 로그, 로테이션 포함 (CI의 E2E 데모) | 포함 |
| `log` | 검증됨 | 실제 애플리케이션 로그, 로테이션 포함 (CI의 E2E 데모) | 포함 |
| `tcp` | 실험적 | 로컬 리스너 대상 단위 테스트 | 포함 |
| `grpc` | 실험적 | 단위 테스트. 실제 grpc.health.v1 서버는 M8 랩 | 제외 |
| `host` | 실험적 | /proc 샘플과 모의 SSH 실행기 | 포함 |
| `docker` | 실험적 | 모의 Docker Engine API | 포함 |
| `db` | 실험적 | 단위 테스트. PostgreSQL·MySQL 서버는 M8 랩 | 포함 (MySQL 드라이버는 전체 빌드만) |

## 2.2 실행기 (롤백 전략)

| 플러그인 | 전략 | 수준 | 현재 시험 방식 (검증 대상) | 최소 빌드 |
|---|---|---|---|---|
| `webhook` | 범용 (사내 배포 시스템 호출) | 검증됨 | 실제 HTTP 배포 엔드포인트와 확인 URL (CI의 E2E 데모) | 포함 |
| `exec` | 범용 (스크립트) | 실험적 | 단위 테스트 (로컬, 모의 SSH 실행기) | 포함 |
| `symlink` | A. 디렉토리 전환 | 실험적 | 로컬 릴리스 디렉토리와 모의 systemctl | 포함 |
| `container` | B. 컨테이너 전환 | 실험적 | 모의 Docker Engine API | 포함 |
| `kvm` | D. VM 스냅샷 | 실험적 | 모의 virsh | 포함 |
| `vsphere` | D. VM 스냅샷 | 실험적 | govmomi vcsim 시뮬레이터. vCenter 7·8은 M8 랩 | 제외 |
| `nutanix` | D. VM 스냅샷 | 실험적 | 모의 Prism Element v2 API. AOS 릴리스별 경로 미확인 | 포함 |
| `openstack` | D. 인스턴스 스냅샷 | 실험적 | Keystone·Nova·Cinder·Glance 상태 있는 모의 서버(internal/executor/ostest). 사내 OpenStack은 M8 랩 | 포함 |

## 2.3 트래픽 제어기

| 플러그인 | 방식 | 수준 | 현재 시험 방식 (검증 대상) | 최소 빌드 |
|---|---|---|---|---|
| `nginx` | upstream 파일의 `down` 토글, `nginx -t` 실패 시 복원, reload | 실험적 | 생성한 upstream 파일과 모의 nginx | 포함 |
| `haproxy` | Runtime API로 서버 상태 drain·maint·ready | 실험적 | 모의 runtime API 소켓 | 포함 |
| `envoy` | 파일 기반 EDS의 `health_status: DRAINING` | 실험적 | 생성한 EDS 파일 | 포함 |
| `f5` | iControl REST로 풀 멤버 session·state 변경 | 실험적 | 모의 iControl REST. BIG-IP VE는 M8 랩 | 포함 |
| `aws_alb` | 타깃 등록 해제·등록과 상태 대기 | 실험적 | 모의 ELBv2 API. AWS 테스트 계정은 M8 랩 | 제외 |
| `octavia` | 풀 멤버 `admin_state_up` 변경, LB 상태 대기·409 재시도 | 실험적 | Octavia v2 상태 있는 모의 서버. 사내 OpenStack은 M8 랩 | 포함 |

## 2.4 최소 빌드

최소 빌드는 vSphere(govmomi), AWS ALB(AWS SDK), gRPC 프로브, MySQL 드라이버를 빼서 바이너리를 38MB에서 18MB로 줄입니다(docs/05 M6 구현 결과). CI는 매 PR마다 최소 빌드로 `go vet`과 주요 패키지 테스트를 실행합니다(.github/workflows/ci.yml `package` 작업).

# 3. 플러그인 외 연동 대상의 시험 현황

호환성 표는 플러그인만 다룹니다. 도입 검토에 필요한 나머지 연동의 시험 현황은 다음과 같습니다.

| 연동 | 현재 시험 방식 | 실제 제품 대상 확인 |
|---|---|---|
| PostgreSQL (상태 저장소) | CI에서 실제 `postgres:16` 서비스로 저장소 테스트. 저장소 단절 장애 주입 시험(롤백 중·롤백 시작 시점, internal/orchestrator/chaos_test.go)은 파일 저장소에 장애를 흉내 내는 래퍼를 씌워 실행 | PostgreSQL 16만 확인. 다른 버전은 미확인. 실제 PostgreSQL을 끊는 장애 시험은 없음 |
| OIDC IdP | 모의 IdP (internal/auth/oidctest) | 없음 (Keycloak·Azure AD·Okta 등) |
| HashiCorp Vault | 모의 Vault (internal/secrets/vaulttest) | 없음 |
| ServiceNow | 모의 ServiceNow (internal/itsm/snowtest). 일시 오류(429·5xx·연결 오류) 재시도와 인시던트 중복 방지 포함 | 없음 |
| 알림 (Slack, Teams, 이메일, PagerDuty) | 단위 테스트 (모의 HTTP 서버) | 없음 |
| SIEM (syslog) | 단위 테스트. TLS 전송(`tls://`, RFC 5425)은 시험용 TLS 수집기로 프레임 형식과 신뢰할 수 없는 인증서 거부를 확인(internal/audit/syslog_tls_test.go) | 없음 |
| CI 제품 (Jenkins, GitLab CI, GitHub Actions) | 연동 예제 제공 (examples/ci) | 예제를 각 제품에서 실행한 기록 없음 |

# 4. 실행 플랫폼

## 4.1 서버·CLI 바이너리

| OS·아키텍처 | 제공 | 확인 수준 |
|---|---|---|
| Linux amd64 | 정적 바이너리, 패키지, 이미지 | CI의 `go test -race`, E2E 데모, 패키지 설치, 이미지 실행이 모두 이 플랫폼에서 실행됨 |
| Linux arm64 | 정적 바이너리, 패키지, 이미지 | CI에서 교차 빌드만 확인 (실행 테스트 없음) |
| Linux ppc64le | 정적 바이너리, 패키지, 이미지 | CI에서 교차 빌드만 확인 (실행 테스트 없음) |
| Windows amd64 | 정적 바이너리 (CLI·CI 용도) | CI에서 교차 빌드만 확인. Windows 서비스 래퍼·설치 패키지 없음 |

모든 바이너리는 `CGO_ENABLED=0` 정적 빌드이며, Go 툴체인은 go1.27.2로 고정되어 있습니다(go.mod, ci.yml). 개발 중 Windows에서 로컬 명령의 따옴표 인자가 깨지는 버그를 찾아 고쳤지만(CHANGELOG.md "Fixed"), Windows 실행 테스트는 CI에 없습니다.

## 4.2 설치 패키지

| 패키지 | CI 확인 내용 | 확인하지 않은 것 |
|---|---|---|
| deb | Debian 12 컨테이너(amd64)에서 폐쇄망 번들의 `install.sh`로 설치, 서비스 계정 생성(`id vigilante`), 서버 systemd 유닛 파일 존재, 기본 설정 `validate`. 디렉토리·설정 파일 권한은 출력만 하고 값은 비교하지 않음 | 컨테이너에 systemd가 돌지 않아 서비스를 실제 기동하는 시험 없음, Ubuntu 등 다른 배포판, amd64 외 아키텍처 |
| rpm | Rocky Linux 9 컨테이너에서 같은 항목 | RHEL 자체, RHEL 8 계열, amd64 외 아키텍처 |

출처: .github/workflows/ci.yml의 "install the deb and the rpm" 단계. master `dd9a055`와 `537870c`에서 이 단계를 포함한 CI 전체가 통과했습니다. 패키지 메타데이터의 maintainer(noreply 주소)와 license(`Proprietary`)는 임시값입니다(packaging/nfpm.yaml).

## 4.3 컨테이너 이미지와 Kubernetes

| 항목 | 현재 확인 수준 |
|---|---|
| 컨테이너 이미지 | distroless static 기반, non-root. CI에서 linux/amd64 이미지를 빌드해 `version` 실행 확인. 멀티 아키텍처 빌드·게시는 릴리스 워크플로우에만 있고 아직 실행된 적 없음 |
| Helm 차트 | CI에서 `helm template`으로 렌더링만 확인: 기본값(단일 노드, 개발용 `auth.allowAnonymous=true`), 리플리카 3에 영구 볼륨 끔(`persistence.enabled=false`), 영구 볼륨(파일 저장소용)을 켠 채 리플리카 3을 주면 거부하는지, 인증 설정이 없으면 거부하는지, HA+서버 TLS 설정에서 전달 주소가 `https://`, 프로브가 HTTPS, `GOMEMLIMIT`이 한도의 90%로 나오는지. 차트는 PostgreSQL 사용 여부를 검사하지 않으며, `existingConfigMap`을 쓰면 설정 내용(인증·TLS)을 읽지 못함. **실제 클러스터에 설치·업그레이드한 시험은 없음** |
| 차트 인증·TLS | `config`에 인증(`auth.service_accounts`, `auth.oidc`, 값이 주어진 `server.auth_token_env`)이 없으면 렌더링 거부. 서버 TLS(`server.tls.cert_file` 또는 `tls.enabled`, 인증서는 `tls.secretName`)를 켜면 HA 전달 주소·프로브·포트 이름·Ingress 백엔드·ServiceMonitor를 https로 구성하고, 팔로워는 `server.ha.tls`의 CA와 이름으로 리더를 검증. `server.tls.client_auth: require`는 프로브가 인증서를 낼 수 없어 거부(docs/07 "Kubernetes") |
| 차트 보안 기본값 | `runAsNonRoot`, UID 65532, `readOnlyRootFilesystem`, 모든 capability 제거, `allowPrivilegeEscalation: false`, seccomp RuntimeDefault (deploy/helm/vigilante/values.yaml) |
| 차트 자원 기본값 | 요청 CPU 100m·메모리 512Mi, 메모리 한도 2Gi, `GOMEMLIMIT`은 한도의 90%(`goMemLimit`으로 변경). 부하 시험의 최대 힙(대상 2,000 × 프로브 3에 352 MiB, × 프로브 10에 1.1 GiB)을 기준으로 정한 값이며, 프로브 1천 개당 약 60 MiB에 여유를 더해 잡음(docs/07) |

## 4.4 상태 저장소

| 백엔드 | 용도 | 확인 수준 |
|---|---|---|
| 파일 (JSONL 저널) | 단일 노드, CI 단발 실행 | 단위 테스트, E2E 데모 |
| PostgreSQL | 여러 노드 공유, HA | PostgreSQL 16으로 CI 테스트. 리더 교체 후 롤백 재개 테스트 포함(docs/04 S12) |

저장소 장애 시 동작: 저장소에 쓰지 못한 기록은 메모리 대기열에 순서대로 쌓았다가 저장소가 돌아오면 같은 순서로 기록합니다(`vigilante_store_pending_writes`). 장애 중에 새로 시작하는 롤백은 `safety.rollback_lease.wait`(10초) 동안 서비스 잠금(저장소 리스)을 다시 시도한 뒤, 기본값 `on_unavailable: proceed`이면 프로세스 안 잠금만으로 진행하고 배포 이벤트·감사(`lease.unavailable`)·경고 알림을 남깁니다. 저장소가 돌아왔을 때 다른 프로세스가 리스를 잡고 있으면 `lease.conflict`로 알리며, `fail`로 두면 롤백하지 않습니다(internal/safety/safety.go `Acquire`, docs/04 S12a, 시험 `TestChaosStoreOutageDuringRollback`·`TestChaosStoreOutageLeaseFailMode`). 한계: 대기열은 메모리에만 있어 장애 중 프로세스가 죽으면 유실되고, 10만 건을 넘으면 버립니다. HA에서 장애가 `lease_ttl`보다 길면 리더가 물러나 판정이 멈추고, 다른 노드가 리더가 되면 대기열은 버려집니다. 장애 시험은 파일 저장소에 장애를 흉내 낸 것이며 실제 PostgreSQL 단절과 HA 조합은 시험하지 않았습니다.

## 4.5 대상 호스트

| 항목 | 지원 | 비고 |
|---|---|---|
| 접속 방식 | 에이전트 없이 SSH (OpenSSH), bastion 다단 경유 | 호스트 키 검증 기본 |
| `host` 프로브 | Linux `/proc` | AIX·Solaris·HP-UX는 `exec` 기반 프로브 추가 필요 |
| `symlink` 실행기 | systemd·sysv·none | AIX·Solaris용 비원자 전환 옵션(`atomic: false`)이 있으나 시험 기록 없음 |
| 선택형 에이전트 | Linux | systemd 유닛 제공 |

# 5. 검증 계획 (M8 실장비 랩)

## 5.1 검증 방법

랩에서는 조합(서비스 하나 = 실행기 + 트래픽 제어기 + 프로브)마다 `vigilante lab run`으로 실제 시나리오를 돌립니다. 운영 경로(`vigilante watch`와 같은 엔진)를 그대로 쓰므로, 통과하면 그 조합이 실제로 동작한다는 뜻입니다(docs/09-compatibility.md "검증 기록 방법").

```bash
vigilante lab run -c lab.yaml --service order-os --label "사내 OpenStack 2024.1 (Octavia)" \
  --inject "ansible-playbook deploy-bad.yml" --reset "ansible-playbook deploy-good.yml" --repeat 3
vigilante lab summary lab-results.jsonl
```

한 번의 실행은 다음 순서입니다.

1. `vigilante doctor` 통과 확인 (실패 항목이 있으면 시작하지 않음. `--skip-doctor`로 생략 가능)
2. 체크포인트(스냅샷) 생성
3. 로드밸런서 풀 상태 기록
4. 불량 버전 배포 (`--inject`)
5. 관측과 탐지, 그리고 서비스의 롤백 계획 실행(드레인, 복원과 버전 확인, 트래픽 복귀). 결과 파일에는 "observe and roll back" 한 단계로 기록
6. 이전에 트래픽을 받던 멤버가 모두 다시 켜졌는지 확인 (LB가 성공이라고 답해도 멤버가 빠져 있으면 실패)
7. `--reset`을 준 경우 복구 명령 실행

결과는 실행마다 한 줄씩 `lab-results.jsonl`에 쌓이고(단계별 성공·소요 시간, 탐지·롤백 시간, 풀 상태 전후), `vigilante lab summary`가 장비·조합별 실행 수, 통과 수, 탐지·롤백 시간 중앙값, 발견한 문제를 표로 만듭니다. 승인 모드 서비스는 랩이 바로 승인하며, 랩 실행은 파일럿 판정 품질 보고서에서 제외됩니다. CI의 E2E 데모도 같은 명령을 실제 프로세스에 돌립니다.

## 5.2 "검증됨" 전환 기준

플러그인이 랩에서 **3회 연속 통과**하면 다음을 함께 고칩니다.

1. internal/compat/compat.go의 항목: 수준을 `Verified`로, 검증 대상에 제품과 버전을 기록 (예: `F5 BIG-IP VE 17.1, iControl REST`)
2. docs/09-compatibility.md의 표: 상태와 검증 대상, 근거로 `vigilante lab summary` 표 첨부
3. 이 문서(VGL-BD-03)의 해당 행과 개정 이력

## 5.3 랩 구성 대상

로드맵 M8-1이 정한 랩 구성 대상입니다(docs/05-roadmap.md). 장비 확보가 선행 조건이며, 2026-10-11 현재 확보되지 않았습니다.

- 사내 OpenStack(또는 DevStack): Nova·Cinder·Glance·Octavia
- F5 BIG-IP (VE 평가판)
- vCenter 7·8
- Nutanix CE: Prism v2 경로를 실제로 확인하고, 필요하면 v3·v4 API로 전환
- AWS 테스트 계정 (ALB·NLB)
- HAProxy 2.x, Nginx, Envoy
- Docker·Podman 버전별

## 5.4 플러그인별 검증 계획

"랩 대상" 열이 "명시 없음"인 플러그인은 로드맵의 랩 구성 목록에 별도 대상이 적혀 있지 않은 것입니다. 이 플러그인들은 Linux 호스트나 KVM 하이퍼바이저만 있으면 시험할 수 있지만, 랩 계획에 대상과 일정을 추가해야 합니다.

| 플러그인 | 랩 대상 | 확인할 핵심 동작 | 알려진 위험·미확인 사항 |
|---|---|---|---|
| `openstack` | 사내 OpenStack 또는 DevStack | 볼륨 부팅: Cinder 스냅샷과 `revert_to_snapshot`(볼륨 API 3.40 이상). 이미지 부팅: Nova 스냅샷과 `rebuild` | 백엔드·릴리스에 따라 사용 중 볼륨의 revert가 거부될 수 있음. 루트 볼륨 교체 대체 경로는 미구현 |
| `octavia` | 사내 OpenStack 또는 DevStack | 멤버 드레인·복귀, `PENDING_UPDATE` 대기, 409 재시도, 멤버 ONLINE 대기 | neutron-lbaas 미지원 |
| `f5` | BIG-IP VE 평가판 | 풀 멤버 session·state 변경과 복귀 | 파티션 Operator 역할로 충분한지 확인 필요 (docs/10) |
| `vsphere` | vCenter 7·8 | 스냅샷 생성·복원·삭제, 전원 상태 | 폴더 한정 사용자 역할로 충분한지 확인 필요 |
| `nutanix` | Nutanix CE | Prism Element v2 경로의 스냅샷 생성·복원 | AOS 릴리스별 API 경로 미확인. v3·v4 전환 가능성 |
| `aws_alb` | AWS 테스트 계정 | 타깃 등록 해제(draining 완료 대기)·등록(healthy 대기) | 최소 IAM 정책(docs/10 예시)의 충분성 |
| `haproxy` | HAProxy 2.x | Runtime API로 drain·maint·ready 전환 | — |
| `nginx` | Nginx | upstream 파일 토글, `nginx -t` 실패 시 복원, reload, HA 쌍 적용 | — |
| `envoy` | Envoy | 파일 기반 EDS 갱신과 원자적 교체 | — |
| `container` | Docker·Podman 버전별 | 이전 이미지로 전환, 실패 시 원래 컨테이너 복구 | Docker 소켓 접근은 root와 같음 (rootless Podman 권장) |
| `grpc` | 명시 없음 (실제 grpc.health.v1 서버) | 표준 헬스 체크 응답 판정 | — |
| `db` | 명시 없음 (PostgreSQL·MySQL 서버) | 쿼리 지연, 풀 확보 점검 | MySQL 드라이버는 전체 빌드만 |
| `docker` | 명시 없음 | 재시작·OOM·종료 이벤트 수집, SSH 터널 경유 | — |
| `host` | 명시 없음 | 실제 Linux 호스트의 `/proc` 수집 | Linux 전용 |
| `tcp` | 명시 없음 | 연결 성공·지연 | — |
| `symlink` | 명시 없음 | 원자적 링크 전환, 서비스 재시작, `sudo_scope: changes` 규칙 | sudoers 와일드카드(`*`) 범위 주의 (docs/10) |
| `exec` | 명시 없음 | 사용자 정의 prepare·rollback·verify 명령 | 명령 내용은 고객 책임 |
| `kvm` | 명시 없음 | `virsh snapshot-create-as --atomic`, `snapshot-revert` | libvirt 그룹은 그 호스트의 모든 VM 제어 가능 |

# 6. 출시 게이트

## 6.1 조건

상용 1차 출시는 다음을 모두 충족해야 합니다(docs/05-roadmap.md "상용 1차 범위", docs/11-pilot.md "출시 게이트").

| 조건 | 기준 |
|---|---|
| 1차 범위 실행기 검증 | `symlink`, `container`, `openstack`, `exec`, `webhook` 모두 "검증됨" |
| 1차 범위 트래픽 제어기 검증 | `nginx`, `haproxy`, `f5`, `octavia` 모두 "검증됨" |
| 그 밖의 플러그인 | `vsphere`·`nutanix`·`kvm`은 M8 랩을 통과한 것만 정식, 나머지는 "실험적"으로 출시. `envoy`·`aws_alb`는 M8 결과에 따름 |
| 파일럿 판정 품질 | 3개월, 배포 30건 이상, 오탐 0건, 미탐 0건, 보류(HOLD) 비율 10% 이하, FAIL·HOLD 배포 전부 평가 |
| 실제 롤백 | 파일럿에서 드라이런이 아닌 실제 롤백(승인 모드 포함) 1건 이상 성공 |
| 공급망 | 서명·SBOM·폐쇄망 번들 완료 |

파일럿 조건 중 배포 수, 오탐·미탐, 보류 비율, 실제 롤백, FAIL·HOLD 평가 완료는 `vigilante pilot report`가 자동으로 판정하며, 미달이면 종료 코드 4로 끝납니다. 3개월 기간은 자동 판정 대상이 아니므로 `--since`·`--until`로 기간을 지정해 확인합니다(internal/pilot/pilot.go).

## 6.2 현재 충족 현황 (2026-10-11)

| 조건 | 현재 | 남은 일 |
|---|---|---|
| 1차 범위 실행기 검증 | `webhook`만 검증됨. `symlink`, `container`, `openstack`, `exec`는 실험적 | 랩에서 4개 검증 |
| 1차 범위 트래픽 제어기 검증 | 4개 모두 실험적 | 랩 장비(OpenStack, BIG-IP VE, HAProxy, Nginx) 확보 후 검증 |
| 파일럿 판정 품질 | 시작 전. 측정 도구(`feedback`, `pilot report`)는 구현됨 | 대상 서비스 확보, 3개월 운영 |
| 실제 롤백 1건 | 없음 | 파일럿 2~3개월차 승인 모드 |
| 공급망 | 빌드·SBOM·폐쇄망 번들·서명·검증 과정은 구현되어 CI가 PR마다 임시 키로 확인 | 릴리스 서명 키 생성과 `packaging/cosign.pub` 커밋, 첫 릴리스 게시 |

> **결론:** 2026-10-11 현재 출시 게이트를 충족하지 않습니다. 기능 구현과 CI는 갖춰졌고, 남은 조건은 모두 실장비·실서비스가 필요한 일입니다. 출시 게이트에 들어 있지는 않지만, 부하·장애 시험(M5-4)은 master에 병합되었습니다(PR #12). 관측 쪽 과부하를 대상 장애로 오판하던 문제(바쁜 개발 PC에서 정상 배포 90건 롤백)는 관측 장치 보호(`safety.observer_guard`, 기본 켜짐, PR #13. PR #14부터 서버가 직접 재는 프로브의 지표만 보류하고 액세스 로그 등 대상이 보고한 지표는 그대로 판정)로 대응했으나, 과부하 신호를 주입한 시험으로만 확인했고 실제 과부하 조건에서 다시 측정하지는 않았습니다. 부하 시험은 HTTP 시뮬레이터 기준이라 SSH 부하는 재지 않았습니다.

# 7. 이 문서의 갱신

- 랩에서 플러그인이 "검증됨"으로 바뀌면 5.2의 절차대로 코드·공개 문서와 함께 이 문서를 고치고 개정 이력에 남깁니다.
- 파일럿 결과(판정 품질 보고서)가 나오면 6.2 표를 갱신합니다.
- 이 문서와 바이너리의 표가 다르면 바이너리의 표(`vigilante plugins`)가 우선합니다.
