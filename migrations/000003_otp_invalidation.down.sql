BEGIN;

-- Preserve invalidation if this column is removed and added again while a
-- challenge is still unexpired. An older reader must not accept its fixed code.
UPDATE otp_challenges
    SET attempt_count = max_attempts
    WHERE invalidated_at IS NOT NULL AND used_at IS NULL;

ALTER TABLE otp_challenges
    DROP CONSTRAINT otp_challenges_invalidation_time,
    DROP COLUMN invalidated_at;

COMMIT;
