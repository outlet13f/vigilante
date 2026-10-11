---
title: 관리자 매뉴얼
doc_id: VGL-OP-01
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

이 문서는 Vigilante(통합 롤백 오케스트레이터)를 설치하고 설정하며 운영 환경에서 유지하는 시스템 관리자를 위한 매뉴얼입니다. 설치, 서명 확인, 설정 파일 작성, 인증과 권한, 웹 콘솔 SSO, TLS, 고가용성(HA), 최소 권한, 외부 시스템 연동, 모니터링, 감사, 백업과 복구, 업그레이드, 지원 번들 작성까지의 작업 절차를 다룹니다.

배포 담당자와 운영자의 일상 업무(콘솔 사용, 승인, 판정 평가)는 운영자 매뉴얼(VGL-OP-02)을, 장애 상황별 대응 절차는 장애 대응 런북(VGL-OP-03)을 참고하십시오.

## 1.2 적용 범위

이 문서는 저장소에 구현된 제품(1.0.0 출시 후보 내용)을 기준으로 작성했습니다. 기준 코드는 master 브랜치의 병합 커밋 `537870c`입니다. 여기에는 M5-4(저장소 장애 시 쓰기 대기열, 카오스 시험, 부하 하네스, PR #12)와 검토 결함 수정(관측 장치 과부하 판별, 저장소 장애 중 롤백 시작, 로컬 CLI 제한, 감사 체인 키, SIEM TLS, Helm·HA TLS, PR #13), 관측 장치 과부하 판별의 보류 대상을 서버가 직접 재는 프로브로 좁힌 수정(PR #14)이 포함됩니다. 아직 정식 릴리스는 나오지 않았습니다. 바이너리의 `vigilante version`은 개발 빌드에서 `0.1.0-dev`를 표시하며, 릴리스 빌드는 빌드 시 버전이 주입됩니다.

> 표기 규칙: 명령과 설정 키는 `고정폭`으로 적습니다. `X.Y.Z`는 릴리스 버전, `<org>`는 컨테이너 레지스트리 조직명을 뜻합니다. "확인 필요"로 표시한 항목은 저장소에서 근거를 확정하지 못한 내용입니다.

## 1.3 제품 구성 요약

Vigilante는 단일 정적 Go 바이너리 `vigilante` 하나로 다음 역할을 모두 수행합니다.

| 실행 형태 | 명령 | 용도 |
|---|---|---|
| CI 게이트(단발 실행) | `vigilante watch ...` | 파이프라인 러너가 직접 관측·판정·롤백. 서버 불필요 |
| 중앙 서버 | `vigilante server -c FILE` | REST API(v1·v2), 웹 콘솔, 웹훅, 크래시 재개, HA |
| 서버 위임 CLI | `vigilante watch --server URL ...` | CI 러너가 대상망에 접근할 수 없을 때 서버에 관측을 맡김 |
| 에이전트 | `vigilante agent -c FILE --target NAME --server URL` | 대상 호스트에서 로컬 수집, 오케스트레이터 단절 시 자율 판정 |

모든 판정과 조치는 상태 저장소(파일 저널 또는 PostgreSQL)에 해시 체인으로 기록되며, 이 기록이 곧 감사 기록입니다.

---

# 2. 시스템 요구 사항

## 2.1 플랫폼

| 항목 | 지원 내용 |
|---|---|
| 서버·CLI 바이너리 | Linux amd64·arm64·ppc64le, Windows amd64 (정적 바이너리, CGO 없음) |
| 패키지 | rpm, deb, systemd 유닛 포함. CI가 폐쇄망 번들의 `install.sh`로 `rockylinux:9`·`debian:12` 컨테이너(amd64)에 설치해 계정 생성, 유닛 파일 존재, 기본 설정 `validate`를 확인(systemd를 띄운 실제 기동 시험은 아님). RHEL 자체, 다른 배포판, amd64 외 아키텍처는 미확인 |
| 컨테이너 | distroless static 이미지, non-root(UID 65532), 읽기 전용 루트 파일시스템. 릴리스 워크플로우는 linux amd64·arm64·ppc64le로 빌드하도록 되어 있으나 아직 실행된 적이 없으며, CI는 linux/amd64 이미지만 빌드·실행 확인 |
| Kubernetes | Helm 차트 `vigilante` |
| 상태 저장소 | 파일(단일 노드), PostgreSQL 16(CI에서 확인) |
| 대상 호스트 | 에이전트 없이 OpenSSH로 접근. `host` 프로브는 Linux `/proc` 전용. 에이전트는 Linux |

> PostgreSQL 16 이외 버전의 동작은 저장소에서 확인되지 않았습니다(확인 필요).

## 2.2 자원

Helm 차트 기본값은 요청 CPU `100m`, 메모리 `512Mi`, 메모리 상한 `2Gi`이며, 컨테이너에 `GOMEMLIMIT`을 메모리 상한의 90%로 넣습니다(`goMemLimit`으로 값 지정, `off`면 넣지 않음). 파일 저장소용 영구 볼륨은 `2Gi`입니다. 공식 권장 사양표는 없으며, 현재 근거는 M5-4 부하 시험(PR #12) 결과뿐입니다. 서버 한 대가 시뮬레이터의 HTTP 대상을 관측한 결과(GitHub Actions ubuntu-latest, 2026-10-11)는 다음과 같습니다.

| 규모 | 오판 | 판정 지연 p99 (목표 12초) | 프로브 요청/초 | 최대 힙 | 평균 CPU |
|---|---|---|---|---|---|
| 대상 2,000 × 프로브 3, 동시 배포 100 | 0 | 5.5초 | 2,804 | 352 MiB | 0.45코어 |
| 대상 2,000 × 프로브 10, 동시 배포 100 | 0 | 5.7초 | 9,095 | 1.1 GiB | 0.72코어 |

- 메모리는 프로브 1천 개당 약 60 MiB(프로브 2만 개에 힙 약 1.1 GiB)를 기준으로 여유 있게 잡으십시오. Helm 기본값(상한 `2Gi`)은 프로브 약 2만 개까지를 기준으로 합니다. 그보다 크면 `resources.limits.memory`(와 `requests`)를 올리십시오.
- 이 시험은 HTTP 시뮬레이터 대상이라 SSH(sshd) 부하는 재지 않았습니다. 디스크 사양 기준은 없습니다.
- **관측 장치 과부하:** 다른 작업으로 바쁜 개발용 PC에서 프로브 10개 규모를 돌리자 관측 쪽 과부하로 프로브가 시간 초과로 실패했고, 이를 대상 장애로 판정해 **정상 배포 90건을 롤백**했습니다. PR #13부터는 `safety.observer_guard`(4.11절, 기본 켬)가 서버 자신의 과부하를 감지하면 서버가 직접 재는 프로브의 실패 기반 위반을 롤백 대신 보류(HOLD)합니다. 다만 보류된 배포는 사람이 판단해야 하므로, 서버에 CPU·메모리 여유를 두고 다른 부하와 함께 두지 마십시오. 파일럿 환경에서 실제 규모로 다시 측정하기를 권장합니다.

## 2.3 빌드 종류

| 빌드 | 파일 이름 예 | 내용 |
|---|---|---|
| 전체 빌드 | `vigilante_X.Y.Z_linux_amd64` | 모든 플러그인 |
| 최소 빌드 | `vigilante_X.Y.Z_linux_amd64-minimal` | vSphere·AWS ALB·gRPC 프로브·MySQL 드라이버 제외. 이를 쓰는 설정은 시작할 때 거부 |

최소 빌드에서 빠진 플러그인을 설정에 쓰면 다음과 같은 오류로 시작을 거부합니다.

```
this is the minimal build of vigilante, which does not include: executors.pay-vm-snapshot: type vsphere.
Install the full build (vigilante_<version>_<os>_<arch>, without -minimal)
```

## 2.4 네트워크

| 방향 | 대상 | 포트(기본) | 보호 수단 |
|---|---|---|---|
| 들어옴 | API·웹 콘솔·에이전트 push·CI | 8088 | `server.tls` 또는 TLS 프록시, 토큰·OIDC 인증, 호출 한도 |
| 들어옴 | GitHub·GitLab·Jenkins 웹훅 | 8088 | HMAC 서명·토큰 검증 |
| 나감 | 대상 호스트 SSH | 22 | 키 또는 Vault SSH CA 단기 인증서, 호스트 키 검증 |
| 나감 | LB·하이퍼바이저·클라우드 API | 443 | 전용 계정·최소 역할, 인증서 검증 |
| 나감 | Vault | 8200 | AppRole·Kubernetes 인증, 읽기 전용 정책 |
| 나감 | PostgreSQL | 5432 | 전용 계정, `sslmode=verify-full` 권장 |
| 나감 | SIEM(syslog) | 6514 | `audit.syslog.address: tls://`(RFC 5425, 수집기 인증서 검증, 선택적 클라이언트 인증서). `tcp`·`udp`(514)는 평문 |
| 나감 | IdP(OIDC), ServiceNow, SMTP, Teams·Slack·PagerDuty, 구독 웹훅 | 443·587 | 비밀값은 `*_ref`, 웹훅 수신 호스트는 `api.webhook_allowed_hosts`로 제한 |

---

# 3. 설치

## 3.1 배포물 구성

릴리스(`vX.Y.Z`)마다 다음 파일이 제공됩니다.

| 파일 | 내용 |
|---|---|
| `vigilante_X.Y.Z_linux_{amd64,arm64,ppc64le}`, `vigilante_X.Y.Z_windows_amd64.exe` | 정적 바이너리(전체 빌드) |
| `..._linux_amd64-minimal` 등 | 최소 빌드 바이너리 |
| `vigilante_X.Y.Z_{amd64,arm64,ppc64el}.deb`, `vigilante-X.Y.Z-1.{x86_64,aarch64,ppc64le}.rpm` | 패키지 |
| `vigilante-X.Y.Z.tgz` | Helm 차트 |
| `ghcr.io/<org>/vigilante:X.Y.Z` | 컨테이너 이미지(서명됨) |
| `vigilante_X.Y.Z_airgap_linux_<arch>.tar.gz` | 폐쇄망 번들(바이너리·패키지·이미지 아카이브·차트·SBOM·문서·설치 스크립트) |
| `*.cdx.json` | 바이너리별 SBOM(CycloneDX 1.6) |
| `SHA256SUMS`, `SHA256SUMS.sig`, `cosign.pub` | 체크섬과 서명 |

> Helm 차트의 기본 이미지 저장소는 `ghcr.io/outlet13f/vigilante`입니다. 사내 레지스트리를 쓰면 `image.repository`를 바꾸십시오.

## 3.2 서명 확인

모든 배포물은 `SHA256SUMS`에 체크섬이 있고, `SHA256SUMS`는 릴리스 키로 서명되어 있습니다. 공개 키는 저장소의 `packaging/cosign.pub` 또는 사내에 별도로 배포한 사본을 사용하십시오. 릴리스에 함께 올라간 `cosign.pub`은 편의용이며, 릴리스가 위조되었다면 키도 위조되었을 수 있습니다.

> 현재 저장소에는 `packaging/cosign.pub`이 아직 커밋되어 있지 않습니다. 릴리스 관리자가 첫 릴리스 전에 키를 만들어 커밋하는 절차가 문서화되어 있습니다(확인 필요: 첫 릴리스 시점의 키 배포 경로).

openssl만으로 확인하는 절차(폐쇄망 서버에서도 가능)는 다음과 같습니다.

1. 서명 파일을 DER 형식으로 변환합니다.
2. 신뢰하는 공개 키로 `SHA256SUMS`의 서명을 확인합니다. `Verified OK`가 나와야 합니다.
3. 내려받은 파일의 체크섬을 확인합니다.

```bash
openssl base64 -d -A -in SHA256SUMS.sig -out SHA256SUMS.der
openssl dgst -sha256 -verify cosign.pub -signature SHA256SUMS.der SHA256SUMS   # Verified OK
sha256sum -c --ignore-missing SHA256SUMS
```

cosign이 있으면 다음 명령도 사용할 수 있습니다. 서명은 공개 투명성 로그(Rekor)에 올리지 않으므로 `--insecure-ignore-tlog=true`가 필요합니다.

```bash
cosign verify-blob --key cosign.pub --signature SHA256SUMS.sig --insecure-ignore-tlog=true SHA256SUMS
cosign verify --key cosign.pub --insecure-ignore-tlog=true ghcr.io/<org>/vigilante:X.Y.Z
```

## 3.3 단일 서버 설치 (rpm·deb)

패키지는 `/usr/bin/vigilante`, systemd 유닛 2개(`vigilante-server`, `vigilante-agent`), `/etc/vigilante/` 설정, 서비스 계정 `vigilante`, 상태 디렉토리 `/var/lib/vigilante`를 설치합니다. 유닛은 설치만 되고 활성화되지 않습니다.

1. 패키지를 설치합니다.
   ```bash
   dnf install ./vigilante-X.Y.Z-1.x86_64.rpm      # 또는 apt install ./vigilante_X.Y.Z_amd64.deb
   ```
2. 설정 파일 `/etc/vigilante/vigilante.yaml`을 작성합니다. 전체 예시는 `/usr/share/doc/vigilante/examples/vigilante.yaml`에 있습니다(4장 참고).
3. `*_env`로 참조하는 비밀값(예: `VAULT_ROLE_ID`, `VAULT_SECRET_ID`, DSN)을 `/etc/vigilante/vigilante.env`에 둡니다. 이 파일은 패키지에 포함되지 않으므로 직접 만들고 권한을 `0640 root:vigilante`로 둡니다.
   ```bash
   install -m 0640 -o root -g vigilante /dev/null /etc/vigilante/vigilante.env
   vi /etc/vigilante/vigilante.env
   ```
4. 설정을 검증하고 대상 시스템을 읽기 전용으로 점검합니다.
   ```bash
   vigilante validate -c /etc/vigilante/vigilante.yaml
   vigilante doctor   -c /etc/vigilante/vigilante.yaml
   ```
5. 서버를 시작하고 준비 상태를 확인합니다.
   ```bash
   systemctl enable --now vigilante-server
   curl -fsS http://127.0.0.1:8088/readyz
   ```

설치 시 유의 사항은 다음과 같습니다.

- 패키지 기본 설정은 `listen: "127.0.0.1:8088"`입니다. `auth`를 설정하기 전에는 모든 호출이 익명 admin으로 처리되기 때문입니다. 인증을 설정한 뒤 `listen: ":8088"`로 바꾸고 `server.tls`를 켜거나 TLS 프록시 뒤에 두십시오.
- 상태(저널·감사 기록)는 `/var/lib/vigilante`에 남으며, 패키지를 제거해도 지우지 않습니다.
- 설정을 바꾼 뒤에는 `vigilante validate`로 확인하고 `systemctl restart vigilante-server`로 다시 읽힙니다. 실행 중 설정을 다시 읽는 기능(SIGHUP 등)은 없습니다.
- 서버 유닛은 `NoNewPrivileges`, `ProtectSystem=strict`로 실행하며 쓰기는 `/var/lib/vigilante`만 허용합니다. `connection.type: local`로 서버 호스트 자체를 바꾸는 경우에는 그 경로를 `ReadWritePaths`에 추가해야 합니다.

## 3.4 폐쇄망 번들 설치

인터넷이 되는 곳에서 번들과 `SHA256SUMS`·`SHA256SUMS.sig`를 내려받아 3.2절대로 서명을 확인한 뒤 반입합니다.

1. 번들을 풀고 디렉토리로 이동합니다.
   ```bash
   tar xzf vigilante_X.Y.Z_airgap_linux_amd64.tar.gz && cd vigilante_X.Y.Z_airgap_linux_amd64
   ```
2. 번들 안 모든 파일의 체크섬을 확인합니다.
   ```bash
   ./install.sh --verify
   ```
3. root로 설치합니다. rpm 또는 deb를 자동으로 고릅니다.
   ```bash
   ./install.sh              # 패키지(전체 빌드) 설치
   ./install.sh --minimal    # 또는 최소 빌드 바이너리만 /usr/bin/vigilante로 (유닛·설정 없음)
   ```
4. 이후 절차는 3.3절 2단계부터 같습니다.

Kubernetes에서 쓸 이미지는 사내 레지스트리로 옮깁니다.

```bash
docker load -i image/vigilante_X.Y.Z_linux_amd64.tar
docker tag ghcr.io/<org>/vigilante:X.Y.Z registry.internal/vigilante:X.Y.Z
docker push registry.internal/vigilante:X.Y.Z
# containerd만 있는 노드: ctr -n k8s.io images import image/vigilante_X.Y.Z_linux_amd64.tar
```

## 3.5 Kubernetes (Helm) 설치

1. 값 파일(`values.yaml`)을 준비합니다. 설정 본문은 `config`에 YAML 문자열로 넣거나, 직접 관리하는 ConfigMap 이름을 `existingConfigMap`에 지정합니다(키 `vigilante.yaml`). `config`에는 인증 설정(`auth.service_accounts`, `auth.oidc`, 또는 시크릿을 `env`·`envFrom`으로 넣은 `server.auth_token_env`)이 하나 이상 있어야 합니다. 없으면 차트가 `config has no authentication, ...` 오류로 렌더링을 거부합니다. 인증 없이 모든 호출을 익명 admin으로 받는 개발용 설치만 `auth.allowAnonymous: true`로 허용합니다.
2. 차트를 설치합니다.
   ```bash
   helm install vigilante ./vigilante-X.Y.Z.tgz -n vigilante --create-namespace -f values.yaml
   ```
3. 실행 중인 이미지로 설정을 검증합니다.
   ```bash
   kubectl -n vigilante exec deploy/vigilante -- vigilante validate -c /etc/vigilante/vigilante.yaml
   ```

단일 노드(파일 저장소, 영구 볼륨에 저널) 값 예시는 다음과 같습니다.

```yaml
config: |
  version: v1
  server:
    journal_path: /var/lib/vigilante/journal.jsonl
  auth: {...}
ingress: {enabled: true, host: vigilante.example.internal, tls: [{secretName: vigilante-tls, hosts: [vigilante.example.internal]}]}
```

HA(PostgreSQL, 리플리카 3) 값 예시는 다음과 같습니다.

```yaml
replicaCount: 3
persistence: {enabled: false}
env:
  - name: VIGILANTE_PG_DSN
    valueFrom: {secretKeyRef: {name: vigilante-db, key: dsn}}
config: |
  version: v1
  server:
    state: {backend: postgres, dsn_env: VIGILANTE_PG_DSN}
    ha: {enabled: true}
  auth: {...}
```

파드에서 직접 HTTPS를 제공하는 값 예시는 다음과 같습니다. 인증서는 `kubernetes.io/tls` 시크릿(`tls.crt`, `tls.key`, 선택 `ca.crt`)이며 `/etc/vigilante-tls`에 마운트됩니다.

```yaml
tls: {secretName: vigilante-tls}
config: |
  version: v1
  server:
    tls: {cert_file: /etc/vigilante-tls/tls.crt, key_file: /etc/vigilante-tls/tls.key}
    ha:
      enabled: true
      tls: {ca_file: /etc/vigilante-tls/ca.crt, server_name: vigilante.<네임스페이스>.svc}
  auth: {...}
```

차트 동작에서 알아 둘 점은 다음과 같습니다.

- 파드마다 `VIGILANTE_HA_NODE_ID`(파드 이름)와 `VIGILANTE_HA_ADVERTISE_URL`(`<scheme>://<파드 IP>:8088`)을 넣고, `VIGILANTE_LOG_FORMAT=json`을 설정합니다.
- `config`에 `server.tls.cert_file`이 있으면(`existingConfigMap`이면 `tls.enabled: true`를 직접 지정) HA 알림 주소, 생존·준비 확인, 포트 이름(`https`), Ingress 백엔드 포트, ServiceMonitor가 https를 씁니다. Ingress 컨트롤러는 백엔드에 HTTPS로 연결하도록 설정합니다(ingress-nginx는 `nginx.ingress.kubernetes.io/backend-protocol: HTTPS`). `tls.enabled: true`인데 `server.tls.cert_file`이 없으면 렌더링을 거부합니다.
- 팔로워는 리더의 파드 IP로 HTTPS 전달을 하는데 파드 IP는 보통 인증서에 없습니다. `server.ha.tls`에 CA와 인증서에 들어 있는 이름(예: `vigilante.<네임스페이스>.svc`)을 지정하십시오(7.3절).
- `server.tls.client_auth: require`는 kubelet 프로브와 리더 전달이 클라이언트 인증서를 내지 못하므로 렌더링을 거부합니다. `optional`을 쓰십시오.
- `replicaCount`가 2 이상인데 `persistence.enabled`가 켜져 있으면 렌더링을 거부합니다(파일 저장소는 단일 기록자만 허용).
- 리플리카 1(파일 저장소)은 `Recreate` 전략으로 교체되고, 2 이상이면 PodDisruptionBudget(`minAvailable: 1`)이 적용됩니다.
- 생존 확인은 `/healthz`, 준비 확인은 `/readyz`를 씁니다. `terminationGracePeriodSeconds`는 60초입니다.
- 차트는 `existingConfigMap`을 읽을 수 없으므로 인증과 TLS를 확인하지 않습니다. 설치 안내문(NOTES)이 인증 설정을 확인하라고 안내합니다.
- Prometheus Operator를 쓰면 `serviceMonitor.enabled: true`로 ServiceMonitor를 만들 수 있습니다(14장). TLS를 켜면 `serviceMonitor.tlsConfig`로 파드 인증서 검증 방법을 지정합니다.

## 3.6 에이전트 설치

에이전트는 선택 사항입니다. 로그를 고빈도로 수집해야 하거나, SSH가 막힌 구간이거나, 오케스트레이터 단절 시 자율 판정이 필요할 때 사용합니다.

1. 서버 쪽에서 에이전트용 서비스 계정 토큰을 만들고, 출력된 해시 항목을 `auth.service_accounts`에 추가한 뒤 서버를 재시작합니다.
   ```bash
   vigilante token create --name agents --role agent
   ```
2. 대상 호스트에 같은 패키지를 설치합니다.
   ```bash
   dnf install ./vigilante-X.Y.Z-1.x86_64.rpm
   ```
3. `/etc/vigilante/agent.env`에 서버 주소와 토큰을 넣습니다. `VIGILANTE_TARGET`을 생략하면 호스트 이름을 대상 이름으로 씁니다.
   ```bash
   VIGILANTE_SERVER=https://vigilante.example.internal:8088
   VIGILANTE_TOKEN=vgl_...
   # VIGILANTE_TARGET=order-bm-01
   ```
4. `/etc/vigilante/vigilante.yaml`에 서버와 같은 설정을 둡니다(대상·서비스 정의를 읽습니다).
5. 에이전트를 시작합니다.
   ```bash
   systemctl enable --now vigilante-agent
   ```
6. 에이전트가 앱 로그를 읽을 수 있도록 `vigilante` 사용자를 로그 파일 그룹에 추가합니다.

> `agent.failsafe: rollback`을 쓰면 에이전트가 대상 호스트에서 롤백 명령을 직접 실행합니다. 이때는 9장의 sudoers 규칙이 대상 호스트에도 필요하며, `systemctl edit vigilante-agent`로 쓰기 경로(`ReadWritePaths`)를 추가하고 sudo를 쓰려면 `NoNewPrivileges=no`로 바꿔야 합니다.

## 3.7 설치 후 점검

```bash
vigilante version                                   # 버전, 빌드 종류(full/minimal), 커밋
vigilante plugins                                   # 플러그인별 검증 수준
vigilante validate -c /etc/vigilante/vigilante.yaml
vigilante doctor   -c /etc/vigilante/vigilante.yaml
vigilante store status -c /etc/vigilante/vigilante.yaml   # PostgreSQL일 때
curl -fsS https://<서버>:8088/readyz
```

`vigilante plugins`는 각 플러그인을 `verified`(실제 시스템에서 확인) 또는 `experimental`(모의 서버·시뮬레이터 기준)로 표시합니다. 실험적 플러그인을 쓰는 서비스는 `rollback.mode: approve`로 시작하기를 권장합니다.

---

# 4. 설정 파일 작성

## 4.1 기본 원칙

- 설정 파일은 `/etc/vigilante/vigilante.yaml`(패키지) 또는 Helm의 `config`입니다. 최상위 `version: v1`이 필요합니다.
- 비밀값은 YAML에 쓰지 않습니다. 자격증명에는 평문 비밀번호 키가 없으며, `*_ref`(Vault·파일·환경변수 참조) 또는 `*_env`(환경변수 이름)만 받습니다.
- 기간 값은 Go duration 문자열(`500ms`, `30s`, `5m`, `1h`)입니다. 0은 `0s`로 씁니다.
- 많은 문자열 값에서 Go 템플릿을 쓸 수 있습니다: `{{.Name}}`, `{{.Address}}`, `{{.Kind}}`, `{{.Labels.KEY}}`, `{{.Service}}`, `{{.Version}}`, `{{.PreviousVersion}}`, `{{.DeploymentID}}`, `{{.Phase}}`, `{{.Checkpoint.KEY}}`, `{{env "NAME"}}`. 없는 라벨 키는 오류입니다.
- 작성 후에는 항상 `vigilante validate -c FILE`을 실행합니다. 알 수 없는 키(오타)는 거부되고, 모든 교차 참조(대상→자격증명, 서비스→프로브·실행기·트래픽, 규칙→메트릭)를 검사합니다. 경고는 `WARN:` 줄로 출력됩니다.

## 4.2 최상위 구조

```yaml
version: v1
server:        {...}   # API 주소, 상태 저장소, HA, TLS
agent:         {...}   # 에이전트 동작
credentials:   {...}   # 이름 → 자격증명 (값은 참조)
targets:       [...]   # 관측·제어 대상
traffic:       {...}   # 이름 → 트래픽 제어기
executors:     {...}   # 이름 → 롤백 전략
services:      [...]   # 서비스 = 대상 + 프로브 + 규칙 + 단계 + 롤백 플랜
safety:        {...}   # 서킷브레이커·blast radius·플래핑·관측 쿼럼
notify:        [...]   # 알림
auth:          {...}   # 인증과 역할, 로컬 CLI 제한
audit:         {...}   # SIEM 전송(syslog·TLS), 체인 키, 보존 기간
secrets:       {...}   # Vault와 캐시
api:           {...}   # 오픈 API 호출 한도, OAuth 토큰 수명, 웹훅 서명 키
change_freeze: [...]   # 변경 동결 기간
itsm:          {...}   # ServiceNow 연동
console:       {...}   # 웹 콘솔 로그인
preset_dirs:   [...]   # 조직 프리셋 디렉토리 (선택)
```

## 4.3 server

| 키 | 기본값 | 설명 |
|---|---|---|
| `listen` | `:8088` | REST API 주소. 패키지 기본 설정은 `127.0.0.1:8088` |
| `auth_token_env` | 없음 | 비상용(break-glass) Bearer 토큰 환경변수. 평소에는 비워 두기를 권장 |
| `webhook_secret_env` | 없음 | GitHub HMAC 서명·GitLab `X-Gitlab-Token` 검증 비밀 |
| `journal_path` | `vigilante-journal.jsonl` | 파일 저장소의 저널 경로. 같은 디렉토리에 락 파일 생성 |
| `dry_run` | `false` | 변경 조치를 로그로만 남김(읽기 명령·체크포인트는 실행) |
| `state.backend` | `file` | `file` 또는 `postgres` |
| `state.dsn_ref`, `state.dsn_env`, `state.dsn` | 없음 | PostgreSQL 접속 문자열. `dsn_ref` 또는 `dsn_env` 권장 |
| `state.auto_migrate` | `true` | 시작할 때 스키마 마이그레이션 적용 |
| `ha.enabled` | `false` | 여러 노드 중 하나만 리더로 동작. `postgres` 필수 |
| `ha.advertise_url` | 없음 | 다른 노드가 이 노드에 닿는 주소. `VIGILANTE_HA_ADVERTISE_URL`이 우선 |
| `ha.node_id` | 호스트명 | 리스 기록의 노드 이름. `VIGILANTE_HA_NODE_ID`가 우선 |
| `ha.lease_ttl` | `15s` | 리더 리스 유효 시간(최소 3s). TTL/3마다 갱신 |
| `ha.tls` | 없음 | 팔로워가 https 주소의 리더로 전달할 때의 검증. `ca_file`(노드 인증서의 사설 CA), `server_name`(주소의 호스트 대신 인증서에서 확인할 이름), `cert_file`·`key_file`(클라이언트 인증서). 없으면 시스템 신뢰 저장소와 주소의 호스트로 검증(7.3절) |
| `metrics_public` | `false` | `/metrics`를 인증 없이 제공 |
| `tls.cert_file`, `tls.key_file` | 없음 | HTTPS 직접 제공(8장) |
| `tls.client_ca_file`, `tls.client_auth` | 없음, `none` | 클라이언트 인증서 검증(`optional`, `require`) |
| `tls.min_version` | `1.2` | `1.3`으로 올릴 수 있음 |

> CI 단발 실행(`vigilante watch`)을 여러 러너에서 쓸 때는 서킷·플래핑 이력을 공유하도록 `journal_path`를 공유 경로로 두십시오. 파일 저장소의 락은 같은 파일시스템을 공유하는 프로세스 사이에서만 배타적입니다.

## 4.4 credentials와 비밀값

### 4.4.1 자격증명 종류

| `type` | 사용하는 키 |
|---|---|
| `ssh` | `user`, `ssh_ca`(권장), `private_key_ref` 또는 `private_key_file`, `passphrase_ref`·`passphrase_env`, `password_ref`·`password_env`, `use_ssh_agent`, `known_hosts_file`(기본 `~/.ssh/known_hosts`), `insecure_ignore_host_key` |
| `basic` | `username_ref`·`username_env`(또는 `user`), `password_ref`·`password_env`. F5, vCenter, Prism, 웹훅, ServiceNow |
| `token` | `token_ref`·`token_env`. 웹훅 Bearer, ServiceNow OAuth |
| `aws` | `region`, `profile`. 나머지는 AWS 기본 자격증명 체인(IAM Role 권장) |
| `openstack` | `auth_url`, `region`, `interface`, `application_credential_id`, `application_credential_secret_ref`, `cacert` 또는 `user`·`password_ref`·`project_name` |

### 4.4.2 비밀값 참조 형식

`*_ref`는 `*_env`보다 우선합니다.

| 참조 | 예 | 값의 출처 |
|---|---|---|
| `vault:<mount>/<path>#<key>` | `vault:secret/prod/f5#password` | Vault KV v2 (`secrets.vault` 필요) |
| `env:NAME` | `env:F5_PASSWORD` | 환경변수 |
| `file:/path` | `file:/run/secrets/f5-password` | 파일 내용(끝 줄바꿈 제거) |

### 4.4.3 Vault 연동 절차

1. Vault에 Vigilante 전용 정책을 만듭니다. 필요한 권한은 KV 읽기와 SSH 서명뿐입니다.
   ```hcl
   path "secret/data/prod/vigilante/*"     { capabilities = ["read"] }
   path "ssh-client-signer/sign/vigilante" { capabilities = ["update"] }
   ```
2. 인증 방식(`token`, `approle`, `kubernetes`)을 정하고 `secrets` 절을 작성합니다.
   ```yaml
   secrets:
     cache_ttl: 5m
     vault:
       address: https://vault.example.internal:8200
       namespace: ops                    # Vault Enterprise (선택)
       ca_file: /etc/vigilante/vault-ca.pem
       auth: approle                     # token | approle | kubernetes
       role_id_env: VAULT_ROLE_ID
       secret_id_env: VAULT_SECRET_ID
       # token_env: VAULT_TOKEN          # token 인증 (기본 VAULT_TOKEN)
       # k8s_role: vigilante             # kubernetes 인증
       # auth_mount: approle             # 로그인 경로가 기본값과 다를 때
   ```
3. `VAULT_ROLE_ID`, `VAULT_SECRET_ID`를 `/etc/vigilante/vigilante.env`에 둡니다.
4. 자격증명의 `*_ref`를 Vault 참조로 바꿉니다.
5. `vigilante doctor`로 모든 `*_ref`가 실제로 해석되는지 확인합니다. `ssh_ca`는 일회용 키로 서명까지 받아 봅니다.

Vault 동작에서 알아 둘 점은 다음과 같습니다.

- `approle`과 `kubernetes`는 처음 쓸 때 로그인하고, 토큰 만료 30초 전에 다시 로그인합니다. 403이 오면 한 번 다시 로그인해 재시도합니다.
- 비밀값은 메모리 캐시에만 두며 `cache_ttl`(기본 5m)이 지나면 다시 가져옵니다. 설정·저널·감사 기록에는 참조 문자열만 남습니다.
- 한 번이라도 해석한 값(6자 이상)은 로그에서 `[REDACTED]`로 가려집니다.
- Vault가 응답하지 않아도 캐시에 있는 값은 `cache_ttl`이 끝날 때까지 그대로 씁니다. 캐시가 만료된 뒤에는 그 자격증명이 필요한 프로브·실행기만 실패합니다(만료된 값을 계속 쓰지는 않습니다). 롤백 경로가 Vault에 의존하지 않게 하려면 `cache_ttl`을 관측 창보다 길게 두십시오.

### 4.4.4 SSH 단기 인증서 (ssh_ca)

```yaml
credentials:
  ssh-deploy:
    type: ssh
    user: deploy
    ssh_ca: {mount: ssh-client-signer, role: vigilante, ttl: 30m}
    private_key_file: ~/.ssh/vigilante_ed25519     # 선택: Vault 장애 시 대체 수단
    known_hosts_file: ~/.ssh/known_hosts
```

- 메모리에서 일회용 ed25519 키를 만들고 공개키만 Vault `POST <mount>/sign/<role>`로 보내 서명받습니다. 개인키는 디스크에 남지 않습니다.
- 인증서는 수명의 80%가 지나면 새 키로 다시 발급합니다.
- 대상 서버 sshd는 CA 공개키만 신뢰하면 됩니다(`TrustedUserCAKeys`). 인증서의 principal이 로그인 사용자와 같아야 합니다.
- `ssh_ca`와 개인키를 함께 지정하면 인증서를 먼저 시도합니다.

## 4.5 targets

```yaml
targets:
  - name: order-bm-01                # 고유 이름
    kind: baremetal                  # baremetal | vm | cloud_vm | container_host (정보성)
    address: 10.10.1.11
    labels: {instance_id: i-0a1b2c3d4e5f60001}   # 템플릿에서 사용
    connection:
      type: ssh                      # ssh | local | none
      credential: ssh-deploy
      port: 22
      bastion: bastion-dc1           # 다른 target을 점프 호스트로
      sudo: true
      sudo_scope: changes            # 9장
      timeout: 10s
      max_sessions: 8                # 10장
      reserved_sessions: 2
```

## 4.6 services와 프리셋

서비스는 대상, 프로브, 규칙, 단계, 롤백 플랜을 묶은 단위입니다. 처음에는 프리셋으로 시작하고 필요한 값만 바꾸기를 권장합니다.

```yaml
services:
  - name: billing-api
    team: billing                    # 권한 범위 team= 과 동결·알림 라우팅에 쓰임
    targets: [billing-os-01, billing-os-02]
    preset: java-web@1               # 버전 고정 권장
    overrides:
      health_url: "http://{{.Address}}:8080/actuator/health"
      access_log: /var/log/billing/access.log
      app_log: /var/log/billing/application.log
    rollback: {mode: approve, executor: billing-os-snap, traffic: billing-lb}
```

| 내장 프리셋 | 대상 | 필수 값 |
|---|---|---|
| `java-web` | Java/Spring 웹(베어메탈·VM) | `health_url`, `access_log`, `app_log` |
| `container-api` | VM 위 단독 컨테이너 | `container`, `health_url` |
| `static-web` | 정적 웹·프록시 | `health_url`, `access_log` |
| `worker` | 배치·큐 워커 | `app_log` |

프리셋 작업 절차는 다음과 같습니다.

1. 사용 가능한 프리셋과 파라미터를 확인합니다: `vigilante presets`
2. 실제로 펼쳐지는 내용을 미리 봅니다: `vigilante presets show java-web --set error_rate_pct=3`
3. 서비스에 `preset`과 `overrides`를 씁니다. 서비스에 직접 쓴 프로브·규칙은 같은 id·이름의 프리셋 항목을 대체합니다.
4. `vigilante validate`로 확인합니다. 출력에 `billing-api: preset java-web@1 -> N probes, M rules`가 표시됩니다.

조직 프리셋은 최상위 `preset_dirs: [presets]`(설정 파일 기준 상대경로)에 같은 형식의 파일을 두고 `preset: corp/java-web@2`처럼 씁니다.

### 4.6.1 프로브

공통 키는 `id`(점 금지, 메트릭 접두어), `type`, `interval`(기본 2s), `timeout`(기본 1s)입니다. 메트릭 이름은 `<id>.<metric>`입니다.

| type | 주요 설정 | 대표 메트릭 |
|---|---|---|
| `http` | `url`, `expect_status`, `json_path`, `json_expect` | `up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts` |
| `grpc` | `address`, `service`, `tls` | `up`, `latency_ms`, `consecutive_failures` |
| `tcp` | `address` | `up`, `latency_ms` |
| `host` | `devices` | `up`(SSH로 읽기 성공 1, 실패 0), `load_per_cpu`, `cpu_busy_pct`, `mem_available_pct`, `disk_util_pct` |
| `docker` | `container`, `socket` | `running`, `restarts`, `oom_killed`, `oom_events` |
| `log` | `path`, `patterns`, `remote_grep` | `lines`, `match.<name>` |
| `access_log` | `path`, `format`(combined, json) | `requests`, `count_5xx`, `error_rate_5xx`, `latency_ms` |
| `db` | `driver`(postgres, mysql), `dsn_env`, `pool_size`, `pool_check_interval` | `up`, `query_ms`, `pool_acquired`, `pool_acquire_ms` |

## 4.7 rules

서비스마다 `action: rollback` 규칙이 1개 이상 있어야 합니다. 없으면 `validate`가 거부합니다.

```yaml
rules:
  - name: fatal-errors
    action: rollback                 # rollback | notify | hold
    when:
      any:
        - {metric: access.count_5xx, ratio_of: access.requests, window: 30s, op: ">", value: 2, for: 2}
        - {metric: applog.match.exception, agg: rate, window: 10s, op: ">", value: 10}
        - {metric: health.consecutive_timeouts, op: ">=", value: 3}
  - name: latency-regression
    action: rollback
    when: {metric: health.latency_ms, agg: p99, window: 1m, op: ">", baseline: {increase_pct: 200}, min_value: 300, for: 3, reset_after: 2}
```

| 키 | 기본값 | 설명 |
|---|---|---|
| `metric` | 필수 | `<probe-id>.<metric>` |
| `agg` | `last` | `last avg min max sum count rate p50 p90 p95 p99` |
| `window` | `30s` | 집계 구간 |
| `ratio_of` | 없음 | 가중 비율(%) 계산의 분모 메트릭 |
| `op`, `value` | `>`, 없음 | 비교 연산과 절대 임계치 |
| `baseline.increase_pct`, `min_value` | 없음 | 기준선 대비 증가율과 절대 하한 |
| `for`, `reset_after` | `1`, `1` | 연속 위반 횟수, 정상 복귀 횟수 |
| `scope` | `target` | `target` 또는 `service` |
| `absent` | `unknown` | 데이터 없음 처리: `unknown`, `breach`, `ok` |

## 4.8 phases

```yaml
phases:
  canary:  {targets: [order-bm-01], observation_window: 10m, warmup: 60s, eval_interval: 5s, min_samples: 200, on_inconclusive: hold}
  rolling: {percent: 50, observation_window: 15m}
  full:    {observation_window: 20m, rules: [fatal-errors, latency-regression]}
```

| 키 | 기본값 | 설명 |
|---|---|---|
| `targets`, `percent` | canary=첫 대상, rolling=50%, full=전체 | 이 단계까지 새 버전이 깔린 대상(누적) |
| `observation_window` | `5m` | 이 시간 동안 위반이 없으면 PASS |
| `warmup` | `0s` | 판정 보류 구간(수집은 계속) |
| `eval_interval` | `5s` | 판정 주기 |
| `rules` | 전체 | 이 단계에 적용할 규칙 |
| `min_samples` | `0` | 미달이면 INCONCLUSIVE |
| `on_inconclusive` | `hold` | `hold`, `pass`, `rollback` |

서비스 수준의 `control_targets: [auto]`는 아직 배포되지 않은 대상을 실시간 대조군으로 씁니다. 대조군에서도 같은 규칙이 위반되면 환경 문제로 보고 HOLD합니다.

## 4.9 롤백 플랜과 모드

```yaml
rollback:
  mode: approve                      # auto | approve (생략하면 auto, validate가 경고)
  approval: {timeout: 30m, on_timeout: hold, drain_first: true}
  executor: order-symlink            # 1차 전략
  traffic: nginx-edge                # 트래픽 제어기 (선택)
  scope: deployed                    # deployed | failed
  parallelism: 2
  step_timeout: 2m
  retry: {attempts: 3, backoff: 2s, max_backoff: 30s}
  plan:                              # 생략 시 [drain] → rollback → verify → [enable]
    - action: traffic.drain
    - action: app.rollback
    - action: app.verify
    - {action: probe.verify, probe: health, successes: 3, timeout: 90s}
    - action: traffic.enable
  escalation:
    - {executor: legacy-kvm-snapshot, require_approval: true}
```

| 단계 | 동작 |
|---|---|
| `traffic.drain` | 대상을 풀에서 제외하고 `drain_wait` 대기. 실패해도 제자리 롤백 진행 |
| `app.rollback` | 실행기의 롤백(멱등) |
| `app.verify` | 버전 사실 확인(링크 대상, 이미지 태그, VM 상태 등) |
| `probe.verify` | 지정 프로브가 연속 `successes`회 성공해야 통과 |
| `traffic.enable` | 풀 복귀. 앞 단계가 실패하면 실행하지 않아 대상은 격리 상태로 남음 |
| `wait` | `duration` 대기 |

롤백 모드 선택 기준은 다음과 같습니다.

| 모드 | 동작 | 권장 상황 |
|---|---|---|
| `auto` | FAIL이면 즉시 롤백 | 판정 품질이 검증된 서비스 |
| `approve` | 롤백 계획을 만들고 `AWAITING_APPROVAL`(종료 코드 3)로 대기. 콘솔·API·CLI로 승인 또는 거절 | 파일럿 기간, 실험적 플러그인을 쓰는 서비스 |

- `approval.timeout`(기본 30m)이 지나면 `on_timeout`에 따릅니다. `hold`(기본)는 계속 기다리며 한 번 더 상위 호출하고, `rollback`은 자동 롤백으로 넘어갑니다(이때는 서킷·플래핑 제한 적용). 만료 처리는 서버 리더가 15초마다 하며, CI 단발 실행만 쓰는 구성에서는 동작하지 않습니다.
- `drain_first: true`는 승인을 기다리는 동안 위반 대상을 트래픽에서 뺍니다. `rollback.traffic`이 필요합니다.
- `approve` 모드 서비스에서는 `agent.failsafe: rollback`이어도 에이전트가 혼자 롤백하지 않습니다.

## 4.10 executors와 traffic

| 실행기 `type` | 전략 | 주요 키 |
|---|---|---|
| `symlink` | 디렉토리 전환 | `link`, `releases_dir`, `release`, `init`, `unit`, `restart_cmd`, `atomic` |
| `container` | 컨테이너 이미지 전환 | `name`, `socket`, `image_repo`, `tag`, `pull`, `stop_timeout_sec` |
| `vsphere` | VM 스냅샷 | `url`, `credential`, `vm`, `snapshot`, `power_on` |
| `nutanix` | VM 스냅샷 | `url`, `credential`, `vm_uuid`, `snapshot` |
| `kvm` | VM 스냅샷 | `hypervisor`, `domain`, `snapshot` |
| `openstack` | 인스턴스 스냅샷 | `credential`, `server_id`, `mode`, `revert_timeout`, `keep_snapshots` |
| `exec` | 범용 명령 | `prepare`, `rollback`, `verify`, `on` |
| `webhook` | 사내 배포 콘솔 호출 | `url`, `method`, `body`, `verify_url`, `credential` |

| 트래픽 `type` | 방식 | 주요 키 |
|---|---|---|
| `nginx` | upstream 파일의 `down` 토글, `nginx -t` 후 reload | `hosts`, `upstream_file`, `member_format` |
| `haproxy` | Runtime API `set server ... state` | `hosts`, `socket` 또는 `address`, `backend`, `server_name` |
| `envoy` | 파일 기반 EDS | `hosts`, `eds_file`, `member_format` |
| `f5` | iControl REST 풀 멤버 변경 | `url`, `credential`, `pool`, `member_format` |
| `aws_alb` | 타깃 등록·해제 | `credential`, `target_group_arn`, `target_id`, `port` |
| `octavia` | 풀 멤버 `admin_state_up` 변경 | `credential`, `pool_id`, `member_port` |

공통 키 `drain_wait`는 드레인 후 대기 시간입니다.

> 검증 수준: 실행기 중 "검증됨"은 `webhook` 하나이고, 나머지 실행기 7개와 트래픽 제어기 6개는 모두 실험적(모의 서버·시뮬레이터 기준)입니다. 실장비(F5, vCenter, Nutanix, AWS, OpenStack 등) 검증은 아직 없습니다. `vigilante plugins`로 확인하십시오.

## 4.11 safety

```yaml
safety:
  observer_quorum: true
  circuit_breaker: {failure_threshold: 2, window: 1h, open_duration: 0s}
  blast_radius:    {min_healthy: 1, min_healthy_percent: 50}
  flapping:        {max_rollbacks_per_hour: 3, cooldown: 5m}
  rollback_lease:  {wait: 10s, on_unavailable: proceed}
  observer_guard:  {max_lag: 1s, loopback_timeout: 1s, timeout_share: 0.5, min_services: 3, grace: 1m}
```

| 키 | 기본값 | 설명 |
|---|---|---|
| `observer_quorum` | `false` | 중앙과 에이전트가 모두 위반을 봐야 롤백. 불일치면 HOLD |
| `rollback_lease.wait` | `10s` | 롤백을 시작할 때 서비스 잠금(상태 저장소 lease)을 얻으려고 저장소를 다시 시도하는 시간 |
| `rollback_lease.on_unavailable` | `proceed` | 그래도 저장소에 닿지 않을 때. `proceed`는 이 프로세스 안의 잠금만으로 롤백하고 배포 이벤트(`safety`), 감사 기록(`lease.unavailable`), 경고 알림을 남깁니다. 롤백 중 저장소가 돌아오면 lease를 다시 잡고, 다른 프로세스가 이미 잡고 있으면 `lease.conflict`로 경고합니다. `fail`은 롤백을 시작하지 않습니다(ROLLBACK_FAILED) |
| `observer_guard.disabled` | `false` | `true`면 관측 장치 과부하 판별을 끕니다 |
| `observer_guard.max_lag` | `1s` | 내부 250ms 타이머가 이보다 늦게 깨면 과부하(CPU 부족, GC, VM 정지) |
| `observer_guard.loopback_timeout` | `1s` | 프로세스 안 TCP 에코 왕복이 이보다 길면 과부하(소켓·네트워크 스택 고갈) |
| `observer_guard.timeout_share`, `min_services` | `0.5`, `3` | 관측 중인 대상의 이 비율 이상에서, 이 개수 이상의 서비스에 걸쳐 프로브가 시간 초과면 관측 쪽 문제로 봄. 서비스 하나의 불량 릴리스로는 걸리지 않음 |
| `observer_guard.grace` | `1m` | 회복 후에도 이 시간 동안 보류를 유지(과부하 중 쌓인 연속 실패 수와 윈도우 값이 빠질 시간) |
| `circuit_breaker.failure_threshold` | `3` | `window` 안의 롤백 실패 횟수가 이에 이르면 서킷 OPEN |
| `circuit_breaker.window` | `1h` | 실패를 세는 구간 |
| `circuit_breaker.open_duration` | `0s` | 0이면 수동 리셋만. 양수면 그 시간 뒤 HALF_OPEN |
| `blast_radius.min_healthy`, `min_healthy_percent` | `1`, 없음 | 드레인 후에도 남아야 할 최소 정상 멤버 |
| `flapping.max_rollbacks_per_hour` | `3` | 서비스별 시간당 자동 롤백 상한 |
| `flapping.cooldown` | 없음 | 직전 자동 롤백 후 다음 자동 롤백까지 최소 간격 |

관측 장치 과부하 판별(`observer_guard`)은 서버 자신의 측정을 믿을 수 없는 동안 서버가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, 그리고 SSH로 읽는 `host`. `host`의 `up`은 서버에서 본 SSH 도달 여부)의 `up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts`, `timeout` 지표와, 어느 프로브든 `probe_error`를 쓰는 규칙의 위반을 롤백 대신 보류로 바꿉니다. 그 상태로 관측 창이 끝나면 배포는 `HELD`가 되고, 이유에 `observer degraded (...), not attributed to the release`가 붙으며 경고 알림 `<서비스> <단계> HELD — human decision needed`가 갑니다. `log`·`access_log`·`docker` 프로브의 지표는 이름이 같아도(예: 액세스 로그의 `latency_ms`, 서버 자신의 요청 처리 시간) 대상이 보고한 값이므로 그대로 판정하고, 호스트 자원 값(`cpu_busy_pct` 등)도 그대로 판정합니다(PR #14. 그 전에는 프로브와 관계없이 지표 이름만 보아 액세스 로그의 `latency_ms`도 보류했습니다). 설정 키는 바뀌지 않았습니다. 감시 지표는 14.2절의 `vigilante_observer_*`입니다.

## 4.12 설정 적용 절차

1. 설정 파일을 수정합니다.
2. `vigilante validate -c /etc/vigilante/vigilante.yaml`을 실행하여 `OK:` 줄과 `WARN:` 줄을 확인합니다.
3. `vigilante doctor -c /etc/vigilante/vigilante.yaml [--service S]`로 접속·sudo·로그 형식·이전 릴리스·LB 풀을 점검합니다. `[FAIL]`이 있으면 종료 코드 1입니다.
4. `systemctl restart vigilante-server`(Helm은 `helm upgrade`)로 적용합니다. 서버는 재시작 후 진행 중이던 관측을 이어서 판정하고, 롤백은 멈춘 단계부터 재개합니다.
5. `curl -fsS https://<서버>:8088/readyz`로 준비 상태를 확인합니다.

---

# 5. 인증과 권한

## 5.1 인증 방식

| 방식 | 토큰 형태 | 신원 표기 | 용도 |
|---|---|---|---|
| 서비스 계정 토큰 | `vgl_…` | `sa:<name>` | CI, 에이전트. 설정에는 SHA-256 해시만 저장 |
| OIDC JWT | IdP 발급 JWT | `user:<preferred_username>` | 사람(사내 SSO) |
| API 키 | `vgk_…` | `client:<이름>` | 단순 연동(스크립트, SIEM 수집) |
| OAuth 액세스 토큰 | `vat_…` | `client:<이름>` | 서버 간 연동(client credentials) |
| 비상용 토큰 | `server.auth_token_env` 값 | `token:legacy` | break-glass, admin. 호출 한도 미적용 |
| 콘솔 세션 쿠키 | 콘솔 로그인 후 | OIDC 사용자 | 웹 콘솔 |

아무것도 설정하지 않으면 인증이 꺼지며, 서버가 시작 시 경고를 남기고 모든 작업이 `anonymous`로 기록됩니다. 운영 환경에서는 반드시 인증을 켜십시오.

## 5.2 역할과 범위

| 역할 | 할 수 있는 일 |
|---|---|
| `viewer` | 배포·서킷·지표 조회 |
| `deployer` | 배포 생성, 단계 관측 시작, 중단, 기준선 측정, `mark-good`, 판정 평가 |
| `operator` | 수동 롤백, 승인 대기 롤백의 승인·거절 |
| `admin` | 서킷 리셋·차단, 변경 동결 선언, API 클라이언트 관리, 그 외 전부 |
| `agent` | 에이전트 전용: 샘플 전송, 하트비트 |

역할은 아래로 갈수록 상위 권한을 포함합니다(`agent`는 별개). 범위(scope)는 `*`(전체), `team=<팀>`(서비스의 `team` 값), `service=<이름>` 중 하나입니다. 서킷 리셋, 감사 기록 조회, `/metrics`처럼 특정 서비스에 속하지 않는 작업은 `*` 범위가 필요합니다.

## 5.3 OIDC 설정

1. IdP(Keycloak, Azure AD, Okta 등)에 Vigilante 클라이언트를 등록하고 클라이언트 ID(audience)를 정합니다.
2. 그룹 클레임이 ID 토큰에 들어가도록 IdP를 설정합니다.
3. `auth.oidc`를 작성합니다.
   ```yaml
   auth:
     oidc:
       issuer: https://sso.example.internal/realms/ops
       audience: vigilante
       groups_claim: groups                 # 기본 groups
       username_claim: preferred_username   # 기본 preferred_username
   ```
4. 서버를 재시작합니다. 서버는 시작할 때 issuer의 discovery 문서를 가져오므로 서버에서 IdP에 접근할 수 있어야 합니다. 접근할 수 없으면 `oidc discovery for <issuer>: ...` 오류로 시작하지 않습니다.

## 5.4 역할 바인딩

```yaml
auth:
  role_bindings:
    - {group: sre-oncall, role: operator}
    - {group: platform-admins, role: admin}
    - {group: payments-dev, role: deployer, scope: team=payments}
    - {user: alice, role: viewer}
  four_eyes: true
```

- `group` 또는 `user`에 역할과 범위를 줍니다. `scope`를 생략하면 `*`입니다.
- 역할 바인딩이 하나도 없는 OIDC 사용자는 인증은 되지만 아무 작업도 할 수 없으며, 콘솔 로그인은 거부되고 감사 기록에 `denied`로 남습니다.

## 5.5 서비스 계정

1. 토큰을 발급합니다. 토큰은 한 번만 표준 출력에 나오고, 설정에 넣을 해시 항목이 표준 오류에 나옵니다.
   ```bash
   vigilante token create --name ci-order-api --role deployer --scope service=order-api --expires 2027-06-30
   ```
2. 출력된 항목을 `auth.service_accounts`에 추가합니다.
   ```yaml
   auth:
     service_accounts:
       - name: ci-order-api
         token_sha256: fb4afd06a1bdd94f9f3febfaa8b22a2d3fd2db9d10bf0c51ab3a75ba8e9fd8a9
         expires: 2027-06-30
         roles: [{role: deployer, scope: "service=order-api"}]
   ```
3. 서버를 재시작합니다.
4. 토큰을 CI 비밀 변수(예: `VIGILANTE_TOKEN`)에 저장합니다.
5. 신원과 권한을 확인합니다.
   ```bash
   VIGILANTE_TOKEN=vgl_... vigilante whoami --server https://vigilante.example.internal:8088
   ```
   `grants: none — this identity can authenticate but not act`가 나오면 역할이 없는 것입니다.

토큰을 폐기하려면 해당 항목을 지우고 서버를 재시작합니다(실행 중 설정을 다시 읽는 기능이 없으므로, HA에서는 모든 노드를 재시작해야 폐기가 적용됩니다). `expires`는 그 날짜까지 유효하며, 만료일을 두고 주기적으로 교체하기를 권장합니다.

## 5.6 API 클라이언트 (API 키·OAuth)

API 클라이언트는 설정 파일이 아니라 v2 API로 관리하며 상태 저장소에 남습니다. admin 역할과 `config:write` 스코프가 필요합니다.

1. 클라이언트를 등록합니다. 응답의 비밀값은 이때 한 번만 보입니다.
   ```bash
   curl -X POST "$API/v2/api-clients" -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" -d '{
     "name": "deploy-console", "type": "oauth",
     "scopes": ["deployments:read", "deployments:write"],
     "grants": ["deployer@team=payments"],
     "rate_limit": {"rate": 10, "burst": 20}
   }'
   ```
2. OAuth 클라이언트는 client credentials로 토큰을 받습니다.
   ```bash
   curl -u "$CLIENT_ID:$CLIENT_SECRET" -d grant_type=client_credentials "$API/v2/oauth/token"
   ```
3. 관리 작업은 다음 요청으로 합니다.

| 작업 | 요청 |
|---|---|
| 목록·조회 | `GET /v2/api-clients`, `GET /v2/api-clients/{id}` |
| 스코프·grant·만료·한도 변경 | `PATCH /v2/api-clients/{id}` |
| 비밀 회전 | `POST /v2/api-clients/{id}/secret` (이전 비밀과 그 토큰 즉시 무효) |
| 폐기 | `DELETE /v2/api-clients/{id}` |

| 스코프 | 허용 범위 |
|---|---|
| `deployments:read` | 배포·작업·서비스·대상·서킷·지표 조회 |
| `deployments:write` | 배포 등록, 단계 관측, 중단, 기준선, 마지막 정상 버전 |
| `rollbacks:execute` | 수동 롤백 |
| `approvals:write` | 승인 |
| `circuit:admin` | 서킷 닫기·열기 |
| `audit:read` | 감사 기록 조회(viewer@* 필요) |
| `metrics:write` | 샘플 전송 |
| `config:write` | API 클라이언트·동결·웹훅 관리(admin 필요) |

## 5.7 4-eyes 원칙

`auth.four_eyes: true`이면 배포를 만든 사람이나 롤백을 요청한 사람은 그 승인 요청을 직접 승인·거절할 수 없습니다. 위반하면 API는 `403 forbidden`을 돌려주고 감사 기록에 남습니다. 승인 모드(`rollback.mode: approve`)를 쓰는 조직에서는 켜 두기를 권장합니다.

> 주의: 역할·범위 검사는 서버 API(콘솔 포함)에서만 적용됩니다. `vigilante rollback`, `mark-good`, `status` 같은 CLI 명령은 `--server`를 주어도 서버로 위임하지 않고(`--server`를 따르는 것은 `watch`, `circuit`, `feedback`, `whoami`, `support-bundle`, `agent`뿐), 설정 파일과 상태 저장소에 직접 접근해 실행합니다(작업자는 `cli:<사용자>@<호스트>`로 기록). 로컬 승인·거절(`rollback --approve`·`--reject`)에도 4-eyes를 적용하지만, 비교 대상은 이 CLI 작업자 이름입니다. 권한이 큰 로컬 명령의 제한은 5.9절을 참고하십시오.

## 5.8 호출 한도

```yaml
api:
  rate_limit: {rate: 20, burst: 40}            # 호출자별 v2 호출 한도 (daily 선택)
  emergency_rate_limit: {rate: 1, burst: 10}   # 롤백·승인·중단·서킷 전용 버킷
  token_ttl: 1h                                # OAuth 액세스 토큰 수명 (최대 24h)
```

한도를 넘으면 `429`와 `Retry-After`를 돌려줍니다. 조회가 폭주해도 비상 버킷의 롤백 요청은 그대로 받습니다.

## 5.9 로컬 CLI 제한 (break-glass)

`--server` 없이 실행한 CLI는 API를 거치지 않고 상태 저장소를 직접 다루므로 역할 검사를 받지 않습니다. 그래서 인증이 설정된 환경에서는 권한이 큰 로컬 명령을 기본으로 거부합니다.

```yaml
auth:
  local_cli: auto     # auto(기본) | full | restricted
```

| 값 | 동작 |
|---|---|
| `auto` | 인증(`auth.service_accounts`, `auth.oidc`, `server.auth_token_env` 중 하나)이 설정되어 있으면 `restricted`, 없으면 `full` |
| `restricted` | 인증이 없어도 제한 |
| `full` | 제한하지 않음 |

제한 대상은 다음 네 가지입니다. 수동 롤백(`--approve` 없는 `rollback`), `mark-good`, `status`, `prepare`, `baseline`은 제한하지 않습니다.

| 로컬 명령 | 감사 동작(break-glass 시) |
|---|---|
| `vigilante rollback --approve`·`--reject`(승인 대기 롤백의 결정) | `breakglass.rollback.approve`, `breakglass.rollback.reject` |
| `vigilante rollback --approve`(승인 대기가 아닌 배포: 승인이 필요한 에스컬레이션 허용) | `breakglass.escalation.approve` |
| `vigilante circuit reset`·`trip` | `breakglass.circuit.reset`, `breakglass.circuit.trip` |
| 동결 중 `watch`·`prepare --freeze-override` | `breakglass.freeze.override` |

- 제한된 명령은 `<작업> refused: the API has authentication, so privileged local commands are restricted (auth.local_cli). Run it through the server with --server and an operator or admin token, or pass --break-glass REASON (audited and alerted)` 오류로 끝납니다. 종료 코드는 1이고, 동결 예외(`watch`·`prepare`)만 게이트 거부와 같은 3입니다.
- 평소에는 콘솔, API, 또는 `--server`와 operator·admin 토큰(`VIGILANTE_TOKEN`)으로 실행합니다. 서버 장애처럼 그럴 수 없을 때만 `--break-glass "이유"`를 붙입니다.
- break-glass를 쓰면 감사 기록 `breakglass.<작업>`과 critical 알림 `Break-glass: cli:<사용자>@<호스트> ran <작업> locally`가 남습니다. 알림은 그 CLI가 읽은 설정의 `notify` 채널로 보냅니다.
- 로컬 승인·거절에는 `auth.four_eyes`도 적용됩니다(break-glass 제외).
- 상태 저장소 접근 권한(PostgreSQL 계정, 저널 파일 권한) 자체가 이 명령들의 권한과 같으므로 그 계정은 운영자에게만 주십시오.

---

# 6. 웹 콘솔 SSO 설정

웹 콘솔은 `https://<서버>:8088/console/`에서 제공됩니다. 화면은 바이너리에 내장되어 있고 외부 CDN을 쓰지 않습니다. 콘솔은 공개 API(v2)만 호출하므로 사용자가 할 수 있는 일은 그 사용자의 역할·범위와 같습니다.

## 6.1 설정 절차

1. 5.3절대로 `auth.oidc`를 설정합니다.
2. IdP 클라이언트에 콜백 주소 `https://<콘솔 주소>/console/auth/callback`을 등록합니다. IdP가 발급하는 ID 토큰의 `aud`가 `auth.oidc.audience`와 같아야 합니다.
3. 세션 쿠키 암호화 키(32자 이상)를 Vault에 저장합니다.
4. `console` 절을 작성합니다.
   ```yaml
   console:
     redirect_url: https://vigilante.example.internal/console/auth/callback
     session_key_ref: "vault:secret/prod/vigilante#console_session_key"
     client_id: vigilante-console          # 생략 시 auth.oidc.audience
     client_secret_ref: "vault:secret/prod/vigilante#console_client_secret"   # 기밀 클라이언트만
     scopes: [openid, profile, email]      # 그룹 클레임이 별도 스코프면 추가
   ```
5. 서버를 재시작하고 브라우저에서 `/console/`에 접속하여 "회사 계정으로 로그인"이 표시되는지 확인합니다.

## 6.2 동작과 보안

- 로그인은 OIDC authorization code + PKCE입니다.
- 세션은 ID 토큰을 AES-GCM으로 암호화한 HttpOnly 쿠키(`SameSite=Lax`, https면 `Secure`)이며 서버에 세션 상태가 없습니다. ID 토큰 만료 시각(최대 12시간)에 끝납니다.
- `session_key_ref`가 없으면 재시작마다 키가 바뀌어 다시 로그인해야 하고 HA 노드 간 세션이 공유되지 않습니다(`validate` 경고). HA에서는 모든 노드가 같은 값을 써야 합니다.
- `redirect_url`이 `http://`이면 쿠키에 `Secure`가 붙지 않으므로 `validate`가 경고합니다.
- 쿠키로 인증한 변경 요청은 `X-CSRF-Token` 헤더가 필요합니다(double-submit).
- 보안 헤더: `Content-Security-Policy`(자기 출처만), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. HTTPS로 서비스하면(`server.tls`, `https://` `redirect_url`, 또는 TLS 요청) `Strict-Transport-Security: max-age=31536000`(HSTS, `includeSubDomains` 없음)도 보냅니다. 이 헤더들은 로그인·콜백·로그아웃을 포함한 콘솔의 모든 응답에 붙습니다. 같은 도메인의 다른 호스트까지 HTTPS로 묶으려면 프록시에서 `includeSubDomains`를 더하십시오.
- SSO를 설정하지 않으면(`auth.oidc` 또는 `redirect_url`이 없으면) 콘솔은 서비스 계정 토큰·API 키를 입력받습니다. 토큰은 그 브라우저 탭(sessionStorage)에만 남습니다.
- 콘솔을 끄려면 `console.disabled: true`를 씁니다.

---

# 7. TLS 설정

## 7.1 서버 HTTPS

1. 서버 인증서와 키를 `/etc/vigilante/tls/`에 둡니다. `vigilante` 계정이 읽을 수 있어야 합니다.
2. 설정을 추가합니다.
   ```yaml
   server:
     listen: ":8088"
     tls:
       cert_file: /etc/vigilante/tls/server.crt
       key_file: /etc/vigilante/tls/server.key
       min_version: "1.2"           # 또는 "1.3"
   ```
3. 서버를 재시작합니다.

인증서·키 파일이 바뀌면 재시작 없이 새 인증서를 씁니다(cert-manager, 갱신 작업). 파일을 반쯤 쓴 상태에서는 기존 인증서를 계속 씁니다. TLS를 직접 켜지 않으면 HTTP로 동작하므로 앞에 TLS 프록시·인그레스를 두십시오.

## 7.2 클라이언트 인증서 (에이전트 상호 인증)

```yaml
server:
  tls:
    cert_file: /etc/vigilante/tls/server.crt
    key_file: /etc/vigilante/tls/server.key
    client_ca_file: /etc/vigilante/tls/agents-ca.pem
    client_auth: optional            # none | optional | require
agent:
  tls:
    ca_file: /etc/vigilante/tls/server-ca.pem
    cert_file: /etc/vigilante/tls/agent.crt
    key_file: /etc/vigilante/tls/agent.key
```

- `optional`은 제시된 인증서만 검증합니다(에이전트는 인증서, 브라우저·CI는 토큰). `require`는 모든 클라이언트에 인증서를 요구합니다. 어느 경우든 API 토큰 인증은 그대로 필요합니다.
- 에이전트 인증서 파일은 바뀌면 다시 읽습니다. `client_ca_file`과 `agent.tls.ca_file`은 시작할 때 읽으므로 CA를 바꾸면 재시작이 필요합니다.

## 7.3 HA 노드 간 신뢰

팔로워는 요청을 리더의 `advertise_url`로 전달합니다. 주소가 https이면 팔로워가 리더 인증서를 검증합니다. 방법은 둘 중 하나입니다.

- `server.ha.tls`를 설정합니다(권장). `ca_file`에 노드 인증서의 사설 CA를, `server_name`에 인증서에 들어 있는 이름을 지정하면 주소의 호스트(예: 파드 IP) 대신 그 이름으로 검증합니다. `server.tls.client_auth: require`이면 `cert_file`·`key_file`로 클라이언트 인증서도 줍니다.
  ```yaml
  server:
    ha:
      enabled: true
      tls: {ca_file: /etc/vigilante/tls/ca.pem, server_name: vigilante.example.internal}
  ```
- `server.ha.tls`가 없으면 시스템 신뢰 저장소와 주소의 호스트로 검증합니다. 사설 CA를 쓰면 각 노드의 시스템 신뢰 저장소에 CA를 넣으십시오(`update-ca-trust` 또는 `update-ca-certificates`).

`server.ha.tls.ca_file`은 서버를 시작할 때 읽으므로 CA를 바꾸면 재시작이 필요합니다(클라이언트 인증서는 바뀌면 다시 읽음). 파일을 읽지 못하면 서버가 시작하지 않습니다.

---

# 8. 고가용성(HA) 구성

## 8.1 동작 원리

- 여러 `vigilante server` 노드가 같은 PostgreSQL을 공유하고, 리더 리스를 가진 노드 하나만 판정·롤백을 실행하고 상태를 기록합니다.
- 리더 자리를 잃은 노드의 기록은 DB가 거부합니다(펜싱). 새 리더는 공유 상태를 다시 읽고 진행 중이던 롤백을 완료된 단계부터 이어서 끝냅니다.
- 팔로워는 `/healthz`, `/readyz`, `/metrics`, `/console/` 외의 모든 API 요청을 리더로 전달합니다. 클라이언트는 어느 노드로 보내도 됩니다.
- 리스 만료는 DB 시계 기준이므로 노드 간 시계 차이의 영향을 받지 않습니다.
- 리더가 DB에 닿지 못한 채 `lease_ttl`이 지나면 리더 자리에서 물러나 판정·롤백을 멈춥니다. 다른 노드가 리더가 되면 그 노드가 이어받습니다.

> **저장소 장애 시 기록:** 저장소 쓰기가 실패하면(펜싱 제외) 그 기록을 메모리 대기열에 순서대로 쌓았다가 저장소가 돌아오면 같은 순서로 씁니다(PR #12, 재시도 간격 최대 5초, 지표 `vigilante_store_pending_writes`). 이미 시작된 롤백은 저장소를 기다리지 않고 계속합니다. 장애 중에 새로 시작하는 롤백은 서비스 잠금(상태 저장소의 서비스 lease)을 `safety.rollback_lease.wait`(10초) 동안 다시 시도한 뒤, 기본(`on_unavailable: proceed`)은 프로세스 안 잠금만으로 진행하고 이벤트·감사(`lease.unavailable`)·경고 알림을 남깁니다(PR #13, 4.11절, 시험 `TestChaosStoreOutageDuringRollback`·`TestChaosStoreOutageLeaseFailMode`). 한계는 다음과 같습니다.
> - 대기열은 메모리에만 있어 장애 중 프로세스가 죽으면 그 몫은 잃습니다. 정상 종료 시에는 최대 10초 동안 비우기를 시도하고, 남은 기록은 잃었다고 로그에 남깁니다.
> - 대기열 상한은 100,000건이며, 넘친 기록은 버리고 `vigilante_store_errors_total{reason="dropped"}`로 셉니다.
> - HA에서 장애가 `lease_ttl`보다 길면 리더가 물러나 판정이 멈추고, 다른 노드가 리더가 되면 이 노드의 대기열은 펜싱되어 버려집니다.
> - lease 없이 진행한 롤백 동안에는 다른 프로세스(다른 CI 잡, 다른 서버)가 같은 서비스에 조치할 수 있습니다. 저장소가 돌아왔을 때 다른 프로세스가 lease를 잡고 있으면 `lease.conflict` 경고가 갑니다. 이를 허용하지 않으려면 `on_unavailable: fail`을 쓰십시오.

## 8.2 구성 절차

1. PostgreSQL에 전용 DB와 계정(스키마 소유자)을 만듭니다. 다른 DB 권한은 주지 않습니다.
2. DSN을 Vault 또는 환경변수에 둡니다. `sslmode=verify-full`을 권장합니다.
3. 노드 2~3대에 패키지를 설치하고 같은 설정을 둡니다.
   ```yaml
   server:
     listen: ":8088"
     state: {backend: postgres, dsn_ref: "vault:secret/prod/vigilante#dsn"}
     ha: {enabled: true, lease_ttl: 15s}
     tls: {cert_file: /etc/vigilante/tls/server.crt, key_file: /etc/vigilante/tls/server.key}
   console:
     session_key_ref: "vault:secret/prod/vigilante#console_session_key"   # 모든 노드 동일
   ```
4. 노드마다 다른 값인 `advertise_url`은 `/etc/vigilante/vigilante.env`에 둡니다.
   ```bash
   VIGILANTE_HA_ADVERTISE_URL=https://vigilante-1.internal:8088
   ```
5. 스키마를 DBA가 통제해야 하면 `server.state.auto_migrate: false`로 두고 새 바이너리로 `vigilante store migrate -c FILE`을 먼저 실행합니다(15장).
6. 노드를 하나씩 시작합니다. 로그에 `elected leader`가 한 노드에서만 나와야 합니다.
7. 앞단 로드밸런서의 헬스 체크를 `GET /readyz`로 설정합니다.
8. 확인합니다.
   ```bash
   curl -fsS https://vigilante-1.internal:8088/healthz   # role, leader, active, circuit, store
   curl -fsS https://vigilante-2.internal:8088/readyz    # {"ready":true,"checks":{"store":"ok","leader":"..."}}
   vigilante store status -c /etc/vigilante/vigilante.yaml
   ```

## 8.3 상태 확인 지표

| 확인 대상 | 방법 |
|---|---|
| 리더 노드 | `GET /healthz`의 `role`(`leader`, `follower`)과 `leader`, 지표 `vigilante_leader` |
| 판정·롤백 가능 여부 | `GET /healthz`의 `active`, 지표 `vigilante_engine_active` |
| 리더 인지 여부 | `GET /readyz`의 `checks.leader`(`self`, 노드 이름, `none`) |
| 펜싱 발생 | `vigilante_store_errors_total{reason="fenced"}` |

> `ha.lease_ttl`을 짧게 하면 전환이 빨라지지만 DB 순간 지연에도 리더가 바뀔 수 있습니다. 개발 문서의 실측값은 TTL 3초 설정에서 리더 강제 종료 후 4.1초 만의 전환입니다.

---

# 9. 대상 호스트 최소 권한 (sudo)

## 9.1 원칙

`connection.sudo: true`만 두면(`sudo_scope: all`, 기본값) 모든 명령을 `sudo -n sh -c '<명령>'`로 실행하므로 sudoers에 사실상 무제한 권한이 필요하고, 그 호스트의 root와 같습니다. `validate`와 `doctor`가 이를 경고합니다. `sudo_scope: changes`이면 읽기 명령은 sudo 없이, 바꾸는 명령만 하나씩 `sudo -n`으로 실행하므로 그 명령들만 허용하면 됩니다.

## 9.2 설정 절차

1. 대상 호스트에 전용 계정(예: `vigilante`)을 만들고 SSH 인증을 설정합니다(Vault SSH CA 권장).
2. 대상의 연결 설정을 바꿉니다.
   ```yaml
   targets:
     - {name: order-01, address: 10.0.1.11, connection: {type: ssh, credential: ssh-vigilante, sudo: true, sudo_scope: changes}}
   ```
3. 필요한 sudoers 규칙을 생성합니다. 대상에 접속해 명령 경로까지 확인합니다(접속하지 않으려면 `--no-resolve`).
   ```bash
   vigilante sudoers -c vigilante.yaml --target order-01 > vigilante.sudoers
   ```
4. 대상 호스트에서 문법을 확인하고 설치합니다.
   ```bash
   visudo -cf vigilante.sudoers && install -m 0440 vigilante.sudoers /etc/sudoers.d/vigilante
   ```
5. 규칙마다 허용 여부를 확인합니다. `doctor`는 `sudo -n -l`로 확인만 하고 실행하지 않습니다.
   ```bash
   vigilante doctor -c vigilante.yaml
   ```

생성되는 규칙의 예(symlink 실행기, systemd)는 다음과 같습니다.

```
Defaults:vigilante !requiretty
vigilante ALL=(root) NOPASSWD: /usr/bin/ln -sfn /opt/order/releases/* /opt/order/current.vigilante-tmp
vigilante ALL=(root) NOPASSWD: /usr/bin/mv -Tf /opt/order/current.vigilante-tmp /opt/order/current
vigilante ALL=(root) NOPASSWD: /usr/bin/systemctl restart order-api
```

## 9.3 유의 사항

- 직접 쓴 명령(`exec` 실행기, `restart_cmd`, 바꾼 `test_cmd`·`reload_cmd`)은 쓴 그대로 실행됩니다. 필요하면 명령 안에 `sudo -n`을 넣고 규칙을 직접 추가하십시오.
- sudoers의 `*`는 공백을 포함한 임의 문자열과 맞습니다. 더 엄격하게 하려면 고정 인자를 받는 래퍼 스크립트만 허용하고 `restart_cmd`로 부르십시오.
- HAProxy는 sudo 대신 runtime API 소켓 권한(`stats socket /run/haproxy/admin.sock mode 660 group vigilante level admin`)을 씁니다.
- Docker 소켓 접근은 root와 같습니다. 가능하면 rootless Podman을 쓰십시오.
- KVM은 하이퍼바이저의 `libvirt` 그룹으로 `virsh snapshot-*`, `domstate`를 허용합니다.
- 외부 시스템 계정(F5, vCenter, Nutanix, OpenStack, AWS, ServiceNow, PostgreSQL)도 대상 범위에 한정한 최소 역할을 주십시오.

---

# 10. SSH 세션 예산

sshd는 연결당 세션 수를 제한합니다(OpenSSH `MaxSessions` 기본 10). 로그 스트림은 실행되는 동안 세션을 하나씩 쥐므로, 로그 프로브가 많으면 롤백 명령이 세션을 얻지 못할 수 있습니다. Vigilante는 대상마다 세션 예산을 두고 그중 일부를 롤백 전용으로 예약합니다.

| 키 | 기본값 | 설명 |
|---|---|---|
| `connection.max_sessions` | `8` | 이 대상에 동시에 여는 SSH 세션 상한. sshd `MaxSessions`보다 작게 |
| `connection.reserved_sessions` | `2` | 그중 롤백 단계, 트래픽 드레인·복귀만 쓰는 몫 |

수집 작업은 `max_sessions - reserved_sessions`까지만 쓰고, 모자라면 실패하지 않고 기다립니다. 대기는 `vigilante_ssh_session_waits_total{priority="normal"}` 또는 `{priority="urgent"}`로 집계됩니다.

조정 절차는 다음과 같습니다.

1. `vigilante doctor`의 `용량` 항목 "SSH 세션 수"를 봅니다. 상시 로그 스트림 수가 수집 몫 이상이면 `[FAIL]`, 주기 명령까지 합쳐 넘으면 `[WARN]`입니다.
2. 대상의 sshd `MaxSessions`를 올리고(예: 20), `connection.max_sessions`를 그보다 작게 올립니다. `max_sessions`가 10을 넘으면 doctor가 sshd 설정을 함께 바꾸라고 경고합니다.
3. 또는 로그가 많은 대상은 에이전트 모드로 전환하거나 `log.remote_grep`으로 대상에서 먼저 걸러 냅니다.
4. 적용 후 `vigilante_ssh_session_waits_total`의 증가율을 확인합니다.

---

# 11. 변경 동결

## 11.1 설정 파일의 동결 창

```yaml
change_freeze:
  - name: weekend
    weekly: {from: "fri 18:00", to: "mon 09:00", timezone: Asia/Seoul}
    services: [billing-api]
  - name: year-end-closing
    reason: 결산 기간
    start: "2026-12-28T00:00:00+09:00"
    end: "2027-01-02T00:00:00+09:00"
    teams: [payments]                  # services·teams 생략 시 전 서비스
    allow_rollback: true               # 기본 true
```

- 동결 중인 서비스의 새 배포 등록과 단계 관측 시작을 거부합니다. API는 `409 change_frozen`, CLI(`watch`, `prepare`)는 종료 코드 3입니다(`watch --server`에서 서버가 거부해도 3).
- 자동 롤백은 기본으로 허용합니다. `allow_rollback: false`인 기간에는 자동 롤백 대신 실패 대상을 격리하고 사람에게 넘깁니다. 수동 롤백은 항상 가능합니다.
- 주간 창의 시각은 `timezone`(생략 시 서버 지역 시간) 기준입니다.

## 11.2 실행 중 동결 선언

장애 대응처럼 설정 변경 없이 동결해야 하면 admin이 API(또는 콘솔 "변경 동결" 화면)로 선언합니다. 선언은 상태 저장소에 남아 리더가 바뀌어도 유지됩니다.

```bash
curl -X POST "$API/v2/freezes" -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"incident-123","reason":"장애 대응 중","ends_at":"2026-10-12T09:00:00Z","teams":["payments"],"allow_rollback":true}'
curl "$API/v2/freezes" -H "Authorization: Bearer $ADMIN_TOKEN"                       # 설정 창 + 선언된 동결
curl -X DELETE "$API/v2/freezes/<id>" -H "Authorization: Bearer $ADMIN_TOKEN"        # 조기 종료
```

긴급 배포 예외 처리는 운영자 매뉴얼(VGL-OP-02)의 변경 동결 업무 절차를 참고하십시오.

---

# 12. ServiceNow 연동

## 12.1 설정 절차

1. ServiceNow에 통합 사용자를 만들고 `change_request` 읽기·쓰기(work notes), `incident` 읽기·생성 권한만 줍니다.
2. 자격증명을 등록합니다(`basic` 또는 OAuth `token`).
3. `itsm` 절을 작성합니다.
   ```yaml
   itsm:
     servicenow:
       url: https://company.service-now.com
       credential: snow-integration
       change_gate:
         enabled: true
         teams: [payments]                 # services·teams 생략 시 전 서비스
         allowed_states: ["-2", "-1"]      # Scheduled, Implement (기본)
         check_window: true                # 계획된 작업 시간 안이어야 함 (기본)
         on_error: closed                  # closed(기본) | open
       incidents:
         enabled: true
         on: [rollback_failed, circuit_opened]
         assignment_group: SRE
         urgency: 1
         impact: 2
       work_notes: true
   ```
4. 서버를 재시작하고, 게이트가 적용되는 서비스로 시험 배포를 등록하여 티켓 검증을 확인합니다.

## 12.2 동작

| 기능 | 동작 |
|---|---|
| 변경 티켓 게이트 | 새 배포는 승인(`approval: approved`)되고 허용 상태이며 계획 시간 안인 변경 번호가 필요. API는 `X-Change-Ticket` 헤더나 본문 `change_ticket`(v1·v2), CLI는 `--ticket`(`watch --server`도 서버로 전달). 거부 시 API `409 change_ticket_invalid`, CLI 종료 코드 3. 단계를 시작할 때마다 다시 확인 |
| 장애 시 정책 | `on_error: closed`면 `503 itsm_unavailable`(Retry-After)로 거부, `open`이면 진행하되 `change_ticket.unverified: true` 표시 |
| 인시던트 | 롤백 실패와 서킷 열림 때 백그라운드로 생성. `correlation_id`로 중복 방지(응답을 못 받은 생성이 실제로 저장됐어도 재시도가 찾아냄). 감사 기록 `itsm.incident` |
| 작업 노트 | 검증된 티켓이 있는 배포의 진행 결과를 변경 티켓 work notes에 기록 |

- 롤백은 어떤 경우에도 ServiceNow를 기다리지 않습니다.
- 인시던트와 작업 노트는 서버 리더가 처리합니다. CI 단발 실행만 쓰는 구성에서는 게이트만 동작합니다.
- 일시 오류(접속 오류, 시간 초과, 429, 5xx)는 1초, 2초, 4초 뒤 최대 세 번 다시 시도합니다(처음 시도를 합쳐 최대 4회). 그 밖의 4xx(인증 실패, 필드 거부)는 다시 시도하지 않습니다. 모두 실패하면 로그 `servicenow call failed`(시도 횟수 포함)를 남기고 나중에 다시 만들지 않습니다.
- 호출 결과는 `vigilante_itsm_calls_total{kind,result}`로 집계됩니다. `result`는 호출마다 `ok` 또는 재시도 후 `error`이고, 다시 시도할 때마다 `retry`가 하나씩 늡니다.

---

# 13. 알림

```yaml
notify:
  - {type: teams, url_ref: "vault:secret/prod/teams#payments_webhook", teams: [payments], min_level: warning}
  - type: email
    min_level: critical
    smtp: {host: smtp.example.internal, from: vigilante@example.internal, to: [sre@example.internal], username: vigilante, password_ref: "vault:secret/prod/smtp#password"}
  - {type: pagerduty, routing_key_ref: "vault:secret/prod/pagerduty#sre", min_level: critical}
  - {type: slack, url_env: SLACK_WEBHOOK_URL, min_level: warning}
```

| 키 | 설명 |
|---|---|
| `type` | `slack`, `teams`, `email`, `pagerduty`, `webhook` |
| `url`, `url_env`, `url_ref` | slack·teams·webhook 대상 URL. URL에 비밀이 들어가므로 `url_ref` 권장 |
| `min_level` | `info`, `warning`, `critical` |
| `services`, `teams` | 이 채널로 보낼 서비스·팀. 서비스가 없는 알림(서킷 등)은 모든 채널로 전송 |
| `smtp` | `host`, `port`(기본 587 STARTTLS, 465는 TLS), `from`, `to`, `username`, `password_ref`, `implicit_tls`, `no_starttls` |
| `routing_key_ref`, `routing_key_env` | PagerDuty integration key |

- 같은 배포의 같은 알림은 채널마다 10분에 한 번만 보냅니다.
- 알림 실패는 기록만 하고 롤백을 막지 않습니다.
- 권장 구성: 롤백 실패·서킷 열림·승인 요청(모두 critical)은 PagerDuty나 온콜 채널로, 롤백 완료(warning)는 팀 채널로 보냅니다.

## 13.1 이벤트 웹훅 구독

사내 시스템이 이벤트를 받아야 하면 v2 웹훅 구독을 씁니다. 먼저 서명 마스터 키를 설정합니다.

```yaml
api:
  webhook_signing_key_ref: "vault:secret/vigilante/webhooks#key"   # 없으면 웹훅 비활성
  webhook_allowed_hosts: [".example.internal"]                     # 허용 호스트(접미사)
```

```bash
curl -X POST "$API/v2/webhooks" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{
  "url": "https://incident-bot.example.internal/vigilante",
  "types": ["vigilante.rollback.failed", "vigilante.circuit.opened"],
  "teams": ["payments"]
}'
```

응답의 `secret`(`whsec_…`)은 한 번만 보입니다. 실패하면 1초, 5초, 30초, 2분, 10분, 30분 뒤 재시도하고, 연속 5건이 dead-letter가 되면 구독을 끄고 운영 알림을 보냅니다. 마스터 키를 바꾸면 모든 구독의 비밀이 바뀌므로 각 구독에 `POST /v2/webhooks/{id}/secret`으로 새 비밀을 받아 수신 측에 전달하십시오.

---

# 14. 모니터링

## 14.1 헬스 엔드포인트

세 엔드포인트는 각 노드가 직접 답하며 리더로 전달하지 않습니다.

| 엔드포인트 | 용도 | 응답 |
|---|---|---|
| `GET /healthz` | 생존 확인. 프로세스가 응답하면 200(인증 불필요) | `ok`, `role`(`single`, `leader`, `follower`), `leader`, `active`, `circuit`, `dry_run`, `store` |
| `GET /readyz` | 준비 확인. LB·Kubernetes가 트래픽 판단 | 저장소 Ping 실패 또는 HA에서 리더를 모르면 503. `{"ready":..,"checks":{"store":..,"leader":..}}` |
| `GET /metrics` | Prometheus 텍스트 형식 | 14.2절 지표. 기본은 `viewer@*` 토큰 필요 |

> 파일 저장소의 `/readyz` 저장소 검사는 저널 파일이 존재하는지만 확인합니다. 디스크가 가득 차 기록이 실패해도 `/readyz`는 200일 수 있으므로 `vigilante_store_errors_total`과 `vigilante_store_pending_writes`를 함께 감시하십시오.

## 14.2 지표 목록

레이블에는 대상 이름이나 배포 ID를 넣지 않습니다.

| 지표 | 종류 | 레이블 | 의미 |
|---|---|---|---|
| `vigilante_probe_samples_total` | counter | type | 중앙 프로브가 수집한 샘플 수 |
| `vigilante_probe_restarts_total` | counter | type | 오류로 재시작한 프로브 수 |
| `vigilante_agent_samples_total` | counter | 없음 | 에이전트가 보낸 샘플 수 |
| `vigilante_evaluation_seconds` | histogram | 없음 | 단계 규칙 1회 평가 시간 |
| `vigilante_verdicts_total` | counter | service, phase, verdict | 단계 판정 결과 |
| `vigilante_rollback_trigger_seconds` | histogram | 없음 | 실패 판정부터 롤백 시작 기록까지 |
| `vigilante_rollbacks_total` | counter | service, result | `rolled_back`, `failed`, `await_approval`, `blocked`, `handed_over` |
| `vigilante_rollback_approvals_total` | counter | service, decision | `requested`, `approved`, `rejected`, `expired` |
| `vigilante_rollback_duration_seconds` | histogram | result | 롤백 플랜 실행 시간 |
| `vigilante_store_append_seconds` | histogram | backend | 상태 저장 지연 |
| `vigilante_store_errors_total` | counter | reason | `fenced`(리더 상실), `error`(쓰기 실패, 재시도 대기열로), `dropped`(대기열 가득 참) |
| `vigilante_store_pending_writes` | gauge | 없음 | 저장소 장애 동안 메모리에 쌓인 기록 수 |
| `vigilante_observer_degraded` | gauge | 없음 | 이 서버 자신의 측정을 믿을 수 없는 동안 1(4.11절 `observer_guard`) |
| `vigilante_observer_degradations_total` | counter | signal | 관측 장치가 과부하 상태가 된 횟수. 첫 신호별(`scheduling lag`, `loopback`, `spread`) |
| `vigilante_observer_holds_total` | counter | 없음 | 관측 장치 과부하 때문에 롤백 대신 보류로 바꾼 규칙 위반 수 |
| `vigilante_ssh_connections` | gauge | 없음 | 풀에 있는 SSH 연결 수 |
| `vigilante_ssh_sessions` | gauge | 없음 | 열린 SSH 세션 수 |
| `vigilante_ssh_dials_total` | counter | result | SSH 접속 시도(`ok`, `error`) |
| `vigilante_ssh_session_waits_total` | counter | priority | 세션을 기다린 명령 수(`normal`, `urgent`) |
| `vigilante_api_requests_total` | counter | method, route, code | API 요청 수 |
| `vigilante_api_request_seconds` | histogram | route | API 요청 지연 |
| `vigilante_itsm_calls_total` | counter | kind, result | ServiceNow 호출 결과(`ok`, `error`, `retry`) |
| `vigilante_audit_exported_total` | counter | 없음 | SIEM 전송 건수 |
| `vigilante_audit_export_dropped_total` | counter | 없음 | SIEM 미전송 건수 |
| `vigilante_circuit_state` | gauge | state | 현재 서킷 상태가 1(`closed`, `open`, `half_open`) |
| `vigilante_deployments` | gauge | state | 상태별 배포 수 |
| `vigilante_leader` | gauge | 없음 | HA 리더이면 1(HA가 아니면 항상 1) |
| `vigilante_engine_active` | gauge | 없음 | 판정·롤백 가능하면 1 |
| `vigilante_dry_run` | gauge | 없음 | 드라이런이면 1 |
| `vigilante_agents_connected` | gauge | 없음 | 30초 안에 하트비트를 보낸 에이전트 수 |
| `vigilante_build_info` | gauge | version, flavor, go_version | 빌드 정보 |
| `process_start_time_seconds`, `go_goroutines`, `go_memstats_heap_alloc_bytes` | gauge | 없음 | 프로세스 정보 |

## 14.3 Prometheus 수집 설정

1. `viewer@*` 서비스 계정을 만듭니다.
   ```bash
   vigilante token create --name prometheus --role viewer --scope '*' --expires 2027-06-30
   ```
2. 토큰을 Prometheus 서버의 파일에 두고 수집 작업을 추가합니다.
   ```yaml
   scrape_configs:
     - job_name: vigilante
       authorization: {credentials_file: /etc/prometheus/vigilante-token}
       static_configs: [{targets: ["vigilante-1:8088", "vigilante-2:8088"]}]
   ```
3. Kubernetes에서 Prometheus Operator를 쓰면 Helm 값 `serviceMonitor.enabled: true`를 켭니다. 이때 `server.metrics_public: true`를 설정하거나 수집 작업에 토큰을 주어야 합니다.

## 14.4 권장 경보 규칙 예

아래는 저장소의 지표 이름으로 작성한 예시이며, 임계치는 조직에 맞게 조정하십시오.

```yaml
groups:
  - name: vigilante
    rules:
      - alert: VigilanteCircuitOpen
        expr: vigilante_circuit_state{state="open"} == 1
      - alert: VigilanteRollbackFailed
        expr: increase(vigilante_rollbacks_total{result="failed"}[10m]) > 0
      - alert: VigilanteStoreWritesQueued
        expr: vigilante_store_pending_writes > 0
        for: 1m
      - alert: VigilanteStoreWritesDropped
        expr: increase(vigilante_store_errors_total{reason="dropped"}[5m]) > 0
      - alert: VigilanteNoActiveEngine
        expr: max(vigilante_engine_active) == 0
        for: 1m
      - alert: VigilanteSSHUrgentWaits
        expr: increase(vigilante_ssh_session_waits_total{priority="urgent"}[10m]) > 0
      - alert: VigilanteAuditExportDropped
        expr: increase(vigilante_audit_export_dropped_total[15m]) > 0
      - alert: VigilanteObserverDegraded
        expr: vigilante_observer_degraded == 1
        for: 1m
      - alert: VigilanteObserverHolds
        expr: increase(vigilante_observer_holds_total[10m]) > 0
      - alert: VigilanteITSMErrors
        expr: increase(vigilante_itsm_calls_total{result="error"}[15m]) > 0
```

`lease.unavailable`·`lease.conflict`(저장소 장애 중 롤백)와 `breakglass.*`(로컬 CLI 비상 실행)는 지표가 아니라 알림(각각 warning, critical)과 감사 기록으로 남습니다. 알림 채널(13장)의 `min_level`이 이를 받도록 두고, 감사 기록을 정기적으로 조회하십시오(`vigilante audit query --action breakglass.circuit.reset` 등).

## 14.5 로그

- `VIGILANTE_LOG_FORMAT=json`이면 한 줄에 JSON 객체 하나로 출력합니다(기본 text). Helm 차트는 json을 씁니다.
- `VIGILANTE_LOG=debug` 또는 `warn`으로 수준을 바꿉니다.
- 배포 관련 로그에는 `deployment`, API 요청 로그에는 `request_id`가 붙습니다. 팔로워가 리더로 전달할 때 같은 값을 넘기므로 두 노드의 로그를 한 요청으로 묶을 수 있습니다.
- systemd 환경에서는 `journalctl -u vigilante-server`로 봅니다.

---

# 15. 감사와 SIEM 연동

## 15.1 감사 기록의 내용

모든 판정과 조치는 상태 저장소에 기록되고, 이 기록이 곧 감사 기록입니다. 기록에는 작업자(`actor`: `user:alice`, `sa:ci-order`, `client:<이름>`, `cli:<사용자>@<호스트>`, 자동 조치는 `system`), 출처(`source`: api, cli, webhook, system, ui), 동작(`action`), 서비스·배포, 사유, 변경 티켓이 남습니다. 권한 거부(403)도 `action: denied`로 남습니다. 기록은 직전 기록의 해시를 포함하는 해시 체인(SHA-256)으로 묶여 있어, 한 건을 고치거나 지우면 `audit verify`가 검출합니다. 단, 해시 체인만으로는 저장소 쓰기 권한이 있는 사람이 변조 지점 이후의 체인을 모두 다시 계산하는 것을 막지 못합니다. 이를 막으려면 체인 키(15.2절 `audit.chain_key_ref`)를 설정하십시오. 체인 키 없이 운영하면 `audit verify`가 표준 출력에 내는 JSON의 `head`(마지막 기록의 해시)를 외부에 보관하거나 SIEM 사본과 대조해 보완하십시오.

## 15.2 SIEM 전송 설정

```yaml
audit:
  syslog:
    address: tls://siem.example.internal:6514   # tls://(RFC 5425, 포트 생략 시 6514) | tcp:// | udp://
    format: rfc5424                              # rfc5424(JSON 본문, 기본) | cef
    tls:                                         # tls:// 전용. 없으면 시스템 루트 CA로 검증
      ca_file: /etc/vigilante/siem-ca.pem        # 수집기 인증서의 사설 CA
      cert_file: /etc/vigilante/siem-client.crt  # 수집기가 클라이언트 인증서를 요구할 때(key_file과 함께)
      key_file: /etc/vigilante/siem-client.key
      server_name: siem.example.internal         # 기본은 address의 호스트
      min_version: "1.2"                         # 1.2(기본) | 1.3
  chain_key_ref: vault:secret/prod/vigilante#audit_chain_key   # 체인 키(선택). env:NAME, file:/path도 가능
  retention: 8760h                               # audit prune의 보존 기간 (기본값 없음, 예시 값)
```

- `tls://`는 RFC 5425 형식(`길이 공백 메시지`)으로 보내고 수집기 인증서와 이름을 검증합니다. 검증에 실패하면 보내지 않고, 로그 `SIEM unreachable; audit entries queue up`(오류 내용 포함)을 남기며 최대 1분 간격으로 다시 접속합니다. `audit.syslog.tls`는 `tls://` 주소에서만 쓸 수 있고, `tls://`에는 호스트 이름이 있어야 합니다(`validate`가 검사).
- `tcp://`·`udp://`는 평문(줄 단위)이므로 신뢰 망 안에서만 쓰십시오.
- `audit.retention`에는 기본값이 없습니다. 비워 두면 `audit prune`에 `--before` 또는 `--older-than`을 반드시 주어야 합니다.
- 저장된 뒤 비동기로 보냅니다. SIEM이 느리거나 끊겨도 롤백을 막지 않으며, 큐가 가득 차면 버리고 `vigilante_audit_export_dropped_total`로 셉니다.
- 빠진 구간은 `vigilante audit export`로 채웁니다.
- 서버 시작 로그에 `audit records exported`가 전송 대상과 함께 표시됩니다.

**체인 키(`chain_key_ref`):** 설정하면 새 기록마다 체인 해시의 HMAC-SHA256(`mac`)이 붙습니다. 키가 없으면 MAC을 만들 수 없으므로, 기록을 고치고 체인을 다시 계산해도 `audit verify`가 MAC 불일치·누락으로 검출합니다.

- 키는 32바이트 이상이어야 하고, 같은 저장소에 쓰는 모든 노드가 같은 키를 씁니다. 키는 저장소 쓰기 권한이 있는 계정(DB 관리자, 저널 파일 소유자)이 읽을 수 없는 곳(Vault 권장)에 두십시오.
- 키를 설정하기 전 기록은 `unkeyed`로 세고 변조로 보지 않습니다. 검증 결과의 `keyed_from`이 키가 보호하기 시작한 위치입니다. 처음 설정한 뒤 이 값을 SIEM이나 변경 티켓에 남겨 두십시오. 누군가 MAC을 모두 지우면 체인은 키 없이 쓴 것처럼 보이며(`audit verify`가 경고), 이때 기록해 둔 위치와 비교해 검출합니다.
- PostgreSQL은 기록 본문(JSON)에 MAC을 담으므로 스키마 마이그레이션이 필요 없습니다.
- 키를 설정하면 상태 저장소를 여는 모든 명령(서버, CLI)이 그 키를 해석할 수 있어야 합니다(`dsn_ref`와 같음).
- **키 교체는 지원하지 않습니다.** 키를 바꾸면 이전 키로 쓴 기록(앵커 포함)이 MAC 불일치로 보고됩니다. 유출이 의심될 때만 바꾸고, 바꾸기 전에 `audit export`로 아카이브를 남겨 이전 키로 검증해 두십시오(`audit verify --file F --key REF`).

## 15.3 감사 명령

| 명령 | 하는 일 |
|---|---|
| `vigilante audit verify -c FILE [--key REF]` | 저장소 전체 체인 검증(`chain_key_ref`가 있거나 `--key`를 주면 MAC도). 끊어지면 종료 코드 1 |
| `vigilante audit verify --file ARCHIVE.jsonl [-c FILE \| --key REF]` | 아카이브 파일만 검증. `-c`나 `--key`를 주면 MAC도 검증 |
| `vigilante audit query -c FILE [--actor A] [--action denied] [--service S] [--since 2026-10-01] [--limit 200]` | 감사 기록 조회 |
| `vigilante audit export -c FILE --out F.jsonl` | 전체 기록을 체인 그대로 내보내기 |
| `vigilante audit prune -c FILE --out ARCHIVE.jsonl [--before 2025-10-01 \| --older-than 8760h]` | 보존 기간이 지난 기록을 아카이브로 옮기고 삭제. 둘 다 없으면 `audit.retention`을 쓰며, 그것도 없으면 오류 |

API로는 `GET /v2/audit-events`(viewer@* 와 `audit:read`) 또는 `GET /v1/audit?format=csv`로 조회합니다.

## 15.4 정기 점검 절차

1. 매일 또는 매주 `vigilante audit verify -c /etc/vigilante/vigilante.yaml`을 실행하고 종료 코드를 감시합니다. 정상이면 표준 오류에 `audit chain of ... intact: N entries verified`가 나옵니다. 체인 키를 쓰면 이어서 `chain key protects the chain from entry <keyed_from> on: <N> MACs verified`가 나오므로, 위치가 처음 기록해 둔 `keyed_from`과 같은지 확인합니다. 키가 없으면 `no chain key: MACs not checked ...`, MAC이 하나도 없으면 `WARNING: no entry carries a MAC ...`가 나옵니다.
2. `vigilante audit query --action denied --since <날짜>`로 반복되는 권한 거부를 확인합니다.
3. 보존 기간이 지나면 `audit prune`으로 아카이브합니다. 파일 저장소는 서버가 그 파일을 쓰지 않을 때(서버 정지 후) 실행하십시오. 아카이브 파일은 `audit verify --file`로 따로 검증할 수 있으며, 자동 삭제는 하지 않습니다.

---

# 16. 상태 저장소 백업과 복구

Vigilante에는 전용 백업·복원 명령이 없습니다. 아래 절차는 파일 저장소와 PostgreSQL의 일반적인 방법을 제품 동작에 맞춰 정리한 것입니다.

## 16.1 파일 저장소

저널은 한 줄에 기록 하나인 JSONL이며, 기록할 때마다 fsync합니다.

백업 절차는 다음과 같습니다.

1. 가능하면 서버를 멈춥니다(`systemctl stop vigilante-server`). 멈출 수 없으면 복사본의 마지막 줄이 잘릴 수 있으므로 3단계 검증을 반드시 합니다.
2. 저널을 복사합니다.
   ```bash
   cp /var/lib/vigilante/journal.jsonl /backup/vigilante/journal-$(date +%Y%m%d%H%M).jsonl
   ```
3. 복사본의 체인을 검증합니다.
   ```bash
   vigilante audit verify --file /backup/vigilante/journal-202610111200.jsonl
   ```
4. 서버를 멈췄다면 다시 시작합니다.

복구 절차는 다음과 같습니다.

1. 서버를 멈춥니다.
2. 현재 저널을 보관용으로 옮겨 두고 백업 파일을 `journal_path` 위치에 복사합니다. 소유자를 `vigilante:vigilante`로 맞춥니다.
3. `vigilante audit verify -c /etc/vigilante/vigilante.yaml`과 `vigilante status -c /etc/vigilante/vigilante.yaml`로 확인합니다.
4. 서버를 시작합니다.

## 16.2 PostgreSQL

1. 정기적으로 `pg_dump`(또는 조직의 PostgreSQL 백업 체계)로 전용 DB를 백업합니다.
   ```bash
   pg_dump -Fc -d "$VIGILANTE_PG_DSN" -f /backup/vigilante/vigilante-$(date +%Y%m%d).dump
   ```
2. 업그레이드와 스키마 되돌리기 전에는 반드시 백업합니다(17장).
3. 복원은 모든 서버를 멈춘 뒤 `pg_restore`로 하고, `vigilante store status`, `vigilante audit verify`로 확인한 다음 서버를 시작합니다.

> 백업 시점 이후의 기록(판정, 롤백 단계, 서킷 전이, 감사 기록)은 복원하면 사라집니다. 진행 중이던 배포가 있었다면 복원 후 `vigilante status`로 상태를 확인하고 필요하면 수동으로 정리하십시오. PostgreSQL 백업·복원 도구의 구체적 옵션은 제품에서 검증되지 않았습니다(확인 필요).

---

# 17. 업그레이드와 다운그레이드

## 17.1 버전 정책

릴리스 번호는 SemVer `MAJOR.MINOR.PATCH`입니다. PATCH는 버그·보안 수정, MINOR는 기능 추가(기존 설정·API·스크립트 유지, 스키마 변경은 추가만), MAJOR는 호환되지 않는 변경이 있을 수 있습니다. CLI 종료 코드(0, 1, 2, 3, 4), 설정 `version: v1`, REST API v2, 지표 이름은 같은 MAJOR 안에서 유지됩니다. 에이전트는 서버보다 한 MINOR 낮은 버전까지 지원하므로 서버를 먼저 올립니다.

## 17.2 업그레이드 전 점검

1. 릴리스 노트와 `CHANGELOG.md`의 "Upgrade notes"를 읽습니다.
2. 서명과 체크섬을 확인합니다(3.2절).
3. 새 바이너리로 현재 설정을 검사하고 새 경고를 먼저 정리합니다.
   ```bash
   ./vigilante_<new>_linux_amd64 validate -c /etc/vigilante/vigilante.yaml
   ```
4. PostgreSQL을 쓰면 백업하고 스키마 상태를 봅니다. 종료 코드 4는 적용 대기 또는 더 새로운 스키마가 있다는 뜻입니다.
   ```bash
   vigilante store status -c /etc/vigilante/vigilante.yaml
   ```
5. 진행 중인 롤백이 없는 시간을 고릅니다(`GET /v2/deployments?state=ROLLING_BACK`).

## 17.3 단일 노드 업그레이드

```bash
dnf upgrade ./vigilante-<new>.x86_64.rpm      # 또는 apt install ./vigilante_<new>_amd64.deb
# 폐쇄망 번들: 압축을 푼 디렉토리에서 ./install.sh
vigilante version && curl -fsS localhost:8088/readyz
```

실행 중인 서버는 패키지 설치 후 자동으로 재시작됩니다. 재시작 동안(수 초) API가 응답하지 않습니다. 이미 등록된 배포를 기다리는 `vigilante watch --server`는 2초마다 상태 조회를 다시 시도하지만, 재시작 중에 새 배포 등록 요청을 보낸 `watch --server`는 재시도하지 않고 종료 코드 1로 끝나므로 파이프라인이 없는 시간에 올리십시오.

## 17.4 HA 순차 업그레이드

1. 아무 노드의 `GET /healthz`로 리더를 확인합니다.
2. 팔로워를 한 대씩 올리고 `/readyz`가 200이 될 때까지 기다립니다. 처음 올라온 새 버전 노드가 추가 마이그레이션을 적용합니다.
3. 마지막으로 리더를 올립니다. 리더는 종료할 때 리스를 즉시 놓으므로 새 버전 팔로워가 몇 초 안에 이어받습니다.
4. `vigilante store status`가 종료 코드 0으로 끝나는지 확인합니다.

`server.state.auto_migrate: false`이면 2단계 전에 새 바이너리로 `vigilante store migrate -c FILE`을 한 번 실행합니다. Helm은 `helm upgrade vigilante ./vigilante-<new>.tgz -f my-values.yaml`로 롤링 업데이트됩니다.

Helm 차트를 PR #13 이전 차트에서 올릴 때는 다음을 먼저 확인합니다.

- 차트는 `config`에 인증 설정이 없으면 렌더링을 거부합니다. 인증 없이 쓰던 설치는 업그레이드 전에 `auth`를 설정하거나, 개발용이면 `auth.allowAnonymous: true`를 줍니다.
- 메모리 기본값이 요청 512Mi·한도 2Gi로 바뀌고 `GOMEMLIMIT`이 들어갑니다. values에 메모리 한도를 직접 준 설치는 그 값을 그대로 쓰므로 2.2절의 크기 기준과 비교합니다.
- `config`에 `server.tls.cert_file`이 있으면 포트 이름이 `https`로 바뀌고 프로브·HA 알림 주소·ServiceMonitor가 https를 씁니다. 포트 이름 `http`를 참조하던 외부 설정(Ingress 주석, 모니터링)이 있으면 함께 고칩니다. `server.tls.client_auth: require`는 거부되므로 `optional`로 바꿉니다.

## 17.5 다운그레이드

- PATCH·MINOR는 이전 버전 패키지를 설치하면 됩니다(`dnf downgrade`, `apt install vigilante=<old>`, `helm rollback`).
- 호환되지 않는 스키마 변경(`-- vigilante:breaking`)이 들어간 버전에서 내려가면 이전 버전은 다운그레이드 가드로 시작을 거부합니다. 되돌리는 절차는 다음과 같습니다.

1. 모든 서버를 멈춥니다.
2. PostgreSQL을 백업합니다.
3. 새 버전 바이너리로 스키마를 되돌립니다. 되돌릴 수 없는 마이그레이션이 하나라도 있으면 아무것도 바꾸지 않고 실패합니다.
   ```bash
   vigilante store migrate -c /etc/vigilante/vigilante.yaml --down-to 6 --yes   # 6은 예시: 이전 버전이 아는 마지막 스키마 번호
   ```
   현재 저장소의 마이그레이션은 `001_init.sql` 하나뿐이므로, 이 절차는 이후 릴리스에서 스키마가 추가된 뒤에 쓰입니다.
4. 이전 버전을 설치하고 서버를 시작합니다.

첫 마이그레이션(테이블 생성)은 감사 기록이 있으므로 되돌리지 않습니다.

---

# 18. 지원 번들

문제를 개발팀에 전달할 때는 지원 번들을 만듭니다. 번들은 아무것도 바꾸지 않으며(스키마 마이그레이션·롤백 없음), 설정이 깨져 있어도 가능한 부분을 계속 모읍니다.

```bash
VIGILANTE_TOKEN=vgl_... vigilante support-bundle -c /etc/vigilante/vigilante.yaml --server https://127.0.0.1:8088
```

| 옵션 | 설명 |
|---|---|
| `--out F.zip` | 출력 파일(기본 `vigilante-support-<호스트>-<UTC 시각>.zip`) |
| `--server URL` | 실행 중인 서버의 `/healthz`, `/readyz`, `/metrics`, `/v2/circuit` 수집 |
| `--log FILE` | 포함할 로그 파일(반복 가능, 각 파일 마지막 20MB) |
| `--since 24h` | 포함할 systemd 저널 기간(`vigilante-server`, `vigilante-agent`) |
| `--no-doctor`, `--doctor-timeout 10s` | 대상 점검 생략, 점검별 제한 시간 |
| `--no-verify` | 감사 체인 검증 생략(큰 저널에서 느릴 때) |

번들에는 버전·플러그인, 환경변수 이름(값 제외), 비밀값을 가린 설정과 검증 결과, 스키마 상태, 배포 상태 요약, 감사 체인 검증 결과, doctor 결과, 서버 응답, 로그가 들어갑니다. 비밀값은 `REDACTED`로 바뀌지만, 보내기 전에 내용을 직접 확인하십시오.

---

# 부록 A. 관리자용 명령 요약

| 명령 | 용도 |
|---|---|
| `vigilante version` | 버전·빌드 종류·커밋 |
| `vigilante plugins` | 플러그인 검증 수준 |
| `vigilante validate -c FILE` | 설정 검증 |
| `vigilante doctor -c FILE [--service S] [--json] [--junit F]` | 읽기 전용 사전 점검 |
| `vigilante presets [list]`, `vigilante presets show NAME[@V] --set k=v` | 프리셋 조회 |
| `vigilante token create --name N --role R --scope S --expires D` | 서비스 계정 토큰 발급 |
| `vigilante whoami --server URL` | 토큰 신원 확인 |
| `vigilante server -c FILE` | 서버 실행 |
| `vigilante agent -c FILE --target T --server URL` | 에이전트 실행 |
| `vigilante sudoers -c FILE [--target H] [--no-resolve] [--json]` | sudoers 규칙 생성 |
| `vigilante store status -c FILE [--json]` | PostgreSQL 스키마 상태(최신 아니면 종료 코드 4) |
| `vigilante store migrate -c FILE [--down-to N --yes]` | 스키마 마이그레이션·되돌리기 |
| `vigilante audit verify [--key REF]`, `query`, `export`, `prune` | 감사 기록 관리 |
| `vigilante circuit status --server URL` (또는 `reset`, `trip`) | 서킷 조회·리셋·차단(서버 위임, 리셋·차단은 admin 토큰). 로컬(`-c FILE`)로 리셋·차단하려면 인증 환경에서 `--break-glass "이유"` 필요(5.9절) |
| `vigilante support-bundle -c FILE [--server URL]` | 진단 번들 |

# 부록 B. 파일과 경로

| 경로 | 내용 |
|---|---|
| `/usr/bin/vigilante` | 바이너리 |
| `/etc/vigilante/vigilante.yaml` | 설정(0640 root:vigilante) |
| `/etc/vigilante/vigilante.env` | 서버 비밀 환경변수(직접 생성, 0640 root:vigilante) |
| `/etc/vigilante/agent.env` | 에이전트 환경(`VIGILANTE_SERVER`, `VIGILANTE_TOKEN`, `VIGILANTE_TARGET`) |
| `/var/lib/vigilante/` | 상태 디렉토리(저널, 락 파일) |
| `/usr/lib/systemd/system/vigilante-server.service`, `vigilante-agent.service` | systemd 유닛 |
| `/usr/share/doc/vigilante/examples/vigilante.yaml` | 참조 설정 |

# 부록 C. 환경변수

| 변수 | 용도 |
|---|---|
| `VIGILANTE_TOKEN` | `--server` 원격 호출과 에이전트의 API 토큰 |
| `VIGILANTE_LOG` | 로그 수준(`debug`, `info`, `warn`) |
| `VIGILANTE_LOG_FORMAT` | `json`이면 JSON 로그 |
| `VIGILANTE_HA_ADVERTISE_URL`, `VIGILANTE_HA_NODE_ID` | HA 노드별 주소와 이름(설정보다 우선) |
| `VIGILANTE_DEPLOYMENT_ID`, `VIGILANTE_VERSION` | CI 밖에서 `--id`, `--version` 자동 채움 |
| `VAULT_TOKEN` | Vault `token` 인증의 기본 환경변수 |
