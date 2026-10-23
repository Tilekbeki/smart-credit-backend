# Smart Credit — backend

Backend учебного кабинета предпринимателя. Сохраняет заявку в SQLite, передаёт синтетические финансовые показатели отдельному scoring-сервису, хранит результат проверки, подтверждение условий и запись о зачислении на **учебный счёт**. Менеджер не участвует в типовом сценарии.

Это прототип для хакатона. Он не отправляет заявку в банк, не подписывает юридический договор и не переводит реальные деньги. Суммы, одобрение и условия создаются учебными правилами. Ставка **20% годовых** используется только для иллюстративного аннуитетного расчёта и не является ставкой Альфа-Банка или индивидуальным предложением.

## Запуск

Нужен Python 3.12+ без сторонних библиотек. Сначала запустите сервис из репозитория scoring на порту 8001, затем:

```sh
python3 -m backend.app
```

По умолчанию backend слушает `127.0.0.1:8000`, использует `data/applications.sqlite3` и обращается к `http://127.0.0.1:8001/v1/score`. Параметры меняются переменными `HOST`, `PORT`, `DB_PATH`, `SCORING_URL`. В Docker можно задать `HOST=0.0.0.0`, `DB_PATH=/data/applications.sqlite3`, `SCORING_URL=http://scoring:8001`.

Проверка: `GET http://127.0.0.1:8000/health`.

## API

Все ответы — JSON, статусы и события сохраняются в SQLite. Для локального frontend доступен CORS с `localhost` и `127.0.0.1`.

Контракты запроса и ответа scoring находятся в `contracts/scoring.schema.json` и `contracts/scoring-response.schema.json` и синхронизированы с scoring-репозиторием.

| Метод и маршрут | Назначение |
| --- | --- |
| `GET /api/v1/profiles` | Названия четырёх учебных компаний |
| `POST /api/v1/applications` | Отправить заявку, статус `submitted` |
| `GET /api/v1/applications/{id}` | Получить заявку после обновления страницы |
| `GET /api/v1/applications` | Список заявок |
| `POST /api/v1/applications/{id}/evaluate` | Запросить автоматическое решение |
| `POST /api/v1/applications/{id}/accept` | Подтвердить учебные условия |
| `POST /api/v1/applications/{id}/disburse` | Записать зачисление на учебный счёт |

Создание заявки:

```json
{
  "profile_id": "shop",
  "goal_id": "stock",
  "goal_description": "",
  "product_id": "turnover",
  "amount": 1500000,
  "term_months": 12,
  "contact_name": "Алия Соколова",
  "contact_role": "Владелец бизнеса"
}
```

`goal_id`: `stock`, `expenses`, `seasonal`, `equipment`, `upgrade`, `other`. Для `other` нужен `goal_description` минимум из четырёх символов. `product_id`: `turnover` или `investment`. Подтверждение условий принимает `{"accepted": true}`; без явного подтверждения `signed` недоступен.

Ответы создания и дальнейших действий возвращают полную заявку. Важные поля: `id`, `status`, `events: [{status, at, title}]`, `decision`, `score`, `approved_amount`, `reasons`, `factors`, `offer`, `acceptance`, `receipt`, `simulation: true`. `offer` содержит `amount`, `term_months`, `annual_rate_percent`, `monthly_payment`, `total_payment`, `overpayment` и `illustrative: true`. `receipt` содержит `transaction_id`, `credited_at`, `account_masked`, `amount`, `simulation: true`. Ошибки: `{error, message}`, текст `message` на русском.

Путь статусов: `submitted → processing → approved | counter_offer | rejected → signed → funded`. После `rejected` подтверждение невозможно. Повторные вызовы `evaluate`, `accept`, `disburse` после успешного перехода возвращают сохранённый результат. Повторное `disburse` не создаёт вторую запись о зачислении. Если scoring временно недоступен, заявка остаётся в `processing`, появляется `evaluation_error`, а `evaluate` можно безопасно повторить.

## Проверка

```sh
python3 -m unittest discover -s tests -v
python3 -m json.tool contracts/scoring.schema.json
python3 -m json.tool contracts/scoring-response.schema.json
```

Тесты проверяют успешный путь, альтернативную сумму, отказ, ошибочные переходы, сохранение после перезапуска, повторное зачисление, восстановление после недоступности scoring и HTTP-контракт.

## Go backend — этап 2: демонстрационная авторизация

Go backend расположен в корне этого репозитория рядом с сохранённым Python-прототипом.
Он запускается отдельно, по умолчанию на порту **8080**, и предоставляет probes и
демонстрационную авторизацию OTP/JWT. Описанные выше кредитные Python API в Go ещё
не реализованы.

