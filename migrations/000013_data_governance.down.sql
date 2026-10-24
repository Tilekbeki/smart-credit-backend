DO $$ BEGIN
 RAISE EXCEPTION 'Rollback 000013 is intentionally blocked: financial provenance, corrections and audit must be preserved. Restore a tested pre-upgrade backup.';
END $$;
