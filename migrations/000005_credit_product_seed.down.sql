BEGIN;

-- Existing foreign keys deliberately prevent rollback from removing used terms.
DELETE FROM credit_products WHERE code IN ('turnover', 'investment');

COMMIT;
