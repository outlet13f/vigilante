# 08. 버전 정책과 업그레이드

## 버전 체계

릴리스 번호는 [유의적 버전(SemVer)](https://semver.org/lang/ko/) `MAJOR.MINOR.PATCH`입니다.

| 올라가는 자리 | 의미 | 업그레이드 |
|---|---|---|
| PATCH (1.2.**3**) | 버그·보안 수정. 동작 변경 없음 | 언제든 적용. 되돌리기도 자유 |
| MINOR (1.**3**.0) | 기능 추가. 기존 설정·API·스크립트는 그대로 동작 | 순차 업그레이드(아래) 가능. 스키마 변경은 추가만 |
| MAJOR (**2**.0.0) | 호환되지 않는 변경 가능. 릴리스 노트의 "업그레이드 주의"를 반드시 확인 | 절차와 되돌리기 방법을 노트에 따로 적음 |

`-rc.1` 같은 접미사가 붙은 버전은 사전 릴리스(release candidate)이며 운영 환경에는 권장하지 않습니다. `vigilante version`이 버전, 빌드 종류(full·minimal), 커밋, 빌드 시각을 보여 줍니다.

## 호환성 약속

같은 MAJOR 안에서는 아래를 깨지 않습니다. 깨야 하는 변경은 다음 MAJOR로 미루고, 그 전 MINOR에서 폐기 예고(경고)를 먼저 냅니다.

| 대상 | 약속 | 지키는 장치 |
|---|---|---|
| REST API v2 (`/v2`, `api/openapi.yaml`) | 필드·엔드포인트·값 추가만. 삭제·의미 변경은 `/v3`에서. 폐기는 `Deprecation`·`Sunset` 헤더로 알리고 최소 6개월 유지 | CI의 oasdiff 하위호환 검사(PR 차단) |
| REST API v1 | 동결. 기능을 더하지 않고, 제거는 MAJOR에서 6개월 전 예고 후 | 계약 테스트 |
| 설정 파일 (`version: v1`) | 키 추가만, 새 키의 기본값은 기존 동작. 키 제거·의미 변경은 설정 `version: v2`와 변환 명령(`vigilante config migrate`)이 함께 나옴 | 폐기 예고는 `vigilante validate` 경고와 릴리스 노트로 (지금은 폐기 예정 키 없음) |
| CLI 종료 코드 | 0 PASS · 1 오류 · 2 롤백 완료 · 3 롤백 실패/서킷 OPEN/승인 대기/동결 · 4 HOLD는 고정 | E2E 데모 |
| CLI 명령·플래그 | 추가만. 제거는 한 MINOR 동안 경고 후 MAJOR에서 | — |
| 이벤트 (CloudEvents `type`) | 종류 추가만. 수신 측은 모르는 종류를 무시해야 함 | 명세의 `x-extensible-enum` |
| 웹훅 서명 형식 | Standard Webhooks 형식 유지 | 테스트 |
| 상태 저장소 스키마 (PostgreSQL) | MINOR의 마이그레이션은 추가만(이전 릴리스 노드가 계속 동작). 호환되지 않는 변경은 MAJOR에서, 표시를 붙여서 | 다운그레이드 가드(아래) |
| 파일 저장소 저널 | 새 기록 종류는 추가만. 이전 릴리스는 모르는 기록을 건너뜀 | — |
| 지표 이름 (`/metrics`) | 이름·레이블 유지. 바꿀 때는 MAJOR에서, 한 MINOR 동안 둘 다 냄 | 테스트 |
| 에이전트 ↔ 서버 | 서버보다 한 MINOR 낮은 에이전트까지 지원. 서버를 먼저 올림 | — |

**지원 기간(제안, 조직 정책에 맞춰 조정):** 현재 MAJOR의 최신 MINOR 두 개에 보안·치명 버그 수정을 PATCH로 냅니다.

## 업그레이드 전 점검

1. 릴리스 노트(GitHub Release, `CHANGELOG.md`)의 "업그레이드 주의"를 읽습니다.
2. 서명과 체크섬을 확인합니다([07-install.md](07-install.md#서명-확인)).
3. 새 바이너리로 현재 설정을 검사합니다. 새 경고가 있으면 먼저 정리합니다.
   ```bash
   ./vigilante_<new>_linux_amd64 validate -c /etc/vigilante/vigilante.yaml
   ```
4. PostgreSQL을 쓰면 백업하고 스키마 상태를 봅니다.
   ```bash
   vigilante store status -c /etc/vigilante/vigilante.yaml   # 종료 코드 4 = 적용 대기 또는 더 새로운 스키마
   ```
5. 진행 중인 롤백이 없는 시간을 고릅니다(`GET /v2/deployments?state=ROLLING_BACK`). 진행 중이어도 이어서 처리되지만, 확인 작업이 줄어듭니다.

## 단일 노드 업그레이드

```bash
# 패키지: 실행 중인 서버는 설치 후 자동으로 재시작됩니다.
dnf upgrade ./vigilante-<new>.x86_64.rpm      # 또는 apt install ./vigilante_<new>_amd64.deb
# 폐쇄망 번들: 압축을 푼 디렉토리에서
./install.sh
vigilante version && curl -fsS localhost:8088/readyz
```

재시작 동안(수 초) API가 응답하지 않습니다. CI의 `vigilante watch --server`는 재시도하며, 관측 중이던 배포는 재시작 후 이어서 판정하고, 롤백 중이던 배포는 멈춘 단계부터 다시 실행합니다(크래시 재개와 같은 경로).

## HA 순차 업그레이드 (무중단)

여러 노드가 PostgreSQL을 공유하는 구성입니다. **팔로워를 먼저, 리더를 마지막에** 올립니다.

1. 리더를 확인합니다: 아무 노드의 `GET /healthz` → `leader`.
2. 팔로워를 한 대씩 올리고 `/readyz`가 200이 될 때까지 기다립니다. 처음 올라온 새 버전 노드가 추가 마이그레이션을 적용하며, 이전 버전 리더는 계속 동작합니다.
3. 마지막으로 리더를 올립니다. 리더는 종료할 때 리더 리스를 즉시 놓으므로, 새 버전 팔로워가 몇 초 안에 이어받고 진행 중이던 롤백을 재개합니다.
4. `vigilante store status`가 0으로 끝나는지 확인합니다.

`server.state.auto_migrate: false`이면 서버가 스키마를 바꾸지 않습니다. 2번 전에 새 바이너리로 `vigilante store migrate -c FILE`을 한 번 실행하십시오(DBA가 변경 시점을 통제하는 환경).

### Kubernetes (Helm)

```bash
helm upgrade vigilante ./vigilante-<new>.tgz -f my-values.yaml
```

- `replicaCount` 2 이상(PostgreSQL·HA): 롤링 업데이트와 PodDisruptionBudget으로 위 절차가 자동으로 진행됩니다.
- `replicaCount: 1`(파일 저장소): 저널 쓰기는 한 프로세스만 해야 하므로 `Recreate`로 교체되며, 수 초간 중단됩니다.

### 에이전트

서버를 모두 올린 뒤 에이전트를 올립니다. 에이전트는 한 MINOR 낮은 버전까지 그대로 동작하므로 서두를 필요는 없습니다.

## 되돌리기(다운그레이드)

**PATCH·MINOR:** 이전 버전 패키지를 설치하면 됩니다(`dnf downgrade`, `apt install vigilante=<old>`, `helm rollback`). MINOR 마이그레이션은 추가만 하므로 이전 버전이 그대로 동작합니다.

**호환되지 않는 스키마 변경이 들어간 버전:** 이전 버전은 시작을 거부하고 다음과 같이 알려 줍니다(다운그레이드 가드). 잘못된 상태 기록을 막기 위한 것입니다.

```
state store schema is newer than this binary: migration 7 (applied by vigilante 2.0.0) is not known
to this binary (1.4.2, knows up to 6) and older releases cannot use it; run vigilante 2.0.0 or newer,
or revert with `vigilante store migrate --down-to 6` from that release
```

되돌리려면:

1. 모든 서버를 멈춥니다.
2. PostgreSQL을 백업합니다.
3. **새 버전 바이너리로** 스키마를 되돌립니다. 되돌릴 수 없는 마이그레이션이 하나라도 있으면 아무것도 바꾸지 않고 실패합니다.
   ```bash
   vigilante store migrate -c /etc/vigilante/vigilante.yaml --down-to 6 --yes
   ```
4. 이전 버전을 설치하고 서버를 시작합니다.

첫 마이그레이션(테이블 생성)은 되돌리지 않습니다. 감사 기록이 들어 있기 때문입니다.

## 스키마 마이그레이션 작성 규칙 (개발자용)

- 파일: `internal/store/migrations/NNN_설명.sql`, 번호는 빈틈없이 1부터. 되돌릴 수 있으면 `NNN_설명.down.sql`을 함께 둡니다.
- MINOR에서는 추가만: 테이블·열(기본값 있는)·인덱스 추가. 이전 버전 노드가 같은 DB에서 계속 동작해야 합니다.
- 이전 버전이 감당할 수 없는 변경(열 이름 변경·삭제, 의미 변경)은 MAJOR에서 하고, 파일에 `-- vigilante:breaking` 줄을 넣습니다. 이 표시가 있어야 이전 버전이 시작을 거부합니다.
- 큰 테이블 변경은 잠금 시간을 확인합니다. 마이그레이션은 한 트랜잭션에서 실행되며, 여러 노드가 동시에 시작해도 advisory lock으로 한 번만 실행됩니다.
