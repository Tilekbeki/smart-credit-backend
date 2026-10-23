BEGIN;

ALTER TABLE otp_challenges
    ADD COLUMN invalidated_at timestamptz,
    ADD CONSTRAINT otp_challenges_invalidation_time
        CHECK (invalidated_at IS NULL OR invalidated_at >= created_at);

COMMIT;
