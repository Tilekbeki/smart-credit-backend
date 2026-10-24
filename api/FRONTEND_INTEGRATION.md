# Подключение frontend к Smart Credit

Реализована 41 операция. Источник точных DTO, nullable-полей, enum и HTTP-кодов —
[OpenAPI v1](openapi.yaml). Префикс `/api/v1`, локальный backend — `http://localhost:8080`.
Python-прототип в backend/ имеет другой API и не используется React-приложением.

## Запуск и браузер

```sh
docker compose up -d --build
curl --fail http://localhost:8080/ready
```

Backend запускается без scoring. Для финальных решений нужен отдельный Python;
[локальная интеграция](../docs/development.md#настоящий-python-scoring) использует
реальный logreg_v1. Пока scoring недоступен, отображайте безопасное состояние
processing, не рисуйте кредитный отказ. Полный перечень возможностей — в README.

В Go не настроен CORS. Для отдельного frontend в разработке используйте proxy,
направляющий `/api` на backend, либо единый origin через reverse proxy. Не
переносите CORS-конфигурацию старого Python-прототипа на текущий контракт.

## Маршруты

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
| GET | `/api/v1/applications` | CLIENT, членство и приватность draft | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}` | CLIENT, членство и приватность draft | IMPLEMENTED |
| PATCH | `/api/v1/applications/{applicationId}` | CLIENT, свой draft | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/timeline` | CLIENT, членство и приватность draft | IMPLEMENTED |
| POST | `/api/v1/applications/{applicationId}/documents` | CLIENT, свой draft | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/documents` | CLIENT — членство и приватность draft; BANK_EMPLOYEE — отправленные | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/documents/{documentId}` | CLIENT — членство и приватность draft; BANK_EMPLOYEE — отправленные | IMPLEMENTED |
| POST | `/api/v1/applications/{applicationId}/submit` | CLIENT, создатель draft с правом записи | IMPLEMENTED |
| GET | `/api/v1/applications/{applicationId}/decision` | CLIENT, членство и приватность draft | IMPLEMENTED |
| POST | `/api/v1/applications/{applicationId}/accept` | CLIENT с правом записи | IMPLEMENTED |
| POST | `/api/v1/applications/{applicationId}/decline` | CLIENT с правом записи | IMPLEMENTED |
| GET | `/api/v1/bank/applications` | BANK_EMPLOYEE | IMPLEMENTED |
| GET | `/api/v1/bank/applications/{applicationId}` | BANK_EMPLOYEE | IMPLEMENTED |
| GET | `/api/v1/bank/applications/{applicationId}/assessment` | BANK_EMPLOYEE | IMPLEMENTED |
| POST | `/api/v1/bank/applications/{applicationId}/decisions` | BANK_EMPLOYEE, manual_review | IMPLEMENTED |
| GET | `/api/v1/me/companies` | CLIENT | IMPLEMENTED |
| PATCH | `/api/v1/me` | CLIENT, BANK_EMPLOYEE | IMPLEMENTED |
| PATCH | `/api/v1/companies/{companyId}` | Право записи | IMPLEMENTED |
| POST | `/api/v1/companies/{companyId}/change-requests` | Право записи | IMPLEMENTED |
| GET | `/api/v1/companies/{companyId}/change-requests` | Член компании | IMPLEMENTED |
| POST | `/api/v1/companies/{companyId}/change-requests/{requestId}/cancel` | Право записи | IMPLEMENTED |
| POST | `/api/v1/companies/{companyId}/financial-declarations` | Право записи | IMPLEMENTED |
| GET | `/api/v1/companies/{companyId}/financial-declarations` | Член компании; draft приватен | IMPLEMENTED |
| GET | `/api/v1/bank/companies/{companyId}/memberships` | BANK_EMPLOYEE | IMPLEMENTED |
| PUT | `/api/v1/bank/companies/{companyId}/memberships/{userId}` | BANK_EMPLOYEE | IMPLEMENTED |
| GET | `/api/v1/bank/companies/{companyId}/change-requests` | BANK_EMPLOYEE | IMPLEMENTED |
| POST | `/api/v1/bank/companies/{companyId}/change-requests/{requestId}/review` | BANK_EMPLOYEE | IMPLEMENTED |
| GET | `/api/v1/bank/companies/{companyId}/financial-declarations` | BANK_EMPLOYEE; draft скрыт | IMPLEMENTED |
| POST | `/api/v1/bank/companies/{companyId}/financial-declarations/{requestId}/review` | BANK_EMPLOYEE | IMPLEMENTED |
| POST | `/api/v1/bank/applications/{applicationId}/retry-processing` | BANK_EMPLOYEE; failed job | IMPLEMENTED |

