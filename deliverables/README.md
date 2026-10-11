# 산출물

SI 표준 산출물, 운영 매뉴얼, 영업·도입 심사 자료, 기획서 13종입니다. 원본은 `src/*.md`이고, `docx/`의 Word 파일은 `build.py`로 만든 결과입니다. 내용은 master `537870c`(PR #14까지) 기준이며, 상태는 모두 검토본입니다.

| 구분 | 문서 |
|---|---|
| SI 표준 | 01 요구사항 정의서, 02 아키텍처 설계서, 03 상세 설계서, 04 인터페이스 정의서, 05 데이터 설계서, 06 테스트 계획·결과서 |
| 운영 | 11 관리자 매뉴얼, 12 운영자 매뉴얼, 13 장애 대응 런북 |
| 영업·도입 심사 | 21 제품 소개서, 22 보안 점검표 응답서, 23 호환성·검증 현황 |
| 기획 | 31 기획서 |

## Word 파일 만들기

```
pip install python-docx==1.1.2
python deliverables/build.py              # 전체
python deliverables/build.py src/22-security-checklist.md   # 한 문서
```

코드·설정이 바뀌면 원본 Markdown을 고치고 다시 만듭니다. 문서마다 앞부분(front matter)의 `version`과 `history`에 변경 내역을 남깁니다.
