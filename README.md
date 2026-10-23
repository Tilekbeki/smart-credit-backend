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

## Go backend — этап 1: инфраструктура

Go backend расположен в корне этого репозитория рядом с сохранённым Python-прототипом.
Он запускается отдельно, по умолчанию на порту **8080**, и пока предоставляет только
`GET /health` и `GET /ready`. Описанные выше Python API в Go ещё не реализованы.

Архитектура: один процесс `net/http` + chi, структурированные JSON-логи `log/slog`,
пул PostgreSQL `pgx/v5/pgxpool`. Конфигурация загружается из ENV с проверкой значений.
При старте выполняется проверка подключения к БД; ошибка конфигурации, подключения
или занятого порта завершает процесс с ненулевым кодом. SIGINT/SIGTERM запускают
graceful shutdown HTTP, затем закрывается пул. DSN и секреты не выводятся в логи.

```text
cmd/api/             точка входа, сигналы, жизненный цикл сервера и пула
internal/config/     ENV и валидация
internal/httpapi/    chi, логирование запросов, health/readiness
internal/postgres/   настройка и проверка pgxpool
migrations/          схема и синтетические данные, golang-migrate
api/openapi.yaml    контракт реализованных probes
```

Каталоги `internal/auth`, `application`, `credit`, `scoring`, `document`, `worker`
зарезервированы локально под следующие этапы. Пустые каталоги Git не сохраняет;
Go-файлы-заглушки для них не создаются.

### Запуск через Docker Compose

Нужны Docker Engine и Docker Compose. Все команды выполняются из `smart-credit-backend`.

```sh
cp .env.example .env
# В .env замените POSTGRES_PASSWORD; для локального go run обновите и DATABASE_URL.
docker compose up -d postgres
docker compose run --rm migrate up
docker compose up -d --build backend

curl -i http://localhost:8080/health
curl -i http://localhost:8080/ready
docker compose logs backend
```

В обычном Compose работают только `backend` и `postgres`. `migrate` — одноразовая
CLI-команда в профиле `tools`, не постоянный сервис. Миграции намеренно применяются
отдельно перед запуском backend, а не каждым экземпляром приложения.

Используются закреплённые теги образов: Go `1.26.10-alpine3.24`, runtime Alpine
`3.24.3`, PostgreSQL `17.11-alpine3.24`, golang-migrate `v4.20.1`.
Приложение в контейнере работает без root. Порты PostgreSQL и HTTP публикуются только
на `127.0.0.1`. Данные БД и будущие документы находятся в именованных volumes.

```sh
docker compose stop
docker compose start
docker compose down
```

`down` сохраняет volumes. Для удаления тестовых данных нужна отдельная осознанная
операция удаления volumes. Scoring в этом Compose пока отсутствует.

### ENV и локальный запуск Go

Для сборки нужен Go 1.26+. `.env` автоматически читает Compose; Go-приложение
самостоятельно dotenv-файлы не загружает. Пример настройки bash:

```sh
cp .env.example .env
# Установите локальные значения в .env; это shell-совместимый файл с доверенными настройками.
set -a
. ./.env
set +a
docker compose up -d postgres
docker compose run --rm migrate up
go run ./cmd/api
```

Не запускайте локальный Go и Compose backend одновременно на одном HTTP-порту.

| ENV | Значение по умолчанию / назначение |
| --- | --- |
| `HTTP_HOST`, `HTTP_PORT` | `127.0.0.1`, `8080`; Compose задаёт host `0.0.0.0` |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` |
| `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT` | `10s`, `15s` |
| `HTTP_IDLE_TIMEOUT`, `HTTP_SHUTDOWN_TIMEOUT` | `60s`, `15s` |
| `DATABASE_URL` | Обязательный PostgreSQL DSN, без значения в Go-коде |
| `DB_MAX_CONNS`, `DB_MIN_CONNS` | `10`, `2`; min не больше max |
| `DB_CONNECT_TIMEOUT`, `DB_PING_TIMEOUT` | `5s`, `2s`; ping меньше HTTP write timeout |
| `DB_MAX_CONN_LIFETIME`, `DB_MAX_CONN_IDLE_TIME` | `1h`, `15m` |
| `LOG_LEVEL` | `info`; также `debug`, `warn`, `error` |
| `DOCUMENT_STORAGE_PATH` | `./data/documents`; пока не используется |
| `SCORING_URL` | `http://127.0.0.1:8001`; пока не используется |
| `JWT_SECRET` | Без значения по умолчанию; пока не используется и не требуется |
| `DEMO_OTP_CODE` | `1234`; пока нет OTP API |

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
Это только данные для будущей авторизации, вход пока не реализован. Продукты
не заполняются до согласования условий. SQLite не импортируется.

```sh
docker compose run --rm migrate version
docker compose run --rm migrate up        # повторный запуск: no change
docker compose run --rm migrate down 1    # удалить только demo seed
docker compose run --rm migrate up 1      # восстановить seed
```

Полный откат в пустой тестовой БД: `docker compose run --rm migrate down 2`.
Откат seed откажет, если на demo-пользователей уже ссылаются заявки: данные не
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

# Дополнительно: реальное подключение и повторное использование пула.
TEST_DATABASE_URL="$DATABASE_URL" go test -v ./internal/postgres
docker compose build backend
```

Unit-тесты проверяют конфигурацию, probes, таймаут readiness и безопасные ошибки
подключения. Тест с реальной БД пропускается, если `TEST_DATABASE_URL` не задан.
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

### Границы первого этапа

Пока отсутствуют авторизация, OTP API, JWT middleware, API продуктов, рекомендаций,
заявок и документов, scoring-клиент, worker и кредитные решения. Go не обращается
к Python scoring. Загрузка документов и применение резервных ENV-настроек не
имитируются. Следующий этап начинается отдельно после проверки инфраструктуры.