## Авторизация и права

1. Получите GET `/demo/profiles` без токена: три CLIENT и один BANK_EMPLOYEE.
   companyName может быть null и у CLIENT с несколькими компаниями; после входа
   выбирайте из /me/companies.
2. Отправьте POST `/auth/otp/request` с `{"profileId":"UUID"}`; сохраните otpRequestId.
3. Отправьте POST `/auth/otp/verify` с `{"otpRequestId":"UUID","code":"1234"}`.
4. Передавайте `Authorization: Bearer <accessToken>`. GET `/me` возвращает текущего пользователя.

JWT действует 8 часов, OTP — 5 минут, максимум 5 попыток. OTP одноразовый; новый
challenge отменяет предыдущий. Код задаётся DEMO_OTP_CODE; реальных SMS нет.
Не передавайте role, reviewedBy или actorId в запросах кредитных действий.
Идентичность определяет сервер. В POST recommendations/applications companyId
теперь служит явным селектором и проверяется по актуальному членству PostgreSQL. Секрет подписи frontend не нужен.

CLIENT читает собственные черновики и отправленные заявки компаний с действующим
членством; чужая/несуществующая — 404. Отзыв доступа действует и для старого JWT.
BANK_EMPLOYEE не видит draft ни в списке, ни по UUID, ни через документы;
status=draft в банковском фильтре даёт 400. Другая роль получает 403, отсутствие
или некорректный JWT — 401. Банк не редактирует и не отправляет клиентскую анкету.

## Продукт, рекомендация и черновик

GET `/credit-products` возвращает turnover/investment. Ориентируйтесь на реальные
границы и цели продукта из ответа. POST `/recommendations` принимает purpose,
requestedAmountRub, termMonths, monthlyRevenueRub, monthlyExpensesRub. Он не
создаёт заявку, не вызывает scoring и не гарантирует решение.

POST `/applications` создаёт новый независимый draft. Разрешены несколько
черновиков одного продукта; выбранный applicationId определяет дальнейшее действие.

```json
{
  "productId": "turnover",
  "purpose": "stock",
  "requestedAmountRub": 1500000,
  "termMonths": 12,
  "monthlyRevenueRub": 2400000,
  "monthlyExpensesRub": 1500000
}
```

Все семь полей draft необязательны и nullable; седьмое — purposeDescription.
Цели: stock/expenses/seasonal/equipment/upgrade. other отклоняется сразу. Деньги — целые рубли, срок — целые месяцы.

PATCH `/applications/{id}` требует version и хотя бы одно изменяемое поле:

```json
{"version": 1, "requestedAmountRub": 1200000, "purposeDescription": null}
```

Отсутствующее поле сохраняется, null очищает его. После любого изменения
сохраняйте возвращённую version. При version_conflict перечитайте заявку;
не перезаписывайте чужое изменение автоматически.

## Документы и отправка

POST `/applications/{id}/documents` использует multipart/form-data: поле
`documentType` и файл `file`. Типы — BANK_STATEMENT и EQUIPMENT_INVOICE;
форматы — PDF, PNG, JPEG. Файл непустой, максимум 5242880 байт, весь multipart
максимум 5308416 байт. Не подставляйте имя в URL: сохраняйте documentId из ответа.

GET documents возвращает метаданные; GET documents/{documentId} — двоичный файл.
Банк получает доступ только после submit. После отправки загружать документы нельзя.
Финансовые документы приватны; серверные storage keys не передаются.

