# Frontend Integration — API Freeze v1

Контракт Go backend кредитного конвейера «Альфа-Будущее».
Источник схем и HTTP-кодов: [openapi.yaml](openapi.yaml), версия API `1.0.0`.
Публичный префикс — `/api/v1`, локальный адрес — `http://localhost:8080`.
Python-прототип не является контрактом React-приложения.

**Черновики приватны для предпринимателя. Банк не видит их ни в списках,
ни по UUID, ни через документы. Банковская видимость начинается после
успешного submit.** Предприниматель может хранить несколько черновиков,
в том числе одного продукта; ограничений их количества в MVP нет.
Отправка выбранного черновика не отправляет и не удаляет остальные.

## Доступность операций

`IMPLEMENTED` — можно подключать к работающему Go backend сейчас.
`PLANNED` — согласованный контракт; обработчик ещё отсутствует, frontend
использует mock. Наличие операции в OpenAPI не означает её реализацию.
В спецификации запланированные операции имеют
`x-implementation-status: planned`. Сейчас это 19 реализованных и 7
запланированных операций.

| Метод | Путь | Доступ | Статус |
| --- | --- | --- | --- |
| GET | `/health` | Публичный | IMPLEMENTED |
| GET | `/ready` | Публичный | IMPLEMENTED |
| GET | `/api/v1/demo/profiles` | Публичный | IMPLEMENTED |
| POST | `/api/v1/auth/otp/request` | Публичный | IMPLEMENTED |
| POST | `/api/v1/auth/otp/verify` | Публичный | IMPLEMENTED |
| GET | `/api/v1/me` | CLIENT, BANK_EMPLOYEE | IMPLEMENTED |
| GET | `/api/v1/companies/{companyId}` | CLIENT, своя компания | IMPLEMENTED |
| GET | `/api/v1/credit-products` | CLIENT, BANK_EMPLOYEE | IMPLEMENTED |
| GET | `/api/v1/credit-products/{productId}` | CLIENT, BANK_EMPLOYEE | IMPLEMENTED |
| POST | `/api/v1/recommendations` | CLIENT | IMPLEMENTED |
| POST | `/api/v1/applications` | CLIENT | IMPLEMENTED |
| GET | `/api/v1/applications` | CLIENT, свои заявки | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}` | CLIENT, своя заявка | IMPLEMENTED |
| PATCH | `/api/v1/applications/{applicationId}` | CLIENT, свой draft | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/timeline` | CLIENT, своя заявка | IMPLEMENTED |
| POST | `/api/v1/applications/{applicationId}/documents` | CLIENT, свой draft | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/documents` | CLIENT — свои; BANK_EMPLOYEE — только отправленные | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/documents/{documentId}` | CLIENT — свои; BANK_EMPLOYEE — только отправленные | IMPLEMENTED |
| POST | `/api/v1/applications/{applicationId}/submit` | CLIENT, своя заявка | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/decision` | CLIENT, своя заявка | PLANNED |
| POST | `/api/v1/applications/{applicationId}/accept` | CLIENT, своя заявка | PLANNED |
| POST | `/api/v1/applications/{applicationId}/decline` | CLIENT, своя заявка | PLANNED |
| GET | `/api/v1/bank/applications` | BANK_EMPLOYEE | PLANNED |
| GET | `/api/v1/bank/applications/{applicationId}` | BANK_EMPLOYEE | PLANNED |
| GET | `/api/v1/bank/applications/{applicationId}/assessment` | BANK_EMPLOYEE | PLANNED |
| POST | `/api/v1/bank/applications/{applicationId}/decisions` | BANK_EMPLOYEE, manual_review | PLANNED |

## Запуск и подключение

Из каталога `smart-credit-backend`:

```sh
docker compose up -d --build
curl --fail http://localhost:8080/ready
```

`.env` не обязателен. PostgreSQL, миграции, синтетические профили и сохранённый
демонстрационный JWT-секрет подготавливаются автоматически. Готовность
контейнеров проверяйте через `docker compose ps`; первый запуск асинхронный.

Эта команда запускает backend и PostgreSQL. Отдельный Python scoring пока не
поставляется с репозиторием: проверенного Docker-образа или доступного исходника
для воспроизводимой сборки здесь нет. Рабочий Compose не содержит заглушку.
Для сквозной обработки нужен реальный сервис по `SCORING_URL`; по умолчанию
Compose использует внутренний адрес `http://scoring:8001`, зарезервированный для
его поставки. `SCORING_TIMEOUT` по умолчанию `5s`, допустим положительный таймаут
до `1m`. Проверки через httptest/Newman
с тестовым HTTP-сервисом подтверждают локальный контракт, но не совместимость
с реальным Python scoring.

