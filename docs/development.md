# Разработка и проверка

Команды выполняются из корня smart-credit-backend. Для Go нужны версия из go.mod
и PostgreSQL; Docker позволяет запустить backend без локальных Go и Python.

## Запуск

```sh
docker compose up -d --build
docker compose ps
docker compose logs backend migrate
```

Миграции и JWT-секрет настраиваются автоматически. Нужные порты по умолчанию:
HTTP 8080, PostgreSQL 5432. `.env.example` — справочник; обязательного `.env` нет.
Для проверки именно без локального файла используйте `--env-file /dev/null`.

В Go ENV читается процессом напрямую, файл .env автоматически не загружается.
Пример локального запуска рядом с Compose PostgreSQL:

```sh
APP_ENV=demo \
DATABASE_URL='postgres://smart_credit:smart-credit-demo-local-only@127.0.0.1:5432/smart_credit?sslmode=disable' \
SCORING_URL=http://127.0.0.1:8001 go run ./cmd/api
```

Это публичный демонстрационный пароль для локальной среды. Настоящие секреты
не включайте в команды, логи, Postman-экспорт или Git.

## Конфигурация

| ENV | Значение по умолчанию / назначение |
| --- | --- |
| `APP_ENV` | Go: `production`, Compose: `demo`; в `production` нельзя автоматически создавать JWT-секрет |
| `HTTP_HOST`, `HTTP_PORT` | `127.0.0.1`, `8080`; Compose задаёт host `0.0.0.0` |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` |
| `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT` | `10s`, `15s` |
| `HTTP_IDLE_TIMEOUT`, `HTTP_SHUTDOWN_TIMEOUT` | `60s`, `15s` |
| `DATABASE_URL` | Обязательный PostgreSQL DSN, без значения в Go-коде |
| `DB_MAX_CONNS`, `DB_MIN_CONNS` | `10`, `2`; min не больше max |
| `DB_CONNECT_TIMEOUT`, `DB_PING_TIMEOUT` | `5s`, `2s`; ping меньше HTTP write timeout |
| `DB_MAX_CONN_LIFETIME`, `DB_MAX_CONN_IDLE_TIME` | `1h`, `15m` |
| `AUTH_DB_TIMEOUT` | `3s`; таймаут операций авторизации, компаний, продуктов, заявок и документов в PostgreSQL, меньше `HTTP_WRITE_TIMEOUT` |
| `LOG_LEVEL` | `info`; также `debug`, `warn`, `error` |
| `DOCUMENT_STORAGE_PATH` | `./data/documents`; приватный каталог файлов, в Compose фиксирован внутри постоянного volume |
| `SCORING_URL` | `http://127.0.0.1:8001`; base URL отдельного Python HTTP scoring |
| `SCORING_TIMEOUT` | `5s`; положительный таймаут до `1m` для одного HTTP-вызова scoring, также ограничен context worker |
| `JWT_SECRET` | Не менее 32 байт для HS256; обязателен в `production`, в `demo` пустое значение включает генерацию; не хранить в Git |
| `JWT_SECRET_FILE` | Локальный Go: `./data/secrets/jwt-secret`; Compose фиксирует `/var/lib/smart-credit/secrets/jwt-secret` в постоянном volume |
| `JWT_ISSUER` | `smart-credit-backend`; проверяется при каждом запросе с JWT |
| `JWT_AUDIENCE` | `smart-credit-frontend`; проверяется при каждом запросе с JWT |
| `DEMO_OTP_CODE` | `1234`; четыре цифры, сравнение на backend, реальные SMS не отправляются |

В контейнере DSN строится из POSTGRES_USER/PASSWORD/DB с host=postgres;
POSTGRES_PORT меняет только внешний порт. SCORING_URL — base URL, клиент сам
добавляет /v1/score. JWT_SECRET_FILE и документы находятся на постоянных volumes.
В production секрет должен быть настроен явно; режим demo создаёт его при первом запуске.

## Настоящий Python scoring

Исходники scoring принадлежат отдельному репозиторию. Дополнительный Compose
параметризован путём, стандартный запуск backend от этого пути не зависит:

```sh
SCORING_CONTEXT=/absolute/path/to/smart-credit-scoring \
  docker compose -f compose.yaml -f compose.scoring.yaml up -d --build
```

SCORING_MODEL по умолчанию logreg_v1; rules_v1 выбирается явно в окружении.
SCORING_PORT задаёт внешний порт Python (по умолчанию 8001). Optional Compose
использует неизменённый Dockerfile соседнего проекта, read-only контейнер и /health.
SCORING_CONTEXT должен указывать на каталог с Dockerfile и scoring/, а не на
внешний каталог распакованного архива; путь с пробелами заключайте в кавычки.
Обучение модели или установка requirements-ml.txt для HTTP-инференса не нужны.

