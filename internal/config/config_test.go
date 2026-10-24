package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func environment(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(environment(map[string]string{
		"DATABASE_URL": "postgres://user:password@localhost/database",
		"JWT_SECRET":   strings.Repeat("d", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Address() != "127.0.0.1:8080" {
		t.Fatalf("unexpected default HTTP address: %s", cfg.HTTP.Address())
	}
	if cfg.HTTP.ReadHeaderTimeout <= 0 || cfg.HTTP.ReadTimeout <= 0 || cfg.HTTP.WriteTimeout <= 0 || cfg.HTTP.IdleTimeout <= 0 || cfg.HTTP.ShutdownTimeout <= 0 {
		t.Fatal("all server timeouts must have positive defaults")
	}
	if cfg.Database.MaxConns != 10 || cfg.Database.MinConns != 2 {
		t.Fatalf("unexpected default pool limits: max=%d min=%d", cfg.Database.MaxConns, cfg.Database.MinConns)
	}
	if cfg.Database.ConnectTimeout != 5*time.Second || cfg.Database.PingTimeout != 2*time.Second {
		t.Fatal("unexpected default connection/readiness timeout")
	}
	if cfg.Database.PingTimeout >= cfg.HTTP.WriteTimeout {
		t.Fatal("readiness must expire before the response write timeout")
	}
	if cfg.LogLevel != slog.LevelInfo || cfg.DemoOTPCode != "1234" || cfg.JWTSecret != strings.Repeat("d", 32) {
		t.Fatal("unexpected logging or reserved authentication defaults")
	}
	if cfg.DocumentStoragePath == "" || cfg.ScoringURL == "" {
		t.Fatal("future integration settings must have defaults")
	}
	if cfg.ScoringTimeout != 5*time.Second {
		t.Fatal("scoring requests must have a bounded default timeout")
	}
	if cfg.MaxActiveDraftsPerClient != 20 {
		t.Fatal("unexpected default draft limit")
	}
	if cfg.AppEnv != "production" || cfg.JWTSecretFile != "./data/secrets/jwt-secret" {
		t.Fatal("local configuration must default to explicit production secrets")
	}
}

func TestLoadOverridesAndIPv6Address(t *testing.T) {
	cfg, err := load(environment(map[string]string{
		"DATABASE_URL":                 "postgres://custom:password@db/custom",
		"HTTP_HOST":                    "::1",
		"HTTP_PORT":                    "9090",
		"HTTP_READ_HEADER_TIMEOUT":     "1s",
		"HTTP_READ_TIMEOUT":            "3s",
		"HTTP_WRITE_TIMEOUT":           "4s",
		"HTTP_IDLE_TIMEOUT":            "30s",
		"HTTP_SHUTDOWN_TIMEOUT":        "8s",
		"DB_MAX_CONNS":                 "7",
		"DB_MIN_CONNS":                 "0",
		"DB_CONNECT_TIMEOUT":           "300ms",
		"DB_PING_TIMEOUT":              "100ms",
		"DB_MAX_CONN_LIFETIME":         "2h",
		"DB_MAX_CONN_IDLE_TIME":        "5m",
		"LOG_LEVEL":                    "debug",
		"DOCUMENT_STORAGE_PATH":        "/tmp/custom-documents",
		"SCORING_URL":                  "https://scoring.example.test/v1",
		"SCORING_TIMEOUT":              "2s",
		"JWT_SECRET":                   "future-auth-secret-at-least-32-bytes",
		"DEMO_OTP_CODE":                "0123",
		"MAX_ACTIVE_DRAFTS_PER_CLIENT": "7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Address() != "[::1]:9090" {
		t.Fatalf("IPv6 address is invalid: %s", cfg.HTTP.Address())
	}
	if cfg.HTTP.ReadHeaderTimeout != time.Second || cfg.HTTP.ReadTimeout != 3*time.Second || cfg.HTTP.WriteTimeout != 4*time.Second || cfg.HTTP.IdleTimeout != 30*time.Second || cfg.HTTP.ShutdownTimeout != 8*time.Second {
		t.Fatal("HTTP timeout overrides were not applied")
	}
	if cfg.Database.URL != "postgres://custom:password@db/custom" || cfg.Database.MaxConns != 7 || cfg.Database.MinConns != 0 || cfg.Database.ConnectTimeout != 300*time.Millisecond || cfg.Database.PingTimeout != 100*time.Millisecond || cfg.Database.MaxConnLifetime != 2*time.Hour || cfg.Database.MaxConnIdleTime != 5*time.Minute {
		t.Fatal("database overrides were not applied")
	}
	if cfg.LogLevel != slog.LevelDebug || cfg.DocumentStoragePath != "/tmp/custom-documents" || cfg.ScoringURL != "https://scoring.example.test/v1" || cfg.JWTSecret != "future-auth-secret-at-least-32-bytes" || cfg.DemoOTPCode != "0123" {
		t.Fatal("logging or reserved configuration overrides were not applied")
	}
	if cfg.ScoringTimeout != 2*time.Second {
		t.Fatal("scoring timeout override was not applied")
	}
	if cfg.MaxActiveDraftsPerClient != 7 {
		t.Fatal("draft limit override ignored")
	}
}

func TestLoadRejectsInvalidConfigurationWithoutEchoingValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "missing DSN", key: "DATABASE_URL", value: ""},
		{name: "blank DSN", key: "DATABASE_URL", value: " \t "},
		{name: "zero draft limit", key: "MAX_ACTIVE_DRAFTS_PER_CLIENT", value: "0"},
		{name: "negative draft limit", key: "MAX_ACTIVE_DRAFTS_PER_CLIENT", value: "-1"},
		{name: "invalid draft limit", key: "MAX_ACTIVE_DRAFTS_PER_CLIENT", value: "secret-invalid"},
		{name: "excessive draft limit", key: "MAX_ACTIVE_DRAFTS_PER_CLIENT", value: "10001"},
		{name: "unknown app environment", key: "APP_ENV", value: "staging"},
		{name: "empty app environment", key: "APP_ENV", value: ""},
		{name: "port is not numeric", key: "HTTP_PORT", value: "secret-port"},
		{name: "zero port", key: "HTTP_PORT", value: "0"},
		{name: "oversized port", key: "HTTP_PORT", value: "65536"},
		{name: "pool maximum zero", key: "DB_MAX_CONNS", value: "0"},
		{name: "pool maximum overflows int32", key: "DB_MAX_CONNS", value: "2147483648"},
		{name: "negative pool minimum", key: "DB_MIN_CONNS", value: "-1"},
		{name: "pool minimum exceeds maximum", key: "DB_MIN_CONNS", value: "11"},
		{name: "invalid duration", key: "DB_CONNECT_TIMEOUT", value: "secret-duration"},
		{name: "zero ping timeout", key: "DB_PING_TIMEOUT", value: "0s"},
		{name: "negative HTTP timeout", key: "HTTP_READ_HEADER_TIMEOUT", value: "-1s"},
		{name: "readiness exceeds response timeout", key: "DB_PING_TIMEOUT", value: "15s"},
		{name: "empty storage path", key: "DOCUMENT_STORAGE_PATH", value: ""},
		{name: "invalid log level", key: "LOG_LEVEL", value: "secret-log-level"},
		{name: "unsupported scoring scheme", key: "SCORING_URL", value: "ftp://scoring.example.test"},
		{name: "scoring credentials", key: "SCORING_URL", value: "https://user:private-password@scoring.example.test"},
		{name: "scoring query", key: "SCORING_URL", value: "https://scoring.example.test?token=private-token"},
		{name: "scoring fragment", key: "SCORING_URL", value: "https://scoring.example.test#private-token"},
		{name: "scoring without host", key: "SCORING_URL", value: "https:///path"},
		{name: "invalid scoring timeout", key: "SCORING_TIMEOUT", value: "secret-duration"},
		{name: "zero scoring timeout", key: "SCORING_TIMEOUT", value: "0s"},
		{name: "negative scoring timeout", key: "SCORING_TIMEOUT", value: "-1s"},
		{name: "oversized scoring timeout", key: "SCORING_TIMEOUT", value: "61s"},
		{name: "missing JWT secret", key: "JWT_SECRET", value: ""},
		{name: "short JWT secret", key: "JWT_SECRET", value: "private-key"},
		{name: "blank JWT secret", key: "JWT_SECRET", value: strings.Repeat(" ", 32)},
		{name: "empty JWT secret file", key: "JWT_SECRET_FILE", value: ""},
		{name: "empty issuer", key: "JWT_ISSUER", value: ""},
		{name: "empty audience", key: "JWT_AUDIENCE", value: ""},
		{name: "zero auth DB timeout", key: "AUTH_DB_TIMEOUT", value: "0s"},
		{name: "auth exceeds response timeout", key: "AUTH_DB_TIMEOUT", value: "15s"},
		{name: "OTP length", key: "DEMO_OTP_CODE", value: "12345"},
		{name: "OTP letters", key: "DEMO_OTP_CODE", value: "abcd"},
		{name: "OTP non-ASCII", key: "DEMO_OTP_CODE", value: "١٢"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				"DATABASE_URL": "postgres://user:private-password@localhost/database",
				"JWT_SECRET":   "private-jwt-secret-at-least-32-bytes",
			}
			values[test.key] = test.value
			_, err := load(environment(values))
			if err == nil {
				t.Fatal("expected a configuration error")
			}
			if !strings.Contains(err.Error(), test.key) {
				t.Fatalf("error must identify the setting: %v", err)
			}
			for _, sensitive := range []string{"private-password", "private-token", "private-jwt-secret-at-least-32-bytes", "secret-port", "secret-duration", "secret-log-level"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatal("configuration error leaked a supplied value")
				}
			}
		})
	}
}

func TestLoadDemoInitializesPersistentSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "jwt-secret")
	values := map[string]string{
		"DATABASE_URL":    "postgres://user:password@localhost/database",
		"APP_ENV":         "demo",
		"JWT_SECRET_FILE": path,
	}
	first, err := load(environment(values))
	if err != nil {
		t.Fatal(err)
	}
	values["JWT_SECRET"] = "" // Compose passes an empty value when no override is configured.
	second, err := load(environment(values))
	if err != nil {
		t.Fatal(err)
	}
	if first.AppEnv != "demo" || first.JWTSecretFile != path || first.JWTSecret != second.JWTSecret || len(first.JWTSecret) < 32 {
		t.Fatal("demo configuration must reuse the persisted random secret")
	}
}

func TestLoadDoesNotInitializeSecretForInvalidConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist", "jwt-secret")
	_, err := load(environment(map[string]string{
		"DATABASE_URL":    "postgres://user:password@localhost/database",
		"APP_ENV":         "demo",
		"JWT_SECRET_FILE": path,
		"DEMO_OTP_CODE":   "invalid",
	}))
	if err == nil || !strings.Contains(err.Error(), "DEMO_OTP_CODE") {
		t.Fatal("expected OTP configuration error")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("invalid configuration must not create the secret directory")
	}
}

func TestLoadDemoDoesNotFallbackFromInvalidExplicitSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist", "jwt-secret")
	_, err := load(environment(map[string]string{
		"DATABASE_URL":    "postgres://user:password@localhost/database",
		"APP_ENV":         "demo",
		"JWT_SECRET_FILE": path,
		"JWT_SECRET":      "invalid-explicit-secret",
	}))
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatal("invalid explicit JWT_SECRET must fail")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("invalid explicit secret must not trigger automatic generation")
	}
}
