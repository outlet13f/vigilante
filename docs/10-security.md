# 10. 보안 가이드

Vigilante는 운영 서버를 재시작하고, 로드밸런서에서 대상을 빼고, VM을 스냅샷으로 되돌립니다. 이 권한이 새면 그 자체로 장애 수단이 되므로, 아래 기준으로 권한을 최소화하십시오. 설정 키는 [02-config-spec.md](02-config-spec.md), 설치는 [07-install.md](07-install.md).

## 통신 경로

| 방향 | 대상 | 포트(기본) | 보호 |
|---|---|---|---|
| 들어옴 | API·웹 콘솔·에이전트 push·CI | 8088 | `server.tls`(TLS 1.2+, 선택적 클라이언트 인증서) 또는 TLS 프록시, 토큰·OIDC 인증, 호출 한도 |
| 들어옴 | GitHub·GitLab·Jenkins 웹훅 | 8088 | 서명(HMAC)·토큰 검증 |
| 나감 | 대상 호스트 SSH | 22 | 키 또는 Vault SSH CA 단기 인증서, 호스트 키 검증 |
| 나감 | LB·하이퍼바이저·클라우드 API (F5, vCenter, Nutanix, OpenStack, AWS) | 443 | 전용 계정·최소 역할(아래), 인증서 검증(`tls_skip_verify`는 시험용) |
| 나감 | Vault | 8200 | AppRole·Kubernetes 인증, 읽기 전용 정책 |
| 나감 | PostgreSQL | 5432 | 전용 계정, `sslmode=verify-full` 권장 |
| 나감 | IdP(OIDC), ServiceNow, SMTP, Teams·Slack·PagerDuty, 구독 웹훅 | 443·587 | 비밀값은 `*_ref`, 웹훅 수신 호스트는 `api.webhook_allowed_hosts`로 제한 |

## 서버

- **인증을 반드시 켭니다.** `auth`가 없으면 모든 호출이 익명 admin이며 서버가 경고를 남깁니다. 패키지 기본 설정은 그래서 `127.0.0.1`에서만 받습니다.
- **역할은 좁게:** CI는 `deployer@service=<서비스>`, 운영자는 `operator@team=<팀>`, admin은 소수. 승인 모드에서는 `auth.four_eyes: true`로 요청자와 승인자를 분리합니다.
- **TLS:** `server.tls`로 직접 HTTPS를 켜거나 TLS 프록시·인그레스 뒤에 둡니다. 에이전트는 `client_auth: optional`과 에이전트 인증서로 상호 인증할 수 있습니다(토큰 인증은 그대로 필요).
- **프로세스:** 패키지의 systemd 유닛은 `vigilante` 계정, `NoNewPrivileges`, `ProtectSystem=strict`(쓰기는 `/var/lib/vigilante`만)로 실행합니다. 컨테이너 이미지는 distroless, non-root, 읽기 전용 루트 파일시스템입니다.
- **감사:** 모든 조작과 거부는 해시 체인으로 묶인 감사 기록에 남습니다. `audit.syslog`로 SIEM에 실시간 전송하고, 주기적으로 `vigilante audit verify`를 실행하십시오.
- **웹 콘솔:** CSP(자기 출처만), `X-Frame-Options: DENY`, HttpOnly·SameSite 세션 쿠키, CSRF 토큰. HA에서는 `console.session_key_ref`를 공유합니다.
- **`/metrics`:** 기본은 `viewer@*` 토큰이 필요합니다. `metrics_public`은 스크레이퍼가 신뢰 망에 있을 때만.

## 비밀값

- 설정 파일에는 비밀값을 넣지 않습니다. 자격증명 구조에는 평문 비밀번호 키가 아예 없고, `*_ref`(Vault·파일·환경변수) 또는 `*_env`만 받습니다. Vault(`vault:<mount>/<path>#<key>`)를 권장합니다.
- `/etc/vigilante/vigilante.env`(환경변수 비밀)는 `0640 root:vigilante`.
- Vault 정책 예(KV v2, SSH CA):
  ```hcl
  path "secret/data/prod/vigilante/*" { capabilities = ["read"] }
  path "ssh-client-signer/sign/vigilante" { capabilities = ["update"] }
  ```
- 해석된 비밀값은 로그에서 가려지고, `vigilante support-bundle`은 설정·로그의 비밀값과 토큰을 `REDACTED`로 바꿉니다. 그래도 보내기 전에 내용을 확인하십시오.
- 서비스 계정 토큰과 API 키는 SHA-256만 저장합니다. 만료(`expires`)를 두고 주기적으로 교체하십시오.

## 대상 호스트 (SSH)

전용 계정(예: `vigilante`)을 만들고, 키는 Vault SSH CA 단기 인증서를 권장합니다(`credentials.<name>.ssh_ca`). 읽기(로그, `/proc`, `readlink`, `systemctl is-active`)는 권한 상승이 필요 없습니다.

**`connection.sudo: true`는 쓰지 않기를 권장합니다.** 이 설정은 모든 명령을 `sudo -n sh -c '<명령>'`로 실행하므로 sudoers에 사실상 무제한 권한(`ALL`)이 필요하고, 그 호스트의 root와 같습니다. 명령별 sudo 분리는 로드맵 M5-3에서 다룹니다. 지금은 아래처럼 **필요한 명령만 sudo로 허용하고 실행기에서 그 명령을 지정**하십시오.