Для браузерного frontend используйте общий origin или proxy dev-сервера,
направляющий `/api` на `http://localhost:8080`. Go backend сейчас не настраивает
CORS для отдельного browser origin. Это не мешает Postman, Newman и curl.
Frontend не вызывает Python scoring напрямую.

## Авторизация

1. `GET /api/v1/demo/profiles` возвращает `{items: [...]}`: три CLIENT и один
   BANK_EMPLOYEE. Профиль: `id`, `name`, `phone`, `role`, `companyName`,
   `bankRelationship`. У сотрудника `companyName=null` и
   `bankRelationship=null`. У первой учебной компании `alfa_account`, у двух
   других `external_account`.
2. `POST /api/v1/auth/otp/request` с `{"profileId":"UUID"}` возвращает `201`:
   `otpRequestId`, `expiresAt`, `maskedPhone`.
3. `POST /api/v1/auth/otp/verify` с `{"otpRequestId":"UUID","code":"1234"}`
   возвращает `200`: `accessToken`, `tokenType:"Bearer"`, `expiresAt`,
   `userId`, `role`, `companyId`.
4. Защищённые запросы передают `Authorization: Bearer <accessToken>`.
   `GET /api/v1/me` возвращает `id`, `name`, `phone`, `role`, `companyId`.

OTP демонстрационный: по умолчанию `1234`, задаётся `DEMO_OTP_CODE` на backend.
SMS не отправляется. Код действует 5 минут, максимум 5 попыток; правильная
пятая попытка допустима. Новый challenge отменяет предыдущие активные.
Неверный код — `400 invalid_otp`, использованный — `409 otp_used`, заменённый —
`409 otp_invalidated`, истёкший — `410 otp_expired`, исчерпание попыток —
`429 otp_attempts_exceeded`.

JWT действует 8 часов. Backend проверяет HS256, подпись, срок, issuer, audience
и subject. Роль и компания проверяются по PostgreSQL, не передаются клиентом
в запросе и не предполагаются отдельными JWT claims. У BANK_EMPLOYEE
`companyId=null`. Refresh/logout API сейчас нет. При `401` повторите вход.
JWT не включайте в URL, логи, Git и экспортируемые коллекции.

## Форматы данных

- UUID используются для пользователей, компаний, заявок, документов и предложений.
- `productId` — публичный код `turnover` или `investment`, не UUID БД.
- Денежные значения — JSON integer в целых рублях; не строки и не дробные суммы.
- `annualRatePercent` — JSON number: `22` означает 22% годовых.
- Время — RFC 3339 UTC, например `2026-10-24T10:00:00Z`;
  `registrationDate` — `YYYY-MM-DD`.
- Неизвестное nullable-значение — `null`, пустая коллекция — `[]`.
- `version` заявки — положительное целое для optimistic locking. Это не версия
  продукта, предложения или строки кредитного решения.
- JSON-запросы требуют `Content-Type: application/json`; неизвестные поля
  запрещены, предел тела — 64 KiB. Submit имеет отдельное правило без тела.

## Компания, продукты и рекомендации

`companyId` берите из verify или `/me`, затем вызывайте
`GET /api/v1/companies/{companyId}`. Чужая компания и BANK_EMPLOYEE получают `403`.
Продукты доступны обеим ролям; список возвращает `{items: [...]}`.

`Company.bankRelationship` повторяет признак из тестового профиля:
`alfa_account` означает учебный счёт в Альфа-Банке, `external_account` —
отсутствие такого счёта в этом прототипе. Это отдельное свойство от доверенного
источника оборотов. У компаний `external_account` источник в seed остаётся
`unverified`; финансовые значения в `Company.financials` — синтетический профиль,
а не автоматически полученные банком обороты. Для такого пути frontend должен
предлагать проверить и ввести выручку/расходы, не подписывая их как банковские
данные. Оба пути сохраняют черновик и отправляют заявку через те же маршруты.
Для оборотного кредита внешнему клиенту backend запросит выписку при submit;
надёжным источником требований остаётся ответ `422 details`. Произвольную новую
компанию создать через текущий API нельзя: вход доступен лишь seed-профилям.

