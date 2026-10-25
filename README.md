# Smart Credit — Go backend

Кредитный конвейер для хакатона «Альфа-Будущее»: предприниматель подбирает продукт,
сохраняет заявку, получает решение и подтверждает предложение. Сотрудник банка
рассматривает неоднозначные случаи. Реализованы 27 операций API v1 и проверки /health и /ready.

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
- Роли CLIENT и BANK_EMPLOYEE; каждый клиент связан с одной компанией.
- Самостоятельное редактирование дополнительного email и контактов своей компании с версиями и аудитом.
- Кредитные продукты, предварительные рекомендации, независимые черновики.
- Лимит активных черновиков (20 по умолчанию), безопасное удаление собственного draft.
- Отправка заявки, проверки компании и кредитной истории, сохранённый scoring.
- Автоматическое одобрение, отказ, контроффер или ручное рассмотрение.
- Сохранённые рекомендации и факторы Python отдельно от итоговой оценки Go в банковском кабинете.
- Ручные решения банка, внутренние комментарии и отдельное объяснение клиенту.
- Принятие и отклонение предложений, история и идемпотентность.

`signed` означает демонстрационное подтверждение условий. Реальная выдача денег,
юридически значимый договор и электронная подпись не реализованы. Данные и продукты
синтетические; авторизация не предназначена для промышленной банковской системы.

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

- [Демонстрация для жюри: полные пользовательские сценарии и отдельная Postman Collection](docs/JURY_DEMO_GUIDE.md).
- [OpenAPI v1](api/openapi.yaml) — точные DTO, роли и коды ответов.
- [Подключение frontend](api/FRONTEND_INTEGRATION.md) — сценарии и ограничения интерфейса.
- [Единая Postman Collection](api/postman/smart-credit.postman_collection.json).

Импортируйте коллекцию и запустите сверху вниз в Collection Runner. Working directory
для файлов — `api/postman`; JWT, UUID, версии и ключи передаются автоматически.
В Postman Desktop укажите полный путь к этой папке: импорт JSON не переносит
файлы `fixtures/`. Коллекция рассчитана на синтетические demo-профили; настройка
`maxActiveDraftsPerClient` в Runner должна совпадать с ENV backend (по умолчанию 20).

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
`MAX_ACTIVE_DRAFTS_PER_CLIENT=20` ограничивает только неудалённые заявки в статусе
`draft`; отправленные заявки в лимит не входят. Docker Compose применяет новую
миграцию `000012_contacts_and_draft_lifecycle` автоматически.

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
