BEGIN;

ALTER TABLE documents
    DROP CONSTRAINT documents_active_metadata_check,
    DROP CONSTRAINT documents_document_type_check,
    DROP COLUMN document_type;

COMMIT;