Предварительные рекомендации:

```json
{
  "purpose": "stock",
  "requestedAmountRub": 1000000,
  "termMonths": 24,
  "monthlyRevenueRub": 2400000,
  "monthlyExpensesRub": 1500000
}
```

Ответ `/recommendations` содержит `products`, `recommendedProductId`,
`paymentBudgetRub`, объяснения и ограничения. Это предварительный подбор,
не кредитное решение и не действующее предложение для accept.

## Несколько приватных черновиков

Каждый `POST /api/v1/applications` создаёт отдельный draft. Разрешено пустое `{}`
или частичное заполнение; повторное создание не обновляет предыдущий черновик.

```json
{
  "productId": "turnover",
  "purpose": "stock",
  "requestedAmountRub": 1000000,
  "termMonths": 24,
  "monthlyRevenueRub": 2400000,
  "monthlyExpensesRub": 1500000
}
```

Сохраните `applicationId` и `version` из `201 Application`. Для редактирования
используйте PATCH выбранного UUID:

```json
{
  "version": 1,
  "requestedAmountRub": 900000,
  "purposeDescription": null
}
```

Отсутствующее поле не меняется, `null` очищает его. PATCH требует хотя бы одно
редактируемое поле. Успех возвращает новую версию. При `409 version_conflict`
перечитайте заявку; не повторяйте изменение молча поверх чужой версии.

`GET /api/v1/applications?status=draft&limit=20&offset=0` показывает только
черновики текущего владельца и компании. Несколько записей одного продукта
показываются отдельными карточками. Лимит по умолчанию 20, максимум 100;
offset от 0 до 2147483647. Один необязательный status, неизвестные и повторные
query-параметры дают `400`. Ответ: `{items,total,limit,offset}`;
порядок `createdAt DESC, applicationId DESC`.

Для клиента чужая заявка и чужие документы возвращают `404`, в том числе если
UUID известен. Сотрудник не может использовать клиентские create/list/get/
patch/timeline/submit маршруты: другая роль получает `403`.

**Известное несоответствие:** существующий `Application.availableActions`
содержит только `["edit"]` для draft и `[]` для остальных состояний. Submit уже
реализован, но пока не входит в этот массив. Не считайте массив полным перечнем
доступных действий; его расширение — отдельная задача backend.

## Документы

Загрузка разрешена CLIENT только в собственный draft:

```js
const data = new FormData();
data.append("documentType", "BANK_STATEMENT");
data.append("file", selectedFile);
const response = await fetch(`/api/v1/applications/${applicationId}/documents`, {
  method: "POST",
  headers: { Authorization: `Bearer ${accessToken}` },
  body: data,
});
const document = await response.json(); // при успехе HTTP 201
```

Не задавайте Content-Type вручную: браузер сформирует multipart boundary.
Ровно два поля: `documentType` и `file`. Типы: `BANK_STATEMENT`,
`EQUIPMENT_INVOICE`. Форматы: PDF/PNG/JPEG с соответствующим расширением;
непустой файл максимум 5242880 байт, весь запрос максимум 5308416 байт.
Несколько документов одного типа допустимы. API удаления/замены нет.

Метаданные: `documentId`, `applicationId`, `documentType`, `fileName`,
`contentType`, `sizeBytes`, `uploadedAt`. Список возвращает `{items: [...]}`.
Скачивание возвращает binary attachment; используйте fetch с JWT и `blob()`,
а не прямую ссылку без Authorization и не токен в query.

CLIENT читает документы собственных draft и отправленных заявок.
BANK_EMPLOYEE получает `404 application_not_found` при list/download draft,
даже с действительными UUID заявки и документа. После успешного submit
банк может читать документы. Банк не загружает документы от имени клиента.

## Submit и текущая обработка

```js
const response = await fetch(`/api/v1/applications/${applicationId}/submit`, {
  method: "POST",
  headers: { Authorization: `Bearer ${accessToken}` },
}); // без body: даже {} и пробельное тело запрещены
const submitted = await response.json();
```

