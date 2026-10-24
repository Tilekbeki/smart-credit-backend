package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/auth"
)

const (
	demoClientID   = "20000000-0000-4000-8000-000000000001"
	demoEmployeeID = "20000000-0000-4000-8000-000000000004"
)

// Every invocation owns a schema and search_path. Even when the configured test
// database is shared with development, its existing tables/data are untouched.
func authTestPool(t *testing.T) *pgxpool.Pool { return authTestPoolThrough(t, 999999) }

func authTestPoolThrough(t *testing.T, through int) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid TEST_DATABASE_URL")
	}
	adminCfg.ConnConfig.ConnectTimeout = 5 * time.Second
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		t.Fatal("cannot create PostgreSQL test pool")
	}
	t.Cleanup(admin.Close)
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("cannot generate test schema suffix")
	}
	schema := "auth_test_" + hex.EncodeToString(random[:])
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal("cannot create isolated authentication test schema")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error("cannot remove isolated authentication test schema")
		}
	})
	cfg := adminCfg.Copy()
	cfg.MaxConns = 16
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot create isolated authentication test pool")
	}
	t.Cleanup(pool.Close)
	paths, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil || len(paths) == 0 {
		t.Fatal("cannot find migration fixtures")
	}
	sort.Strings(paths)
	for _, path := range paths {
		number, _ := strconv.Atoi(filepath.Base(path)[:6])
		if number > through {
			continue
		}
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read migration %s", filepath.Base(path))
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("cannot apply migration %s", filepath.Base(path))
		}
	}
	return pool
}

