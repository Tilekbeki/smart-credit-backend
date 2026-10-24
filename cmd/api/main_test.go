package main

import (
	"strings"
	"testing"
)

func TestProductionFailsBeforeDemoProvidersStart(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", strings.Repeat("test-key-", 8))
	t.Setenv("DATABASE_URL", "postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err := run(); err == nil || !strings.Contains(err.Error(), "production integrations are not configured") {
		t.Fatalf("production must fail closed before DB/demo initialization: %v", err)
	}
}