Успех — `202`:

```json
{
  "applicationId": "40000000-0000-4000-8000-000000000001",
  "status": "submitted",
  "submittedAt": "2026-10-24T10:00:00Z",
  "nextStage": "prechecks"
}
```

Повтор в submitted/processing безопасен: `202`, тот же submittedAt, без второй
задачи и повторного события. Ответ может уже содержать status processing и
nextStage awaiting_scoring. Это зафиксированный укрупнённый индикатор завершённых
предварительных проверок: он сохраняется во время scoring и после сохранения его
результата. Он не подтверждает наличие результата scoring. Idempotency-Key для
существующего submit не нужен.
Другие состояния дают `409`. Остальные черновики не меняются.

Для submit нужны все поля кроме purposeDescription; для purpose=other описание
обязательно. Проверяются границы продукта, возраст компании и обязательные
готовые документы. Для инвестиционного продукта нужен EQUIPMENT_INVOICE;
необходимость выписки зависит от доверенного источника финансовых данных.
Этот внутренний признак не опубликован в Company: ориентируйтесь на `422 details`,
а не пытайтесь вывести его из имени тестового профиля.

Worker выполняет синтетические проверки компании/кредитной истории, затем
вызывает отдельный scoring по HTTP из того же существующего механизма jobs.
После строгой проверки ответ сохраняется в PostgreSQL; задача остаётся в
awaiting_decision, заявка — в processing. Окончательное кредитное решение
на этом этапе не создаётся. При недоступности scoring выполняются ограниченные
повторы; техническая ошибка не превращается в rejected.

Опрос текущего Application и timeline доступен, но подробная стадия, результаты
scoring и исчерпание повторов через публичный API пока не выдаются.
В timeline порядок задаётся sequence; actorId=null у системных событий.
SCORING_STARTED означает начало обработки scoring, SCORING_COMPLETED — надёжное
сохранение результата, а не кредитное решение. Каждое событие появляется один
раз; попытки HTTP-вызова могут повторяться при сбое. Финансовые данные, score,
factors и технические причины ошибок в timeline не публикуются.

## Решение и предложение — PLANNED

GET `/api/v1/applications/{applicationId}/decision` будет возвращать `200` для
любого состояния собственной заявки, включая draft и отсутствие решения.
До реализации этого маршрута используйте mock, а не ожидайте такой JSON
от существующего GET application.

Поля CreditDecision: applicationId, status, version, decision, score, riskLevel,
reasons, assessedAt, reviewedBy, offer, processing. decision имеет значения
approved/counter_offer/rejected/null. score — integer 0..100/null; riskLevel —
LOW/MEDIUM/HIGH/null. Пороги и направленность шкалы в этом freeze не определяются.
assessedAt — время финального решения либо null; reviewedBy — UUID сотрудника
для ручного решения, иначе null. reasons — безопасные пояснения клиенту.

Ожидание:

```json
{
  "applicationId": "40000000-0000-4000-8000-000000000001",
  "status": "processing",
  "version": 3,
  "decision": null,
  "score": null,
  "riskLevel": null,
  "reasons": [],
  "assessedAt": null,
  "reviewedBy": null,
  "offer": null,
  "processing": {
    "stage": "scoring",
    "state": "waiting",
    "retryAt": null,
    "message": "Ожидается результат оценки."
  }
}
```

processing.stage: prechecks/scoring/decision; state: waiting/running/retrying/
failed. retryAt и message nullable. Техническая ошибка не превращается в rejected:
сохраняется submitted/processing и безопасная диагностика. Ручного retry API нет.

Ручное рассмотрение; числовой score и MEDIUM здесь только пример формата:

```json
{
  "applicationId": "40000000-0000-4000-8000-000000000001",
  "status": "manual_review",
  "version": 7,
  "decision": null,
  "score": 62,
  "riskLevel": "MEDIUM",
  "reasons": [],
  "assessedAt": null,
  "reviewedBy": null,
  "offer": null,
  "processing": null
}
```

Альтернативное предложение:

