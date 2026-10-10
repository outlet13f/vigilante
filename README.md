# Vigilante — Unified Rollback Orchestrator

이기종 하이브리드 인프라(베어메탈 · OpenStack/vSphere/Nutanix/KVM · EC2/Azure VM · Docker/Podman · Nginx/HAProxy/Envoy/F5/ALB/Octavia)의 배포를 **외부 APM 없이 자체 측정·판정하고, 실패 시 즉시 자동 롤백**하는 단일 Go 바이너리.

```
배포 ─▶ vigilante watch ─▶ 수집(HTTP/gRPC/TCP·/proc·docker.sock·로그·5xx·DB 풀)
                         ─▶ 판정(복합 규칙·베이스라인·연속 실패·대조군·관측 쿼럼)
                         ─▶ 롤백(드레인 → symlink/컨테이너/스냅샷 → 검증 → 복귀)
                         ─▶ 안전장치(서킷 브레이커·blast radius·플래핑·크래시 재개)
```

## 문서

| 문서 | 내용 |
|---|---|
| [docs/01-architecture.md](docs/01-architecture.md) | 하이브리드 아키텍처 결정, 구성도, 데이터 흐름, 상태 머신, 운영 토폴로지 |
| [docs/02-config-spec.md](docs/02-config-spec.md) | `vigilante.yaml` 전체 명세 (프로브 메트릭 카탈로그, 규칙 문법, 실행기/트래픽) |
| [docs/03-engine-design.md](docs/03-engine-design.md) | 비동기 수집, 3값 규칙 평가, 실행기 인터페이스, 확장·테스트 |
| [docs/04-safety-circuit-breaker.md](docs/04-safety-circuit-breaker.md) | 롤백 실패·비상 정지 시나리오 18종, 서킷 상태도, 런북 |
| [docs/05-roadmap.md](docs/05-roadmap.md) | 엔터프라이즈 제품화 로드맵 M0~M8, 상용 1차 범위, 실행 순서, 진행 현황, 결정 필요 사항 |
| [docs/06-api.md](docs/06-api.md) · [api/openapi.yaml](api/openapi.yaml) | 오픈 API v2: 공통 규약, 리소스, 오류 코드, v1 대응 |
| [docs/07-install.md](docs/07-install.md) | 설치: 패키지·HA·폐쇄망 번들·Kubernetes(Helm)·에이전트, 서명 확인 |
| [docs/08-upgrade.md](docs/08-upgrade.md) · [CHANGELOG.md](CHANGELOG.md) | 버전 정책(SemVer)과 호환성 약속, 순차 업그레이드, 되돌리기 |
| [docs/09-compatibility.md](docs/09-compatibility.md) | 호환성 매트릭스: 플러그인별 검증 수준(검증됨·실험적) |
| [docs/10-security.md](docs/10-security.md) | 보안 가이드: 통신 경로, 최소 권한(sudoers·외부 시스템 역할), 비밀값, 공급망 |
| [docs/11-pilot.md](docs/11-pilot.md) | 파일럿 운영: 판정 평가(오탐·미탐) 기록, 판정 품질 보고서와 출시 게이트 |
| [examples/config/vigilante.yaml](examples/config/vigilante.yaml) | 4개 서비스 × 전 인프라 유형(OpenStack 포함) 참조 설정 |
| [examples/ci/](examples/ci/) | Jenkins / GitLab CI / GitHub Actions 연동 |

## 빠른 시작

```bash
go build -o bin/vigilante ./cmd/vigilante          # CGO 불필요, 정적 바이너리 (-tags minimal: 최소 빌드)
bin/vigilante plugins                              # 플러그인과 검증 수준 (docs/09)
bin/vigilante validate -c examples/config/vigilante.yaml
bin/vigilante presets                              # 내장 규칙 프리셋과 파라미터
bin/vigilante doctor -c vigilante.yaml             # 배포 전 읽기 전용 점검: 접속·sudo·로그 형식·이전 릴리스·LB 풀

# 로컬 E2E 데모 (Linux/macOS/Git Bash): 불량 배포 자동 롤백 → 롤백 경로 고장 → 서킷 OPEN → 리셋 → 정상 배포
./examples/demo/run-demo.sh
```

### CI 파이프라인에서 (단발 실행)

```bash
# 최초 1회: 지금 운영 중인 버전을 기준(known-good)으로 등록
vigilante mark-good -c vigilante.yaml --service order-api --version v41

vigilante prepare  -c vigilante.yaml --service order-api                       # 체크포인트/스냅샷
vigilante baseline -c vigilante.yaml --service order-api --out baseline.json   # 배포 전 기준점
# ... canary 배포 ...
vigilante watch    -c vigilante.yaml --service order-api --phase canary --baseline baseline.json
# exit 0 PASS · 2 롤백 완료 · 3 롤백 실패/서킷 OPEN/승인 대기 · 4 HOLD · 1 오류
```

