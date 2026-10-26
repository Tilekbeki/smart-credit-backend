# Модель данных

Схема соответствует миграциям `migrations/000001`–`000012`. PostgreSQL хранит
состояние заявок, снимки входных данных, задания обработки, решения и аудит.
Содержимое загруженных документов лежит в приватном Docker volume; таблица
`documents` хранит только метаданные и серверный ключ файла.

```mermaid
erDiagram
    companies o|--o{ users : company_id
    companies ||--o{ applications : company_id
    companies o|--o{ contact_changes : company_id
    users ||--o{ otp_challenges : user_id
    users ||--o{ applications : owner_id
    users ||--o{ documents : uploaded_by
    users o|--o{ application_events : actor_id
    users o|--o{ credit_decisions : decided_by
    users ||--o{ idempotency_keys : user_id
    users ||--o{ contact_changes : actor_id
    users o|--o{ contact_changes : user_id
    credit_products o|--o{ applications : product_id
    credit_products ||--o{ offers : product_id
    applications ||--o{ application_events : application_id
    applications ||--o{ documents : application_id
    applications ||--o{ jobs : application_id
    applications ||--o{ credit_decisions : application_id
    applications ||--o{ offers : application_id
    jobs o|--o{ credit_decisions : scoring_job_id
    credit_decisions ||--o{ offers : decision_id
    offers o|--o| applications : accepted_offer_id

    companies {
        uuid id PK
        string profile_key UK
        string name
        bigint monthly_revenue
        bigint monthly_expenses
        bigint version
    }
    users {
        uuid id PK
        uuid company_id FK
        string role
        string phone UK
        bigint version
    }
    otp_challenges {
        uuid id PK
        uuid user_id FK
        datetime expires_at
        datetime used_at
    }
    credit_products {
        uuid id PK
        string code UK
        bigint min_amount
        bigint max_amount
        bigint version
    }
    applications {
        uuid id PK
        uuid owner_id FK
        uuid company_id FK
        uuid product_id FK
        uuid accepted_offer_id FK
        string status
        bigint version
        jsonb financial_snapshot
        jsonb product_snapshot
        datetime deleted_at
    }
    application_events {
        uuid id PK
        uuid application_id FK
        bigint sequence
        string event_type
        string status
        uuid actor_id FK
    }
    documents {
        uuid id PK
        uuid application_id FK
        uuid uploaded_by FK
        string document_type
        string status
        string storage_key UK
        string checksum_sha256
    }
    jobs {
        uuid id PK
        uuid application_id FK
        string processing_stage
        string status
        jsonb precheck_results
        jsonb scoring_response
        jsonb assessment_result
    }
    credit_decisions {
        uuid id PK
        uuid application_id FK
        uuid scoring_job_id FK
        uuid decided_by FK
        string source
        string outcome
        string policy_version
        string customer_explanation
    }
    offers {
        uuid id PK
        uuid application_id FK
        uuid decision_id FK
        uuid product_id FK
        string status
        bigint amount
        bigint monthly_payment
    }
    idempotency_keys {
        uuid id PK
        uuid user_id FK
        string operation
        string key
        datetime expires_at
    }
    contact_changes {
        uuid id PK
        uuid actor_id FK
        uuid user_id FK
        uuid company_id FK
        bigint version
        jsonb before_contact
        jsonb after_contact
    }
```

## Важные ограничения

- `applications(owner_id, company_id)` ссылается на пару `users(id, company_id)`:
  владелец заявки принадлежит той же компании. Сотрудник банка читает только
  отправленные заявки через проверку роли в API.
- `users.company_id`, `application_events.actor_id` и
  `credit_decisions.decided_by` могут быть пустыми: сотрудник банка не привязан к
  компании клиента, системное событие не имеет автора, автоматическое решение
  не имеет сотрудника. `credit_decisions.scoring_job_id` также необязателен; схема
  БД не ограничивает число версий решений на одно задание.
- При отправке заявки `financial_snapshot` и `product_snapshot` фиксируют
  исторические данные. Последующее изменение компании или продукта не переписывает
  уже отправленную заявку.
- `jobs` хранит стадии prechecks → scoring → decision, попытки и сохранённые ответы.
  `credit_decisions` содержит итог Go; `offers` существует для одобрения или
  альтернативных условий. Рекомендация Python в `jobs.scoring_response` не равна
  кредитному решению.
- `application_events` упорядочены уникальной парой `(application_id, sequence)`.
  `contact_changes` ведёт отдельный аудит контактов и не входит в клиентскую
  кредитную историю.
- `applications.accepted_offer_id` и `offers.decision_id` используют составные
  внешние ключи с `application_id`, чтобы связывать записи одной заявки.
  `idempotency_keys` защищает повторное выполнение ручного решения и ответа клиента.
- Удаление draft мягкое: `deleted_at` скрывает его из активного списка. Данные
  отправленных заявок, документов и событий сохраняются для демонстрационного
  аудита. Реальное хранение персональных данных и банковские операции здесь не
  реализованы.

[Диаграмма состояний](credit-process.md) · [Архитектура](architecture.md) ·
[SQL-миграции](../migrations/).
