# Демонстрация умного кредитного кабинета — руководство для жюри

Это подробный API-маршрут предпринимателя от входа в кабинет до кредитного
решения и принятия предложения. Те же основные действия уже доступны в React
через общий Docker Compose репозитория `smart-credit`. Postman позволяет отдельно
проверить контракты, роли и крайние случаи. Авторизация, документы и ожидание
обработки входят в историю получения кредита.

Это руководство запускает **отдельный backend-стенд** с соседним каталогом
`smart-credit-scoring`, Postman и тестовым OTP `1234`. Для основного показа жюри
используйте общий [README](https://github.com/Tilekbeki/smart-credit#readme):
один Docker Compose, веб-интерфейс и OTP `1221`. Команды и профили двух стендов
не следует смешивать.

Материалы рассчитаны на жюри, продуктовых аналитиков, frontend-разработчиков и
технических экспертов. Данные, люди, компании и условия продукта синтетические.

## Быстрая демонстрация за 10–15 минут

Сначала запустите окружение из следующего раздела. Во время выступления запускайте
**папку целиком в Collection Runner**, затем показывайте несколько её ответов.
Вся коллекция не нужна для короткого выступления.

| Время, ориентировочно | История | Что показать |
| --- | --- | --- |
| 0–4 мин | **01 — Успешное получение кредита** | Профиль и компания, дополнительный email, новая заявка, `approved`, платёж, принятие и `signed` |
| 4–6 мин | **02 — Автоматический отказ** | Заявка прошла `202`, score `35`, риск `HIGH`, объяснение Go |
| 6–8 мин | **03 — Альтернативное предложение** | Запрошено 1 500 000 ₽, предложено 641 064 ₽, платёж 60 000 ₽, принятие |
| 8–13 мин | **04 — Ручное рассмотрение** | `manual_review`, вход Марии, подробный assessment, два комментария, возвращение Ивана и принятие |
| 13–15 мин | Вопросы | Отличие рекомендации Python от решения Go; приватность данных |

Это оценка времени показа с пояснениями. Поочерёдное ручное нажатие **Send** для
каждого запроса и чтение всех ответов займут примерно 25–40 минут. Для короткого
показа на 7–8 минут оставьте папки 01 и 04. Дополнительные истории 05–07 показывают
контакты, черновики и разграничение доступа.

## 1. Подготовка одного изолированного окружения

### 1.1. Что делает система

Предприниматель выбирает продукт, сохраняет анкету, прикладывает документы и
отправляет заявку. Go backend выполняет предварительные проверки, вызывает
отдельный Python scoring и принимает окончательное решение. При необходимости
к процессу подключается сотрудник банка.

| Компонент | Его роль в демонстрации |
| --- | --- |
| Go backend | OTP/JWT, две роли, одна компания клиента, заявки, документы, решения и история |
| PostgreSQL | Пользователи и компании, снимки отправленных заявок, очередь обработки, результаты и аудит |
| Worker внутри Go | Последовательно выполняет prechecks → scoring → Go Decision Engine |
| Python `logreg_v1` | Возвращает score, рекомендацию, сумму, причины и финансовые факторы |
| Симуляторы ФНС и БКИ | Формируют сохранённые предварительные проверки по синтетическим данным |
| Postman | Проверяет те же действия и дополнительные API-сценарии наряду с React-интерфейсом |

В системе только `CLIENT` и `BANK_EMPLOYEE`. У клиента ровно одна компания;
она определяется после входа. Выбора компании, представителей, OCR и
автоматического анализа сумм в загруженном PDF нет. `signed` — демонстрационное
принятие условий, без выдачи денег, электронной подписи и юридического договора.

### 1.2. Запуск с чистыми данными без удаления старой базы

Нужны Docker Engine, Docker Compose, Postman Desktop и соседний локальный каталог
`smart-credit-scoring` с его `Dockerfile` и `scoring/`. Go и Python на компьютере
для контейнерного запуска не обязательны. Все команды ниже выполняйте из
**`smart-credit-backend`**, в одном терминале Bash/Linux/macOS.

```sh
export JURY_PROJECT="smart-credit-jury-$(date +%Y%m%d-%H%M%S)"
export SCORING_CONTEXT="$(cd ../smart-credit-scoring && pwd)"
export SCORING_MODEL=logreg_v1
export APP_ENV=demo
export DEMO_OTP_CODE=1234
export HTTP_PORT=18088
export POSTGRES_PORT=15488
export SCORING_PORT=18089
export MAX_ACTIVE_DRAFTS_PER_CLIENT=20

docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml up -d --build
```

Сохраните значение `JURY_PROJECT`. Новый project получает собственные volumes
PostgreSQL, документов и JWT-секрета. `--env-file /dev/null` исключает значения из
местного `.env`; явно заданное окружение терминала сохраняет приоритет. В чистом
терминале не задавайте другие `POSTGRES_*`, `DATABASE_URL` или `JWT_*` настройки.
`SCORING_CONTEXT` при другом расположении исходников замените полным локальным путём.

При первом запуске применяются миграции до `000012`, создаются demo-пользователи
и секрет JWT. Не направляйте демонстрацию на старую базу другого checkout.
Например, существующая схема версии `14` не соответствует этой версии проекта
с миграциями до `12`. Её нельзя исправлять изменением `schema_migrations` или
подключением прежнего volume к новому project.

Если порты заняты, остановите только своё прежнее demo-окружение либо выберите
три свободных порта и измените `baseUrl` коллекции. Для совершенно чистого показа
создайте **новое имя project**. Старые volumes удалять не нужно.

Остановка своего окружения с сохранением данных:

```sh
docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml down
```

Для возвращения к той же базе используйте тот же `JURY_PROJECT` и команду `up`.
Запуск новой демонстрации с новым project требует свободных портов; остановленная
предыдущая база остаётся в своём volume.

### 1.3. Проверка backend, PostgreSQL и настоящей модели

```sh
docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml ps -a

curl --fail http://localhost:18088/health
curl --fail http://localhost:18088/ready
curl --fail http://localhost:18089/health
```

Ожидаются:

```json
{"status":"ok","service":"backend"}
```

```json
{"status":"ready"}
```

```json
{"status":"ok","service":"scoring"}
```

Контейнеры backend, postgres и scoring должны работать; однократный `migrate`
завершается с кодом `0`. `/ready` проверяет подключение Go к PostgreSQL, но не
готовность Python. Проверка самой базы и её схемы:

```sh
docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml exec -T postgres \
  sh -c 'pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"'

docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml exec -T postgres \
  sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "SELECT version,dirty FROM schema_migrations"'
```

Ожидаются `accepting connections` и `12|f`. Эти команды ничего не изменяют.

`/health` Python не содержит имени модели. Чтобы подтвердить именно `logreg_v1`,
проверьте конфигурацию и загруженный класс внутри **того же scoring-контейнера**:

```sh
docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml exec -T scoring \
  python -c 'import os; from scoring.models import get_model; print(os.environ["SCORING_MODEL"]); print(type(get_model()).__name__)'

docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml exec -T scoring \
  python -c 'import hashlib,pathlib; print(hashlib.sha256(pathlib.Path("scoring/artifacts/logreg_v1.json").read_bytes()).hexdigest())'
```

Проверенная поставка возвращает `logreg_v1`, `LogisticModel` и хеш:

```text
2c1badc982400edefbd29bd33480b204f6988410d0e642d868943a6de45436ff
```

Если модель или артефакт отличаются, точные score и суммы из руководства нужно
проверить заново. Не переключайте сервис на `rules_v1` ради прохождения сценария.
Базовый `compose.yaml` без `compose.scoring.yaml` достаточен для запуска Go,
но не для демонстрации окончательных кредитных решений.

### 1.4. Импорт Postman и передача состояния

Импортируйте **отдельную демонстрационную коллекцию**:

[`api/postman/smart-credit-jury-demo.postman_collection.json`](../api/postman/smart-credit-jury-demo.postman_collection.json).
Основная регрессионная коллекция `smart-credit.postman_collection.json` здесь не
используется и не изменяется.

1. В настройке Postman Desktop **Working directory** выберите полный путь к
   `smart-credit-backend/api/postman`, например
   `/home/grimveqsum/Desktop/Projects/Go/HackathonAlfa/smart-credit-backend/api/postman`.
2. Проверьте, что `fixtures/sample.pdf` в Body запроса загрузки доступен. Импорт
   JSON не копирует файлы; папку `fixtures` как Working directory выбирать не нужно.
3. В Variables коллекции установите `baseUrl=http://localhost:18088`, без
   завершающего `/`. При других портах замените адрес.
4. Оставьте `demoOtpCode=1234`, `pollTimeoutMs=60000`,
   `maxActiveDraftsPerClient=20`, `manualOutcome=approved`.
5. В Environment не переопределяйте эти настройки случайными значениями.
   Выберите папку → **Run** либо всю коллекцию сверху вниз, одну итерацию.

В коллекции семь папок и 133 запроса. Папки 01–06 можно выполнять самостоятельно.
Папка 07 использует заявку и внутренний комментарий, созданные папкой 04: сначала
выполните 04 в той же импортированной коллекции. Не запускайте две папки с OTP
одного пользователя параллельно: новый challenge отменяет предыдущий.

| Переменная | Кто её заполняет и для чего |
| --- | --- |
| `clientProfileId`, `clientCompanyId` | Выбор профиля и OTP verify; компания определяется backend |
| `clientAccessToken` | Вход предпринимателя; папки 05–06 входят под Анной, остальные — под Иваном |
| `bankAccessToken` | Отдельный вход Марии, только банковские запросы |
| `otherClientAccessToken` | Вход второго предпринимателя в проверках безопасности |
| `approvalApplicationId`, `refusalApplicationId`, `counterApplicationId`, `manualApplicationId` | ID новой заявки соответствующей истории |
| `…Version`, `…DocumentId`, `…OfferId` | Ответы backend; версии пользователя, компании и заявки независимы |
| `…AcceptKey`, `manualManualKey` | UUID Idempotency-Key для одного логического действия |
| `…AcceptBodyJSON`, `manualManualBodyJSON` | Исходное тело для безопасного сетевого повтора действия |

Переносить JWT, UUID или версии руками не нужно. В документе нет настоящих JWT.
Не экспортируйте коллекцию с заполненными токенами. В ручном режиме нажимайте
Send строго по порядку; polling автоматически повторяется только в Runner/Newman.
После изменения переменных в Postman нажмите Save.

Для автоматической проверки при уже установленном Newman:

```sh
newman run api/postman/smart-credit-jury-demo.postman_collection.json \
  --working-dir api/postman --iteration-count 2
```

При отсутствии Newman на компьютере можно использовать контейнер. Он обращается
к backend по имени сервиса и его внутреннему порту:

```sh
docker run --rm --network "${JURY_PROJECT}_default" \
  -v "$PWD/api/postman:/etc/newman:ro" --workdir /etc/newman \
  postman/newman:6.1.3-alpine run smart-credit-jury-demo.postman_collection.json \
  --env-var "baseUrl=http://backend:${HTTP_PORT}" --iteration-count 2
```

### 1.5. Общие правила чтения ответов

Все пути в следующих таблицах приведены с префиксом `/api/v1`. Полный URL —
`{{baseUrl}}` плюс путь. Для JSON нужен `Content-Type: application/json`.
Авторизованные запросы передают `Authorization: Bearer {{clientAccessToken}}` или
`{{bankAccessToken}}`; запросы OTP и списка профилей выполняются без токена.

**Вход предпринимателя** — единый шаг истории из трёх запросов:

```http
GET /api/v1/demo/profiles
```

Ответ `200` содержит `items` с именем, ролью, номером и companyName. Затем:

```http
POST /api/v1/auth/otp/request
Content-Type: application/json

{"profileId":"{{clientProfileId}}"}
```

Ответ `201`: `otpRequestId`, `expiresAt`, `maskedPhone`. Реальная SMS не отправляется.

```http
POST /api/v1/auth/otp/verify
Content-Type: application/json

{"otpRequestId":"{{clientOtpRequestId}}","code":"{{demoOtpCode}}"}
```

Ответ `200`: `accessToken`, `tokenType=Bearer`, `expiresAt`, `userId`,
`role=CLIENT`, единственный `companyId`. Для банка та же последовательность с
`bankProfileId` и `bankOtpRequestId`, результат `role=BANK_EMPLOYEE`, `companyId=null`.
Роль не передаётся в теле запроса и не выбирается клиентом.

**Загрузка выписки** — `POST /api/v1/applications/{id}/documents`, Body → form-data:

| Ключ | Тип в Postman | Значение |
| --- | --- | --- |
| `documentType` | Text | `BANK_STATEMENT` |
| `file` | File | `fixtures/sample.pdf` |

Не задавайте multipart boundary вручную. Ответ `201` возвращает `documentId`,
`applicationId`, `documentType`, `fileName`, `contentType`, `sizeBytes`, `uploadedAt`.
Сервер проверяет формат PDF/PNG/JPEG, размер до 5 МиБ, имя и SHA-256. OCR нет.

**Обязательность документов:** для `turnover` Ивана seed задаёт доверенный источник
финансов, поэтому выписка не обязательна. В историях 01–04 коллекция прикладывает её
добровольно, чтобы показать работу с документом. Для `turnover` Анны и Павла
`BANK_STATEMENT` обязательна. Для `investment` нужен `EQUIPMENT_INVOICE`; продукт
имеет цели `equipment`/`upgrade`, сумму 300 000–10 000 000 ₽, срок 12–60 месяцев,
ставку 20%. Инвестиционный кредит не используется в четырёх основных историях.

**Отправка:** `POST /api/v1/applications/{id}/submit` **без тела**, не с `{}`.
`202` означает, что заявка принята, снимки зафиксированы и создано задание.
Это ещё не одобрение. До конечного результата читается
`GET /api/v1/applications/{id}/decision`: `submitted`/`processing`,
`processing.stage=prechecks|scoring|decision`, безопасное сообщение и возможный retry.

Runner повторяет запрос раз в 500 мс, максимум 60 секунд. Ожидаемые исходы:
`approved`, `rejected`, `counter_offer`, `manual_review`. `decision=null` и `offer=null`
при `manual_review` корректны: окончательное решение должен принять сотрудник.
Технический `processing.state=failed` — повод проверить сервисы, а не кредитный отказ.

**Принятие:** `POST /api/v1/applications/{id}/accept`, JSON
`{"version":N,"offerId":"UUID"}` и `Idempotency-Key: UUID`. Коллекция сама берёт
версию и offerId из решения, сохраняет ключ и исходное тело. Не меняйте их при
сетевом повторе. Успех `200`: заявка `signed`, предложение `accepted`, исходное
решение `approved`/`counter_offer` сохраняется. `/disburse` отсутствует.

## 2. Проверенные входные данные и результаты

Проверено заново **25 октября 2026 года**, на описанном выше артефакте `logreg_v1`
и реальном Go Decision Engine `go-demo-v2`, через HTTP и PostgreSQL. Это результаты
новой демонстрационной коллекции, а не перенос старого отчёта регрессионных тестов.

Основной предприниматель — **Иван Лавкин**, телефон `+79990000001`, компания
**ИП «Лавка у дома»**. Seed ID пользователя:
`20000000-0000-4000-8000-000000000001`, компании:
`10000000-0000-4000-8000-000000000001`.
Сотрудник — **Мария Банкова**, `+79990000004`, ID
`20000000-0000-4000-8000-000000000004`.
Коллекция получает эти профили из PostgreSQL через API.

Во всех четырёх новых заявках Ивана: `productId=turnover`, `purpose=stock`,
`termMonths=12`. Из серверного симулятора БКИ берутся долг **500 000 ₽**,
действующие ежемесячные платежи **90 000 ₽**, просрочки **0**.
Их не передают в контактные PATCH и не подменяют в PostgreSQL.

| История | monthlyRevenueRub | monthlyExpensesRub | requestedAmountRub | Python: score / decision / approvedAmountRub | Результат Go до принятия |
| --- | ---: | ---: | ---: | --- | --- |
| 01 | 2 400 000 | 1 500 000 | 1 500 000 | 97 / approved / 1 500 000 | LOW, approved; 1 500 000 ₽, 22%, 12 мес., 140 392 ₽/мес. |
| 02 | 600 000 | 540 000 | 100 000 | 35 / rejected / 0 | HIGH, rejected; предложения нет |
| 03 | 600 000 | 300 000 | 1 500 000 | 96 / counter_offer / 809 633 | LOW, counter_offer; 641 064 ₽, 22%, 12 мес., 60 000 ₽/мес. |
| 04 | 10 000 000 | 8 800 000 | 1 400 000 | 78 / counter_offer / 1 190 000 | MEDIUM, manual_review; до сотрудника предложения нет |

В анкете используются синтетические показатели **новой заявки**, а не изменения
финансов компании. Компания в seed остаётся прежней. Это разрешённый текущим API
демонстрационный ввод, а не банковское подтверждение финансов из PDF.

Значения привязаны к артефакту и seed. UUID заявок/документов/предложений,
assessedAt, даты и текущие версии выдаёт сервер. Их нельзя брать из старого запуска.

## 3. Сценарий №1 — Предприниматель получает автоматическое одобрение кредита

### Цель

Иван пополняет товарные запасы. Его денежного потока достаточно для запрошенных
условий, и процесс завершается без ручного кредитного решения сотрудника.

### Предварительные условия и участники

Работают три сервиса из раздела 1, исходные demo-данные не изменены. Участник
истории — Иван. Для технического показа Python/Go Мария входит в отдельный
банковский режим, не принимая решение за алгоритм.

### Входные данные

| Поле | Значение |
| --- | --- |
| Компания, клиент | ИП «Лавка у дома», Иван Лавкин |
| Продукт, цель | turnover — Оборотный кредит; stock — товарные запасы |
| Ставка, границы продукта | 22%; 100 000–3 000 000 ₽; 6–24 месяца; бизнес от 6 месяцев |
| Выручка / расходы / срок | 2 400 000 ₽ / 1 500 000 ₽ / 12 месяцев |
| Черновая / окончательная сумма | 1 400 000 ₽ → 1 500 000 ₽ после исправления |
| Документ | Добровольная BANK_STATEMENT, fixtures/sample.pdf |
| Переменная новой заявки | approvalApplicationId |

### Пошаговое прохождение

Запустите папку **01 — Успешное получение кредита**. Иван видит свои сведения,
выбирает продукт, сохраняет анкету, уточняет сумму и отправляет её в банк.

| Действие человека; запросы в папке | Метод и endpoint | Тело / важные поля | Ожидаемый ответ и смысл |
| --- | --- | --- | --- |
| Входит в кабинет; 01–03 | GET `/api/v1/demo/profiles`, POST `/api/v1/auth/otp/request`, POST `/api/v1/auth/otp/verify` | Общая последовательность из 1.5, код 1234 | 200 → 201 → 200; CLIENT и companyId из backend |
| Проверяет профиль и компанию; 04–05 | GET `/api/v1/me`; GET `/api/v1/companies/{{clientCompanyId}}` | Без тела | 200; name, phone, additionalEmail, contact, financials |
| Добавляет email и убеждается, что он сохранён; 06–07 | PATCH `/api/v1/me`; GET `/api/v1/me` | `{"version":{{clientVersion}},"additionalEmail":"jury.owner@example.test"}` | 200; новая версия, телефон входа прежний |
| Сравнивает продукты и получает предварительный подбор; 08–09 | GET `/api/v1/credit-products`; POST `/api/v1/recommendations` | purpose=stock, сумма 1500000, срок 12, выручка 2400000, расходы 1500000 | 200; turnover, paymentBudgetRub=360000; подбор не вызывает scoring и не обещает кредит |
| Сохраняет черновик; 10 | POST `/api/v1/applications` | JSON ниже, первоначальная сумма 1400000 | 201; applicationId, status=draft, version=1 |
| Уточняет сумму; 11 | PATCH `/api/v1/applications/{{approvalApplicationId}}` | `{"version":{{approvalVersion}},"requestedAmountRub":1500000}` | 200; draft, новая version; документ ещё можно добавить |
| Прикрепляет выписку и видит её в списке; 12–13 | POST `/api/v1/applications/{{approvalApplicationId}}/documents`; GET того же пути | Multipart из 1.5 | 201 → 200; documentId относится к этой заявке |
| Отправляет заполненную заявку; 14 | POST `/api/v1/applications/{{approvalApplicationId}}/submit` | Без тела | 202; submittedAt и nextStage; обработка началась |
| Ждёт и читает объяснение; 15–16 | GET `/api/v1/applications/{{approvalApplicationId}}/decision`; GET `/api/v1/applications/{{approvalApplicationId}}/timeline` | Runner повторяет decision, затем читает историю | 200; approved, score 97, LOW, offer; в истории SCORING_COMPLETED и APPLICATION_APPROVED |
| Жюри смотрит оценку со стороны банка; 17–19 | Вход Марии через OTP; GET `/api/v1/bank/applications/{{approvalApplicationId}}/assessment` | bankAccessToken | 201 → 200 → 200; Python approved/1500000 и отдельный goDecision=approved |
| Принимает предложенные условия; 20–21 | POST `/api/v1/applications/{{approvalApplicationId}}/accept`; GET `/api/v1/applications/{{approvalApplicationId}}/decision` | Сохранённые version, offerId и Idempotency-Key | 200; signed, offer.status=accepted |
| Завершает показ контакта; 22 | PATCH `/api/v1/me` | Исходный email/null с актуальной clientVersion | 200; контакт восстановлен, версия не откатывается |

Тело первоначального черновика; `purposeDescription` в коллекции также получает
метку конкретного запуска:

```json
{
  "productId": "turnover",
  "purpose": "stock",
  "purposeDescription": "Жюри: автоматическое одобрение",
  "requestedAmountRub": 1400000,
  "termMonths": 12,
  "monthlyRevenueRub": 2400000,
  "monthlyExpensesRub": 1500000
}
```

Тело предварительного подбора:

```json
{"purpose":"stock","requestedAmountRub":1500000,"termMonths":12,"monthlyRevenueRub":2400000,"monthlyExpensesRub":1500000}
```

### Ожидаемый результат

Фактические поля `offer` после Go-решения: `amountRub=1500000`,
`annualRatePercent=22`, `termMonths=12`, `monthlyPaymentRub=140392`.
До принятия `status=approved`; после — `status=signed`, `decision=approved`.
`customerExplanation=null` у автоматического решения: объяснение находится в reasons:

> Компания прошла обязательные проверки.
> Низкий кредитный риск и допустимая финансовая нагрузка.

Python объясняет свою рекомендацию отдельно: операционный поток покрывает
оценочный платёж, в синтетической истории нет просрочек.

### Что показать жюри

- Одно действие отправки запускает весь конвейер и сохраняет понятную историю.
- До подтверждения Иван видит сумму, ставку, срок и реальный платёж Go.
- Принятие предложения приводит к signed; выдача денег не имитируется как факт.

### Как будет выглядеть во frontend

Вход → кабинет → профиль и компания → подбор продукта → редактируемая анкета →
документы → экран обработки → карточка одобрения → подтверждение условий.
В банковском режиме доступен отдельный блок сохранённого assessment.

## 4. Сценарий №2 — Предприниматель получает автоматический отказ из-за кредитного риска

### Цель

Иван хочет небольшой кредит, но нагрузка относительно свободного денежного потока
слишком высока. Жюри должно увидеть полноценную подачу заявки и кредитный отказ.

### Предварительные условия и участники

Те же сервисы, модель и исходная компания Ивана. Папка **02 — Автоматический отказ**
начинается со своего входа и создаёт отдельную заявку; папку 01 запускать не нужно.
Мария входит только для технического просмотра сохранённых результатов.

### Входные данные

| Поле | Значение |
| --- | --- |
| Клиент / продукт / цель | Иван / turnover / stock |
| Выручка / расходы | 600 000 / 540 000 ₽ |
| Сумма / срок | 100 000 ₽ / 12 месяцев |
| Операционный поток / маржа | 60 000 ₽ / 10% |
| Старые платежи / просрочки | 90 000 ₽ / 0, из БКИ |
| Документ | Добровольная BANK_STATEMENT, sample.pdf |
| ID заявки | refusalApplicationId |

Все значения допустимы для API. Сумма равна минимуму продукта, срок допустим,
маржа ровно 10% не нарушает обязательную границу. Это не ошибка валидации.

```json
{
  "productId":"turnover","purpose":"stock",
  "purposeDescription":"Жюри: отказ по высокому риску",
  "requestedAmountRub":100000,"termMonths":12,
  "monthlyRevenueRub":600000,"monthlyExpensesRub":540000
}
```

### Пошаговое прохождение

Иван входит, проверяет свою компанию, выбирает оборотный кредит и отправляет
новую анкету. Только после обработки он получает объяснение банка.

| Действие; запросы | Метод и endpoint | Тело | Ответ / что происходит |
| --- | --- | --- | --- |
| Вход, проверка профиля и компании; 01–05 | GET `/api/v1/demo/profiles` → POST `/api/v1/auth/otp/request` → POST `/api/v1/auth/otp/verify`; GET `/api/v1/me`; GET `/api/v1/companies/{{clientCompanyId}}` | Общий вход 1.5 | 200/201/200; CLIENT, своя компания; затем 200/200 |
| Выбор продукта и сохранение анкеты; 06–07 | GET `/api/v1/credit-products`; POST `/api/v1/applications` | JSON выше | 200 → 201; новый draft, собственный applicationId |
| Приложение выписки и проверка связи; 08–09 | POST `/api/v1/applications/{{refusalApplicationId}}/documents`; GET того же пути | Multipart 1.5 | 201 → 200; документ сохранён у нужной заявки |
| Отправка; 10 | POST `/api/v1/applications/{{refusalApplicationId}}/submit` | Без тела | **202**, заявка принята, отказа ещё нет |
| Ожидание результата; 11 | GET `/api/v1/applications/{{refusalApplicationId}}/decision` | Без тела, polling | 200; итог rejected, score 35, HIGH, offer=null |
| Просмотр истории; 12 | GET `/api/v1/applications/{{refusalApplicationId}}/timeline` | Без тела | 200; APPLICATION_SUBMITTED → проверки → SCORING_COMPLETED → APPLICATION_REJECTED |
| Технический просмотр оценки; 13–15 | Вход Марии; GET `/api/v1/bank/applications/{{refusalApplicationId}}/assessment` | bankAccessToken | 201/200/200; разные причины Python и Go |

### Ожидаемый результат

Python: `score=35`, `decision=rejected`, `approvedAmountRub=0`. Его причина:

> Свободного денежного потока недостаточно даже для уменьшенной суммы.

Фактор `debt_service_share=1.6544`: оценочные текущие и новые платежи превышают
операционный поток. `estimated_new_monthly_payment=9263.45` — оценка Python, а не
готовое банковское предложение.

Go: `riskLevel=HIGH`, `decision=rejected`, предложения нет. Причина:

> Высокий кредитный риск по демонстрационной кредитной политике.

Статус остаётся rejected, а не processing/ошибка. Действия accept для отказа нет.

### Что показать жюри

- Разницу между HTTP202 при подаче и кредитным rejected после конвейера.
- Объяснимый высокий риск при формально корректной анкете.
- Сохранённую историю, подтверждающую реальный расчёт.

### Как будет выглядеть во frontend

Вход → компания → продукт → анкета → документы → обработка → экран отказа с
причинами и историей. Кнопки принятия предложения нет; банковские factors клиенту
не показываются.

## 5. Сценарий №3 — Банк предлагает предпринимателю меньшую сумму кредита

### Цель

Иван просит больше, чем может обслуживать. Банк сохраняет возможность сделки:
вместо полного отказа предлагает допустимую меньшую сумму.

### Предварительные условия и участники

Независимая папка **03 — Альтернативное предложение**, те же seed Ивана и logreg_v1.
Отдельный вход и новая заявка. Мария читает assessment, но не принимает ручное решение.

### Входные данные

| Поле | Значение |
| --- | --- |
| Клиент / продукт / цель | Иван / turnover / stock |
| Выручка / расходы | 600 000 / 300 000 ₽ |
| Запрошено / срок | 1 500 000 ₽ / 12 месяцев |
| Операционный поток / старые платежи | 300 000 / 90 000 ₽ |
| Бюджет нового платежа | 60 000 ₽ |
| Документ / ID | Добровольная BANK_STATEMENT; counterApplicationId |

```json
{
  "productId":"turnover","purpose":"stock",
  "purposeDescription":"Жюри: альтернативное предложение",
  "requestedAmountRub":1500000,"termMonths":12,
  "monthlyRevenueRub":600000,"monthlyExpensesRub":300000
}
```

### Пошаговое прохождение

Иван проходит привычный путь подачи, затем сравнивает запрошенные и доступные
условия и принимает меньшую сумму.

| Действие; запросы | Метод и endpoint | Тело | Ответ / что происходит |
| --- | --- | --- | --- |
| Вход, профиль, компания; 01–05 | Общий вход 1.5; GET `/api/v1/me`; GET `/api/v1/companies/{{clientCompanyId}}` | OTP 1234 | 200/201/200, затем 200/200; CLIENT и его компания |
| Выбор продукта, новая анкета; 06–07 | GET `/api/v1/credit-products`; POST `/api/v1/applications` | JSON выше | 200 → 201, draft |
| Загрузка выписки; 08 | POST `/api/v1/applications/{{counterApplicationId}}/documents` | Multipart 1.5 | 201; свой documentId |
| Отправка; 09 | POST `/api/v1/applications/{{counterApplicationId}}/submit` | Без тела | 202; зафиксированы исходные условия |
| Ожидание и история; 10–11 | GET `/api/v1/applications/{{counterApplicationId}}/decision`; GET `/api/v1/applications/{{counterApplicationId}}/timeline` | Polling | 200; counter_offer, LOW, активное предложение; COUNTER_OFFER_CREATED в истории |
| Сравнение расчётов; 12–14 | Вход Марии; GET `/api/v1/bank/applications/{{counterApplicationId}}/assessment` | bankAccessToken | 201/200/200; Python counter_offer, score 96, рекомендация 809633 |
| Принятие и проверка; 15–16 | POST `/api/v1/applications/{{counterApplicationId}}/accept`; GET `/api/v1/applications/{{counterApplicationId}}/decision` | offerId, version, постоянный UUID ключа | 200; signed и accepted |

### Ожидаемый результат

| Показатель | Фактическое значение |
| --- | ---: |
| Запрошено | 1 500 000 ₽ |
| Рекомендовано Python | 809 633 ₽ |
| Окончательно предложено Go | **641 064 ₽** |
| Ставка / срок | 22% / 12 месяцев |
| Ежемесячный платёж Go | **60 000 ₽** |
| Score / риск | 96 / LOW |

Бюджет Go: `max(0, floor((600000−300000)/2)−90000)=60000`.
Go применяет аннуитет по ставке продукта 22%, округляет платёж вверх до целого
рубля, доступную сумму вниз и ограничивает её запросом и рекомендацией Python.
Python оценивает платёж по своим иллюстративным 20%, поэтому его сумма не равна
окончательному предложению банка.

Python указывает меньшую доступную сумму и превышение нагрузки исходной заявки.
Go объясняет: «Исходная сумма превышает допустимую финансовую нагрузку» и
«Предложена меньшая сумма в пределах продукта и доступного бюджета».
После принятия `decision=counter_offer` сохраняется, `status=signed`.

### Что показать жюри

- Система предлагает исполнимый вариант, сохраняя ценность для клиента.
- Высокий score 96 не отменяет ограничение платёжного бюджета.
- Python рекомендует, Go независимо определяет условия реального demo-offer.

### Как будет выглядеть во frontend

Вход → компания → продукт → анкета → документы → обработка → сравнение
«запрошено / предложено» → условия и платёж → принятие альтернативы → signed.

## 6. Сценарий №4 — Предприниматель отправляет заявку, требующую решения сотрудника банка

### Цель

Автоматическая оценка показывает средний риск. Иван и Мария последовательно
работают с одной заявкой, сохраняя границы доступа и понятное объяснение клиенту.

### Предварительные условия и участники

Папка **04 — Ручное рассмотрение** запускается самостоятельно. Основной вариант:
`manualOutcome=approved`, `manualCustomerExplanation` пустое — коллекция подставит
публичный текст из примера ниже. Иван — CLIENT, Мария — BANK_EMPLOYEE. Токены раздельны.

### Входные данные

| Поле | Значение |
| --- | --- |
| Клиент / компания | Иван / ИП «Лавка у дома» |
| Продукт / цель | turnover / stock |
| Выручка / расходы | 10 000 000 / 8 800 000 ₽ |
| Запрос / срок | 1 400 000 ₽ / 12 месяцев |
| Документ | Добровольная BANK_STATEMENT, sample.pdf |
| ID | manualApplicationId |
| Python | score 78, counter_offer, рекомендация 1 190 000 ₽ |
| Автоматический Go | MEDIUM, manual_review; предложения ещё нет |

```json
{
  "productId":"turnover","purpose":"stock",
  "purposeDescription":"Жюри: ручное рассмотрение",
  "requestedAmountRub":1400000,"termMonths":12,
  "monthlyRevenueRub":10000000,"monthlyExpensesRub":8800000
}
```

### Пошаговое прохождение: Иван подаёт заявку

| Действие; запросы | Метод и endpoint | Тело | Ответ / объяснение |
| --- | --- | --- | --- |
| Входит, смотрит профиль, компанию и продукт; 01–06 | Общий вход 1.5; GET `/api/v1/me`, `/api/v1/companies/{{clientCompanyId}}`, `/api/v1/credit-products` | OTP 1234 | 200/201/200; затем 200; один CLIENT и одна компания |
| Сохраняет анкету; 07 | POST `/api/v1/applications` | JSON выше | 201, новая draft |
| Прикладывает выписку и проверяет её; 08–09 | POST и GET `/api/v1/applications/{{manualApplicationId}}/documents` | Multipart 1.5 | 201 → 200 |
| Отправляет; 10 | POST `/api/v1/applications/{{manualApplicationId}}/submit` | Без тела | 202, началась асинхронная обработка |
| Дожидается ручного рассмотрения и читает историю; 11–12 | GET `/api/v1/applications/{{manualApplicationId}}/decision`; GET `/api/v1/applications/{{manualApplicationId}}/timeline` | Polling | 200; manual_review, score 78, MEDIUM, decision=null, offer=null; MANUAL_REVIEW_REQUIRED |

Иван получает не отказ, а сообщение о необходимости рассмотрения. Предварительные
проверки уже завершены, поэтому Мария работает с сохранёнными результатами.

### Пошаговое прохождение: Мария рассматривает ту же заявку

| Действие; запросы | Метод и endpoint | Тело / токен | Ответ / объяснение |
| --- | --- | --- | --- |
| Входит в банковский кабинет; 13–15 | POST `/api/v1/auth/otp/request`, POST `/api/v1/auth/otp/verify`; GET `/api/v1/me` | bankProfileId → bankOtpRequestId; bankAccessToken | 201 → 200 → 200; BANK_EMPLOYEE, companyId=null |
| Находит отправленную заявку; 16 | GET `/api/v1/bank/applications?status=manual_review&companyId={{clientCompanyId}}&limit=100` | bankAccessToken | 200; нужный applicationId в items |
| Открывает карточку и условия; 17 | GET `/api/v1/bank/applications/{{manualApplicationId}}` | Без тела | 200; компания, productTerms, исходная сумма/срок, availableActions=[decide], актуальная version |
| Читает документы; 18–19 | GET `/api/v1/applications/{{manualApplicationId}}/documents`; GET `/api/v1/applications/{{manualApplicationId}}/documents/{{manualDocumentId}}` | bankAccessToken | 200; метаданные и PDF; доступ после submit |
| Изучает оценку; 20 | GET `/api/v1/bank/applications/{{manualApplicationId}}/assessment` | bankAccessToken | 200; prechecks, scoring, goDecision, причины manual_review |
| Принимает ручное решение; 21 | POST `/api/v1/bank/applications/{{manualApplicationId}}/decisions` | JSON ниже; bankAccessToken; Idempotency-Key | 201; approved и предложение; сотрудник и аудит определены backend |
| Проверяет внутренний аудит; 22 | GET `/api/v1/bank/applications/{{manualApplicationId}}/assessment` | bankAccessToken | 200; manualDecision содержит внутренний и публичный комментарии |

Сокращённый фактический блок сохранённого Python-результата, без динамического
assessedAt, из ответа шага 20:

```json
{
  "score":78,
  "decision":"counter_offer",
  "approvedAmountRub":1190000,
  "reasons":[
    "По оценке денежного потока доступна меньшая сумма.",
    "Невысокая операционная маржа снижает рекомендуемый лимит."
  ],
  "factors":{
    "operating_cash_flow":1200000,
    "operating_margin":0.12,
    "estimated_new_monthly_payment":129688.31,
    "debt_service_share":0.1831,
    "existing_debt_to_annual_revenue":0.0042,
    "overdue_count":0
  }
}
```

Go сохраняет score 78 и риск MEDIUM. Его отдельные причины:
«Средний кредитный риск требует решения сотрудника банка» и «Рекомендация скоринга
не согласуется с автоматическим одобрением по кредитной политике».
Это не дополнительные Python reasons.

Ручное решение основного варианта:

```http
POST /api/v1/bank/applications/{{manualApplicationId}}/decisions
Content-Type: application/json
Authorization: Bearer {{bankAccessToken}}
Idempotency-Key: {{manualManualKey}}
```

```json
{
  "version":4,
  "decision":"approved",
  "internalComment":"JURY_INTERNAL_ONLY: проверены синтетические результаты и допустимость платежа.",
  "customerExplanation":"Исходные условия одобрены после рассмотрения заявки сотрудником банка.",
  "offer":{"amountRub":1400000,"termMonths":12}
}
```

`version=4` — наблюдавшийся пример для новой заявки без PATCH. Коллекция использует
реальную актуальную `manualVersion`, а не число из документа. JSON и UUID ключа
сохраняются при первом решении для повторной отправки без изменения смысла.

Одобрение исходных условий допустимо: поток 1 200 000 ₽, маржа 12%, бюджет
`1200000/2−90000=510000` ₽. Платёж Go131 033 ₽ укладывается в него. Нет критических
оснований отказа; средний риск и рекомендательный конфликт разрешает сотрудник.
Go сохраняет ставку 22% из снимка продукта, не принимает ставку от сотрудника.

### Пошаговое прохождение: Иван получает ответ банка

| Действие; запросы | Метод и endpoint | Тело / токен | Ответ / объяснение |
| --- | --- | --- | --- |
| Возвращается в ту же заявку; 23 | GET `/api/v1/applications/{{manualApplicationId}}` | clientAccessToken | 200; approved, тот же владелец и ID |
| Читает объяснение и историю; 24–25 | GET `/api/v1/applications/{{manualApplicationId}}/decision`; GET `/api/v1/applications/{{manualApplicationId}}/timeline` | clientAccessToken | 200; customerExplanation и публичные reasons; внутреннего комментария нет |
| Принимает предложение; 26–27 | POST `/api/v1/applications/{{manualApplicationId}}/accept`; GET `/api/v1/applications/{{manualApplicationId}}/decision` | Актуальные version, offerId, сохранённый Idempotency-Key | 200; signed, offer.status=accepted |

### Ожидаемый результат

По умолчанию: automatic `manual_review` → manual `approved` → client `signed`.
Сумма 1 400 000 ₽, ставка 22%, срок 12, платёж 131 033 ₽. В банковском assessment
`goDecision` по-прежнему показывает автоматический вывод manual_review,
`manualDecision` — последующее решение Марии. История сохраняет обоих участников.

`internalComment` и legacy `comment` видит только банк. В клиентских карточке,
решении и timeline нет ни поля внутренней заметки, ни текста `JURY_INTERNAL_ONLY`.
`customerExplanation` доступен Ивану. Если отдельное публичное объяснение не
указать при обычном API-вызове, backend использует безопасную стандартную формулировку.

### Дополнительные ручные исходы

Для каждого варианта **заново запустите всю папку 04**, чтобы создать другую заявку.
Принятое решение по старой заявке нельзя заменить другим.

| Переменная manualOutcome | Тело решения, выбранное коллекцией | Результат |
| --- | --- | --- |
| approved, по умолчанию | offer.amountRub=1400000, termMonths=12 | 201 approved → принятие → signed; платёж 131033 |
| rejected | offer=null | 201 rejected, публичное объяснение, предложения нет; запрос accept автоматически пропускается |
| counter_offer | offer.amountRub=1000000, termMonths=12 | 201 counter_offer → принятие → signed; платёж 93595 |

`manualCustomerExplanation` можно заполнить своим публичным текстом длиной до 1000
символов. При пустом значении коллекция подставляет безопасную фразу под выбранный
исход. Сначала установите переменные, затем запускайте папку. Чтобы восстановить
основной показ, верните `manualOutcome=approved`.

Наблюдавшиеся публичные тексты дополнительных вариантов:
«По результатам ручного рассмотрения кредит не одобрен» и
«После рассмотрения предлагаем меньшую сумму с допустимым платежом».
Текст может быть вашим, если он передан в customerExplanation; не копируйте
туда внутреннюю заметку.

Сотрудник не может увеличить сумму выше запроса/границ продукта, произвольно
изменить исходные условия при approved или обойти обязательную финансовую политику.
Для отказа предложение запрещено. Недопустимые ручные условия дают 422.

### Что показать жюри

- Два пользователя работают с одной заявкой без ручной передачи ID или токенов.
- Сотрудник понимает причины автоматического направления на рассмотрение.
- Клиент получает объяснение решения без внутренних банковских заметок.

### Как будет выглядеть во frontend

Иван: вход → анкета → документы → ожидание → ручное рассмотрение.
Мария: отдельный вход → очередь → карточка компании/условий → PDF → scoring и Go →
форма решения с двумя полями комментариев.
Иван: обновлённое решение → публичное объяснение → принятие предложения.

## 7. Сценарий №5 — Предприниматель управляет контактной информацией

### Цель, предварительные условия и участники

**Анна Северова**, `+79990000002`, CLIENT компании **ООО «Мастерская Север»**,
исправляет контакты. Папка 05 самостоятельна; её изменяемые контакты не используются
в основных историях Ивана. Сервисы и чистое demo-окружение подготовлены по разделу 1.

### Входные данные

| Что меняется | Демонстрационное значение |
| --- | --- |
| additionalEmail | jury.anna@example.test |
| contact.phone | +79990000092 |
| contact.email | jury.workshop@example.test |
| contact.address | Демонстрационный город, Адрес для связи, дом 92 |

Остальные поля компании и номер входа по OTP не меняются. Исходные контакты сохраняются для восстановления.

### Пошаговое прохождение

| Действие; запросы | Метод и endpoint | Тело | Ответ |
| --- | --- | --- | --- |
| Анна входит и проверяет профиль/компанию; 01–06 | Вход 1.5; GET `/api/v1/me`; GET `/api/v1/companies/{{clientCompanyId}}` | Профиль Анны | 200/201/200 и чтения 200; раздельные версии пользователя/компании |
| Исправляет личный контакт; 07 | PATCH `/api/v1/me` | `{"version":{{clientVersion}},"additionalEmail":"jury.anna@example.test"}` | 200; дополнительный email и новая version |
| Исправляет контакты компании; 08 | PATCH `/api/v1/companies/{{clientCompanyId}}` | JSON ниже | 200; изменён только contact, новая company.version |
| Повторно читает данные; 09–10 | GET `/api/v1/me`; GET компании | Без тела | 200; новые контакты сохранились между запросами |
| Проверяет защищённые реквизиты; 11 | PATCH компании | version, contact.email и запрещённый `inn` | 400 invalid_request; частичного сохранения нет |
| Проверяет запрет самостоятельной смены роли; 12 | PATCH `/api/v1/me` | version, additionalEmail и `role=BANK_EMPLOYEE` | 400 invalid_request |
| Завершает демонстрацию без изменения исходных контактов; 13–14 | PATCH `/api/v1/me`; PATCH компании | Исходные значения с актуальными версиями | 200; контакты восстановлены, аудит и рост версий сохраняются |

```json
{
  "version":1,
  "contact":{
    "phone":"+79990000092",
    "email":"jury.workshop@example.test",
    "address":"Демонстрационный город, Адрес для связи, дом 92"
  }
}
```

В Postman вместо 1 используется `{{companyVersion}}`. Пустая строка/null в
контактах компании запрещены. additionalEmail можно очистить явным null.
Устаревшая version даёт 409 version_conflict: перечитайте сущность, а не повторяйте
изменение вслепую. Имя, ИНН, регистрационные данные, финансы, роль и привязка
компании не входят в whitelist; банк не редактирует эти данные вообще.

### Ожидаемый результат и что показать жюри

Контакты доступны для самостоятельного исправления, юридическая и финансовая
информация защищена, изменения атомарно записаны вместе с аудитом. Покажите новый
email, прежний OTP-телефон и отказ на изменение ИНН. Контактный аудит не выдаётся
за кредитное событие в клиентской timeline.

### Как будет выглядеть во frontend

Вход → профиль/компания → форма только разрешённых контактов → сохранить →
повторное чтение; при 409 экран предлагает обновить данные. Юридические поля readonly.

## 8. Сценарий №6 — Предприниматель сохраняет, редактирует и удаляет заявки

### Цель, предварительные условия и участники

Анна работает с незавершёнными анкетами и освобождает лимит. Папка 06 самостоятельна.
Используйте отдельную demo-базу, лимит 20 и исходную компанию Анны. Поля этой компании
не меняются. Помощник работает только с синтетическими заявками данной папки.

### Входные данные

| Поле | Значение |
| --- | --- |
| Клиент / продукт / цель | Анна / turnover / stock |
| Выручка / расходы / срок | 4 200 000 / 2 800 000 ₽ / 12 месяцев |
| Сумма редактируемого draft | 300 000 → 400 000 ₽ |
| Другая отправляемая заявка | 300 000 ₽, тот же срок/финансовый ввод |
| Обязательный документ отправляемой заявки | BANK_STATEMENT, sample.pdf |
| ID | draftApplicationId и sentApplicationId |

### Пошаговое прохождение

| Действие; запросы | Метод и endpoint | Тело | Ответ / смысл |
| --- | --- | --- | --- |
| Анна входит; 01–04 | Вход 1.5; GET `/api/v1/me` | Профиль Анны | 200/201/200, профиль 200 |
| Сохраняет черновик; 05 | POST `/api/v1/applications` | Анкета из таблицы входа; сумма 300000 | 201, draft |
| Находит и открывает его; 06–07 | GET `/api/v1/applications?status=draft&limit=100`; GET `/api/v1/applications/{{draftApplicationId}}` | Без тела | 200; заявка в списке, доступна для правки |
| Меняет сумму; 08 | PATCH `/api/v1/applications/{{draftApplicationId}}` | `{"version":{{draftVersion}},"requestedAmountRub":400000}` | 200; новая сумма и version |
| Удаляет, повторяет и проверяет отсутствие; 09–12 | DELETE `/api/v1/applications/{{draftApplicationId}}?version={{draftVersion}}`; повтор DELETE; GET заявки; GET списка | Без тела, актуальная версия в query | 204 → 204 → 404 application_not_found → 200 без удалённой заявки |
| Создаёт другую заполненную анкету; 13 | POST `/api/v1/applications` | Та же анкета, сумма 300000 | 201; новый sentApplicationId |
| Пытается отправить без обязательного документа; 14 | POST `/api/v1/applications/{{sentApplicationId}}/submit` | Без тела | 422 validation_failed, documents.BANK_STATEMENT, required_document_missing; заявка остаётся draft |
| Добавляет выписку и отправляет; 15–16 | POST `/api/v1/applications/{{sentApplicationId}}/documents`; POST `/api/v1/applications/{{sentApplicationId}}/submit` | Multipart 1.5; затем без тела | 201 → 202; эта заявка теперь в обработке |
| Читает её и проверяет запрет удаления; 17–18 | GET заявки; DELETE `/api/v1/applications/{{sentApplicationId}}?version={{sentVersion}}` | Текущая версия | 200 → 409 invalid_status; отправленная заявка сохранена |
| Запускает помощник лимита; 19–22 | GET списка draft и служебные POST; следующий POST; служебные DELETE; контрольный GET | Синтетические новые draft, созданные только помощником | В таблице ниже |

Удаление мягкое: заявка и её документы скрыты из публичного API, но приватные
метаданные/файлы и аудит остаются. Новые обращения к удалённым документам получают 404.
Отправка и удаление одной заявки синхронизированы; удалить уже отправленную нельзя.
Для удаления чужой заявки возвращается 404. Старые черновики сверх лимита не
удаляются автоматически.

### Помощник лимита без двадцати ручных действий

Помощник встроен в четыре последних запроса папки 06; отдельный скрипт не нужен.
Внутренние `pm.sendRequest` видны в результатах Runner и Postman Console.

| Этап | Действие | Проверка |
| --- | --- | --- |
| 19 | Считает текущие draft Анны, сохраняет исходное число, создаёт только недостающие до 20 | Каждый новый POST возвращает 201; ID/версии сохраняются в limitCreatedJSON |
| 20 | Пытается создать следующий draft | 409 **draft_limit_reached**, понятное message |
| 21 | Читает 20 и удаляет по сохранённым ID только свои новые draft | DELETE204; существовавшие ранее заявки не входят в список очистки |
| 22 | Повторно читает список | Число активных draft равно исходному; свободные слоты восстановлены |

Здесь ограничение проверяется последовательными HTTP-вызовами. Защита от
конкурентного создания реализована в backend и покрыта его PostgreSQL-тестами;
эта папка не выдаёт последовательный тест за нагрузочный. Не запускайте её
параллельно с созданием draft того же пользователя в другом окне.

### Ожидаемый результат и что показать жюри

Анне не нужно завершать анкету за один раз. Черновик можно исправить и
удалить; отправленную заявку удалить нельзя. Лимит 20 удерживает количество активных
анкет, а помощник показывает 409 и освобождает только собственные тестовые слоты.

### Как будет выглядеть во frontend

Кабинет → мои заявки → черновик → редактирование → удалить с подтверждением →
список без удалённого элемента. На 409 draft_limit_reached интерфейс предлагает
удалить ненужный draft или отправить заполненную заявку. DELETE204 не разбирается
как JSON.

## 9. Сценарий №7 — Система защищает данные предпринимателей и банка

### Цель, предварительные условия и участники

Иван, другой предприниматель Анна и Мария демонстрируют разные полномочия.
**Сначала выполните папку 04 в той же коллекции.** Здесь используется её сохранённый
manualApplicationId и реальный внутренний комментарий. Основной вариант 04
завершается signed; после ручного отказа также допустим rejected.

### Входные данные

| Что используется | Значение |
| --- | --- |
| Владелец | Иван, clientAccessToken |
| Другой CLIENT | Анна, otherClientAccessToken |
| Сотрудник | Мария, bankAccessToken |
| Заявка | manualApplicationId из 04 |
| Внутренняя заметка для проверки | Синтетический маркер JURY_INTERNAL_ONLY |

### Пошаговое прохождение

| Действие; запросы | Метод и endpoint | Токен / тело | Ожидаемый ответ |
| --- | --- | --- | --- |
| Иван входит и получает свою заявку; 01–05 | Вход 1.5, GET `/api/v1/me`; GET `/api/v1/applications/{{manualApplicationId}}` | clientAccessToken | Вход 200/201/200, чтения 200 |
| Анна входит отдельно; 06–08 | GET `/api/v1/demo/profiles`, POST `/api/v1/auth/otp/request`, POST `/api/v1/auth/otp/verify` | otherClientProfileId → otherClientAccessToken | 200 → 201 → 200 |
| Анна пытается прочитать заявку Ивана; 09 | GET `/api/v1/applications/{{manualApplicationId}}` | otherClientAccessToken | **404 application_not_found** — чужая заявка не раскрывается |
| Иван пытается читать банковский assessment; 10 | GET `/api/v1/bank/applications/{{manualApplicationId}}/assessment` | clientAccessToken | **403 forbidden** — роли недостаточно |
| Мария входит и читает банковские данные; 11–13 | Банк OTP; GET того же assessment | bankAccessToken | 201 → 200 → **200**, internalComment доступен банку |
| Мария пытается изменить клиентские контакты; 14–15 | PATCH `/api/v1/me`; PATCH `/api/v1/companies/{{clientCompanyId}}` | bankAccessToken; version и тестовый email | **403 forbidden** оба раза; сотрудник не редактирует контакты |
| Иван читает публичный результат; 16 | GET `/api/v1/applications/{{manualApplicationId}}/decision` | clientAccessToken | **200**, customerExplanation есть, internalComment и его текста нет |
| Посетитель без входа читает результат; 17 | GET того же decision | Без Authorization | **401 unauthorized** |

Пример запрещённого контакта от имени банка:

```json
{"version":1,"contact":{"email":"must-not-save@example.test"}}
```

Роль проверяется до записи; это письмо никуда не сохраняется. Ожидаемые
401/403/404 в этой папке означают успешную проверку защиты, а не поломку показа.

### Ожидаемый результат и что показать жюри

CLIENT видит только собственные заявки своей компании. BANK_EMPLOYEE видит
отправленные заявки и внутренние оценки, но не может редактировать сведения
предпринимателя. Клиент получает публичное объяснение без внутренних заметок.
Покажите один 404 между клиентами, один 403 на банковском API и различие двух ответов.

### Как будет выглядеть во frontend

Раздельные кабинеты, отсутствие банковских кнопок у CLIENT, отдельный вход
сотрудника. Серверные проверки действуют и при прямом HTTP-запросе; скрытие кнопки
в React не заменяет авторизацию backend.

## 10. Как читать assessment и финансовые числа

| Поле ответа bank assessment | Значение для жюри |
| --- | --- |
| companyCheck / creditHistory | Сохранённые симуляторы предварительных проверок и БКИ |
| scoring.score / assessedAt | Балл реального Python и время сохранённой оценки |
| scoring.decision / approvedAmountRub / reasons | Рекомендация Python; не окончательное решение банка |
| scoring.factors | Шесть факторов, которые вернула модель; snake_case имена сохранены |
| goDecision | Автоматический вывод Go: decision, score, riskLevel, reasons, assessedAt |
| riskLevel / manualReviewReasons / recommendations | Существующие поля банковского представления; рекомендации здесь не нужно путать со scoring.decision |
| manualDecision | Решение сотрудника, два вида комментариев, автор и время; до ручного решения null |
| processing | Техническое состояние обработки, пока результат не готов |

`GET assessment` не запускает scoring повторно. Пока результатов нет, scoring,
goDecision и другие блоки могут быть null. `debt_service_share` и
`existing_debt_to_annual_revenue` по контракту Python допускают null, например при
нулевом знаменателе; не подменяйте отсутствие нулём. Дата assessedAt динамическая.

| Фактор Python | Понятное объяснение |
| --- | --- |
| operating_cash_flow | Выручка минус операционные расходы за месяц |
| operating_margin | Доля операционного потока в выручке; 0.12 — 12% |
| estimated_new_monthly_payment | Оценочный новый платёж Python; это не offer.monthlyPaymentRub Go |
| debt_service_share | Доля текущих и оценочного нового платежей в операционном потоке |
| existing_debt_to_annual_revenue | Существующий долг относительно годовой выручки |
| overdue_count | Количество просрочек из синтетической кредитной истории |

Python использует иллюстративные 20%, Go — ставку снимка выбранного продукта.
Формула Go: `P = ceil(A × r / (1−(1+r)^−n))`, где A — сумма, r — годовая ставка
в процентах /1200, n — число месяцев. При нулевой ставке P=ceil(A/n).
Бюджет: `max(0, floor(max(0,revenue−expenses)/2)−existingMonthlyPayments)`.
Точная целочисленная/рациональная математика находится в backend; в руководстве
приведены фактические ответы, не альтернативный расчёт условий.

Score больше — риск ниже: LOW 80–100, MEDIUM 50–79, HIGH 0–49. Высокий score не
отменяет финансовые ограничения, документы и регистрационные проверки.

## 11. Повторы, остановка показа и устранение проблем

### Повторное прохождение

Основные папки каждый раз выполняйте **с первого запроса**. Они создают новые UUID
заявок, документов, ключей действий и обновляют свои переменные. Отправленные
заявки предыдущих показов сохраняются в истории и не занимают лимит draft.
В папке 04 другое решение требует новой заявки; прежнее решение не изменяется.

Сетевой повтор accept/manual decision внутри одного прохождения использует
исходные key/body. Самостоятельная подмена version или текста с тем же ключом
даёт 409 idempotency_conflict. Это защита от повторного/неоднозначного действия.

Папка 01 восстанавливает прежний дополнительный email, папка 05 — исходные контакты,
папка 06 — только свои вспомогательные draft. Счётчики версий и аудит после этого
сохраняются. Параллельно показывать контактные изменения одной сущности не нужно.

Если прогон оборвался до восстановления контактов/очистки, не удаляйте все данные.
Используйте сохранённые исходные значения, limitCreatedJSON и собственные ID для
точечного завершения работы либо начните показ в **новом project**. Полный обычный
повтор папок проверен; прерванный прогон не объявляется полностью очищенным.

### Что делать при ошибках

| Симптом | Причина / действие |
| --- | --- |
| Первый запрос не соединяется | Проверьте ps, /ready, порт и baseUrl; в Docker Newman используйте backend:HTTP_PORT, не localhost контейнера |
| Migrate сообщает неизвестную/более новую версию | Подключён volume другого checkout. Создайте отдельный project, старую базу не откатывайте |
| Upload400 вместо 201, затем documentId пуст | Postman не прочитал fixtures/sample.pdf. Исправьте Working directory и перезапустите папку с начала |
| Submit422 documents.BANK_STATEMENT | У этого профиля выписка обязательна; загрузите File, не текст с именем файла |
| Submit400 | Уберите тело, включая пустой JSON-объект; submit принимает запрос без тела |
| Polling остаётся processing, затем ошибка проверки | Проверьте Python/SCORING_MODEL, сеть и логи. Не объявляйте техническую ошибку отказом банка |
| OTP409/410 | Challenge использован/заменён или истёк; начните вход заново, не запускайте OTP одного пользователя параллельно |
| PATCH/DELETE409 version_conflict | Перечитайте нужную сущность; не путайте версии пользователя, компании и заявки |
| POST applications409 draft_limit_reached | Активные draft уже заняли 20 слотов; удаляйте только свои ненужные draft либо используйте чистую demo-базу |
| Сотрудник получает 404 для draft | Так предусмотрена приватность: банк видит заявку после submit |
| Папка 07 требует manualApplicationId | Выполните папку 04 в той же коллекции; токены и ID заполняются автоматически |
| Изменились score/предложенная сумма | Сверьте артефакт модели, seed, продукт и debt history; не меняйте политику под ожидаемый ответ |

Логи своего окружения:

```sh
docker compose -p "$JURY_PROJECT" --env-file /dev/null \
  -f compose.yaml -f compose.scoring.yaml logs --tail 80 backend scoring migrate
```

## 12. Перенос демонстрационных сценариев в React

Go не содержит настройки CORS для произвольного frontend-origin. В разработке
нужен proxy `/api` на backend либо единый origin через reverse proxy. Токены
двух ролей и их экраны не смешиваются. Полные DTO и права:
[OpenAPI](../api/openapi.yaml), [frontend-интеграция](../api/FRONTEND_INTEGRATION.md).
Новых API для этого маршрута не требуется.

| Сценарий | Экраны frontend | Используемые API | Ожидаемый результат |
| --- | --- | --- | --- |
| 01 Автоодобрение | Вход → кабинет → профиль/компания → продукт → анкета → документы → обработка → одобрение → принятие | profiles, otp/request+verify, me, companies/{id}, credit-products, recommendations, applications POST/PATCH, documents, submit, decision, timeline, accept | approved → signed, понятные условия и платёж |
| 02 Автоотказ | Вход → компания → продукт → новая анкета → документы → ожидание → отказ/история | Те же клиентские чтения и подача; decision/timeline; assessment только банку | rejected с причинами, без offer/кнопки принятия |
| 03 Контроффер | Вход → анкета → обработка → сравнение сумм → условия → принятие | Подача, decision, timeline, accept; assessment в банковском интерфейсе | counter_offer → signed, сумма меньше запроса |
| 04 Ручное рассмотрение | Вход Ивана → анкета → ожидание; отдельный вход Марии → очередь → карточка/документы → scoring → ручное решение; возврат Ивана → объяснение/принятие | bank/applications, bank detail/assessment/decisions; client decision/timeline/accept; общие документы с ролью | manual_review → ручной исход → при предложении signed |
| 05 Контакты | Вход → профиль/компания → форма контактов → сохранение → обработка конфликта версии | GET/PATCH me, GET/PATCH companies/{id} | Только разрешённые контакты сохранены, юридические поля readonly |
| 06 Черновики | Вход → список → редактирование → подтверждение удаления → список; уведомление о лимите | applications GET/POST/PATCH/DELETE?version=N; documents/submit для отправки | draft удалён; отправленную заявку удалить нельзя; лимит 409 понятен |
| 07 Доступ | Раздельные кабинеты → собственные данные → безопасное сообщение о недоступном ресурсе | Все вызовы с правильной ролью; серверные 401/403/404 | Чужие данные и внутренние заметки не раскрываются |

В React нужны polling до конечного состояния, отображение nullable результатов,
сохранение актуальных версий, неизменный Idempotency-Key на сетевой повтор и
обработка 204 без JSON. Показывайте реальные availableActions и безопасные reasons.
В клиентский интерфейс не переносите банковские factors/internalComment.

## 13. Что проверено при подготовке этого руководства

Проверки выполнены на новой отдельной базе с миграциями 12 и реальном Python
`logreg_v1` с указанным SHA-256. Пройдены все семь папок; основной путь выполнен
дважды подряд. Дополнительно папка 04 выполнена с manualOutcome=rejected и
manualOutcome=counter_offer, каждый раз с новой заявкой.

| Проверка | Наблюдавшийся результат |
| --- | --- |
| Полная новая demo-коллекция, 2 итерации | 353 HTTP-вызова, 1246 assertions, 0 failures |
| Дополнительные ручные отказ и контроффер | 55 HTTP-вызовов, 246 assertions, 0 failures |
| Автоматические исходы | approved97, rejected35, counter_offer96, manual_review78 |
| Ручное одобрение / отказ / контроффер | Все прошли реальную кредитную политику; предложения приняты там, где они есть |
| Приватность | В клиентских decision/card/timeline нет внутренней заметки; в bank assessment она есть |
| Контакты и лимит | Сохранение/восстановление контактов, защита реквизитов, 20 draft/409 и точечная очистка прошли |
| Сценарии и права | Пути/методы сопоставлены с OpenAPI; CLIENT/BANK_EMPLOYEE и требуемые документы проверены HTTP |

Фактическое количество HTTP-вызовов меняется из-за polling и помощника лимита.
Предложения и score не подменялись, бизнес-логика и seed не изменялись. Реальные
JWT в документ и исходную коллекцию не включены. Основная регрессионная коллекция,
Go, Python scoring, frontend и исторические миграции сохранены.

Postman Desktop как графический интерфейс отдельно не автоматизировался: выполнение
скриптов и файловых запросов проверено Newman. Рабочая папка Desktop всё равно
должна быть настроена локально. Для фактических интеграций ФНС/БКИ, OCR, выдачи
денег и юридического подписания функционала нет — это ограничения самого проекта,
а не скрытые шаги демонстрации.
