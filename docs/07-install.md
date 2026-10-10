# 07. 설치 가이드

단일 서버, HA(여러 서버 + PostgreSQL), 폐쇄망, Kubernetes, 에이전트 설치를 다룹니다. 설정 키는 [02-config-spec.md](02-config-spec.md), 보안 설정은 [10-security.md](10-security.md), 업그레이드는 [08-upgrade.md](08-upgrade.md)를 보십시오.

## 배포 형태

릴리스(GitHub Release `vX.Y.Z`)마다 다음이 나옵니다.

| 파일 | 내용 |
|---|---|
| `vigilante_X.Y.Z_linux_{amd64,arm64,ppc64le}`, `_windows_amd64.exe` | 정적 바이너리, **전체 빌드** (모든 플러그인) |
| `..._linux_amd64-minimal` 등 | **최소 빌드**: vSphere·AWS ALB·gRPC 프로브·MySQL 드라이버 제외, 크기 약 절반. 이 플러그인을 쓰는 설정은 시작할 때 이름을 들어 거부 |
| `vigilante_X.Y.Z_{amd64,arm64,ppc64el}.deb`, `vigilante-X.Y.Z-1.{x86_64,aarch64,ppc64le}.rpm` | 패키지(전체 빌드): `/usr/bin/vigilante`, systemd 유닛 2개, `/etc/vigilante/`, 서비스 계정 `vigilante` |
| `vigilante-X.Y.Z.tgz` | Helm 차트 |
| `ghcr.io/<org>/vigilante:X.Y.Z` | 컨테이너 이미지 (distroless, non-root, 멀티 아키텍처, 서명) |
| `vigilante_X.Y.Z_airgap_linux_<arch>.tar.gz` | **폐쇄망 번들**: 위 바이너리·패키지·이미지 아카이브·차트·SBOM·문서·설치 스크립트 |
| `*.cdx.json` | 바이너리별 SBOM (CycloneDX 1.6, 바이너리에 들어간 모듈 목록 그대로) |
| `SHA256SUMS`, `SHA256SUMS.sig`, `cosign.pub` | 모든 파일의 체크섬과 그 서명 |

## 서명 확인

모든 파일은 `SHA256SUMS`에 있고, `SHA256SUMS`는 릴리스 키로 서명되어 있습니다. **공개 키는 이 저장소의 `packaging/cosign.pub`(또는 사내에 별도로 배포한 사본)을 쓰십시오.** 릴리스에 함께 올라간 `cosign.pub`은 편의용이며, 위조된 릴리스라면 키도 위조될 수 있습니다.

openssl만 있으면 됩니다(폐쇄망 서버에서도):

```bash
openssl base64 -d -A -in SHA256SUMS.sig -out SHA256SUMS.der
openssl dgst -sha256 -verify cosign.pub -signature SHA256SUMS.der SHA256SUMS   # Verified OK
sha256sum -c --ignore-missing SHA256SUMS                                        # 내려받은 파일만 확인
```

cosign이 있으면 `cosign verify-blob --key cosign.pub --signature SHA256SUMS.sig --insecure-ignore-tlog=true SHA256SUMS`로도 됩니다. 이미지는 `cosign verify --key cosign.pub --insecure-ignore-tlog=true ghcr.io/<org>/vigilante:X.Y.Z`. 서명은 공개 투명성 로그(Rekor)에 올리지 않습니다(사내 릴리스이고 폐쇄망에서 확인해야 하므로). 그래서 `--insecure-ignore-tlog`가 필요합니다.

## 단일 서버 (rpm·deb)

```bash
dnf install ./vigilante-X.Y.Z-1.x86_64.rpm      # 또는 apt install ./vigilante_X.Y.Z_amd64.deb
vi /etc/vigilante/vigilante.yaml                # 설정 (예시: /usr/share/doc/vigilante/examples/)
vi /etc/vigilante/vigilante.env                 # *_env 비밀값 (VAULT_TOKEN 등). chmod 0640, root:vigilante
vigilante validate -c /etc/vigilante/vigilante.yaml
vigilante doctor   -c /etc/vigilante/vigilante.yaml   # 대상·자격증명·LB 점검 (읽기 전용)
systemctl enable --now vigilante-server
curl -fsS http://127.0.0.1:8088/readyz
```

