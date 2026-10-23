BEGIN;

ALTER TABLE documents ADD COLUMN document_type varchar(32);

-- Previous stages had no document API. Keep any legacy metadata for diagnosis,
-- but do not advertise untyped files whose contents have not been validated.
UPDATE documents SET status='failed', updated_at=clock_timestamp()
    WHERE status <> 'failed';

ALTER TABLE documents
    ADD CONSTRAINT documents_document_type_check CHECK (
        document_type IN ('BANK_STATEMENT', 'EQUIPMENT_INVOICE')
        OR (document_type IS NULL AND status='failed')
    ),
    ADD CONSTRAINT documents_active_metadata_check CHECK (
        status='failed' OR (
            document_type IS NOT NULL
            AND size_bytes <= 5242880
            AND content_type IN ('application/pdf', 'image/png', 'image/jpeg')
            AND storage_key=id::text
        )
    );

COMMIT;