`--id`와 `--version`은 CI 실행 정보에서 자동으로 채웁니다(Jenkins `BUILD_TAG`·`GIT_COMMIT`, GitLab `CI_PIPELINE_ID`·`CI_COMMIT_TAG`/`CI_COMMIT_SHORT_SHA`, GitHub `GITHUB_RUN_ID`·`GITHUB_SHA`, 또는 `VIGILANTE_DEPLOYMENT_ID`·`VIGILANTE_VERSION`, 없으면 `git describe`). `--previous`는 저널에 기록된 그 서비스의 마지막 성공 배포 버전을 씁니다. full 단계를 통과한 배포가 다음 배포의 `--previous`가 됩니다. 직접 지정한 플래그가 항상 우선하며, 자동으로 채운 값과 출처는 출력과 배포 기록에 남습니다.

### 중앙 서버 / 에이전트

```bash
vigilante token create --name ci-order --role deployer --scope service=order-api --expires 2027-06-30
VIGILANTE_TOKEN=vgl_... vigilante whoami --server https://vigilante:8088
vigilante server -c vigilante.yaml                                      # REST + 웹훅 + 크래시 재개 (auth·HA 설정 시 적용)
# 웹 콘솔: https://vigilante:8088/console/ (SSO는 docs/02 console, 없으면 API 토큰으로 로그인)
vigilante watch --server https://vigilante:8088 --service ... --phase canary
vigilante agent -c vigilante.yaml --target order-bm-01 --server https://vigilante:8088
vigilante circuit -c vigilante.yaml status|reset|trip [--ticket CHG-123]
vigilante audit verify -c vigilante.yaml                               # 감사 기록 변조 검사 (해시 체인)
# 서버 관측: GET /healthz(생존) /readyz(준비) /metrics(Prometheus), VIGILANTE_LOG_FORMAT=json
# 비밀값: credentials의 *_ref = "vault:secret/prod/f5#password" | env:NAME | file:/path
#         ssh_ca: {mount, role} 이면 Vault SSH CA 단기 인증서로 접속 (docs/02 secrets)
vigilante audit query  -c vigilante.yaml --action denied --since 2026-10-01
vigilante rollback -c vigilante.yaml --id $BUILD [--executor vm-snapshot] [--approve]
vigilante store status -c vigilante.yaml                               # PostgreSQL 스키마 상태 (store migrate [--down-to N])
vigilante support-bundle -c vigilante.yaml --server https://vigilante:8088   # 진단 zip (비밀값 제거)
vigilante feedback --id $BUILD --outcome false_positive --note "..."       # 판정 평가 (오탐·미탐 측정)
vigilante pilot report -c vigilante.yaml --since 2026-11-01            # 판정 품질 보고서와 출시 게이트 (미달 시 종료 코드 4)
```

설치 패키지(rpm·deb), 컨테이너 이미지, Helm 차트, 폐쇄망 번들은 릴리스마다 나옵니다. [docs/07-install.md](docs/07-install.md)를 보십시오.

## 검증 현황

- CI(모든 PR): gofmt·vet, `go test -race`(PostgreSQL 16 포함), 최소 빌드 테스트, OpenAPI 린트·하위호환 검사, govulncheck, 릴리스 패키징 전 과정(서명·Debian/Rocky 설치), E2E 데모. 외부 장비 API는 시뮬레이터·mock 기준 (vSphere는 govmomi `vcsim`)
- `examples/demo/run-demo.sh` — 실제 프로세스로 5개 시나리오 통과 (불량 v2 배포 후 약 4초 만에 탐지·롤백)
- 정적 크로스 빌드: linux/amd64, linux/arm64, linux/ppc64le, windows/amd64 (`CGO_ENABLED=0`)

## 알려진 한계

- 실제 F5 / AWS / Nutanix / vCenter 장비와는 연동 테스트하지 않았습니다(시뮬레이터·mock 기준). 플러그인별 수준은 `vigilante plugins`와 [docs/09-compatibility.md](docs/09-compatibility.md). 특히 Nutanix는 Prism Element v2 API 경로 기준이므로 AOS 버전별 확인이 필요합니다. 실장비 검증과 파일럿은 로드맵 M8에서 상용 1차 출시 전에 수행합니다.
- OpenStack(실행기 `openstack`, 트래픽 제어기 `octavia`)은 모의 서버로만 검증했습니다. 특히 Cinder `revert_to_snapshot`은 스토리지 백엔드와 릴리스에 따라 사용 중 볼륨을 거부할 수 있어, M8 실장비 랩에서 동작을 확정합니다.
- 판정 품질(오탐·미탐 비율)은 실제 서비스에서 측정한 적이 없습니다. 프리셋 임계치는 추정값이며, M8 파일럿에서 보정합니다.
- `host` 프로브는 Linux `/proc` 전용입니다. AIX/Solaris/HP-UX는 `exec` 기반 프로브 추가가 필요합니다.
- Azure Load Balancer / Application Gateway, Citrix ADC 등은 `TrafficController` 구현 추가가 필요합니다 (현재는 `exec`/`webhook`으로 우회).
- `connection.sudo: true`는 모든 명령을 `sudo sh -c`로 실행해 사실상 root 권한이 필요합니다. 명령별로 좁히는 방법은 [docs/10-security.md](docs/10-security.md), 근본 해결은 로드맵 M5-3입니다.
- CI(`.github/workflows/ci.yml`)가 PR마다 `go test -race`, 명세 린트, API 하위호환 검사, govulncheck, E2E 데모를 실행합니다.
