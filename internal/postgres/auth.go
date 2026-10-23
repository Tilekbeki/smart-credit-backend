package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/auth"
)

// AuthRepository persists demo authentication. A user row is the common lock for
// requesting and verifying OTPs, so replacement and consumption are serialized.
type AuthRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ auth.Repository = (*AuthRepository)(nil)

func NewAuthRepository(pool *pgxpool.Pool, timeout time.Duration) *AuthRepository {
	return &AuthRepository{pool: pool, timeout: timeout}
}

func (r *AuthRepository) ListProfiles(ctx context.Context) ([]auth.Profile, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `
		SELECT u.id::text, u.full_name, u.phone, u.role, c.name
		FROM users u LEFT JOIN companies c ON c.id = u.company_id
		WHERE u.profile_key IN ('shop', 'workshop', 'cafe', 'bank_employee')
		ORDER BY CASE u.profile_key
		    WHEN 'shop' THEN 1 WHEN 'workshop' THEN 2
		    WHEN 'cafe' THEN 3 WHEN 'bank_employee' THEN 4 END`)
	if err != nil {
		return nil, auth.ErrStorage
	}
	defer rows.Close()
	profiles := make([]auth.Profile, 0, 4)
	for rows.Next() {
		var p auth.Profile
		if err := rows.Scan(&p.ID, &p.Name, &p.Phone, &p.Role, &p.CompanyName); err != nil {
			return nil, auth.ErrStorage
		}
		profiles = append(profiles, p)
	}
	if rows.Err() != nil {
		return nil, auth.ErrStorage
	}
	return profiles, nil
}

func (r *AuthRepository) RequestOTP(ctx context.Context, profileID string) (auth.OTPChallenge, error) {
	if !validUUID(profileID) {
		return auth.OTPChallenge{}, auth.ErrUserNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return auth.OTPChallenge{}, auth.ErrStorage
	}
	defer tx.Rollback(ctx)
	var phone string
	err = tx.QueryRow(ctx, `SELECT phone FROM users
		WHERE id = $1 AND profile_key IN ('shop', 'workshop', 'cafe', 'bank_employee')
		FOR UPDATE`, profileID).Scan(&phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.OTPChallenge{}, auth.ErrUserNotFound
	}
	if err != nil {
		return auth.OTPChallenge{}, auth.ErrStorage
	}
	// Use the database clock after acquiring locks: transaction now() could be
	// stale if this request waited for another verification/request to finish.
	_, err = tx.Exec(ctx, `UPDATE otp_challenges SET invalidated_at = clock_timestamp()
		WHERE user_id = $1 AND used_at IS NULL AND invalidated_at IS NULL
		AND expires_at > clock_timestamp()`, profileID)
	if err != nil {
		return auth.OTPChallenge{}, auth.ErrStorage
	}
	challenge := auth.OTPChallenge{Phone: phone}
	err = tx.QueryRow(ctx, `INSERT INTO otp_challenges
		(user_id, phone, created_at, expires_at, max_attempts)
		VALUES ($1, $2, clock_timestamp(), clock_timestamp() + interval '5 minutes', 5)
		RETURNING id::text, expires_at`, profileID, phone).Scan(&challenge.ID, &challenge.ExpiresAt)
	if err != nil {
		return auth.OTPChallenge{}, auth.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return auth.OTPChallenge{}, auth.ErrStorage
	}
	return challenge, nil
}

func (r *AuthRepository) VerifyOTP(ctx context.Context, requestID string, codeMatches bool) (auth.User, error) {
	if !validUUID(requestID) {
		return auth.User{}, auth.ErrChallengeNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return auth.User{}, auth.ErrStorage
	}
	defer tx.Rollback(ctx)
	// Read only the owner first. Both operations must lock user before challenge.
	var userID string
	err = tx.QueryRow(ctx, `SELECT user_id::text FROM otp_challenges WHERE id = $1`, requestID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrChallengeNotFound
	}
	if err != nil {
		return auth.User{}, auth.ErrStorage
	}
	var user auth.User
	err = tx.QueryRow(ctx, `SELECT id::text, full_name, phone, role, company_id::text
		FROM users WHERE id = $1 FOR UPDATE`, userID).
		Scan(&user.ID, &user.Name, &user.Phone, &user.Role, &user.CompanyID)
	if err != nil {
		return auth.User{}, auth.ErrStorage
	}
	var attempts, maxAttempts int
	var expiresAt time.Time
	var usedAt, invalidatedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT attempt_count, max_attempts, expires_at, used_at, invalidated_at
		FROM otp_challenges WHERE id = $1 FOR UPDATE`, requestID).
		Scan(&attempts, &maxAttempts, &expiresAt, &usedAt, &invalidatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrChallengeNotFound
	}
	if err != nil {
		return auth.User{}, auth.ErrStorage
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return auth.User{}, auth.ErrStorage
	}
	switch {
	case invalidatedAt != nil:
		return auth.User{}, auth.ErrOTPInvalidated
	case usedAt != nil:
		return auth.User{}, auth.ErrOTPUsed
	case !now.Before(expiresAt):
		return auth.User{}, auth.ErrOTPExpired
	case attempts >= maxAttempts:
		return auth.User{}, auth.ErrOTPLimit
	}
	if !codeMatches {
		if _, err := tx.Exec(ctx, `UPDATE otp_challenges SET attempt_count = attempt_count + 1 WHERE id = $1`, requestID); err != nil {
			return auth.User{}, auth.ErrStorage
		}
		// A failed code comparison is still a committed attempt.
		if err := tx.Commit(ctx); err != nil {
			return auth.User{}, auth.ErrStorage
		}
		if attempts+1 >= maxAttempts {
			return auth.User{}, auth.ErrOTPLimit
		}
		return auth.User{}, auth.ErrOTPInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE otp_challenges
		SET attempt_count = attempt_count + 1, used_at = clock_timestamp() WHERE id = $1`, requestID); err != nil {
		return auth.User{}, auth.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return auth.User{}, auth.ErrStorage
	}
	return user, nil
}

func (r *AuthRepository) UserByID(ctx context.Context, id string) (auth.User, error) {
	if !validUUID(id) {
		return auth.User{}, auth.ErrUserNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var user auth.User
	err := r.pool.QueryRow(ctx, `SELECT id::text, full_name, phone, role, company_id::text
		FROM users WHERE id = $1`, id).
		Scan(&user.ID, &user.Name, &user.Phone, &user.Role, &user.CompanyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrUserNotFound
	}
	if err != nil {
		return auth.User{}, auth.ErrStorage
	}
	return user, nil
}

func validUUID(value string) bool {
	var id pgtype.UUID
	return id.Scan(value) == nil && id.Valid
}