```json
{
  "applicationId": "40000000-0000-4000-8000-000000000001",
  "status": "counter_offer",
  "version": 8,
  "decision": "counter_offer",
  "score": 62,
  "riskLevel": "MEDIUM",
  "reasons": ["Предложена меньшая сумма кредита."],
  "assessedAt": "2026-10-24T10:05:00Z",
  "reviewedBy": "20000000-0000-4000-8000-000000000004",
  "offer": {
    "offerId": "60000000-0000-4000-8000-000000000001",
    "applicationId": "40000000-0000-4000-8000-000000000001",
    "productId": "turnover",
    "amountRub": 800000,
    "termMonths": 24,
    "annualRatePercent": 22,
    "monthlyPaymentRub": 41503,
    "expiresAt": null,
    "status": "active"
  },
  "processing": null
}
```

Для approved форма та же: decision/status=approved и предложение с исходными
суммой и сроком. Для rejected: decision/status=rejected, offer=null,
reasons с пояснениями. Все финальные варианты имеют processing=null.
В MVP expiresAt=null; статусы предложения — active/accepted/withdrawn.
Решение принимает Go policy; decision и approved_amount Python scoring
являются рекомендацией. Сырые scoring factors клиенту не передаются.

## Accept / decline — PLANNED

Оба POST принимают `{ "version": 8, "offerId": "UUID" }` и обязательный
Idempotency-Key. Только CLIENT-владелец в approved/counter_offer может выполнить
действие с актуальным активным предложением. Ответ — `200 CreditDecision`.

- Accept: заявка signed, предложение accepted.
- Decline: заявка closed, предложение withdrawn. decision остаётся прежним;
  клиентский отказ не является банковским rejected.
- signed — демонстрационное подтверждение условий, без выдачи денег
  и юридически значимого банковского договора.
- Повтор того же подтверждённого действия с тем же предложением — `200`,
  без повторного события и изменения версии, даже после истечения ключа.
- Противоположное действие после завершения, старое предложение либо
  конфликт версии — `409`. При гонке accept/decline выполняется одно действие.

## Банковский кабинет — PLANNED

Только BANK_EMPLOYEE. Список `/api/v1/bank/applications` принимает status,
companyId, limit и offset. Правила limit/offset и сортировка совпадают с
клиентским списком. Без status возвращаются все восемь банковских состояний;
status=draft — `400 invalid_request`. Неизвестные/повторные параметры — `400`.
Ответ `{items: BankApplication[],total,limit,offset}`.

BankApplication: applicationId, company:{id,name,inn}, product:{id,name},
requestedAmountRub, termMonths, status, version, createdAt, updatedAt,
submittedAt, manualReviewRequired, availableActions. Поля отправленной заявки
заполнены; manualReviewRequired=true и availableActions=["decide"] только
в manual_review, иначе false/[] соответственно.

GET `/bank/applications/{applicationId}` возвращает BankApplicationDetail:
поля ApplicationDetail, productTerms с полным CreditProduct и
manualReviewRequired; availableActions содержит банковские действия.
Сведения компании и условия продукта берутся из снимков submit.
Существующий клиентский ApplicationDetail.company содержит текущую компанию;
не используйте его как неизменяемый снимок для банковской оценки.

GET `/bank/applications/{applicationId}/assessment` возвращает applicationId,
status, version, companyCheck, creditHistory, scoring, riskLevel,
manualReviewReasons, recommendations, manualDecision, processing.

- companyCheck: status, checks:[{code,passed}], problems:[{code,message}],
  checkedAt, durationMs, simulation. creditHistory: existingDebtRub,
  existingMonthlyPaymentsRub, overdueCount, checkedAt, simulation.
- scoring: {score,assessedAt} либо null. riskLevel отдельно nullable.
- manualReviewReasons: [{code,message}]. recommendations:
  [{decision,reasons,offer:{amountRub,termMonths}|null}]. Это рекомендации,
  не действующие предложения; offerId отсутствует.
- manualDecision: {decision,comment,reviewedBy,reviewedAt} либо null.
  Комментарий сотрудника внутренний; клиент получает безопасные reasons.
- Неполученные результаты — null, массивы без элементов — [].

Любой draft в банковском get/assessment/decisions возвращает
`404 application_not_found`. Ограничения проверяет backend, скрытие карточки
на frontend не заменяет авторизацию. Сотрудник не редактирует клиентский draft.