```
# /etc/sudoers.d/vigilante (대상 호스트)
Defaults:vigilante !requiretty
Cmnd_Alias VGL_APP   = /usr/bin/systemctl restart order-api.service
Cmnd_Alias VGL_NGINX = /usr/sbin/nginx -t, /usr/sbin/nginx -s reload
vigilante ALL=(root) NOPASSWD: VGL_APP, VGL_NGINX
```

```yaml
targets:
  - {name: order-01, address: 10.0.1.11, connection: {type: ssh, credential: ssh-vigilante}}   # sudo 없음
executors:
  order-symlink:
    type: symlink
    symlink:
      link: /opt/order/current
      releases_dir: /opt/order/releases
      restart_cmd: "sudo -n /usr/bin/systemctl restart order-api.service"
traffic:
  web-nginx:
    type: nginx
    nginx: {test_cmd: "sudo -n /usr/sbin/nginx -t", reload_cmd: "sudo -n /usr/sbin/nginx -s reload", ...}
```

- **symlink:** `/opt/order`(링크가 있는 디렉토리)에 `vigilante` 그룹 쓰기 권한을 줍니다(`ln -sfn` 후 `mv -Tf`로 원자 교체). 릴리스 디렉토리는 읽기만.
- **nginx:** upstream 파일(`upstream_file`)과 그 디렉토리에 그룹 쓰기 권한. 설정 시험과 reload만 sudo.
- **HAProxy:** sudo 대신 runtime API 소켓 권한: `stats socket /run/haproxy/admin.sock mode 660 group vigilante level admin`.
- **container:** Docker 소켓 접근은 root와 같습니다. 전용 호스트 계정과 감사를 두고, 가능하면 rootless Podman을 쓰십시오.
- **kvm:** 하이퍼바이저에서 `libvirt` 그룹(polkit)으로 `virsh snapshot-*`·`domstate`를 허용합니다. 이 그룹은 그 호스트의 모든 VM을 제어할 수 있으므로 하이퍼바이저 접근 자체를 제한하십시오.
- **에이전트의 자율 롤백(`agent.failsafe: rollback`)**은 대상 호스트에서 위 명령을 직접 실행하므로 같은 sudoers가 필요하고, systemd 유닛에 쓰기 경로를 추가해야 합니다(유닛 파일의 주석 참고).

## 외부 시스템 계정 (최소 역할)

각 제품의 역할 이름과 범위는 버전마다 다를 수 있으므로 제품 문서로 확인하고, M8 랩 검증 결과(09-compatibility.md)를 따르십시오.

| 시스템 | 필요한 일 | 권장 권한 |
|---|---|---|
| F5 BIG-IP | 풀 멤버 조회, session·state 변경 | 대상 파티션의 Operator 역할(멤버 활성·비활성), iControl REST 접근 |
| vCenter | VM 조회, 스냅샷 생성·복원·삭제, 전원 상태 | 대상 VM 폴더에 한정한 사용자 역할: 가상 머신 > 스냅샷 관리 > 생성·되돌리기·제거, 읽기 전용 상속 |
| Nutanix Prism | VM 스냅샷 생성·복원 | 대상 VM에 한정한 VM 관리 권한 |
| OpenStack | 서버 스냅샷·rebuild·정지·시작, Cinder 스냅샷·revert, Octavia 멤버 변경 | 대상 프로젝트의 `member`, Octavia는 `load-balancer_member`. Keystone application credential(프로젝트 고정, 만료 설정) |
| AWS ALB/NLB | 타깃 등록·해제, 상태 조회 | 아래 IAM 정책 |
| ServiceNow | 변경 요청 읽기·작업 노트, 인시던트 생성 | `change_request` 읽기·쓰기(work notes), `incident` 읽기·생성만 가진 통합 사용자 |
| PostgreSQL | 상태 저장소 | 전용 DB의 소유자 계정 하나. 다른 DB 권한 없음 |

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["elasticloadbalancing:RegisterTargets", "elasticloadbalancing:DeregisterTargets"],
     "Resource": "arn:aws:elasticloadbalancing:ap-northeast-2:123456789012:targetgroup/order-tg/*"},
    {"Effect": "Allow", "Action": "elasticloadbalancing:DescribeTargetHealth", "Resource": "*"}
  ]
}
```

## 공급망

- 릴리스 바이너리·패키지·번들은 `SHA256SUMS`와 그 서명으로, 이미지는 cosign 서명으로 확인합니다(07-install.md).
- 바이너리마다 CycloneDX SBOM이 함께 나옵니다. 사내 취약점 관리 도구에 넣으십시오.
- CI는 모든 PR에서 `govulncheck`(도달 가능한 알려진 취약점)를 차단 조건으로 돌립니다.
- 필요한 플러그인만 쓰면 최소 빌드(`-minimal`)로 의존성을 절반 가까이 줄일 수 있습니다.

## 취약점 신고

보안 문제는 공개 이슈가 아닌 저장소 관리자에게 비공개로 알려 주십시오.
