# Smart Credit — Go backend

Кредитный конвейер для хакатона «Альфа-Будущее»: предприниматель подбирает продукт,
сохраняет заявку, получает решение и подтверждает предложение. Сотрудник банка
рассматривает неоднозначные случаи. Реализована 41 REST-операция API v1.

```sh
docker compose up -d --build
```

Нужны Docker Engine и Docker Compose. Команда запускает Go backend и PostgreSQL,
применяет миграции и создаёт JWT-секрет в постоянном volume. `.env` не обязателен.

```sh
curl --fail http://localhost:8080/health
curl --fail http://localhost:8080/ready
```

## Возможности

- Демонстрационная авторизация: выбор профиля, имитация SMS, код `1234`, JWT.
- Роли CLIENT и BANK_EMPLOYEE, несколько компаний, актуальные полномочия представителей.
- Контактные изменения, контролируемое исправление реквизитов и финансовое происхождение.
- Собственные приватные черновики; отправленные заявки доступны представителям компании.
- Кредитные продукты, предварительные рекомендации, независимые черновики.
- Отправка заявки, проверки компании и кредитной истории, сохранённый scoring.
- Автоматическое одобрение, отказ, контроффер или ручное рассмотрение.
- Ручные решения с отдельными внутренним комментарием и объяснением клиенту.
- Принятие и отклонение предложений, история, идемпотентность, восстановление failed jobs.

`signed` означает демонстрационное подтверждение условий. Реальная выдача денег,
юридически значимый договор и электронная подпись не реализованы. Данные и продукты
синтетические; авторизация не предназначена для промышленной банковской системы.
`APP_ENV=demo` включён явно в Compose. Production-запуск закрыт до подключения
настоящих провайдеров идентификации, реестров, БКИ и подписания: один JWT-секрет
не включает демонстрационные банковские проверки в production.

## Технологии и scoring

Go, `net/http` + chi, PostgreSQL, pgx/v5, golang-migrate, JWT, `log/slog`, Docker Compose.
Go работает как модульный монолит: HTTP и PostgreSQL worker находятся в одном процессе.

Отдельный Python scoring возвращает риск и рекомендацию. Проверен настоящий
`logreg_v1`; Go принимает окончательное решение и рассчитывает условия по снимку
продукта. При недоступности scoring заявка сохраняет техническое состояние обработки,
а не получает кредитный отказ. Базовый Compose не требует соседнего репозитория.

Для локальной интеграции с уже имеющимися исходниками scoring:

```sh
SCORING_CONTEXT=/absolute/path/to/smart-credit-scoring \
  docker compose -f compose.yaml -f compose.scoring.yaml up -d --build
curl --fail http://localhost:8001/health
```

Дополнительный файл собирает исходный Dockerfile scoring и выбирает `logreg_v1`.
Исходники scoring не изменяются. [Контракт, сценарии и результаты](docs/scoring-integration.md).
Общий командный Compose настраивается в отдельном репозитории команды.

## API и проверки

- [OpenAPI v1](api/openapi.yaml) — точные DTO, роли и коды ответов.
- [Подключение frontend](api/FRONTEND_INTEGRATION.md) — сценарии и ограничения интерфейса.
- [Единая Postman Collection](api/postman/smart-credit.postman_collection.json).

Импортируйте коллекцию и запустите сверху вниз в Collection Runner. Working directory
для файлов — `api/postman`; JWT, UUID, версии и ключи передаются автоматически.

В Postman Desktop укажите полный путь к `smart-credit-backend/api/postman` в настройке
**Working directory**. Импорт JSON не переносит файлы: запросы загрузки используют
`fixtures/sample.pdf`, `fixtures/sample.png` и другие файлы из этой папки. Перед
запуском убедитесь, что запрос 123, вкладка Body, видит `fixtures/sample.pdf`.
Если загрузки 123–126 возвращают 400, сначала проверьте доступ к файлам: пустые ID
документов затем вызывают 404 при скачивании и 422 при отправке заявок.

```sh
newman run api/postman/smart-credit.postman_collection.json --working-dir api/postman
```

Для полного прогона с настоящим `logreg_v1`:

```sh
newman run api/postman/smart-credit.postman_collection.json \
  --working-dir api/postman --env-var scoringMode=logreg_v1 \
  --env-var expectScoringResult=true --env-var expectDecisionResult=true \
  --env-var expectReadOutcomes=true --env-var expectActionResults=true
```

[Go-тесты, PostgreSQL, CI и воспроизводимый интеграционный запуск](docs/development.md).

## Структура и подробности

```text
cmd/api/       запуск HTTP и worker
internal/      модули приложения, HTTP, PostgreSQL и HTTP-клиент scoring
migrations/    схема PostgreSQL и синтетические данные
contracts/     JSON Schema внешнего scoring
api/           OpenAPI, инструкция frontend, Postman и тестовые файлы
docs/          архитектура, кредитный процесс и разработка
backend/       сохранённый Python-прототип
tests/         тесты Python-прототипа
```

[Архитектура](docs/architecture.md) · [Кредитный процесс](docs/credit-process.md) ·
[Разработка и настройки](docs/development.md) · [Python-прототип](docs/legacy-prototype.md).

[Компании, права, редактирование, подтверждения и миграция](docs/company-data.md).