Архитектура: один процесс `net/http` + chi, структурированные JSON-логи `log/slog`,
пул PostgreSQL `pgx/v5/pgxpool`. Конфигурация загружается из ENV с проверкой значений.
При старте выполняется проверка подключения к БД; ошибка конфигурации, подключения
или занятого порта завершает процесс с ненулевым кодом. SIGINT/SIGTERM запускают
graceful shutdown HTTP, затем закрывается пул. DSN и секреты не выводятся в логи.

```text
cmd/api/             точка входа, сигналы, жизненный цикл сервера и пула
internal/config/     ENV и валидация
internal/auth/       OTP, JWT, роли, контекст текущего пользователя
internal/httpapi/    chi, логирование запросов, health/readiness, auth handlers
internal/postgres/   pgxpool, пользователи, атомарные OTP-операции
migrations/          схема и синтетические данные, golang-migrate
api/openapi.yaml    контракт probes и демонстрационной авторизации
api/postman/         единая коллекция автоматических API-проверок
```

Каталоги `application`, `credit`, `scoring`, `document`, `worker`
зарезервированы локально под следующие этапы. Пустые каталоги Git не сохраняет;
Go-файлы-заглушки для них не создаются.

### Запуск через Docker Compose

Нужны Docker Engine и Docker Compose. Все команды выполняются из `smart-credit-backend`.

```sh
docker compose up -d --build
```

Создавать `.env`, вручную запускать миграции и генерировать JWT-секрет не нужно.
Compose ожидает готовности PostgreSQL, запускает одноразовый `migrate up` с
существующим seed и только после его успешного завершения запускает backend.
Backend в режиме `demo` создаёт криптографически случайный JWT-секрет, если он
ещё не задан, затем запускает HTTP-сервер. Healthcheck контейнера проверяет `/ready`.
Команда запуска возвращает управление до завершения healthcheck; готовность видна
через `docker compose ps`.

```sh
docker compose ps
curl -i http://localhost:8080/health
curl -i http://localhost:8080/ready
docker compose logs backend
```

Повторный `docker compose up -d --build` применяет только недостающие миграции;
начальные пользователи и компании не дублируются. Завершённый `migrate` с кодом `0`
в списке контейнеров — нормальное состояние одноразового сервиса.

Настройки можно переопределить через ENV или существующий `.env`. Пользовательский
`.env` не перезаписывается. `.env.example` описывает доступные настройки и не
требуется для запуска. При использовании уже существующего PostgreSQL volume
сохраняйте его исходные учётные данные: смена переменной пароля не меняет пароль
в уже инициализированной БД.

Используются закреплённые теги образов: Go `1.26.10-alpine3.24`, runtime Alpine
`3.24.3`, PostgreSQL `17.11-alpine3.24`, golang-migrate `v4.20.1`.
Приложение в контейнере работает без root. Порты PostgreSQL и HTTP публикуются только
на `127.0.0.1`. Данные БД, будущие документы и JWT-секрет находятся в отдельных
именованных volumes. Встроенные учётные данные PostgreSQL предназначены только
для изолированной локальной демонстрации.

Секрет хранится в volume `jwt_secrets` в файле
`/var/lib/smart-credit/secrets/jwt-secret`. Файл имеет права `0600`, каталог — `0700`;
контейнер использует его от непривилегированного пользователя. Секрет не попадает в
Git и логи, сохраняется при обычном перезапуске и `docker compose down`.
Явно заданный непустой `JWT_SECRET` имеет приоритет и должен содержать не менее
32 байт. В режиме `production` обязателен явно заданный секрет: генерация и
использование сохранённого demo-файла отключены. Для такого запуска также
необходимо переопределить демонстрационные учётные данные PostgreSQL.

```sh
docker compose stop
docker compose start
docker compose down
```

`down` сохраняет volumes. Для удаления тестовых данных нужна отдельная осознанная
операция удаления volumes. Scoring в этом Compose пока отсутствует.

### ENV и локальный запуск Go

Для сборки нужен Go 1.26+. `.env` автоматически читает Compose; Go-приложение
самостоятельно dotenv-файлы не загружает. Локальный запуск Go по умолчанию
использует `APP_ENV=production` и требует явный `JWT_SECRET`. Для изолированной
демонстрации вне контейнера можно выбрать `demo`:

```sh
docker compose up -d postgres
docker compose run --rm migrate up
APP_ENV=demo \
  DATABASE_URL='postgres://smart_credit:smart-credit-demo-local-only@127.0.0.1:5432/smart_credit?sslmode=disable' \
  go run ./cmd/api
```

