DO $$ BEGIN
 RAISE EXCEPTION 'Rollback 000012 is intentionally blocked: memberships and audit may contain new data. Restore a tested pre-upgrade backup with matching binaries.';
END $$;