POST `/applications/{id}/submit` выполняется **без тела** и без Idempotency-Key.
Сервер проверяет полноту, продукт, возраст компании и обязательные документы.
Для investment нужен EQUIPMENT_INVOICE. BANK_STATEMENT нужен для любого
продукта без подтверждения точных сумм анкеты. Изменённая клиентом выручка
не наследует доверие профиля; investment может требовать оба документа.
Проверка формата и целостности файла не подтверждает его финансовое содержание. Используйте 422 details, не угадывайте требования по имени профиля.

Успех — 202 с applicationId/status/submittedAt/nextStage. Повтор во время
submitted/processing безопасен. После финального исхода повтор submit — 409.
nextStage=awaiting_scoring — укрупнённый индикатор окончания prechecks;
он не доказывает, что ответ scoring уже получен. Отправка не меняет другие draft.

## Решение, обработка и история

GET `/applications/{id}/decision` возвращает 200 во всех состояниях собственной
заявки. CreditDecision содержит applicationId, status, version, decision, score,
riskLevel, reasons, assessedAt, reviewedBy, offer, processing.

| Состояние | Что показывать |
| --- | --- |
| draft | decision/score/riskLevel/offer/processing=null, reasons=[] |
| submitted/processing | decision и offer=null; processing показывает ход обработки |
| manual_review | decision/offer/processing/assessedAt=null, score/risk и причины могут присутствовать |
| approved/counter_offer | Финальное decision и актуальный CreditOffer |
| rejected | Финальное rejected и безопасные reasons, offer=null |
| signed/closed | Последнее decision сохраняется; offer имеет фактический статус |

processing: stage=prechecks/scoring/decision, state=waiting/running/retrying/failed;
retryAt и message nullable. Score=0 — настоящий балл, null — результата нет.
Score 80..100 соответствует LOW, 50..79 — MEDIUM, ниже 50 — HIGH. Новые финансовые
ограничения Go не меняют эти пороги; [правила](../docs/credit-process.md).

CreditOffer содержит offerId, applicationId, productId, amountRub, termMonths,
annualRatePercent, monthlyPaymentRub, expiresAt, status. Ставка в процентах:
22 означает 22%, не 0.22. Платёж рассчитывает backend, expiresAt=null в MVP;
status=active/accepted/withdrawn. Не рассчитывайте окончательные условия из Python factors.

GET timeline возвращает `{items: ApplicationEvent[]}`. Порядок — sequence;
поля события: eventId/eventType/sequence/status/createdAt/actorId/title.
У системных событий actorId=null, у пользовательских — UUID из JWT. Score,
сырые ошибки и внутренние комментарии в историю не попадают.

## Банковский кабинет

GET `/bank/applications` принимает status, companyId, limit, offset.
Данные приходят из снимков submit, черновики исключены SQL-запросом.
BankApplicationPage — items/total/limit/offset; total после фильтров до пагинации.
Карточка GET `/bank/applications/{id}` содержит анкету, историческую компанию,
product/productTerms, status/version/даты, manualReviewRequired/availableActions.
Последующие изменения компании или каталога не меняют исторические сведения.

GET assessment возвращает companyCheck, creditHistory, scoring, riskLevel,
manualReviewReasons, recommendations, manualDecision, processing и идентификатор/
статус/версию заявки. Неполученные результаты — null, списки без значений — [].
Scoring содержит сохранённые score/assessedAt, decision Python, approvedAmountRub,
reasons и шесть исходных factors. model/modelVersion=null, пока сервис не передаёт
достоверные метаданные. goDecision содержит сохранённый результат Go с версией
политики, причинами и manualReviewReasons. GET не обращается к Python.
Recommendations — советы
Go policy без offerId, а не действующие предложения. ManualDecision содержит
внутренний comment/internalComment, отдельный customerExplanation, reviewedBy,
reviewedAt и decision после решения сотрудника. Внутренний текст не попадает клиенту.

POST `/bank/applications/{id}/decisions` доступен только в manual_review:

```json
{
  "version": 4,
  "decision": "counter_offer",
  "internalComment": "Проверены подтверждающие сведения; предлагаю меньшую сумму.",
  "customerExplanation": "Доступна меньшая сумма с учётом подтверждённой нагрузки.",
  "offer": {"amountRub": 800000, "termMonths": 12}
}
```

Обязательны version/decision/offer и ровно одно из internalComment или совместимого
comment. customerExplanation необязателен, до 1000 символов; после trim пустой
текст заменяется стандартной публичной причиной. Для rejected offer=null. Approved сохраняет исходную
сумму/срок; counter_offer не превышает запрошенную сумму и меняет хотя бы одно
условие в пределах продукта. Comment после trim — 1..1000 символов. Дополнительные
вычисляемые поля запрещены. Успех — 201 CreditDecision; комментарий не копируется
в клиентские reasons. Обязательные финансовые ограничения действуют и вручную.

## Принятие, отказ и идемпотентность

POST accept/decline принимает `{"version":5,"offerId":"UUID"}`. Доступ — только
CLIENT с действующим OWNER/AUTHORIZED_REPRESENTATIVE в компании заявки,
approved/counter_offer и актуальным active offer. Accept возвращает
200 с status=signed и offer.status=accepted; decline — closed/withdrawn. Поле
decision остаётся approved/counter_offer. Signed — только демонстрационное подтверждение.

Для decisions/accept/decline обязателен заголовок `Idempotency-Key: UUID`.
Создайте ключ один раз на логическое действие и сохраняйте при сетевом повторе.
Тот же ключ/пользователь/операция/applicationId и каноническое тело возвращают
исходные код и JSON, даже если версия уже выросла. Другое тело — 409 idempotency_conflict.
Права проверяются до возврата сохранённого ответа. Срок ключей — 24 часа.

После истечения ключа повтор manual decision конфликтует; одинаковое accept/decline
может вернуть 200 без нового эффекта по долговечному аудиту исходной/результирующей
версии и того же offerId. Противоположное действие или неактуальное предложение — 409.
После конфликта перечитайте decision и version, не повторяйте чужой offerId.

| Роль и состояние | availableActions |
| --- | --- |
| CLIENT, собственный draft и право записи | edit, submit |
| CLIENT с правом записи, approved/counter_offer с active offer | accept, decline |
| CLIENT, остальные состояния | [] |
| BANK_EMPLOYEE, manual_review | decide |
| BANK_EMPLOYEE, прочие отправленные состояния | [] |

Submit в availableActions не означает, что анкета и документы уже прошли проверку.

## Пагинация, ошибки и тесты

Лимит по умолчанию 20, диапазон 1..100. Offset 0..2147483647. Порядок списков:
createdAt DESC, applicationId DESC. За концом страницы items=[], total сохраняется.
Неизвестные и повторные query-параметры отклоняются с 400.

Ошибки имеют error/message; 422 дополнительно содержит details с field/code/message.
Основные коды: 400 invalid_request; 401 unauthorized; 403 forbidden;
404 application_not_found/product_not_found/company_not_found; 409 company_required/version_conflict/invalid_status/
offer_not_active/idempotency_conflict; 422 validation_failed; 503 service_unavailable.
Загрузка также проверяет формат, размер и MIME; точные коды — в OpenAPI.
Все ответы используют Cache-Control:no-store; даты — RFC 3339 UTC.