Это отдельный вариант для разработки; основной запуск через Compose остаётся
одной командой. Для существующей БД подставьте её локальный DSN. При необходимости
загрузите доверенный shell-совместимый `.env` самостоятельно. Не запускайте
локальный Go и Compose backend одновременно на одном HTTP-порту.

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
| `AUTH_DB_TIMEOUT` | `3s`; таймаут операций авторизации в PostgreSQL, меньше `HTTP_WRITE_TIMEOUT` |
| `LOG_LEVEL` | `info`; также `debug`, `warn`, `error` |
| `DOCUMENT_STORAGE_PATH` | `./data/documents`; пока не используется |
| `SCORING_URL` | `http://127.0.0.1:8001`; пока не используется |
| `JWT_SECRET` | Не менее 32 байт для HS256; обязателен в `production`, в `demo` пустое значение включает генерацию; не хранить в Git |
| `JWT_SECRET_FILE` | Локальный Go: `./data/secrets/jwt-secret`; Compose фиксирует `/var/lib/smart-credit/secrets/jwt-secret` в постоянном volume |
| `JWT_ISSUER` | `smart-credit-backend`; проверяется при каждом запросе с JWT |
| `JWT_AUDIENCE` | `smart-credit-frontend`; проверяется при каждом запросе с JWT |
| `DEMO_OTP_CODE` | `1234`; четыре цифры, сравнение на backend, реальные SMS не отправляются |

В Compose DSN строится из `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` и host
`postgres`; локальный `DATABASE_URL` из `.env` не подставляется в контейнер. Пароль
в этой простой конфигурации должен состоять из URL-safe символов. `POSTGRES_PORT`
меняет локальный порт БД; при его изменении обновите локальный DSN. Внутри Compose
путь документов фиксирован: `/var/lib/smart-credit/documents`, scoring URL по
умолчанию `http://scoring:8001` зарезервирован для будущей интеграции.

### Миграции и начальные данные

`000001` создаёт 11 прикладных таблиц: `companies`, `users`, `otp_challenges`,
`credit_products`, `applications`, `application_events`, `credit_decisions`,
`offers`, `documents`, `jobs`, `idempotency_keys`.

Используются UUID, внешние ключи, ограничения статусов и сумм, индексы очереди и
истории, версии заявок и предложений. Неполные черновики разрешены. Для отправленных
заявок обязательны финансовый и продуктовый снимки JSONB. Результат scoring
сохраняется в `jobs`, окончательное решение backend — в `credit_decisions`.
У задач предусмотрены повторные попытки, время следующего запуска и аренда с токеном.
Техническая ошибка задачи отделена от кредитного отказа. Подтверждение хранится в
`applications` со ссылкой на своё предложение; таблиц выдачи кредита нет.

`000002` добавляет **три синтетические компании**, по одному CLIENT на компанию и
одного BANK_EMPLOYEE без компании. Телефоны тестовых пользователей:
`+79990000001`, `+79990000002`, `+79990000003`, `+79990000004` (сотрудник).
Эти профили доступны для демонстрационного входа. Продукты
не заполняются до согласования условий. SQLite не импортируется.

`000003` добавляет `otp_challenges.invalidated_at` для отзыва предыдущих активных
подтверждений пользователя. Старые миграции не изменены. При откате этой миграции
отозванные challenge получают исчерпанный лимит попыток, чтобы не стать снова
пригодными после удаления поля отзыва.

При обычном запуске миграции выполняются автоматически. Для диагностики и
осознанного управления схемой по-прежнему доступны CLI-команды:

```sh
docker compose run --rm migrate version
docker compose run --rm migrate up        # повторный запуск: no change
docker compose run --rm migrate down 1    # откатить только 000003; auth API после этого несовместим со схемой
docker compose run --rm migrate up 1      # восстановить 000003 перед запуском auth API
```

Полный откат в пустой тестовой БД: `docker compose run --rm migrate down 3`.
Откат seed откажет, если на demo-пользователей уже ссылаются заявки или OTP: данные не
удаляются каскадно. Для локально установленного golang-migrate v4.20.1 доступен
эквивалент: `migrate -path ./migrations -database "$DATABASE_URL" up`.
golang-migrate создаёт собственную служебную таблицу `schema_migrations`.

Будущие сервисы обязаны менять статус и добавлять событие в одной транзакции,
копировать snapshots при submit, проверять роли и фиксировать результат worker
только при совпадении токена аренды. Схема подготавливает эти операции, но не
реализует их бизнес-логику триггерами.

