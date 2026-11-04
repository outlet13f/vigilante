---
title: 보안 점검표 응답서
doc_id: VGL-BD-02
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

이 문서는 고객사의 보안성 검토(도입 심사)에 쓰도록 Vigilante의 보안 기능과 한계를 점검 항목별로 답한 것입니다. 각 응답에는 확인할 수 있는 근거(설정 키, 문서 절, 코드 경로)를 붙였습니다. 근거의 경로는 모두 Vigilante 제품 저장소 기준입니다.

## 1.2 대상과 전제

- **대상 버전:** 첫 출시 후보 1.0.0 (2026-10-11 저장소 master `537870c` 기준). **정식 릴리스는 아직 없습니다.** 실장비 검증과 파일럿(로드맵 M8)이 남아 있습니다. 이 기준 커밋에는 로드맵 M5-4(상태 저장소 장애 시 쓰기 대기열, 장애 주입·부하 시험, PR #12)와 검토에서 나온 결함 수정(관측 장치 과부하 보류, 저장소 장애 중 롤백, 로컬 CLI 통제, 감사 체인 키, SIEM TLS 전송, HSTS, Helm 차트 인증·TLS, PR #13), 관측 장치 과부하 보류 대상을 서버가 직접 재는 프로브로 한정한 수정(PR #14)이 모두 병합되어 있습니다.
- **범위:** Vigilante 소프트웨어(서버, CLI, 에이전트, 웹 콘솔, 설치 산출물)와 그 기본 설정. 고객 운영 환경(OS, 네트워크, IdP, Vault, DB, 대상 호스트)의 보안 설정은 고객 책임이며, 권장 사항은 5장에 적었습니다.
- **외부 점검:** 외부 기관의 침투 테스트, 소스 코드 보안 감사, CC·GS 등 보안 인증은 **받지 않았습니다.** 이 문서의 응답은 개발팀의 자체 확인입니다.

## 1.3 응답 기준

| 응답 | 뜻 |
|---|---|
| 충족 | 제품이 기능을 제공하고 근거로 확인할 수 있음. 설정이 필요한 경우 비고에 적음 |
| 부분 충족 | 기능이 있으나 범위·기본값·검증에 한계가 있음. 한계를 비고에 적음 |
| 미충족 | 제품이 해당 기능을 제공하지 않음 |
| 해당 없음 | 제품 구조상 해당 항목이 적용되지 않음. 이유를 비고에 적음 |

## 1.4 응답 요약

| 영역 | 항목 수 | 충족 | 부분 충족 | 미충족 | 해당 없음 |
|---|---|---|---|---|---|
| 3.1 인증·계정 관리 | 9 | 5 | 3 | 0 | 1 |
| 3.2 접근 통제·권한 | 7 | 7 | 0 | 0 | 0 |
| 3.3 암호화 (전송) | 10 | 7 | 2 | 1 | 0 |
| 3.4 암호화 (저장) | 4 | 2 | 1 | 1 | 0 |
| 3.5 비밀정보 관리 | 6 | 6 | 0 | 0 | 0 |
| 3.6 감사 로그·무결성 | 7 | 5 | 2 | 0 | 0 |
| 3.7 입력 검증·웹 보안 | 11 | 9 | 2 | 0 | 0 |
| 3.8 API 보안 | 8 | 7 | 1 | 0 | 0 |
| 3.9 서버 권한·최소 권한 | 7 | 4 | 3 | 0 | 0 |
| 3.10 네트워크·포트 | 7 | 6 | 1 | 0 | 0 |
| 3.11 가용성·장애 대응 | 7 | 5 | 2 | 0 | 0 |
| 3.12 공급망 보안 | 7 | 3 | 3 | 0 | 1 |
| 3.13 개인정보 | 4 | 3 | 1 | 0 | 0 |
| 3.14 패치·업데이트 | 6 | 3 | 2 | 0 | 1 |
| 3.15 설치·운영 환경 | 4 | 1 | 2 | 1 | 0 |
| 3.16 취약점 신고·외부 검증 | 4 | 0 | 1 | 3 | 0 |
| **합계** | **108** | **73** | **26** | **6** | **3** |

미충족·부분 충족 항목과 보완 방법은 4장에 모았습니다. 1.1판(master `e55b18a` 기준, 충족 68·부분 충족 29·미충족 8)과 비교하면 PR #13으로 AU-02·AC-03·AD-03이 부분 충족에서 충족으로, EN-08·WB-07이 미충족에서 충족으로 바뀌었습니다.

# 2. 시스템 보안 개요

## 2.1 구성 요소와 권한

Vigilante는 운영 서버를 재시작하고, 로드밸런서에서 대상을 빼고, VM을 스냅샷으로 되돌립니다. 이 권한이 새면 그 자체로 장애 수단이 되므로, 보안 설계의 중심은 **누가 조치를 일으킬 수 있는가(인증·권한·승인)**와 **Vigilante가 대상에 가진 권한을 얼마나 좁히는가(최소 권한)**입니다(docs/10-security.md 서두).

| 구성 요소 | 실행 위치 | 주요 권한 |
|---|---|---|
| 서버 (`vigilante server`) | 전용 서버 또는 Kubernetes | 대상 호스트 SSH, LB·하이퍼바이저·클라우드 API, 상태 저장소 |
| CLI (`vigilante watch` 등) | CI 러너, 운영자 PC | 서버 API 호출 또는 직접 실행 시 서버와 같은 권한 |
| 에이전트 (선택) | 대상 호스트 | 로그·`/proc` 읽기, 서버로 샘플 전송. 자율 롤백 시 로컬 변경 명령 |
| 웹 콘솔 | 서버에 내장 | 공개 API(v2)만 호출, 사용자 권한과 동일 |

## 2.2 통신 경로

| 방향 | 대상 | 포트(기본) | 보호 |
|---|---|---|---|
| 들어옴 | API·웹 콘솔·에이전트·CI | 8088 | `server.tls`(TLS 1.2 이상, 선택적 클라이언트 인증서) 또는 TLS 프록시, 토큰·OIDC 인증, 호출 한도 |
| 들어옴 | GitHub·GitLab·Jenkins 웹훅 | 8088 | HMAC 서명·토큰 검증 |
| 나감 | 대상 호스트 SSH | 22 | 키 또는 Vault SSH CA 단기 인증서, 호스트 키 검증 |
| 나감 | F5, vCenter, Nutanix, OpenStack, AWS API | 443 | 전용 계정·최소 역할, 인증서 검증 |
| 나감 | Vault | 8200 | token·AppRole·Kubernetes 인증, 읽기 정책(SSH CA 사용 시 `sign/<role>` update 추가) |
| 나감 | PostgreSQL | 5432 | 전용 계정, `sslmode=verify-full` 권장 |
| 나감 | IdP, ServiceNow, SMTP, 알림 채널, 구독 웹훅 | 443·587 | 비밀값은 `*_ref`, 웹훅 수신 호스트 제한 |
| 나감 | SIEM (syslog) | 6514 | `audit.syslog.address: tls://`(RFC 5425, 수집기 인증서 검증, 선택적 클라이언트 인증서). `tcp://`·`udp://`(514)도 쓸 수 있으나 평문 (3.3 EN-08 참고) |
| 노드 간 | HA 팔로워 → 리더 API 전달 | 8088 | `server.tls`를 켜면 HTTPS. 리더 인증서는 `server.ha.tls`(사설 CA, 확인할 이름, 클라이언트 인증서)로 검증 |

출처: docs/10-security.md "통신 경로", docs/02 `server`(`ha.tls`)·`audit`, internal/audit/syslog.go, internal/tlsconf/tlsconf.go.

# 3. 점검 항목별 응답

## 3.1 인증·계정 관리

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| AU-01 | 사내 통합 인증(SSO) 연동 | 충족 | `auth.oidc`(issuer, audience, groups_claim). 콘솔은 OIDC authorization code + PKCE. docs/02 `auth`·`console`, internal/auth/auth.go, internal/console/console.go | 표준 OIDC 제공자(Keycloak, Azure AD, Okta 등) 대상. 실제 IdP 연동 시험 기록 없음(모의 IdP: internal/auth/oidctest). LDAP/AD 직접 연동은 미지원 |
| AU-02 | 인증 미설정 상태의 보호 | 충족 | `auth`가 없으면 모든 호출이 익명 admin(internal/auth/auth.go `Anonymous`)이므로 배포 형태마다 막아 둠. 서버 시작 시 경고 로그(cmd/vigilante/main.go). 패키지 기본 설정은 `listen: "127.0.0.1:8088"`(packaging/etc/vigilante.yaml). Helm 차트는 `config`에 인증(`auth.service_accounts`, `auth.oidc`, 또는 `env`·`envFrom`으로 값을 넣은 `server.auth_token_env`)이 없으면 렌더링을 거부(deploy/helm/vigilante/templates/_helpers.tpl `vigilante.validate`, CI가 거부 여부 확인). docs/07 "Kubernetes" | 개발용 설치만 `auth.allowAnonymous: true`로 허용. 차트는 `existingConfigMap`의 내용을 읽지 못해 그 경우 인증 여부를 확인하지 않음. 서버 자체는 경고만 남기고 시작하므로, 바이너리·이미지를 직접 실행할 때 `listen`을 외부 주소로 바꾸기 전에 `auth` 설정 필수 |
| AU-03 | 서비스 계정·API 키의 발급·만료·폐기 | 충족 | `vigilante token create`, `auth.service_accounts[].expires`, `/v2/api-clients`(발급·스코프 변경·비밀 회전·폐기). docs/02 `auth`, docs/06 "API 클라이언트 관리" | 서비스 계정 폐기는 설정에서 항목을 지우고 서버를 재시작해야 적용됨(설정 실시간 재적재 기능 없음, HA는 모든 노드 재시작. cmd/vigilante/main.go는 SIGINT·SIGTERM만 처리) |
| AU-04 | 인증 정보의 안전한 저장 | 충족 | 서비스 계정 토큰, API 키, OAuth 클라이언트 비밀은 SHA-256 해시만 저장. 비밀은 256비트 난수(internal/auth/auth.go `NewSecret`) | 발급 응답에서 한 번만 보여 줌. API 클라이언트는 식별용으로 비밀의 앞 8자(접두어 4자 포함)를 함께 저장(internal/orchestrator/clients.go `SecretHint`) |
| AU-05 | 비밀번호 정책 (복잡도·변경 주기) | 해당 없음 | 제품 자체 사용자 계정·비밀번호가 없음. 사람은 OIDC, 시스템은 토큰으로 인증 | IdP의 비밀번호 정책을 따름 |
| AU-06 | 로그인 실패 제한·무차별 대입 방지 | 부분 충족 | 토큰은 256비트 난수라 추측이 사실상 불가능. 호출 한도는 인증된 호출자 기준으로만 적용(internal/api/ratelimit.go `limited`) | 인증 실패 요청 자체에 대한 횟수 제한·차단은 없음. 앞단 프록시·WAF로 제한 권장 |
| AU-07 | 세션 관리 (만료·종료) | 부분 충족 | 콘솔 세션은 ID 토큰 만료 시각(최대 12시간)에 종료(internal/console/console.go `maxSession`). 로그아웃 시 쿠키 삭제 | 유휴 시간 만료(idle timeout)와 동시 세션 제한 없음. 서버에 세션 상태가 없어 개별 세션 강제 종료 불가(세션 키 교체 시 전체 무효) |
| AU-08 | 비상용(break-glass) 계정 통제 | 부분 충족 | `server.auth_token_env` 토큰은 `token:legacy` admin으로 기록되며 평소 비워 두기를 권장(docs/02 `auth`). 인증을 설정한 환경에서 서버 없이 실행하는 권한 큰 로컬 CLI 명령은 `--break-glass 사유`가 있어야 실행되고, 감사 기록(`breakglass.<작업>`)과 critical 알림을 남김(cmd/vigilante/authcmd.go `localPrivileged`, AC-03) | legacy 토큰은 호출 한도 적용 대상에서 제외됨(internal/api/ratelimit.go). legacy 토큰 사용은 감사 기록에는 남지만 별도 알림은 없음 |
| AU-09 | 사용자 식별성 (공용 계정 배제) | 충족 | 모든 기록에 작업자: `user:이름`, `sa:이름`, `client:이름`, 로컬 CLI는 `cli:OS사용자@호스트`, 자동 조치는 `system`. docs/02 `auth`·`audit` | — |

## 3.2 접근 통제·권한

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| AC-01 | 역할 기반 접근 통제 | 충족 | 역할 viewer·deployer·operator·admin과 에이전트 전용 agent. `auth.role_bindings`. docs/02 `auth` | — |
| AC-02 | 권한 범위 제한 | 충족 | 범위 `*`, `team=팀`, `service=이름`. 목록 조회는 권한 있는 서비스만 반환. docs/02 `auth`, docs/06 "인증과 권한" | 서킷 리셋 등 서비스에 속하지 않는 작업은 `*` 범위 필요 |
| AC-03 | 직무 분리 (요청자·승인자 분리) | 충족 | `auth.four_eyes: true`면 배포 생성자·롤백 요청자는 그 건을 승인·거절할 수 없음. API·웹 콘솔(internal/api/server.go, internal/api/v2.go)과 로컬 CLI의 승인·거절·에스컬레이션 승인(cmd/vigilante/authcmd.go `localFourEyes`)에 모두 적용. 인증을 설정하면(`auth.local_cli: auto` 기본) 서버를 거치지 않는 로컬 승인·거절, `circuit reset`·`trip`, `--freeze-override`는 `--break-glass 사유` 없이는 거부(`localPrivileged`). docs/02 `auth`("로컬 CLI")·`rollback` | `four_eyes` 기본값은 꺼짐(설정 필요). `--break-glass`를 쓰면 `four_eyes`도 건너뛰지만 감사 기록과 critical 알림이 남음. 설정 파일을 고칠 수 있거나(`auth.local_cli: full`) 상태 저장소에 직접 쓸 수 있는 사람은 이 통제를 우회할 수 있으므로 설정 파일 쓰기 권한과 저장소 계정은 운영자에게만 줌. 전략별 2인 승인 정책은 미구현(docs/05 M4 구현 결과) |
| AC-04 | 위험 조치의 사전 승인 | 충족 | `rollback.mode: approve`, 에스컬레이션 단계별 `require_approval`. 승인자는 `approved_by`에 기록. docs/02 `rollback` | `rollback.mode`를 적지 않으면 자동(`auto`)이며 `validate`가 경고(internal/config/validate.go `Warnings`) |
| AC-05 | 권한 거부의 기록 | 충족 | 403 거부는 `action: denied`로 감사 기록. 콘솔 로그인 시 역할 바인딩이 없는 사용자도 기록. internal/api/server.go `denied` | — |
| AC-06 | 변경 통제 기간(동결) | 충족 | `change_freeze`(주간 반복·기간 지정), API 선언 `POST /v2/freezes`, 예외는 admin과 사유 기록. docs/02 `change_freeze` | 자동 롤백은 기본 허용(장애 복구), 기간별로 금지 가능(`allow_rollback: false`). 수동 롤백(operator 이상)은 동결과 관계없이 실행됨(internal/orchestrator/rollback.go). 동결 예외는 API(v1 본문 `freeze_override` 포함)에서 admin만 가능하고, 로컬 CLI의 `--freeze-override`는 인증 설정 시 `--break-glass`가 필요함(AC-03). `vigilante watch --server`는 `--freeze-override`를 서버로 전달해 서버가 판단 |
| AC-07 | 콘솔과 API 권한의 일치 | 충족 | 콘솔은 공개 API(v2)만 호출하며 버튼은 역할에 맞는 것만 노출. docs/02 `console` | — |

## 3.3 암호화 (전송 구간)

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| EN-01 | API·웹 콘솔 HTTPS | 충족 | `server.tls.cert_file`·`key_file`, 최소 TLS 1.2(`min_version: 1.3` 선택). 인증서 파일 변경 시 재시작 없이 교체. HA 팔로워가 리더로 요청을 전달할 때는 `server.ha.tls`(CA, 확인할 이름 `server_name`, 클라이언트 인증서)로 리더 인증서를 검증. internal/tlsconf/tlsconf.go(`HAClient`) | `server.tls`가 없으면 평문 HTTP이므로 직접 설정하거나 TLS 프록시 뒤에 두어야 함. Helm 차트는 서버 TLS를 켜면 HA 전달 주소·프로브·Ingress·ServiceMonitor를 https로 바꿈(OP-03) |
| EN-02 | 암호 스위트 통제 | 충족 | Go 표준 라이브러리(crypto/tls) 기본 스위트, TLS 1.2 미만 차단 | 스위트를 직접 지정하는 설정 키는 없음 |
| EN-03 | 검증필 암호모듈(KCMVP) 사용 | 미충족 | 암호 기능은 Go 표준 라이브러리 구현 사용(go.mod) | 국가 검증필 암호모듈을 탑재하지 않았고 검증 이력 없음 |
| EN-04 | 에이전트·서버 상호 인증 (mTLS) | 부분 충족 | `server.tls.client_ca_file`·`client_auth: optional·require`, `agent.tls.cert_file`·`key_file`. docs/02 `server`·`agent` | **에이전트 인증서 자동 발급·갱신·폐기는 미구현.** 사내 CA·cert-manager로 발급해 파일로 배치(docs/05 M6 구현 결과). 인증서가 토큰을 대신하지 않으며 API 토큰 인증은 그대로 적용됨 |
| EN-05 | 대상 호스트 SSH 보호 | 충족 | 호스트 키 검증 기본(`known_hosts_file`), Vault SSH CA 단기 인증서(`ssh_ca`). internal/transport/ssh.go | `insecure_ignore_host_key` 옵션이 있으며 `validate`가 경고하지 않음. 운영 설정에서 사용 금지 권장 |
| EN-06 | 외부 API(LB·하이퍼바이저·클라우드) 인증서 검증 | 충족 | 기본 검증. 사설 CA 지정(OpenStack `cacert` 등). docs/10 "통신 경로" | `tls_skip_verify` 옵션은 시험용이며 `validate`가 경고하지 않음 |
| EN-07 | 상태 저장소(PostgreSQL) 연결 암호화 | 충족 | DSN의 `sslmode=verify-full` 권장(docs/10 "통신 경로") | DSN을 고객이 지정. CI는 로컬 시험용 평문 연결 사용 |
| EN-08 | 감사 기록 SIEM 전송 암호화 | 충족 | `audit.syslog.address: tls://host[:port]`(RFC 5425 길이 접두 프레임, 포트 생략 시 6514)와 `audit.syslog.tls`(`ca_file`, `cert_file`·`key_file`, `server_name`, `min_version` 1.2·1.3). 수집기 인증서와 이름을 검증하고 실패하면 보내지 않고 재시도. internal/audit/syslog.go `NewExporter`, internal/tlsconf/tlsconf.go `Syslog`, 시험 internal/audit/syslog_tls_test.go. docs/02 `audit` | `tls://`를 지정해야 적용됨. `tcp://`·`udp://`(평문)도 계속 쓸 수 있으며 `validate`가 경고하지 않음. 실제 SIEM 제품(수집기)을 상대로 한 시험 기록 없음 |
| EN-09 | 웹훅 무결성 | 충족 | 아웃바운드: Standard Webhooks 형식 HMAC-SHA256 서명과 타임스탬프. 인바운드: GitHub HMAC 서명, GitLab 토큰, 비밀 미설정 시 거부(internal/api/server.go `verifyWebhook`). docs/06 "웹훅" | Jenkins·일반 웹훅은 일반 API 토큰으로 인증 |
| EN-10 | 메일 전송 암호화 | 부분 충족 | SMTP 기본 587 STARTTLS, 465 암묵적 TLS. docs/02 `notify` | STARTTLS는 메일 서버가 지원을 알릴 때만 사용하므로(internal/notify/email.go), 지원하지 않거나 중간에서 제거되면 평문으로 전송됨(단 Go `PlainAuth`는 평문 연결에서 비밀번호 전송을 거부). `no_starttls` 옵션으로 평문 전송 가능(신뢰 망 릴레이 전용으로 안내) |

## 3.4 암호화 (저장 데이터)

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| ST-01 | 설정 파일의 비밀값 평문 저장 금지 | 부분 충족 | 자격증명 구조(`credentials`)에는 평문 비밀번호 키가 없고 `*_ref`(Vault·파일·환경변수) 또는 `*_env`만 받음. docs/02 `credentials`, docs/10 "비밀값" | 상태 저장소 `server.state.dsn`과 `db` 프로브의 `dsn`은 접속 문자열을 직접 쓸 수 있어 비밀번호가 평문으로 들어갈 수 있음. `dsn_ref`·`dsn_env` 사용 권장. 알림 채널의 `url`(Slack·Teams 웹훅 URL은 그 자체가 비밀)도 직접 쓸 수 있으므로 `url_ref`·`url_env` 권장 |
| ST-02 | 저장 데이터(저널·DB) 암호화 | 미충족 | 파일 저널과 PostgreSQL 테이블을 제품이 직접 암호화하지 않음 | 저장 내용에 비밀값은 없음(참조 문자열만 기록, docs/02 `secrets`). 필요 시 디스크·DB 수준 암호화로 보완 |
| ST-03 | 웹 콘솔 세션 데이터 보호 | 충족 | 세션 쿠키는 ID 토큰을 AES-GCM으로 암호화·인증. internal/console/console.go | 키는 `console.session_key_ref`(32자 이상). 미설정 시 재시작마다 키 변경(`validate` 경고) |
| ST-04 | 웹훅 서명 비밀의 비저장 | 충족 | 구독별 서명 비밀은 마스터 키(`api.webhook_signing_key_ref`)에서 계산하며 저장하지 않음. docs/02 `api` | 마스터 키 교체 시 구독별 새 비밀 재발급 필요 |

## 3.5 비밀정보 관리

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| SE-01 | 외부 비밀 저장소 연동 | 충족 | HashiCorp Vault KV v2, 로그인 token·AppRole·Kubernetes, 네임스페이스, 사설 CA. docs/02 `secrets`, internal/secrets | 실제 Vault 대상 시험 기록 없음(모의: internal/secrets/vaulttest). CyberArk 미지원 |
| SE-02 | 로그의 비밀값 가림 | 충족 | 한 번이라도 해석한 값(6자 이상)은 로그 메시지·속성·오류에서 `[REDACTED]`. `*_env` 값 포함. docs/02 `secrets` | — |
| SE-03 | 진단 자료의 비밀값 제거 | 충족 | `vigilante support-bundle`이 설정·로그의 비밀번호·토큰·DSN·키·웹훅 URL·인증 헤더와 텍스트 속 토큰·JWT·URL 비밀번호를 제거. docs/05 M6-4 구현 결과 | 제공 전 내용 확인 권장(docs/07) |
| SE-04 | 장기 SSH 개인키 배포 최소화 | 충족 | `ssh_ca`: 메모리에서 일회용 ed25519 키를 만들고 Vault SSH CA 서명. 개인키는 디스크에 남지 않으며 수명 80% 경과 시 재발급. docs/02 `ssh_ca` | — |
| SE-05 | 비밀 파일 권한 | 충족 | 패키지가 설치하는 `/etc/vigilante/vigilante.yaml`·`agent.env`는 `0640 root:vigilante`(packaging/nfpm.yaml). `vigilante.env`는 같은 권한으로 만들도록 안내(docs/07, docs/10) | CI는 설치 후 권한을 출력만 하고 값을 비교하지는 않음(.github/workflows/ci.yml `stat`) |
| SE-06 | 비밀값 캐시 범위 | 충족 | 메모리 캐시만, 기본 `cache_ttl: 5m`. 설정·저널·감사 기록에는 참조 문자열만. docs/02 `secrets` | — |

## 3.6 감사 로그·무결성

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| AD-01 | 감사 대상의 범위 | 충족 | 모든 판정·조치·권한 거부, 작업자·출처·조치·대상·사유·변경 티켓. docs/02 `audit` | 상태 저장소 장애 중에 생긴 기록은 유실될 수 있음(AV-03 참고) |
| AD-02 | 감사 기록의 위·변조 검출 | 부분 충족 | 기록마다 직전 해시를 포함한 SHA-256 해시 체인. `audit.chain_key_ref`(32바이트 이상, `vault:`·`env:`·`file:`)를 설정하면 기록마다 체인 해시의 HMAC-SHA256(`mac`)을 붙여, 저장소 쓰기 권한만 있고 키가 없는 사람은 기록을 고친 뒤 체인을 다시 계산해도 검출됨. `vigilante audit verify`가 해시와 MAC을 검사하고(`--key REF`로 키 지정) 위치와 원인, 키 설정 전 기록 수(unkeyed), 키가 보호하기 시작한 위치(`keyed_from`)를 보고(internal/journal/journal.go, internal/audit/audit.go, cmd/vigilante/auditcmd.go, 시험 internal/audit/keyed_test.go). docs/02 `audit`, docs/10 "체인 키" | 체인 키는 선택 설정이며 없으면 이전과 같이 키 없는 해시 체인. **MAC을 모두 지우면 키를 설정한 적 없는 체인처럼 보임**(`audit verify`는 경고만 출력하고 성공으로 끝남). 키를 처음 설정한 시점의 `keyed_from`을 SIEM·변경 티켓 등 외부에 남겨 대조해야 함. **키 교체 미지원**(교체하면 이전 키로 쓴 기록이 MAC 불일치로 보고됨). 끝부분 기록을 잘라 내는 것은 체인으로 검출되지 않으므로 검증 결과의 마지막 해시(`head`)를 외부에 보관하거나 SIEM 사본과 대조. 키를 설정하면 저장소를 여는 모든 명령에 키가 필요하며, 키는 저장소 쓰기 권한자(DB 관리자, 저널 파일 소유자)가 읽을 수 없는 곳에 두어야 함 |
| AD-03 | 감사 기록 외부 전송 (SIEM) | 충족 | syslog RFC 5424(JSON 본문) 또는 CEF 실시간 전송. 저장 후 비동기 전송. TLS 전송(`tls://`, EN-08) 지원. docs/02 `audit`, internal/audit/syslog.go | 큐(1만 건)가 가득 차면 버리고 개수를 셈(`vigilante_audit_export_dropped_total`). 누락 구간은 `audit export`로 보충. `tcp://`·`udp://`를 쓰면 평문 |
| AD-04 | 감사 기록 보존·정리 | 충족 | `audit.retention`(기본값 없음, 예: 8760h = 1년)을 `vigilante audit prune`의 기준으로 사용. prune은 지울 구간을 아카이브 파일(`--out`, 필수)로 옮긴 뒤 삭제, 아카이브는 별도 검증 가능. docs/02 `audit` | 자동 삭제는 하지 않음. 정리는 운영자가 명령으로 실행. `audit.retention`이 없으면 `--before`나 `--older-than`을 지정해야 함(cmd/vigilante/auditcmd.go) |
| AD-05 | 감사 기록 조회 권한 | 충족 | `GET /v2/audit-events`는 전체 범위 viewer와 `audit:read` 스코프 필요. docs/06 "리소스" | — |
| AD-06 | 인증 실패 기록 | 부분 충족 | OAuth 클라이언트 인증 실패(internal/api/clients.go), 권한 거부, 역할 없는 콘솔 로그인은 감사 기록 | 잘못된 Bearer 토큰으로 인한 일반 401은 감사 기록이 아닌 요청 로그(원격 주소 포함)에만 남음(internal/api/observe.go) |
| AD-07 | 조치 근거·변경 티켓 연계 | 충족 | 콘솔 조작은 사유 필수, API `X-Change-Ticket`(v1 생성 본문은 `change_ticket`도 받음), CLI `--ticket`. ServiceNow 변경 티켓 작업 노트. docs/02 `audit`·`itsm`, docs/06 | `vigilante watch --server`도 `--ticket`을 서버로 전달함(cmd/vigilante/main.go `watchRemote`) |

## 3.7 입력 검증·웹 보안 (웹 콘솔 포함)

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| WB-01 | 콘텐츠 보안 정책(CSP) | 충족 | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`. internal/console/console.go `securityHeaders` | 인라인 스크립트·외부 CDN 없음 |
| WB-02 | 클릭재킹 방지 | 충족 | `X-Frame-Options: DENY`, CSP `frame-ancestors 'none'` | — |
| WB-03 | CSRF 방지 | 충족 | 쿠키로 인증한 변경 요청은 `X-CSRF-Token` 헤더 필요(double-submit, 상수 시간 비교). CSRF 쿠키는 `SameSite=Strict`. docs/02 `console`, internal/api/server.go `authn` | `Authorization` 헤더 요청은 쿠키를 보지 않음 |
| WB-04 | 세션 쿠키 속성 | 충족 | 세션 쿠키 `HttpOnly`, `SameSite=Lax`, https일 때 `Secure`. internal/console/console.go | `console.redirect_url`이 http면 `Secure`가 빠지며 `validate`가 경고 |
| WB-05 | OIDC 로그인 흐름 보호 | 충족 | PKCE 항상 사용, `state` 상수 시간 비교, 흐름 쿠키 10분 수명. internal/console/console.go | — |
| WB-06 | 교차 사이트 스크립팅(XSS) | 충족 | API 데이터는 모두 텍스트로만 화면에 삽입, CSP로 인라인 스크립트 차단. docs/02 `console` | 외부 웹 취약점 점검 미실시 |
| WB-07 | HTTPS 강제(HSTS) | 충족 | `server.tls`가 설정되었거나, `console.redirect_url`이 `https://`이거나, 요청이 TLS로 들어오면 콘솔 응답에 `Strict-Transport-Security: max-age=31536000`을 보냄(internal/console/console.go `securityHeaders`, 시험 internal/console/console_test.go). docs/10 "웹 콘솔" | `includeSubDomains`·`preload`는 넣지 않음(같은 도메인의 다른 호스트가 평문 HTTP일 수 있으므로). 필요하면 프록시에서 추가. TLS를 끝내는 프록시 뒤에서 `redirect_url`을 http로 두면 보내지 않음 |
| WB-08 | 기타 보안 헤더 | 충족 | `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`. 로그인 경로(`/console/auth/*`)를 포함한 모든 콘솔 응답에 적용 | — |
| WB-09 | 설정 입력 검증 | 충족 | 알 수 없는 키(오타) 즉시 거부, 모든 교차 참조 검사, 롤백 규칙 없는 서비스 거부. docs/02 서두, docs/04 S17 | — |
| WB-10 | 원격 명령 주입 방지 | 부분 충족 | 실행기는 경로·유닛·도메인 이름 인자를 셸 인용해 명령 구성(internal/executor, `ShellQuote`) | 사용자가 직접 쓴 명령(`exec` 실행기, `restart_cmd` 등)은 쓴 그대로 실행. `sudo_scope: all`은 `sudo -n sh -c`로 감쌈. 명령 주입에 대한 외부 점검·퍼징 미실시 |
| WB-11 | 서버 측 요청 위조(SSRF) 방지 | 부분 충족 | 웹훅 구독 URL을 `api.webhook_allowed_hosts`(호스트 접미사)로 제한, 리디렉션 미추종. docs/06 "웹훅" | 허용 목록을 비우면 제한 없음(기본값). 운영에서는 사내 도메인으로 제한 권장 |

## 3.8 API 보안

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| AP-01 | 표준 인증 방식 | 충족 | OAuth 2.0 client credentials(RFC 6749 4.4), API 키, 서비스 계정 토큰, OIDC 토큰. docs/06 "인증과 권한" | — |
| AP-02 | 스코프 기반 권한 축소 | 충족 | 스코프 8종(`deployments:read` 등)과 역할 grant를 함께 판단. 스코프 축소는 발급된 토큰에 즉시 적용. docs/06 | — |
| AP-03 | 호출 한도 | 부분 충족 | 호출자별 토큰 버킷(기본 초당 20·순간 40), 비상 조치(롤백·승인·중단·서킷) 별도 버킷(초당 1·순간 10), 초과 시 429 + `Retry-After`. docs/06 "호출 한도", internal/api/ratelimit.go | v2 경로에만 적용되며 기존 연동용 v1 경로와 비상용 legacy 토큰은 제외. 미인증 요청은 한도 대상이 아님(AU-06) |
| AP-04 | 멱등성 | 충족 | 모든 POST·PUT에 `Idempotency-Key`, 호출자별 24시간 보관, 같은 키·다른 본문은 거부. docs/06 "공통 규약" | 비밀값을 담은 응답(클라이언트 등록·비밀 회전)은 재생하지 않음 |
| AP-05 | 동시 수정 방지 | 충족 | `ETag`·`If-Match`, 불일치 시 412. docs/06 | — |
| AP-06 | 토큰 수명 | 충족 | OAuth 액세스 토큰 기본 1시간, 최대 24시간(`api.token_ttl`). 비밀 회전 시 기존 토큰 즉시 무효. docs/02 `api`, docs/06 | 서비스 계정 토큰은 `expires`를 두지 않으면 만료 없음 |
| AP-07 | 공개 범위 | 충족 | 사내 전용(사내 시스템·개발자). 외부·파트너 공개 계획 없음. docs/06 서두 | — |
| AP-08 | API 계약의 변경 통제 | 충족 | OpenAPI 3.1 명세 원본, 계약 테스트, CI의 Spectral 린트와 oasdiff 하위호환 검사(위반 시 PR의 CI 실패). .github/workflows/ci.yml `api` | 병합 차단 여부는 저장소의 브랜치 보호 설정에 따름 |

## 3.9 서버 권한·최소 권한

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| PR-01 | 전용 비특권 계정으로 실행 | 충족 | 패키지가 시스템 계정 `vigilante`(로그인 셸 `nologin`) 생성(packaging/scripts/preinstall.sh), systemd 유닛 `User=vigilante` | — |
| PR-02 | 서비스 프로세스 격리 (systemd) | 충족 | 서버 유닛: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, 쓰기는 `/var/lib/vigilante`만, `PrivateTmp`, `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectControlGroups`, `RestrictSUIDSGID`, `LockPersonality`(packaging/systemd/vigilante-server.service) | 에이전트 유닛은 일부만 적용. 에이전트 자율 롤백(`failsafe: rollback`)을 쓰면 쓰기 경로와 sudo를 위해 drop-in으로 완화해야 함(packaging/systemd/vigilante-agent.service 주석) |
| PR-03 | 컨테이너 최소 권한 | 충족 | 이미지: distroless static, `USER nonroot`(packaging/container/Dockerfile). Helm: `runAsNonRoot`, UID 65532, `readOnlyRootFilesystem`, capability 전부 제거, `allowPrivilegeEscalation: false`, seccomp RuntimeDefault(deploy/helm/vigilante/values.yaml) | 이미지에 셸·패키지 관리자 없음 |
| PR-04 | 대상 호스트 sudo 최소화 | 부분 충족 | `connection.sudo_scope: changes`면 읽기는 sudo 없이, 변경 명령만 하나씩 `sudo -n`. `vigilante sudoers`가 대상별 sudoers 규칙 생성, `doctor`가 규칙마다 `sudo -n -l`로 확인. docs/10 "대상 호스트" | **기본값은 `sudo_scope: all`**(하위 호환)이며 모든 명령을 `sudo -n sh -c`로 실행해 사실상 root 권한이 필요함. `validate`·`doctor`가 경고. sudoers의 `*`는 공백 포함 문자열과 맞으므로 경로 쓰기 권한 통제 필요 |
| PR-05 | 외부 시스템 계정의 최소 역할 | 부분 충족 | F5·vCenter·Nutanix·OpenStack·AWS·ServiceNow·PostgreSQL별 권장 역할과 AWS IAM 정책 예시(docs/10 "외부 시스템 계정") | 제품 버전별 실제 역할로 충분한지는 실장비에서 확인하지 않음(M8 랩에서 확정) |
| PR-06 | 고위험 접근 경로의 고지 | 부분 충족 | Docker 소켓 접근은 root와 같음, libvirt 그룹은 그 호스트의 모든 VM 제어 가능함을 문서로 고지하고 rootless Podman 권장(docs/10) | `container`·`docker`·`kvm` 플러그인은 구조상 높은 권한이 필요함 |
| PR-07 | SSH 세션 자원 보호 | 충족 | 대상별 동시 세션 상한 `max_sessions`(기본 8), 롤백 전용 `reserved_sessions`(기본 2). docs/02 `targets` | — |

## 3.10 네트워크·포트

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| NW-01 | 수신 포트 최소화 | 충족 | 서버는 8088 하나로 API·콘솔·웹훅·에이전트·지표를 받음. 에이전트는 수신 포트 없음(서버로 push). docs/10 "통신 경로" | — |
| NW-02 | 기본 바인딩 주소 | 충족 | 패키지 기본 `127.0.0.1:8088`(packaging/etc/vigilante.yaml) | 설정 키 기본값은 `:8088`. Helm은 ClusterIP 서비스 |
| NW-03 | 송신 연결 목록 제공 | 충족 | docs/10 "통신 경로" 표 (이 문서 2.2) | — |
| NW-04 | 지표 엔드포인트 접근 통제 | 충족 | `/metrics`는 기본으로 전체 범위 viewer 토큰 필요. `server.metrics_public`으로만 해제. docs/02 `server` | 지표 레이블에 대상 이름·배포 ID를 넣지 않음 |
| NW-05 | 무인증 엔드포인트 | 부분 충족 | `/healthz`·`/readyz`는 인증 없이 응답(internal/api/server.go) | `/healthz` 응답에 노드 역할, 리더, 서킷 상태, 드라이런 여부, 저장소 위치(저널 파일 경로 또는 PostgreSQL 호스트·포트·DB 이름, 비밀번호 제외)가 포함됨. 외부 노출이 필요 없으면 프록시에서 제한 |
| NW-06 | 폐쇄망 운영 | 충족 | 콘솔 외부 자원 없음, 폐쇄망 설치 번들, openssl만으로 서명 확인, 시간대 데이터 내장. docs/07 "폐쇄망", docs/02 `change_freeze` | OIDC 사용 시 서버가 사내 IdP의 discovery 문서에 접근해야 함 |
| NW-07 | 제조사로의 데이터 전송 | 충족 | 사용 통계·원격 진단 전송 기능 없음. 외부 연결은 고객이 설정한 연동 대상뿐. 지원 번들은 고객이 직접 만들어 전달(`vigilante support-bundle`) | — |

## 3.11 가용성·장애 대응

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| AV-01 | 이중화 구성 | 충족 | PostgreSQL 리더 리스 기반 HA, 펜싱, 팔로워의 리더 전달, 리더 교체 후 롤백 재개. docs/02 `server`, docs/07 "HA" | 목표값(가용성 99.9%, 인계 30초 이내)은 제안값이며 측정 전 |
| AV-02 | 비정상 종료 후 복구 | 충족 | 결정을 먼저 기록하고 실행, 재시작·리더 교체 시 완료 단계를 건너뛰고 재개. docs/04 S12 | — |
| AV-03 | 외부 연동 장애 시 핵심 기능 유지 | 충족 | ITSM·알림·SIEM·콘솔 장애가 롤백을 막지 않음. ServiceNow 호출은 연결 오류·429·5xx일 때 1·2·4초 후 재시도하고 인시던트는 백그라운드로 생성(internal/itsm/servicenow.go, internal/api/itsm.go). Vault 장애 시 `cache_ttl` 동안 캐시 값 사용. 상태 저장소 장애 중 쓰지 못한 기록(감사 기록 포함)은 메모리 대기열에 순서대로 쌓았다가 복구 후 기록(`vigilante_store_pending_writes`, internal/orchestrator/engine.go). 장애 중에 새로 시작하는 롤백은 `safety.rollback_lease.wait`(10초) 동안 서비스 잠금(상태 저장소 리스)을 다시 시도한 뒤, 기본값 `on_unavailable: proceed`이면 프로세스 안 잠금만으로 진행하고 배포 이벤트(`safety`)·감사(`lease.unavailable`)·경고 알림을 남김. 저장소가 돌아왔을 때 다른 프로세스가 리스를 잡고 있으면 `lease.conflict`로 알림(internal/safety/safety.go `Acquire`, docs/04 S12a, 시험 `TestChaosStoreOutageDuringRollback`·`TestChaosStoreOutageLeaseFailMode`). docs/05 "설계 원칙" 1, docs/02 `secrets`·`itsm`·`notify`·`safety` | 캐시가 만료되면 그 자격증명이 필요한 프로브·실행기는 실패. 캐시 시간을 관측 창보다 길게 권장. 저장소 장애 중 리스 없이 진행한 롤백은 다른 프로세스(예: 로컬 CLI)의 롤백과 겹칠 수 있음(사후 `lease.conflict` 알림). 겹침을 허용하지 않으려면 `on_unavailable: fail`(롤백하지 않고 ROLLBACK_FAILED). 대기열은 메모리에만 있어 장애 중 프로세스가 죽으면 유실되고, 10만 건을 넘는 기록은 버림(`vigilante_store_errors_total{reason="dropped"}`). HA에서 장애가 `lease_ttl`(기본 15초)보다 길면 리더가 물러나 판정이 멈추고, 다른 노드가 리더가 되면 대기열은 펜싱되어 버려짐 |
| AV-04 | 자동화 오동작 확산 방지 | 충족 | 서킷 브레이커, 장애 반경(blast radius), 반복 롤백 제한, 관측 쿼럼, 환경 요인 보류. docs/04 | — |
| AV-05 | 비상 정지 | 충족 | `vigilante circuit trip`, `POST /v2/circuit/trip`(admin, 사유 필수). docs/04, docs/06 | admin 검사는 API에 적용. 서버 없이 실행하는 로컬 CLI의 `circuit trip`·`reset`은 역할을 검사하지 않는 대신, 인증 설정 시 `--break-glass 사유`가 필요하고 감사·critical 알림이 남음(AC-03) |
| AV-06 | 백업·복구 | 부분 충족 | 스키마 되돌리기(`vigilante store migrate --down-to`)와 다운그레이드 가드. docs/08 "되돌리기" | 상태 저장소 백업 기능은 제품에 없음. PostgreSQL 백업 정책·파일 저널 백업으로 수행 |
| AV-07 | 부하·장애 주입 시험 | 부분 충족 | 카오스 시나리오(internal/orchestrator/chaos_test.go: 롤백 중·롤백 시작 시 저장소 단절, 리스 `fail` 모드, LB API 일시 오류·지연, LB 완전 장애, 관측 장치 과부하 시 보류)와 부하 하네스(test/load, .github/workflows/load.yml, 주 1회와 엔진 변경 PR). GitHub Actions ubuntu-latest에서 대상 2,000 × 프로브 10, 동시 배포 100건: 오판 0건, 판정 지연 p99 5.7초(목표 12초), 프로브 9,095건/초, 최대 힙 1.1 GiB, 평균 0.72코어. 프로브 3개: p99 5.5초, 352 MiB, 0.45코어(docs/05 M5-4 구현 결과). 관측 장치 과부하 대책: `safety.observer_guard`(기본 켜짐)가 내부 타이머 지연(`max_lag` 1초), 루프백 왕복(`loopback_timeout` 1초), 여러 서비스(3개 이상)에 걸친 대상 절반 이상의 프로브 시간 초과를 감시하고, 그동안과 회복 후 `grace`(1분) 동안 서버가 직접 재는 프로브(`http`, `tcp`, `grpc`, `db`, SSH로 읽는 `host`)의 실패 지표(`up`, `latency_ms`, `consecutive_failures`, `consecutive_timeouts`, `timeout`)와 모든 프로브의 `probe_error` 기반 위반을 롤백 대신 보류(HOLD)함(internal/observer, internal/decision/decision.go, internal/orchestrator/engine.go `observerProbes`, docs/04 S14a). 지표 `vigilante_observer_degraded`, `vigilante_observer_degradations_total{signal}`, `vigilante_observer_holds_total`. PR #13의 CI(부하 시험 포함)와 master `dd9a055`의 CI 통과, PR #14의 CI(부하 시험 포함)와 master `537870c`의 CI 통과 | 대상은 HTTP 시뮬레이터라 SSH·sshd 부하는 재지 않음. 관측 장치 보호는 과부하 신호를 주입한 시험(`TestChaosObserverDegradedHolds`, internal/observer/observer_test.go)으로 확인했으며, 정상 배포 90건을 롤백했던 조건(다른 작업으로 바쁜 개발 PC)에서 다시 측정하지는 않음. 관측 장치가 불안정한 동안에는 실제 불량 배포라도 위 지표를 쓰는 규칙의 위반은 롤백되지 않고 보류됨. 5xx 비율, 로그 패턴, 자원 사용률, 그리고 액세스 로그의 `latency_ms`처럼 대상이 보고한 같은 이름의 지표(`log`·`access_log`·`docker` 프로브)를 쓰는 규칙의 위반은 그대로 판정(PR #14). 서버 자원은 위 측정값보다 넉넉히 잡아야 함 |

## 3.12 공급망 보안

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| SC-01 | 소프트웨어 구성 명세(SBOM) | 충족 | 바이너리별 CycloneDX SBOM(cyclonedx-gomod), 폐쇄망 번들에 포함. scripts/release.sh, docs/07 "배포 형태" | 정식 릴리스 전이므로 CI에서 생성만 확인 |
| SC-02 | 배포물 서명 | 부분 충족 | 모든 파일의 `SHA256SUMS`를 cosign 키로 서명, 이미지도 cosign 서명, openssl만으로 확인 가능. CI가 PR마다 임시 키로 `SHA256SUMS` 서명·검증(이미지 서명은 릴리스 워크플로우에서만 하며 아직 실행된 적 없음). docs/07 "서명 확인", .github/workflows/release.yml | **릴리스 서명 키를 아직 만들지 않음**(`packaging/cosign.pub` 없음). 키가 없으면 릴리스 워크플로우는 실패하도록 되어 있음. 서명된 정식 릴리스는 아직 없음 |
| SC-03 | 공개 투명성 로그(Rekor) 기록 | 해당 없음 | 설계상 사용하지 않음(`--tlog-upload=false`). 사내 릴리스이고 폐쇄망에서 확인해야 하기 때문. docs/07, docs/05 M6 구현 결과 | 검증 시 `--insecure-ignore-tlog=true` 필요. 신뢰 기준은 저장소의 공개 키 또는 사내 배포 사본 |
| SC-04 | 의존성 취약점 점검 | 충족 | 모든 PR과 master push에서 govulncheck(v1.8.0) 실행, 발견 시 CI 실패. 표준 라이브러리 취약점 11건과 `golang.org/x/net` 취약점을 찾아 툴체인 go1.27.2 고정, x/net v0.60.0으로 갱신(docs/05 M8 구현 결과) | govulncheck는 코드에서 도달 가능한 Go 모듈 취약점만 다룸 |
| SC-05 | 정적 분석 | 부분 충족 | gofmt, `go vet`, race detector(`go test -race`)를 CI에서 실행 | 보안 전용 정적 분석 도구(SAST)는 사용하지 않음 |
| SC-06 | 컨테이너 이미지 취약점 점검 | 부분 충족 | 기반 이미지는 셸·패키지 관리자가 없는 distroless static | 이미지 취약점 스캐너는 CI에 없음 |
| SC-07 | 의존성 최소화 | 충족 | 최소 빌드가 vSphere·AWS SDK·gRPC·MySQL 의존성을 제외(38MB에서 18MB). OpenStack·F5·Nutanix 클라이언트는 표준 라이브러리로 구현. docs/05 M6·M7 구현 결과 | — |

## 3.13 개인정보

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| PI-01 | 처리하는 개인정보 항목 | 충족 | 운영자 식별 정보만 처리: OIDC 사용자 이름과 그룹(권한 판단), CLI의 OS 사용자@호스트, 조치 사유·메모, API 요청 로그의 원격 주소. docs/02 `auth`·`audit`, internal/api/observe.go | 서비스 최종 이용자의 정보는 수집 목적이 아님. 로그 프로브는 원문을 저장하지 않고 수치 지표로만 변환(docs/02 "probes") |
| PI-02 | 개인정보 보관·파기 | 충족 | 감사 기록의 작업자 정보는 `audit prune`으로 보존 기간 후 정리. docs/02 `audit` | 정리는 운영자가 실행 |
| PI-03 | 진단 자료의 개인정보 | 부분 충족 | 지원 번들은 비밀값을 제거 | 운영자가 `--log`로 지정한 로그 파일은 원문(파일당 최근 20MB)이 포함되므로 개인정보가 들어갈 수 있음. 전달 전 확인 필요 |
| PI-04 | 제3자 제공 | 충족 | 제조사·제3자로 자동 전송 기능 없음(NW-07) | 알림 채널·ServiceNow에 보내는 내용은 고객이 설정 |

## 3.14 패치·업데이트

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| UP-01 | 버전 정책과 호환성 약속 | 충족 | SemVer, API·설정·CLI 종료 코드·스키마·지표의 호환성 약속. docs/08 "호환성 약속" | — |
| UP-02 | 보안 패치 지원 기간 | 부분 충족 | 현재 MAJOR의 최신 MINOR 두 개에 보안·치명 버그 수정(docs/08 "지원 기간") | 문서상 제안값이며 계약상 기한(SLA)은 정하지 않음. 출시 이력이 없어 실적 없음 |
| UP-03 | 무중단 업그레이드 | 충족 | HA 순차 업그레이드(팔로워 먼저, 리더 마지막), Helm 롤링 업데이트와 PodDisruptionBudget. docs/08 | 실제 버전 간 업그레이드 이력은 첫 릴리스 이후 생김 |
| UP-04 | 업그레이드 되돌리기 | 충족 | 이전 패키지 재설치, `.down.sql` 되돌리기, 다운그레이드 가드. docs/08 "되돌리기" | 첫 마이그레이션(감사 기록 포함 테이블 생성)은 되돌리지 않음 |
| UP-05 | 업데이트 파일 무결성 확인 | 부분 충족 | 업그레이드 전 서명·체크섬 확인 절차(docs/08 "업그레이드 전 점검") | SC-02와 같은 한계(릴리스 키 미생성) |
| UP-06 | 자동 업데이트 | 해당 없음 | 자동 업데이트·원격 업그레이드 기능 없음. 고객이 패키지·번들로 적용 | 에이전트 원격 업그레이드는 2차(M5-2) |

## 3.15 설치·운영 환경

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| OP-01 | Linux 서버 설치 패키지 | 충족 | rpm·deb, systemd 유닛, 전용 계정. CI가 Rocky Linux 9·Debian 12 컨테이너에 설치 확인(.github/workflows/ci.yml) | master `dd9a055`와 `537870c`의 CI에서 통과. CI 컨테이너에는 systemd가 돌지 않으므로 서비스 실제 기동은 확인하지 않음. amd64 외 아키텍처도 확인하지 않음. 패키지 메타데이터의 maintainer(noreply 주소)와 license(`Proprietary`)는 임시값(packaging/nfpm.yaml) |
| OP-02 | Windows 서버 운영 | 미충족 | Windows 바이너리는 CLI·CI 용도이며 Windows 서비스 래퍼·설치 패키지 없음(docs/05 M6 구현 결과) | 서버·에이전트는 Linux에서 운영 |
| OP-03 | Kubernetes 배포 | 부분 충족 | Helm 차트(단일·HA), 보안 컨텍스트 기본 적용. 인증 설정이 없으면 렌더링 거부(AU-02). 서버 TLS(`config`의 `server.tls.cert_file` 또는 `tls.enabled`, 인증서는 `tls.secretName` 시크릿)를 켜면 HA 전달 주소·프로브·포트 이름·Ingress 백엔드·ServiceMonitor가 https로 바뀌고, 프로브가 인증서를 낼 수 없는 `client_auth: require`는 거부. 메모리 기본값 요청 512Mi·한도 2Gi, `GOMEMLIMIT`은 한도의 90%(deploy/helm/vigilante/values.yaml, templates/_helpers.tpl). docs/07 "Kubernetes" | CI는 렌더링(인증 없는 설정 거부, HA+TLS 출력 확인)만 함. 실제 클러스터 설치·업그레이드 시험 없음. `existingConfigMap`을 쓰면 차트가 설정 내용을 확인하지 못함 |
| OP-04 | 배포 전 설정 점검 도구 | 부분 충족 | `vigilante validate`(설정), `vigilante doctor`(접속·sudo·자격증명·LB 풀 등 읽기 전용 점검) | `insecure_ignore_host_key`, `tls_skip_verify` 같은 시험용 옵션은 경고하지 않음 |

## 3.16 취약점 신고·외부 검증

| 번호 | 점검 항목 | 응답 | 근거 | 비고 |
|---|---|---|---|---|
| VR-01 | 취약점 신고 창구 | 부분 충족 | 보안 문제는 공개 이슈가 아닌 저장소 관리자에게 비공개로 알리도록 안내(docs/10 "취약점 신고") | 전용 신고 주소, 보안 정책 문서, 대응 기한, 공지 절차는 아직 없음 |
| VR-02 | 외부 침투 테스트 | 미충족 | 수행하지 않음 | 출시 전 수행 여부와 시점은 미정 |
| VR-03 | 보안 인증 (CC, GS 인증 등) | 미충족 | 보유한 인증 없음 | — |
| VR-04 | 보안 취약점 공지 이력 | 미충족 | 정식 릴리스가 없어 공지 이력 없음 | — |

# 4. 미충족·부분 충족 항목 정리

## 4.1 미충족 항목

| 번호 | 항목 | 고객 측 보완 방법 | 제품 개선 계획 |
|---|---|---|---|
| EN-03 | 검증필 암호모듈(KCMVP) | 공공기관 요건이면 별도 협의 필요 | 계획 없음 |
| ST-02 | 저장 데이터 암호화 | 디스크 암호화, DB 수준 암호화 | 로드맵에 없음 |
| OP-02 | Windows 서버 운영 | Linux 서버에서 운영 | 로드맵에 없음 |
| VR-02 | 외부 침투 테스트 | 도입 심사 시 고객 측 점검 수용 가능 여부 협의 | 미정 |
| VR-03 | 보안 인증 | — | 미정 |
| VR-04 | 취약점 공지 이력 | — | 첫 릴리스 이후 |

"로드맵에 없음"은 docs/05-roadmap.md에 해당 작업이 없다는 뜻입니다. 1.1판의 미충족 항목 중 EN-08(SIEM 전송 평문)과 WB-07(HSTS)은 PR #13으로 충족이 되어 이 표에서 뺐습니다.

## 4.2 주요 부분 충족 항목

| 번호 | 한계 | 보완 방법 |
|---|---|---|
| AU-06, AP-03 | 미인증 요청·v1 경로·legacy 토큰에 호출 한도 없음 | 앞단 프록시·WAF 제한, legacy 토큰 미사용 |
| AU-07 | 유휴 세션 만료 없음, 개별 세션 강제 종료 불가 | IdP 토큰 수명 단축, 필요 시 세션 키 교체 |
| EN-04 | 에이전트 인증서 자동 발급·갱신·폐기 미구현 | 사내 CA·cert-manager로 발급, 파일 교체 시 자동 재적재 |
| AD-02 | 체인 키는 선택 설정. MAC을 모두 지우면 키 없는 체인처럼 보이고, 키 교체는 미지원 | `audit.chain_key_ref` 설정(저장소 쓰기 권한자가 읽을 수 없는 Vault 경로), 키 설정 시점의 `keyed_from`과 `audit verify` 결과의 `head`를 외부 보관, SIEM 사본과 대조 |
| PR-04 | `sudo_scope` 기본값 `all` | 모든 대상에 `sudo_scope: changes`와 `vigilante sudoers` 적용 |
| AV-07 | 부하 시험은 HTTP 시뮬레이터 기준(SSH 부하 미측정), 관측 장치 보호는 실제 과부하 조건에서 재측정하지 않음 | 서버 자원을 측정값(프로브 2만 개에 힙 약 1.1 GiB)보다 넉넉히 잡고, `vigilante_observer_degraded`를 감시하며 파일럿 규모로 시작 |
| OP-03 | Helm 차트는 렌더링만 시험, 실제 클러스터 설치 시험 없음 | 고객 클러스터의 시험 네임스페이스에서 설치·업그레이드를 먼저 확인 |
| SC-02, UP-05 | 릴리스 서명 키 미생성 | 첫 릴리스 전 키 생성(키는 Vault·HSM·오프라인 매체 보관, docs/07) |

1.1판에 있던 AU-02(Helm 기본 설정에 인증 없음)와 AC-03(로컬 CLI가 `four_eyes`·역할을 검사하지 않음)은 PR #13으로 충족이 되어 이 표에서 뺐습니다. 두 항목의 남은 조건(`existingConfigMap` 미확인, 설정 파일·저장소 접근 권한 관리)은 3장 비고에 적었습니다.

# 5. 고객 운영 환경 권장 설정

도입 시 아래를 설정하면 위 응답의 "충족" 조건을 갖춥니다. 근거는 docs/10-security.md, docs/07-install.md, docs/02-config-spec.md입니다.

1. `auth`(OIDC와 서비스 계정)를 설정한 뒤에만 `listen`을 외부 주소로 바꿉니다. 서비스 계정에는 `expires`를 둡니다. Helm에서는 `auth.allowAnonymous`를 개발용 설치에만 쓰고, `existingConfigMap`을 쓰면 인증 설정을 직접 확인합니다.
2. `server.tls`로 HTTPS를 켜거나 TLS 프록시 뒤에 둡니다. TLS 프록시 뒤라면 `console.redirect_url`을 `https://`로 두어 HSTS가 나가게 하고, `includeSubDomains`가 필요하면 프록시에서 추가합니다. HA에서 서버 TLS를 켜면 `server.ha.tls`에 노드 인증서의 CA와 인증서에 들어 있는 이름을 지정합니다.
3. 역할은 좁게 부여합니다: CI는 `deployer`에 서비스 범위, 운영자는 `operator`에 팀 범위, admin은 소수. 승인 모드에서는 `auth.four_eyes: true`. `auth.local_cli`는 기본값(`auto`, 인증 설정 시 제한)을 유지하고, 승인·서킷 조작은 `--server`와 토큰으로 하며 `--break-glass` 알림은 경보로 다룹니다. 서버의 설정 파일과 상태 저장소 접속 정보는 서버 계정만 읽고 쓰게 두어, 저장소를 직접 다루어 통제를 우회하지 못하게 합니다.
4. 모든 서비스에 `rollback.mode`를 명시하고, 파일럿을 통과하기 전에는 `approve`로 둡니다.
5. 대상 호스트는 전용 계정, Vault SSH CA 단기 인증서, `sudo_scope: changes`와 `vigilante sudoers` 규칙을 씁니다.
6. 비밀값은 모두 `*_ref`(Vault 권장)로 지정하고 `dsn` 직접 기입을 피합니다. `console.session_key_ref`를 설정합니다.
7. `api.webhook_allowed_hosts`를 사내 도메인으로 제한합니다.
8. `audit.syslog.address`는 `tls://`로 지정하고(`audit.syslog.tls.ca_file`로 수집기 CA 지정), `audit.chain_key_ref`를 Vault에 둡니다. 키를 설정한 시점의 `keyed_from`을 기록하고, 주기적으로 `vigilante audit verify`를 실행해 결과(`head`)를 외부에 보관합니다.
9. `insecure_ignore_host_key`와 `tls_skip_verify`는 운영 설정에서 쓰지 않습니다.
10. 설치 전 `SHA256SUMS` 서명을 저장소 또는 사내 배포 사본의 공개 키로 확인합니다.
11. `safety.observer_guard`는 켜 둔 채(기본) `vigilante_observer_degraded`와 `vigilante_store_pending_writes`를 감시합니다. 상태 저장소 장애 중 리스 없는 롤백(`lease.unavailable`)을 허용하지 않는 환경이면 `safety.rollback_lease.on_unavailable: fail`로 둡니다.