[Postman Collection](postman/smart-credit.postman_collection.json) запускается сверху
вниз, Working directory=api/postman. JWT/UUID/version/ключи передаются автоматически.
Для настоящего scoring используйте scoringMode=logreg_v1 и все четыре expect-флага;
[команды](../docs/development.md#postman-и-newman), [реальные сценарии](../docs/scoring-integration.md).

## Изменения для frontend

| Область | Новое поведение |
| --- | --- |
| Выбор компании | После входа GET /me/companies. При нескольких членствах обязателен выбор; companyId в /me и OTP verify может быть null и для CLIENT |
| Создание | POST applications/recommendations принимает companyId. При нескольких компаниях без него — 409 company_required |
| Список | Элементы дополнены companyId. GET applications принимает фильтр companyId; без фильтра доступны все компании, чужие черновики скрыты |
| Полномочия | Используйте membershipRole/availableActions; VIEWER не получает edit/submit/accept/decline |
| Профиль | /me дополнен displayName, legalName, additionalEmail, version. PATCH меняет только displayName/additionalEmail |
| Компания | version, registrationDataSource, financialProvenance. Контактные, регистрационные и финансовые изменения разделены |
| Исправления | История originalValues/proposedValues, отдельные статусы pending_review/approved/rejected/cancelled |
| Финансы | Ручные значения — client_declared. Статусы pending_review/verified/rejected; загрузка документа не подтверждает суммы |
| Рекомендации | financialVerificationRequired; неподтверждённый бюджет ограничен исходными данными компании |
| Документы | BANK_STATEMENT для любого продукта без подтверждённых сумм; EQUIPMENT_INVOICE дополнительно для investment |
| Assessment | Python reasons/factors и сохранённый goDecision. Метаданные модели могут быть null |
| Ручное решение | internalComment либо comment; customerExplanation отдельно. Клиент получает публичный текст в reasons |
| Цели | other запрещён; берите цели из каталога |
| Сбой | processing.state=failed требует оператора, это не кредитный отказ |

Активную компанию хранит frontend. GET /me/companies возвращает companyId,
companyName, organizationType, membershipRole, status, version и availableActions.
При одном членстве POST можно отправлять без companyId; при нескольких backend
не выбирает первую компанию. Отправленные заявки принадлежат бизнес-контексту
компании; userId сохраняет создателя, события actorId — выполнившего действие.

Все новые повторяемые изменения, кроме PATCH, требуют Idempotency-Key, включая
банковский PUT членства. Для PATCH /me нужна версия пользователя. Для контактов,
деклараций, запросов и их рассмотрения нужна текущая **версия компании**.
Истории исправлений/деклараций возвращают companyVersion у каждого элемента.
version и companyVersionAtRequest относятся к исторической записи и не заменяют
текущую companyVersion. Банку не нужно обращаться к клиентскому GET компании.
Истории ограничены последними 100 записями. Null в новых input запрещён.

Пример финансовой декларации (201, pending_review):

```json
{"version":3,"monthlyRevenueRub":10000000,"monthlyExpensesRub":8800000,"reason":"Уточнены показатели периода"}
```

Отдельное банковское рассмотрение (200):

```json
{"version":4,"outcome":"approved","reason":"Отдельно проверены точные суммы"}
```

Ручное одобрение этих сумм запрещено до отдельного подтверждения декларации;
содержимое загруженного файла автоматически не проверяется. Для доступа банк
отправляет version=0 при первом grant, затем текущую версию членства,
membershipRole, status=active/revoked и reason. Повторный grant после revoke
сохраняет ID членства и увеличивает версию. Доказательство юридического
представительства не реализовано демонстрационным API.

Восстановление: POST `/bank/applications/{id}/retry-processing`, тело version/reason
и Idempotency-Key. Успех 202: applicationId/status/version/stage/recoveryCount.
Только failed job без кредитного решения, максимум три восстановления; успешные
этапы сохраняются. Повтор возвращает первоначальный ответ. Повреждённые снимки
не исправляются повторным запуском автоматически и требуют диагностики.

Для ошибки после submit создайте новый draft из прочитанных полей, явно укажите
companyId, исправьте данные и заново загрузите документы. Исходная заявка и решение
сохраняются. Перенос документов и отзыв отправленной заявки не реализованы.

Телефон входа и legalName в MVP изменить нельзя. Реальная смена контакта требует
внешнего подтверждения владения. Фиксированный OTP для этого не используется.
APP_ENV=production отказывает в запуске до настоящих интеграций идентификации,
реестров, БКИ и подписания. [Подробные правила и миграция](../docs/company-data.md).

Банковский assessment дополнительно возвращает processingDetails:
stage/attemptCount/maxAttempts/recoveryCount/canRetry. Это позволяет отличить
исчерпанные повторы от ожидания и скрыть кнопку восстановления после трёх попыток
оператора. Сервер повторно проверяет возможность операции в транзакции.
