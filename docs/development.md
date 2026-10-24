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
В logreg_v1 меняются только финансовые данные семи дополнительных заявок;
существующие проверки и отрицательные сценарии сохранены. Коллекция содержит
397 запросов; polling может увеличить фактическое число HTTP-вызовов.

## Восстановление и миграции

После `docker compose restart backend` JWT-секрет, заявки, документы, jobs,
решения и ответы идемпотентности сохраняются в БД/volumes. Worker восстанавливает
незавершённый lease; успешный scoring повторно не вызывается. При недоступности
scoring выполняются ограниченные повторы, затем processing.state=failed без rejected.
После исчерпания попыток нужна диагностика оператора: публичного retry API нет.

Исторические SQL-файлы не переименовываются и не редактируются. Версия схемы 11;
новые таблицы для интеграции scoring не потребовались. Down миграции с долговечным
решением/аудитом могут намеренно запрещать потерю данных. Используйте migrate CLI
только для осознанного управления тестовой схемой; обычному запуску это не нужно.

Останавливайте интеграционное окружение той же командой Compose с `down` без -v.
Не удаляйте рабочие volumes. Повтор теста не требует удаления пользовательских данных.

## Python-прототип и лицензии

Сохранены backend/, tests/ и [описание Python-прототипа](legacy-prototype.md).
`python3 -m unittest discover -s tests -v` проверяет его отдельно от Go.
Зависимости Go закреплены в go.mod/go.sum; лицензии зависимостей и notices
сохраняются. Scoring и его THIRD_PARTY_NOTICES остаются в отдельном проекте.