Ручное решение — POST `/api/v1/bank/applications/{applicationId}/decisions`:

```json
{
  "version": 7,
  "decision": "counter_offer",
  "comment": "Предлагаю уменьшить сумму до 800 тысяч рублей.",
  "offer": {"amountRub": 800000, "termMonths": 24}
}
```

Только manual_review, обязателен Idempotency-Key. Комментарий после trim —
1..1000 символов. Для approved offer обязателен и повторяет исходные сумму/срок.
Для counter_offer сумма не больше исходной и находится в границах продукта;
срок также в границах продукта, хотя бы одно условие отличается. Продукт и
ставка фиксированы, платёж рассчитывает backend. Для rejected offer=null.
Некорректные условия — `422`; иной доступный банковский статус — `409`.
Успех — `201 CreditDecision`; reviewedBy определяется backend по JWT-пользователю.

## Жизненный цикл

| Из | В | Инициатор | Готовность |
| --- | --- | --- | --- |
| draft | submitted | CLIENT, submit выбранного черновика | IMPLEMENTED |
| submitted | processing | Worker | IMPLEMENTED |
| processing | approved / counter_offer / rejected / manual_review | Go policy | PLANNED |
| manual_review | approved / counter_offer / rejected | BANK_EMPLOYEE | PLANNED |
| approved / counter_offer | signed | CLIENT, accept | PLANNED |
| approved / counter_offer | closed | CLIENT, decline | PLANNED |

draft — приватное редактирование; submitted — заявка отправлена; processing —
автоматические проверки; manual_review — ожидание сотрудника; approved — исходные
условия одобрены; counter_offer — предложена альтернатива; rejected — отказ банка;
signed — условия демонстрационно подтверждены; closed — предложение отклонено
клиентом. rejected/signed/closed конечны для MVP. Другие переходы запрещены.
awaiting_scoring и awaiting_decision — внутренние состояния job, не статусы заявки.

## Идемпотентность новых POST

Для accept, decline и банковского decisions обязателен Idempotency-Key в формате
UUID. Frontend создаёт ключ один раз на логическое действие, сохраняет вместе
с точным телом до получения ответа и использует снова после сетевой ошибки.
Исправленное пользователем действие получает новый ключ.

Ключ хранится 24 часа в области пользователя, операции и applicationId.
Одинаковые ключ/тело возвращают исходные HTTP-код и JSON; другое тело с тем же
ключом — `409 idempotency_conflict`. Проверка доступа предшествует replay.
После TTL состояние, версия, транзакции и журнал действий по-прежнему исключают
повторный эффект. Повтор accept/decline того же выполненного действия возвращает
200; повтор manual decision с новым/истёкшим ключом после выхода из manual_review
возвращает 409 и не создаёт второе решение. После replay перечитайте актуальное
состояние при необходимости: сохранённый ответ отражает исходную операцию.

```js
const key = crypto.randomUUID();
const body = JSON.stringify({ version: decision.version, offerId: decision.offer.offerId });
// Сохраните key и body; при сетевом повторе используйте именно эту пару.
const response = await fetch(`/api/v1/applications/${decision.applicationId}/accept`, {
  method: "POST",
  headers: {
    Authorization: `Bearer ${accessToken}`,
    "Content-Type": "application/json",
    "Idempotency-Key": key,
  },
  body,
}); // PLANNED: выполняйте против mock до появления обработчика
```

## Ошибки

Основной формат:

```json
{"error":"application_not_found","message":"Заявка не найдена."}
```

Submit и будущая валидация ручного решения могут возвращать `422`:

```json
{
  "error": "validation_failed",
  "message": "Проверьте обязательные поля и документы заявки.",
  "details": [
    {
      "field": "documents.EQUIPMENT_INVOICE",
      "code": "required_document_missing",
      "message": "Загрузите обязательный документ и дождитесь завершения загрузки."
    }
  ]
}
```