- 기본 설정은 `127.0.0.1:8088`에서만 받습니다. `auth`(OIDC·서비스 계정)를 설정하기 전에는 모든 호출이 익명 admin이기 때문입니다. 인증을 설정한 뒤 `listen: ":8088"`로 바꾸고, `server.tls`로 HTTPS를 켜거나 TLS 프록시 뒤에 두십시오.
- 상태(저널·감사 기록)는 `/var/lib/vigilante`에 남습니다. 패키지를 지워도 지우지 않습니다.
- 웹 콘솔: `https://<서버>:8088/console/` (SSO 설정은 02의 `console`).
- CI에서 쓰는 서비스 계정 토큰: `vigilante token create --name ci-order --role deployer --scope service=order-api` → 출력된 SHA-256 줄을 `auth.service_accounts`에, 토큰은 CI 비밀 변수에.

## HA (여러 서버 + PostgreSQL)

1. PostgreSQL(CI에서 16으로 확인)에 전용 DB와 계정을 만듭니다(스키마 소유자). 스키마는 서버가 시작할 때 만들고 올립니다. DBA가 통제하려면 `server.state.auto_migrate: false`와 `vigilante store migrate`(08-upgrade.md).
2. 노드 2~3대에 패키지를 설치하고 같은 설정을 둡니다. 노드마다 다른 값은 `advertise_url`(다른 노드가 이 노드에 닿는 주소)뿐이며, 설정 대신 `/etc/vigilante/vigilante.env`에 `VIGILANTE_HA_ADVERTISE_URL=https://vigilante-1.internal:8088`로 줄 수 있습니다.
   ```yaml
   server:
     listen: ":8088"
     state: {backend: postgres, dsn_ref: "vault:secret/prod/vigilante#dsn"}
     ha: {enabled: true, lease_ttl: 15s}
     tls: {cert_file: /etc/vigilante/tls/server.crt, key_file: /etc/vigilante/tls/server.key}
   ```
3. 앞단 로드밸런서의 헬스 체크는 `GET /readyz`(저장소 연결과 리더 확인)를 씁니다. 어느 노드로 와도 팔로워가 리더로 전달합니다.
4. 사설 CA 인증서를 쓰면 노드 간 전달을 위해 각 노드의 시스템 신뢰 저장소에 CA를 넣습니다(`update-ca-trust` / `update-ca-certificates`).

## 폐쇄망

인터넷이 되는 곳에서 번들과 `SHA256SUMS`·`SHA256SUMS.sig`를 받아 서명을 확인한 뒤 옮깁니다.

```bash
tar xzf vigilante_X.Y.Z_airgap_linux_amd64.tar.gz && cd vigilante_X.Y.Z_airgap_linux_amd64
./install.sh --verify     # 번들 안 모든 파일의 체크섬 확인
./install.sh              # rpm 또는 deb 설치 (root)
./install.sh --minimal    # 또는 최소 빌드 바이너리만 /usr/bin에
```

Kubernetes용 이미지는 사내 레지스트리로 옮깁니다.

```bash
docker load -i image/vigilante_X.Y.Z_linux_amd64.tar
docker tag ghcr.io/<org>/vigilante:X.Y.Z registry.internal/vigilante:X.Y.Z
docker push registry.internal/vigilante:X.Y.Z
# containerd만 있는 노드: ctr -n k8s.io images import image/vigilante_X.Y.Z_linux_amd64.tar
```

## Kubernetes (Helm)

```bash
helm install vigilante ./vigilante-X.Y.Z.tgz -n vigilante --create-namespace -f values.yaml
```

단일 노드(파일 저장소, 볼륨에 저널):

```yaml
config: |
  version: v1
  server:
    journal_path: /var/lib/vigilante/journal.jsonl
  auth: {...}
  ...
ingress: {enabled: true, host: vigilante.example.internal, tls: [{secretName: vigilante-tls, hosts: [vigilante.example.internal]}]}
```

HA(PostgreSQL, 리플리카 3):

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
  ...
```

파드에서 직접 HTTPS(`server.tls`, 인증서는 `kubernetes.io/tls` 시크릿):

```yaml
tls: {secretName: vigilante-tls}     # /etc/vigilante-tls에 마운트
config: |
  version: v1
  server:
    tls: {cert_file: /etc/vigilante-tls/tls.crt, key_file: /etc/vigilante-tls/tls.key}
  ...
