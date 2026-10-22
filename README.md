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