| HTTP | Обработка frontend |
| --- | --- |
| 400 | invalid_request; проверьте тело/query. Для OTP возможен invalid_otp |
| 401 | unauthorized; повторный вход |
| 403 | forbidden; роль/доступ к компании |
| 404 | application_not_found/document_not_found/product_not_found; не раскрывает чужие заявки и draft банку |
| 409 | version_conflict, invalid_status; planned: offer_not_active, idempotency_conflict; также ошибки использованного/заменённого OTP |
| 410 | otp_expired |
| 413 | request_too_large |
| 415 | unsupported_media_type/unsupported_file_type |
| 422 | validation_failed с details |
| 429 | otp_attempts_exceeded |
| 500 | internal_error; внутренние подробности не выдаются |
| 503 | service_unavailable; допускается сетевой повтор с сохранённым ключом для новых POST |

Ориентируйтесь на HTTP-код и error, не на текст message. /ready имеет отдельный
ответ 503: `{"status":"not_ready","error":"database_unavailable"}`.

## Пример работающего сценария без ручного переноса JWT и UUID

Требуются curl и jq. Профиль выбирается из API; все идентификаторы передаются
переменными. Этот пример сохраняет два draft одного продукта и меняет только один.

```sh
BASE_URL=http://localhost:8080
profile_id=$(curl --fail --silent "$BASE_URL/api/v1/demo/profiles" |
  jq -r '[.items[] | select(.role == "CLIENT")][0].id')
otp_id=$(curl --fail --silent "$BASE_URL/api/v1/auth/otp/request" \
  -H 'Content-Type: application/json' \
  --data "$(jq -nc --arg id "$profile_id" '{profileId:$id}')" |
  jq -r .otpRequestId)
session=$(curl --fail --silent "$BASE_URL/api/v1/auth/otp/verify" \
  -H 'Content-Type: application/json' \
  --data "$(jq -nc --arg id "$otp_id" '{otpRequestId:$id,code:"1234"}')")
token=$(printf '%s' "$session" | jq -r .accessToken)
curl --fail --silent "$BASE_URL/api/v1/me" -H "Authorization: Bearer $token"
first=$(curl --fail --silent "$BASE_URL/api/v1/applications" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  --data '{"productId":"turnover"}')
second=$(curl --fail --silent "$BASE_URL/api/v1/applications" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  --data '{"productId":"turnover"}')
application_id=$(printf '%s' "$first" | jq -r .applicationId)
version=$(printf '%s' "$first" | jq .version)
curl --fail --silent -X PATCH "$BASE_URL/api/v1/applications/$application_id" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  --data "$(jq -nc --argjson version "$version" \
    '{version:$version,purpose:"stock",requestedAmountRub:1000000,termMonths:24,monthlyRevenueRub:2400000,monthlyExpensesRub:1500000}')"
curl --fail --silent "$BASE_URL/api/v1/applications?status=draft" \
  -H "Authorization: Bearer $token"
```

Для полноценного сценария с документами и submit используйте существующую
[Postman Collection](postman/smart-credit-stage2-postman-collection.json).
Запустите коллекцию сверху вниз в Collection Runner. Working directory для
файлов — `api/postman`. Профили, JWT, UUID, версии и проверки передаются
автоматически; коллекция не вызывает PLANNED маршруты и не обращается к scoring
напрямую. По умолчанию expectScoringResult=false: проверяются prechecks и
сохранение processing, успешный scoring при этом не заявляется. Только при
доступном совместимом сервисе задайте expectScoringResult=true: коллекция
ограниченно опрашивает timeline до SCORING_COMPLETED (до 30 секунд на заявку),
проверяет историю и отсутствие повторных событий после submit. Содержимое
сохранённого результата проверяется PostgreSQL-интеграционными тестами.

## Передача frontend и оставшиеся задачи backend

Передайте вместе этот документ, openapi.yaml и существующую Postman Collection
с fixtures. Для генерации клиента различайте IMPLEMENTED/PLANNED через extension,
а для UI подготовьте mock всех финальных состояний. Не подключайте вместо Go
одноимённые маршруты старого Python backend.

Оставшиеся задачи: воспроизводимая поставка настоящего Python scoring и
контрактная проверка против него, согласование смысла score и Go risk policy без
дублирования модели, Decision Engine, семь PLANNED обработчиков,
транзакционные accept/decline/manual decisions, хранилище внутренних комментариев
и нормализованной оценки, аудит действия с конкретным предложением, расширение
Application.availableActions. Изменения БД выполняются только новыми миграциями.
На текущем этапе эти задачи не реализуются.