```

- `config`에 인증(`auth.service_accounts`, `auth.oidc`, 또는 시크릿을 `env`로 넣은 `server.auth_token_env`)이 없으면 차트가 렌더링을 거부합니다. 인증 없이 모든 호출을 익명 admin으로 받는 개발용 설치만 `auth.allowAnonymous: true`로 허용합니다. `existingConfigMap`은 차트가 읽지 못하므로 확인하지 않습니다.
- 파드마다 `VIGILANTE_HA_ADVERTISE_URL`(파드 IP)과 노드 ID(파드 이름)를 차트가 넣습니다. `config`에 `server.tls.cert_file`이 있으면(`existingConfigMap`이면 `tls.enabled: true`) 이 주소와 프로브, 포트 이름, ServiceMonitor가 `https`로 바뀝니다. 팔로워는 리더의 파드 IP로 HTTPS 전달을 하는데, 파드 IP는 보통 인증서에 없습니다. `server.ha.tls`에 CA(`ca_file: /etc/vigilante-tls/ca.crt`)와 인증서에 들어 있는 이름(`server_name: vigilante.<네임스페이스>.svc`)을 지정하면 그 이름으로 검증합니다. `tls.client_auth: require`는 프로브와 리더 전달이 클라이언트 인증서를 내지 못해 거부하므로 `optional`을 씁니다.
- 메모리: 기본 요청 512Mi, 한도 2Gi이며 `GOMEMLIMIT`을 한도의 90%로 넣습니다(`goMemLimit`으로 변경). 부하 시험의 최대 힙은 대상 2,000 × 프로브 3(6천 개)에서 352 MiB, × 10(2만 개)에서 1.1 GiB였습니다. 프로브 1천 개당 약 60 MiB에 여유를 더해 잡습니다.
- 파일 저장소로 리플리카를 2 이상 주면 차트가 렌더링을 거부합니다.
- 폐쇄망: `image.repository: registry.internal/vigilante`.
- 설정 확인: `kubectl exec deploy/vigilante -- vigilante validate -c /etc/vigilante/vigilante.yaml`.

## 에이전트

에이전트는 선택 사항입니다(로그 고빈도 수집, SSH가 막힌 구간, 오케스트레이터 단절 시 자율 판정). 대상 호스트에 같은 패키지를 설치합니다.

```bash
vigilante token create --name agents --role agent        # 서버 쪽: 해시를 auth.service_accounts에
dnf install ./vigilante-X.Y.Z-1.x86_64.rpm               # 대상 호스트
vi /etc/vigilante/agent.env       # VIGILANTE_SERVER, VIGILANTE_TOKEN (, VIGILANTE_TARGET)
vi /etc/vigilante/vigilante.yaml  # 서버와 같은 설정 (대상·서비스 정의를 읽음)
systemctl enable --now vigilante-agent
```

사설 CA·클라이언트 인증서는 설정의 `agent.tls`(02-config-spec.md). 에이전트가 앱 로그를 읽을 수 있게 `vigilante` 사용자를 로그 그룹에 넣으십시오.

## 설치 후 점검

```bash
vigilante version                                  # 빌드 종류·커밋
vigilante plugins                                  # 플러그인별 검증 수준 (09-compatibility.md)
vigilante validate -c /etc/vigilante/vigilante.yaml
vigilante doctor   -c /etc/vigilante/vigilante.yaml
vigilante store status -c /etc/vigilante/vigilante.yaml   # PostgreSQL일 때
curl -fsS https://<서버>:8088/readyz
```

문제가 생기면 지원 번들을 만들어 전달합니다. 설정·로그의 비밀값은 `REDACTED`로 바뀌며, 보내기 전에 내용을 확인하십시오.

```bash
vigilante support-bundle -c /etc/vigilante/vigilante.yaml --server https://127.0.0.1:8088
```

## 릴리스 서명 키 (릴리스 관리자)

릴리스 워크플로우(`.github/workflows/release.yml`)는 키가 없으면 실패합니다. 처음 한 번:

```bash
cosign generate-key-pair                 # cosign.key(암호화된 개인 키), cosign.pub
cp cosign.pub packaging/cosign.pub && git add packaging/cosign.pub   # 커밋: 검증의 기준
gh secret set COSIGN_PRIVATE_KEY < cosign.key
gh secret set COSIGN_PASSWORD            # generate-key-pair 때 정한 암호
```

- 개인 키 원본은 저장소나 개인 PC가 아닌 금고(Vault·HSM·오프라인 매체)에 보관합니다.
- 워크플로우는 서명 키가 `packaging/cosign.pub`와 짝이 아니면 서명을 거부합니다.
- 키를 바꿀 때는 새 공개 키를 커밋하고 사내 배포 사본도 바꾼 뒤, 다음 릴리스부터 새 키로 서명합니다. 이전 릴리스는 이전 공개 키로 확인합니다.
- 릴리스: `git tag vX.Y.Z && git push origin vX.Y.Z`. 접미사가 있는 태그(`v1.0.0-rc.1`)는 사전 릴리스로 표시됩니다.