Для интеграционных проверок используйте отдельный project и отдельные volumes:

```sh
SCORING_CONTEXT=/absolute/path/to/smart-credit-scoring \
HTTP_PORT=18093 POSTGRES_PORT=15445 SCORING_PORT=18013 \
  docker compose -p smart-credit-integration --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml up -d --build
```

## Go и PostgreSQL

```sh
go fmt ./...
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build ./...
git diff --check
```

Без TEST_DATABASE_URL PostgreSQL-тесты пропускаются. Каждый интеграционный тест
создаёт отдельную схему с собственным search_path, применяет миграции и удаляет
только свою схему. Старые пользовательские таблицы не очищаются.

```sh
TEST_DATABASE_URL='postgres://smart_credit:smart-credit-demo-local-only@127.0.0.1:15445/smart_credit?sslmode=disable' \
  go test -count=1 ./...
```

Реальный logreg_v1 включается отдельным ENV; тест не использует httptest scoring:

```sh
TEST_DATABASE_URL='postgres://smart_credit:smart-credit-demo-local-only@127.0.0.1:15445/smart_credit?sslmode=disable' \
TEST_SCORING_URL=http://127.0.0.1:18013 \
  go test -count=1 -v ./internal/postgres -run TestRealScoringPipeline
```

Проверяются шесть настоящих ответов: четыре исхода, HIGH и отказ по обязательной
марже. Изменение модели или её артефакта требует пересмотра зафиксированных ожиданий.
Обычные unit/конкурентные тесты с httptest остаются отдельной проверкой Go и
не считаются интеграцией с Python. CI запускает Go, PostgreSQL и Python-прототип;
соседний scoring checkout не требуется для штатного CI.

## Postman и Newman

Импортируйте api/postman/smart-credit.postman_collection.json. Working directory —
api/postman, в нём лежат синтетические PDF/PNG/JPEG. Сценарии сами получают JWT,
applicationId, offerId, version и Idempotency-Key и сбрасывают их в новой итерации.