### Проверки Go

```sh
go mod tidy
go fmt ./...
go vet ./...
go test ./...
go build ./...

# Дополнительно: PostgreSQL-интеграция, включая конкурентные OTP-операции.
TEST_DATABASE_URL="$DATABASE_URL" go test -v ./...
docker compose build backend
```

Unit-тесты проверяют конфигурацию, probes, таймаут readiness, безопасные ошибки,
JWT, auth handlers и middleware. PostgreSQL-тесты проверяют реальные операции OTP,
включая истечение, ограничение попыток, одноразовость, замену challenge и
конкурентный verify. Они используют отдельную временную схему, применяют миграции
и удаляют её после проверки; существующие прикладные данные не затрагиваются.
Тесты с реальной БД пропускаются, если `TEST_DATABASE_URL` не задан.
В Go-задаче CI PostgreSQL запускается автоматически, а `TEST_DATABASE_URL` задан,
поэтому тесты конкурентных OTP-операций выполняются и в CI.
Существующие Python-тесты запускаются прежней командой из раздела выше.

Проверка потери БД на работающем backend:

```sh
docker compose stop postgres
curl -i http://localhost:8080/health  # 200: процесс жив
curl -i http://localhost:8080/ready   # 503: БД недоступна
docker compose start postgres
curl -i http://localhost:8080/ready   # 200 после восстановления PostgreSQL
```

`/ready` проверяет соединение с БД, а не версию миграций. `/health` не зависит от БД.

### Postman Collection и Newman

Коллекция [api/postman/smart-credit-stage2-postman-collection.json](api/postman/smart-credit-stage2-postman-collection.json)
использует Postman Collection v2.1. Импортируйте её в Postman и запустите целиком
через Collection Runner. `baseUrl` по умолчанию — `http://localhost:8080`.
Профили клиентов и сотрудника, OTP challenge и access token передаются между
запросами автоматически; ручное копирование не требуется. Код `1234` соответствует
демонстрационной конфигурации по умолчанию.

Если Newman установлен, тот же набор запускается из корня backend:

```sh
newman run api/postman/smart-credit-stage2-postman-collection.json

# Другой адрес уже запущенного backend:
newman run api/postman/smart-credit-stage2-postman-collection.json \
  --env-var baseUrl=http://localhost:18080
```

При отсутствии локального Newman доступен Docker-вариант для Compose project
`smart-credit` по умолчанию:

```sh
docker run --rm --network smart-credit_default \
  -v "$PWD/api/postman:/etc/newman:ro" \
  postman/newman:6.1.3-alpine \
  run /etc/newman/smart-credit-stage2-postman-collection.json \
  --env-var baseUrl=http://backend:8080
```

При другом имени Compose project или HTTP-порте укажите соответствующие сеть и
внутренний адрес backend.

Коллекция проверяет probes, выбор профилей, OTP, ошибки авторизации, JWT и текущего
пользователя для обеих ролей. Не экспортируйте полученные access token в Git.
Проверки ограничения ролей middleware выполняются также Go-тестами; защищённых
кредитных маршрутов для отдельного бизнес-сценария пока нет.

При добавлении новых API необходимо обновлять OpenAPI и эту же коллекцию,
добавлять позитивные и негативные сценарии с проверками ответа и автоматической
передачей данных, затем повторно проверять существующие API.

### Демонстрационная авторизация

Все демонстрационные пользователи загружаются из PostgreSQL, включая роль и связь
с компанией. Список профилей и OTP-маршруты публичны. `GET /api/v1/me` требует
Bearer Token. Полный контракт: [api/openapi.yaml](api/openapi.yaml).

| Метод и маршрут | Назначение |
| --- | --- |
| `GET /api/v1/demo/profiles` | Три предпринимателя и один сотрудник из seed |
| `POST /api/v1/auth/otp/request` | Создать challenge по `profileId`, имитировать SMS |
| `POST /api/v1/auth/otp/verify` | Проверить код и выдать JWT |
| `GET /api/v1/me` | Текущий пользователь из PostgreSQL |

Порядок входа:

```sh
curl -sS http://localhost:8080/api/v1/demo/profiles

curl -sS http://localhost:8080/api/v1/auth/otp/request \
  -H 'Content-Type: application/json' \
  -d '{"profileId":"20000000-0000-4000-8000-000000000001"}'
```

Первый профиль имеет следующий вид (в ответе списка он находится в `items`):

```json
{
  "id": "20000000-0000-4000-8000-000000000001",
  "name": "Иван Лавкин",
  "phone": "+79990000001",
  "role": "CLIENT",
  "companyName": "ИП «Лавка у дома»"
}
```