func TestAuthRepository(t *testing.T) {
	pool := authTestPool(t)
	repo := NewAuthRepository(pool, 5*time.Second)
	ctx := context.Background()
	challenge := func(t *testing.T, userID string) auth.OTPChallenge {
		t.Helper()
		otp, err := repo.RequestOTP(ctx, userID)
		if err != nil {
			t.Fatalf("request OTP: %v", err)
		}
		return otp
	}
	assertAttemptCount := func(t *testing.T, requestID string, want int) {
		t.Helper()
		var attempts int
		if err := pool.QueryRow(ctx, "SELECT attempt_count FROM otp_challenges WHERE id=$1", requestID).Scan(&attempts); err != nil {
			t.Fatal("cannot read attempt count")
		}
		if attempts != want {
			t.Fatalf("attempt_count = %d, want %d", attempts, want)
		}
	}

	t.Run("profiles load database data and exclude other users", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO users (id, profile_key, company_id, phone, full_name, role)
			VALUES ('20000000-0000-4000-8000-000000000005', 'other',
			'10000000-0000-4000-8000-000000000001', '+79990000005', 'Other user', 'CLIENT')`)
		if err != nil {
			t.Fatal("cannot create additional user fixture")
		}
		profiles, err := repo.ListProfiles(ctx)
		if err != nil || len(profiles) != 4 {
			t.Fatalf("expected four profiles, got %d, err %v", len(profiles), err)
		}
		for i, p := range profiles {
			if p.Name == "" || p.Phone == "" || !validUUID(p.ID) {
				t.Fatal("profile fields were not read")
			}
			if i < 3 && (p.Role != auth.RoleClient || p.CompanyName == nil || *p.CompanyName == "") {
				t.Fatal("client company/role missing")
			}
		}
		if profiles[3].Role != auth.RoleBankEmployee || profiles[3].CompanyName != nil {
			t.Fatal("employee profile must not have a company")
		}
		if _, err := repo.RequestOTP(ctx, "20000000-0000-4000-8000-000000000005"); !errors.Is(err, auth.ErrUserNotFound) {
			t.Fatalf("non-demo user must not request public demo OTP: %v", err)
		}
	})

	t.Run("request stores phone five minute expiry and five attempts", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		if !validUUID(otp.ID) || otp.Phone != "+79990000001" {
			t.Fatal("unexpected persisted OTP identity/phone")
		}
		var created, expires time.Time
		var maxAttempts int
		if err := pool.QueryRow(ctx, `SELECT created_at, expires_at, max_attempts
			FROM otp_challenges WHERE id=$1`, otp.ID).Scan(&created, &expires, &maxAttempts); err != nil {
			t.Fatal("cannot read OTP fixture")
		}
		if difference := expires.Sub(created); difference < 5*time.Minute || difference > 5*time.Minute+time.Second {
			t.Fatalf("unexpected OTP lifetime: %v", difference)
		}
		if !expires.Equal(otp.ExpiresAt) || maxAttempts != 5 {
			t.Fatal("OTP expiry or max_attempts differs from contract")
		}
	})

	t.Run("successful verification consumes challenge", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		user, err := repo.VerifyOTP(ctx, otp.ID, true)
		if err != nil || user.ID != demoClientID || user.Role != auth.RoleClient || user.CompanyID == nil {
			t.Fatalf("unexpected verified user: %+v, err %v", user, err)
		}
		assertAttemptCount(t, otp.ID, 1)
		if _, err := repo.VerifyOTP(ctx, otp.ID, true); !errors.Is(err, auth.ErrOTPUsed) {
			t.Fatalf("OTP replay: %v", err)
		}
		assertAttemptCount(t, otp.ID, 1)
	})

	t.Run("employee has no company", func(t *testing.T) {
		otp := challenge(t, demoEmployeeID)
		user, err := repo.VerifyOTP(ctx, otp.ID, true)
		if err != nil || user.Role != auth.RoleBankEmployee || user.CompanyID != nil {
			t.Fatalf("unexpected employee user: %+v, err %v", user, err)
		}
	})

	t.Run("wrong codes commit attempts and enforce limit", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		for i := 1; i <= 5; i++ {
			want := auth.ErrOTPInvalid
			if i == 5 {
				want = auth.ErrOTPLimit
			}
			if _, err := repo.VerifyOTP(ctx, otp.ID, false); !errors.Is(err, want) {
				t.Fatalf("wrong attempt %d: %v, want %v", i, err, want)
			}
			assertAttemptCount(t, otp.ID, i)
		}
		if _, err := repo.VerifyOTP(ctx, otp.ID, true); !errors.Is(err, auth.ErrOTPLimit) {
			t.Fatalf("correct code bypassed exhausted attempts: %v", err)
		}
		assertAttemptCount(t, otp.ID, 5)
	})

	t.Run("wrong code then valid code", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		if _, err := repo.VerifyOTP(ctx, otp.ID, false); !errors.Is(err, auth.ErrOTPInvalid) {
			t.Fatalf("wrong code: %v", err)
		}
		if _, err := repo.VerifyOTP(ctx, otp.ID, true); err != nil {
			t.Fatalf("valid code after one wrong attempt: %v", err)
		}
		assertAttemptCount(t, otp.ID, 2)
	})

	t.Run("valid fifth attempt is permitted", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		for range 4 {
			if _, err := repo.VerifyOTP(ctx, otp.ID, false); !errors.Is(err, auth.ErrOTPInvalid) {
				t.Fatalf("wrong code before final attempt: %v", err)
			}
		}
		if _, err := repo.VerifyOTP(ctx, otp.ID, true); err != nil {
			t.Fatalf("valid final attempt: %v", err)
		}
		assertAttemptCount(t, otp.ID, 5)
	})

	t.Run("expired code cannot be consumed", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		if _, err := pool.Exec(ctx, `UPDATE otp_challenges SET
			created_at = clock_timestamp() - interval '6 minutes',
			expires_at = clock_timestamp() - interval '1 minute' WHERE id = $1`, otp.ID); err != nil {
			t.Fatal("cannot expire OTP fixture")
		}
		if _, err := repo.VerifyOTP(ctx, otp.ID, true); !errors.Is(err, auth.ErrOTPExpired) {
			t.Fatalf("expired OTP: %v", err)
		}
		assertAttemptCount(t, otp.ID, 0)
	})

	t.Run("new challenge invalidates preceding active one", func(t *testing.T) {
		old := challenge(t, demoClientID)
		latest := challenge(t, demoClientID)
		if _, err := repo.VerifyOTP(ctx, old.ID, true); !errors.Is(err, auth.ErrOTPInvalidated) {
			t.Fatalf("superseded challenge: %v", err)
		}
		if _, err := repo.VerifyOTP(ctx, latest.ID, true); err != nil {
			t.Fatalf("latest challenge: %v", err)
		}
	})

	t.Run("concurrent verification has one winner", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		results := concurrently(16, func() error {
			_, err := repo.VerifyOTP(ctx, otp.ID, true)
			return err
		})
		var successes int
		for _, err := range results {
			if err == nil {
				successes++
			} else if !errors.Is(err, auth.ErrOTPUsed) {
				t.Fatalf("concurrent verification: %v", err)
			}
		}
		if successes != 1 {
			t.Fatalf("successful consumptions = %d, want 1", successes)
		}
		assertAttemptCount(t, otp.ID, 1)
	})

	t.Run("concurrent wrong attempts never exceed five", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		results := concurrently(16, func() error {
			_, err := repo.VerifyOTP(ctx, otp.ID, false)
			return err
		})
		var invalids, limited int
		for _, err := range results {
			switch {
			case errors.Is(err, auth.ErrOTPInvalid):
				invalids++
			case errors.Is(err, auth.ErrOTPLimit):
				limited++
			default:
				t.Fatalf("concurrent wrong code result: %v", err)
			}
		}
		if invalids != 4 || limited != 12 {
			t.Fatalf("invalid/limited = %d/%d, want 4/12", invalids, limited)
		}
		assertAttemptCount(t, otp.ID, 5)
		if _, err := repo.VerifyOTP(ctx, otp.ID, true); !errors.Is(err, auth.ErrOTPLimit) {
			t.Fatalf("correct code bypassed concurrent attempts: %v", err)
		}
	})

	t.Run("concurrent requests leave exactly one active challenge", func(t *testing.T) {
		results := concurrently(8, func() error {
			_, err := repo.RequestOTP(ctx, demoClientID)
			return err
		})
		for _, err := range results {
			if err != nil {
				t.Fatalf("concurrent request: %v", err)
			}
		}
		var active int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM otp_challenges
			WHERE user_id=$1 AND invalidated_at IS NULL AND used_at IS NULL
			AND expires_at>clock_timestamp() AND attempt_count<max_attempts`, demoClientID).Scan(&active); err != nil {
			t.Fatal("cannot count active challenges")
		}
		if active != 1 {
			t.Fatalf("active challenges = %d, want 1", active)
		}
	})

	t.Run("concurrent replacement and verification are serialized", func(t *testing.T) {
		old := challenge(t, demoClientID)
		start := make(chan struct{})
		newResult := make(chan error, 1)
		verifyResult := make(chan error, 1)
		go func() {
			<-start
			_, err := repo.RequestOTP(ctx, demoClientID)
			newResult <- err
		}()
		go func() {
			<-start
			_, err := repo.VerifyOTP(ctx, old.ID, true)
			verifyResult <- err
		}()
		close(start)
		if err := <-newResult; err != nil {
			t.Fatalf("request racing verification: %v", err)
		}
		if err := <-verifyResult; err != nil && !errors.Is(err, auth.ErrOTPInvalidated) {
			t.Fatalf("verification racing request: %v", err)
		}
		var used, invalidated bool
		if err := pool.QueryRow(ctx, `SELECT used_at IS NOT NULL, invalidated_at IS NOT NULL
			FROM otp_challenges WHERE id=$1`, old.ID).Scan(&used, &invalidated); err != nil {
			t.Fatal("cannot read replaced challenge")
		}
		if used == invalidated {
			t.Fatal("old challenge must have exactly one outcome: used or invalidated")
		}
	})

	t.Run("user reads are authoritative and unknown IDs are safe", func(t *testing.T) {
		user, err := repo.UserByID(ctx, demoEmployeeID)
		if err != nil || user.CompanyID != nil || user.Role != auth.RoleBankEmployee || user.Name != "Мария Банкова" {
			t.Fatalf("read employee: %+v, %v", user, err)
		}
		for _, id := range []string{"not-a-uuid", "' OR true--", "00000000-0000-4000-8000-000000000000"} {
			if _, err := repo.UserByID(ctx, id); !errors.Is(err, auth.ErrUserNotFound) {
				t.Fatalf("unknown user ID: %v", err)
			}
			if _, err := repo.RequestOTP(ctx, id); !errors.Is(err, auth.ErrUserNotFound) {
				t.Fatalf("unknown OTP profile ID: %v", err)
			}
			if _, err := repo.VerifyOTP(ctx, id, true); !errors.Is(err, auth.ErrChallengeNotFound) {
				t.Fatalf("unknown OTP request ID: %v", err)
			}
		}
	})

	t.Run("postgres timeout bounds lock waits and sanitizes errors", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal("cannot start locking transaction")
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SELECT id FROM users WHERE id=$1 FOR UPDATE", demoClientID); err != nil {
			t.Fatal("cannot lock user fixture")
		}
		shortRepo := NewAuthRepository(pool, 50*time.Millisecond)
		started := time.Now()
		if _, err := shortRepo.VerifyOTP(ctx, otp.ID, true); err != auth.ErrStorage {
			t.Fatalf("expected sanitized storage error after timeout, got %v", err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("repository timeout did not bound lock wait")
		}
	})

	t.Run("expiry checked after waiting for user lock", func(t *testing.T) {
		otp := challenge(t, demoClientID)
		if _, err := pool.Exec(ctx, `UPDATE otp_challenges SET expires_at=clock_timestamp()+interval '150 milliseconds'
			WHERE id=$1`, otp.ID); err != nil {
			t.Fatal("cannot shorten OTP expiry fixture")
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal("cannot start locking transaction")
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SELECT id FROM users WHERE id=$1 FOR UPDATE", demoClientID); err != nil {
			t.Fatal("cannot lock user fixture")
		}
		result := make(chan error, 1)
		go func() {
			_, err := repo.VerifyOTP(ctx, otp.ID, true)
			result <- err
		}()
		time.Sleep(250 * time.Millisecond)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal("cannot release user lock")
		}
		if err := <-result; !errors.Is(err, auth.ErrOTPExpired) {
			t.Fatalf("OTP expiring during lock wait: %v", err)
		}
		assertAttemptCount(t, otp.ID, 0)
	})

	t.Run("migration rollback and reapply preserve invalidation", func(t *testing.T) {
		old := challenge(t, demoClientID)
		current := challenge(t, demoClientID)
		for _, direction := range []string{"down", "up"} {
			path := "../../migrations/000003_otp_invalidation." + direction + ".sql"
			migration, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("cannot read OTP invalidation %s migration", direction)
			}
			if _, err := pool.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("cannot execute OTP invalidation %s migration", direction)
			}
		}
		if _, err := repo.VerifyOTP(ctx, old.ID, true); !errors.Is(err, auth.ErrOTPLimit) {
			t.Fatalf("downgrade revived superseded OTP: %v", err)
		}
		assertAttemptCount(t, old.ID, 5)
		if _, err := repo.VerifyOTP(ctx, current.ID, true); err != nil {
			t.Fatalf("current challenge stopped working after migration cycle: %v", err)
		}
	})
}

func concurrently(count int, fn func() error) []error {
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return results
}