Для Postman Desktop выберите в настройке **Working directory** полный локальный
путь к `smart-credit-backend/api/postman`, а не к корню репозитория и не к `fixtures`.
Относительный путь `fixtures/sample.pdf` должен разрешаться в существующий файл.
Один импорт JSON коллекции не копирует тестовые файлы. Подробности — в
[документации Postman](https://learning.postman.com/docs/getting-started/installation/settings/working-directory).

Если первые ошибки возникают в загрузках 123–126 (400 вместо 201), проверьте
вкладку Body запроса 123 и доступность локального PDF. Без файла последующие
запросы используют пустой documentId, получают 404 и не могут отправить заявки,
требующие документов (422). После исправления пути запустите коллекцию с начала:
ID и JWT передаются автоматически. Пропущенные проверки scoring при выключенных
expect-флагах не являются ошибками: в экспорте Runner у них может быть `false`,
но счётчик `testPassFailCounts.fail` равен нулю.

```sh
newman run api/postman/smart-credit.postman_collection.json --working-dir api/postman
```

По умолчанию не требуется успешный scoring. Для реального сервиса:

```sh
newman run api/postman/smart-credit.postman_collection.json \
  --working-dir api/postman --env-var baseUrl=http://localhost:18093 \
  --env-var scoringMode=logreg_v1 --env-var expectScoringResult=true \
  --env-var expectDecisionResult=true --env-var expectReadOutcomes=true \
  --env-var expectActionResults=true
```

scoringMode=fixture сохраняет прежние фиксированные тестовые сценарии для
контролируемого HTTP-fixture. Это отдельный режим: настоящий Python не подменяется.
В logreg_v1 используются финансовые данные семи дополнительных заявок;
коллекция отдельно подаёт и подтверждает их через банковский API деклараций.
Существующие проверки и отрицательные сценарии сохранены. Коллекция содержит
465 запросов; polling может увеличить фактическое число HTTP-вызовов.

### Режимы коллекции и повторный запуск

| Настройки | Проверка |
| --- | --- |
| По умолчанию, scoring отсутствует | API, права, технический failed и однократное восстановление |
| По умолчанию, scoring доступен | API и корректный запрет восстановления завершённой заявки |
| `expectScoringResult=true`, остальные expect=false | Ожидание сохранённого scoring без требования конкретного исхода Go |
| `scoringMode=logreg_v1`, все четыре expect=true | Все кредитные исходы, ручные решения, accept/decline и объяснения |

Флаги включают ожидания и дополнительные проверки, а не управляют backend.
Recovery определяется фактическим status/processing и банковской диагностикой;
идемпотентный повтор проверяет исходный ответ и отсутствие второго пакета попыток.
Если worker успел законно завершить обработку после восстановления, проверка
учитывает ровно соответствующие переходам изменения версии.

Последняя проверка Newman, по две последовательные итерации каждого режима:

| Режим | Assertions | Failures |
| --- | ---: | ---: |
| Стандартные настройки, настоящий scoring доступен | 4109 | 0 |
| Стандартные настройки, scoring отсутствует | 4206 | 0 |
| Полный logreg_v1 со всеми expect=true | 5287 | 0 |
| Только expectScoringResult=true | 4172 | 0 |

Дополнительно настоящий scorer подключён после исчерпания попыток перед recovery,
а worker завершил решение до проверки replay: ещё 266 assertions, 0 failures.
Контрольный GET допускает только версии, соответствующие фактическим переходам
конвейера; recoveryCount и maxAttempts подтверждают отсутствие второго пакета.

Для проверки повторного запуска добавьте `--iteration-count 2`. Каждая итерация
создаёт собственные заявки и сбрасывает JWT, ID, версии и выбранную ветку recovery.
Проверены имена профиля с кавычками, обратным слешем и буквальным `{{…}}`:
данные сериализуются в JSON и не интерпретируются как переменные Postman.

Коллекция рассчитана на контролируемые синтетические fixtures: стандартные
профили, исходные финансовые данные и начальные полномочия seed. Она создаёт
заявки, подтверждает тестовые финансовые декларации и проверяет выдачу/отзыв
второго членства. Произвольно изменённые финансовые данные или дополнительные
исходные членства меняют корректные ответы API и требуют отдельного fixture setup.

## Восстановление и миграции

После `docker compose restart backend` JWT-секрет, заявки, документы, jobs,
решения и ответы идемпотентности сохраняются в БД/volumes. Worker восстанавливает
незавершённый lease; успешный scoring повторно не вызывается. При недоступности
scoring выполняются ограниченные повторы, затем processing.state=failed без rejected.
После исчерпания попыток нужна диагностика оператора. Банк может возобновить failed job
через POST /bank/applications/{id}/retry-processing: версия, причина, Idempotency-Key,
не более трёх восстановлений. Успешные этапы сохраняются.

Исторические SQL-файлы не переименовываются и не редактируются. Версия схемы 14;
новые таблицы членства, подтверждений и аудита описаны в company-data.md. Down миграции с долговечным
решением/аудитом могут намеренно запрещать потерю данных. Используйте migrate CLI
только для осознанного управления тестовой схемой; обычному запуску это не нужно.

Останавливайте интеграционное окружение той же командой Compose с `down` без -v.
Не удаляйте рабочие volumes. Повтор теста не требует удаления пользовательских данных.

## Python-прототип и лицензии

Сохранены backend/, tests/ и [описание Python-прототипа](legacy-prototype.md).
`python3 -m unittest discover -s tests -v` проверяет его отдельно от Go.
Зависимости Go закреплены в go.mod/go.sum; лицензии зависимостей и notices
сохраняются. Scoring и его THIRD_PARTY_NOTICES остаются в отдельном проекте.

## Обновление схемы и режимы

Миграции 000001–000011 не изменяются. 000012 переносит принадлежность в членства;
000013 добавляет версии, финансовую историю и исправления; 000014 защищает историю
и отправленные поля от изменения. Перед обновлением остановите старые backend,
сохраните БД и файловый volume. Старые и новые бинарники одновременно не запускаются.
Откат новых миграций блокируется; необходима проверенная резервная копия. Рабочие
Docker volumes не удаляйте. [Порядок и модель](company-data.md).

Compose по умолчанию явно задаёт APP_ENV=demo. При APP_ENV=production процесс
отказывается стартовать до настоящих интеграций идентификации, реестров, БКИ и
подписания. Синтетические профили, фиксированный OTP, симуляторы, ML и signed
не являются production-провайдерами. Это ограничение запуска, а не банковская
сертификация. Реальные SMS/ФНС/БКИ/ЭП в репозитории не реализованы.

Новые регрессионные тесты используют отдельные PostgreSQL schemas и проверяют
также обновление заполненной схемы 000011, несколько компаний, отзыв JWT-доступа,
конкурентные изменения, финансовое происхождение и восстановление jobs.
Подтверждение точных сумм в fixture выполняется отдельно от кредитного решения.

## Проверка этапа 10

Проверены `go fmt ./...`, `go vet ./...`, `go build ./...`,
`go test -count=1 ./...` и `go test -race -count=1 ./...` с PostgreSQL
и настоящим Python logreg_v1. Полные прогоны проходят. Сохранённые восемь тестов
Python-прототипа проходят. OpenAPI проходит структурную проверку; 41 implemented
операция соответствует 41 зарегистрированному HTTP-маршруту.

Единая Postman Collection содержит 465 запросов. На проверенном запуске Newman
с настоящим scoring выполнил 481 HTTP-запрос и 2637 assertions без ошибок; без
scoring — 381 запрос и 2096 assertions без ошибок. Число HTTP-запросов зависит от
polling. JWT, идентификаторы, версии и ключи передаются автоматически.

Миграции проверены на чистых схемах и заполненной схеме 000011. Исторические
заявки, документы, решения, события и idempotency responses сохраняются.
Базовый Compose собирается и запускается без .env и без соседнего scoring;
дополнительный Compose использует оригинальный Dockerfile scoring без изменений.

Отдельный интеграционный сценарий остановил настоящий scorer, исчерпал пять
попыток без кредитного отказа, затем возобновил обработку через банковский API.
Получены score=97, одно решение, одно предложение и одно событие восстановления.
После принятия предложения и перезапуска backend прежний JWT действителен,
recovery/accept replay совпадают побайтно, контрольное состояние заявки не
изменилось, число вызовов scorer не выросло. Тестовые Docker volumes сохранены.

Проверки секретов не нашли заполненных JWT в коллекции, приватных ключей или
новых токенов доступа в изменяемых файлах; .env игнорируется Git и Docker context.
Python scoring, frontend, Python-прототип и миграции 000001–000011 не изменялись.

### Изменённые и добавленные файлы

- `.github/workflows/ci.yml`
- `README.md`
- `api/FRONTEND_INTEGRATION.md`
- `api/openapi.yaml`
- `api/postman/smart-credit.postman_collection.json`
- `cmd/api/main.go`
- `cmd/api/main_test.go`
- `docs/architecture.md`
- `docs/company-data.md`
- `docs/credit-process.md`
- `docs/development.md`
- `docs/scoring-integration.md`
- `internal/application/service.go`
- `internal/application/service_test.go`
- `internal/application/types.go`
- `internal/auth/middleware.go`
- `internal/auth/middleware_test.go`
- `internal/company/company.go`
- `internal/company/company_test.go`
- `internal/company/management.go`
- `internal/company/provenance.go`
- `internal/company/provenance_test.go`
- `internal/credit/service.go`
- `internal/credit/service_test.go`
- `internal/credit/types.go`
- `internal/decision/decode.go`
- `internal/decision/engine.go`
- `internal/decision/engine_test.go`
- `internal/decision/manual.go`
- `internal/decision/provenance_test.go`
- `internal/decision/types.go`
- `internal/document/service.go`
- `internal/document/service_test.go`
- `internal/document/types.go`
- `internal/httpapi/application.go`
- `internal/httpapi/auth.go`
- `internal/httpapi/credit.go`
- `internal/httpapi/credit_test.go`
- `internal/httpapi/document.go`
- `internal/httpapi/lending.go`
- `internal/httpapi/lending_test.go`
- `internal/httpapi/management.go`
- `internal/httpapi/membership_test.go`
- `internal/httpapi/recovery.go`
- `internal/lending/service.go`
- `internal/lending/service_test.go`
- `internal/lending/types.go`
- `internal/pipeline/recovery.go`
- `internal/pipeline/submission.go`
- `internal/pipeline/submission_test.go`
- `internal/postgres/application.go`
- `internal/postgres/auth.go`
- `internal/postgres/auth_test.go`
- `internal/postgres/company_credit.go`
- `internal/postgres/decision_jobs.go`
- `internal/postgres/decision_jobs_test.go`
- `internal/postgres/document.go`
- `internal/postgres/governance_test.go`
- `internal/postgres/lending.go`
- `internal/postgres/lending_test.go`
- `internal/postgres/management.go`
- `internal/postgres/membership.go`
- `internal/postgres/recovery.go`
- `internal/postgres/review.go`
- `internal/postgres/review_test.go`
- `internal/postgres/scoring_integration_test.go`
- `internal/postgres/submission.go`
- `internal/postgres/submission_test.go`
- `internal/postgres/upgrade_test.go`
- `internal/review/service.go`
- `internal/review/types.go`
- `migrations/000012_company_memberships.down.sql`
- `migrations/000012_company_memberships.up.sql`
- `migrations/000013_data_governance.down.sql`
- `migrations/000013_data_governance.up.sql`
- `migrations/000014_history_integrity.down.sql`
- `migrations/000014_history_integrity.up.sql`