Ответ создания challenge — `201 Created` (идентификатор и время приведены для примера):

```json
{
  "otpRequestId": "30000000-0000-4000-8000-000000000001",
  "expiresAt": "2026-10-23T12:05:00Z",
  "maskedPhone": "+7*** *** 0001"
}
```

Используйте фактический `otpRequestId` из предыдущего ответа:

```sh
curl -sS http://localhost:8080/api/v1/auth/otp/verify \
  -H 'Content-Type: application/json' \
  -d '{"otpRequestId":"<otpRequestId>","code":"1234"}'
```

Ответ успешной проверки — `200 OK`:

```json
{
  "accessToken": "<accessToken>",
  "tokenType": "Bearer",
  "expiresAt": "2026-10-23T20:00:00Z",
  "userId": "20000000-0000-4000-8000-000000000001",
  "role": "CLIENT",
  "companyId": "10000000-0000-4000-8000-000000000001"
}
```

Значение `accessToken` необходимо хранить как секрет; не выводите его в общие логи.
Пример запроса текущего пользователя с подстановкой токена:

```sh
curl -sS http://localhost:8080/api/v1/me \
  -H 'Authorization: Bearer <accessToken>'
```

```json
{
  "id": "20000000-0000-4000-8000-000000000001",
  "name": "Иван Лавкин",
  "phone": "+79990000001",
  "role": "CLIENT",
  "companyId": "10000000-0000-4000-8000-000000000001"
}
```

Для сотрудника используйте профиль `20000000-0000-4000-8000-000000000004`;
его `companyName` в списке и `companyId` в verify/me равны `null`.

OTP действует **5 минут**, допускает **5 попыток** и используется один раз.
Правильный код на пятой попытке допустим. Новый challenge делает предыдущие
активные challenge пользователя недействительными. Проверки и изменение состояния
выполняются транзакционно с блокировками PostgreSQL: параллельные запросы не
могут повторно использовать один OTP или превысить лимит попыток. Симуляция SMS
не вызывает внешние сервисы. Код и JWT не логируются; JWT в БД не сохраняется.

JWT действует **8 часов** и содержит идентификатор пользователя в `sub`.
Authentication middleware проверяет HS256, подпись, обязательный срок действия,
issuer и audience, затем загружает актуального пользователя из PostgreSQL.
RequireRole проверяет роли `CLIENT`/`BANK_EMPLOYEE` из контекста. Роль не принимается
из запроса и не берётся из клиентских JWT claims. Контекст содержит пользователя
и его `companyId` для последующих проверок владения; API заявок пока отсутствует.

Новые API возвращают ошибки в едином формате:

```json
{"error":"invalid_otp","message":"Неверный код подтверждения"}
```

| HTTP | Код / условие |
| --- | --- |
| `400` | `invalid_request` — некорректный JSON/UUID/поля; `invalid_otp` — неверный код |
| `401` | `unauthorized` — нет JWT, неверный, истёкший токен или отсутствующий пользователь |
| `403` | `forbidden` — роль не разрешена middleware |
| `404` | `profile_not_found`, `otp_not_found` |
| `409` | `otp_used`, `otp_invalidated` |
| `410` | `otp_expired` |
| `413` | `request_too_large` — тело больше 64 KiB |
| `415` | `unsupported_media_type` — нужен `Content-Type: application/json` |
| `429` | `otp_attempts_exceeded` — исчерпан лимит проверки данного challenge |
| `503` | `service_unavailable` — БД недоступна или истёк таймаут операции |
| `500` | `internal_error` — непредвиденная ошибка без внутренних подробностей |

Поля тела имеют формат camelCase. Неизвестные поля и несколько последовательных
JSON-значений отклоняются. Заголовок `Cache-Control: no-store` запрещает кеширование
ответов. SQL-запросы параметризованы; операции БД ограничены `AUTH_DB_TIMEOUT`.

### Границы второго этапа

Это публичный демонстрационный вход с выбором в том числе сотрудника банка.
Он предназначен для синтетических данных хакатона. Нет реальных SMS, паролей,
refresh token, logout/revocation, отдельного ограничения частоты создания challenge
или промышленного подтверждения владения телефоном. Ограничение пяти попыток
относится к одному challenge; создание нового начинает новый лимит.

API продуктов, рекомендаций, заявок и документов, scoring-клиент, worker и
кредитные решения не реализованы. Go не обращается к Python scoring. Следующий
этап начинается отдельно после проверки демонстрационной авторизации.
