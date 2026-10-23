BEGIN;

-- Nullable additions preserve companies created before detailed profiles existed.
ALTER TABLE companies
    ADD COLUMN organization_type varchar(3) CHECK (organization_type IN ('ИП', 'ООО')),
    ADD COLUMN inn varchar(12) CHECK (inn ~ '^([0-9]{10}|[0-9]{12})$'),
    ADD COLUMN registration_number varchar(15) CHECK (registration_number ~ '^([0-9]{13}|[0-9]{15})$'),
    ADD COLUMN registration_date date,
    ADD COLUMN okved varchar(16) CHECK (okved ~ '^[0-9]{2}(\.[0-9]{1,2}){0,2}$'),
    ADD COLUMN director_name varchar(160) CHECK (btrim(director_name) <> ''),
    ADD COLUMN contact_phone varchar(16) CHECK (contact_phone ~ '^\+[1-9][0-9]{7,14}$'),
    ADD COLUMN contact_email varchar(254) CHECK (btrim(contact_email) <> ''),
    ADD COLUMN contact_address varchar(300) CHECK (btrim(contact_address) <> ''),
    ADD CONSTRAINT companies_registration_type CHECK (
        (organization_type = 'ИП' AND length(inn) = 12 AND length(registration_number) = 15)
        OR (organization_type = 'ООО' AND length(inn) = 10 AND length(registration_number) = 13)
    );

-- All identifiers, contacts and people below are synthetic demonstration data.
UPDATE companies SET
    organization_type = 'ИП', inn = '000000000001', registration_number = '000000000000001',
    registration_date = DATE '2020-03-15', okved = '47.11', director_name = 'Иван Лавкин',
    contact_phone = '+79990000001', contact_email = 'shop@example.test',
    contact_address = 'Демонстрационный город, Учебная улица, дом 1'
WHERE id = '10000000-0000-4000-8000-000000000001';

UPDATE companies SET
    organization_type = 'ООО', inn = '0000000002', registration_number = '0000000000002',
    registration_date = DATE '2019-06-10', okved = '25.62', director_name = 'Анна Северова',
    contact_phone = '+79990000002', contact_email = 'workshop@example.test',
    contact_address = 'Демонстрационный город, Учебная улица, дом 2'
WHERE id = '10000000-0000-4000-8000-000000000002';

UPDATE companies SET
    organization_type = 'ООО', inn = '0000000003', registration_number = '0000000000003',
    registration_date = DATE '2022-09-01', okved = '56.10', director_name = 'Павел Вкусов',
    contact_phone = '+79990000003', contact_email = 'cafe@example.test',
    contact_address = 'Демонстрационный город, Учебная улица, дом 3'
WHERE id = '10000000-0000-4000-8000-000000000003';

ALTER TABLE credit_products
    ADD COLUMN purposes text[] NOT NULL DEFAULT '{}',
    ADD COLUMN min_business_age_months integer NOT NULL DEFAULT 0 CHECK (min_business_age_months >= 0),
    ADD CONSTRAINT credit_products_purposes CHECK (
        purposes <@ ARRAY['stock', 'expenses', 'seasonal', 'equipment', 'upgrade']::text[]
        AND array_position(purposes, NULL) IS NULL
    );

COMMIT;
