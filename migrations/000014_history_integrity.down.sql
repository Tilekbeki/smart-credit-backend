DO $$ BEGIN
 RAISE EXCEPTION 'Rollback 000014 is intentionally blocked: use a tested backup and matching binaries to preserve audit and financial verification guarantees.';
END $$;
