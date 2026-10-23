package postgres

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/config"
)

func databaseConfig(dsn string) config.Database {
	return config.Database{
		URL: dsn, MaxConns: 3, MinConns: 0,
		ConnectTimeout: 100 * time.Millisecond, PingTimeout: 50 * time.Millisecond,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute,
	}
}

func TestOpenRejectsMalformedDSNWithoutLeakingCredentials(t *testing.T) {
	cfg := databaseConfig("postgres://user:private-password@localhost:invalid-port/database")
	pool, err := Open(context.Background(), cfg)
	if pool != nil {
		pool.Close()
		t.Fatal("malformed DSN returned a pool")
	}
	if err == nil || strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), cfg.URL) {
		t.Fatal("expected a sanitized DSN error")
	}
}

func TestOpenFailsWhenDatabaseIsUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := databaseConfig("postgres://user:private-password@" + address + "/database?sslmode=disable")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pool, err := Open(ctx, cfg)
	if pool != nil {
		pool.Close()
		t.Fatal("unreachable database returned a pool")
	}
	if err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("expected a sanitized connection error")
	}
}

func TestOpenBoundsTimeSpentOnStalledPostgresHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stop := make(chan struct{})
	defer close(stop)
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		// Accept TCP but never answer the PostgreSQL startup packet.
		<-stop
	}()
	cfg := databaseConfig("postgres://user:private-password@" + listener.Addr().String() + "/database?sslmode=disable")
	cfg.ConnectTimeout = 40 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	pool, err := Open(ctx, cfg)
	if pool != nil {
		pool.Close()
		t.Fatal("stalled database returned a pool")
	}
	if err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("expected a sanitized connection timeout")
	}
	if time.Since(started) > time.Second {
		t.Fatal("connection attempt exceeded its configured timeout")
	}
	select {
	case <-accepted:
	default:
		t.Fatal("test did not reach the stalled PostgreSQL handshake")
	}
}

func TestOpenRealPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	cfg := databaseConfig(dsn)
	cfg.ConnectTimeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("opening test PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		// Do not print driver errors: they may include connection details.
		t.Fatal("test PostgreSQL query failed")
	}
	if one != 1 {
		t.Fatalf("unexpected query result: %d", one)
	}
	poolCfg := pool.Config()
	if poolCfg.MaxConns != cfg.MaxConns || poolCfg.MinConns != cfg.MinConns || poolCfg.MaxConnLifetime != cfg.MaxConnLifetime || poolCfg.MaxConnIdleTime != cfg.MaxConnIdleTime || poolCfg.ConnConfig.ConnectTimeout != cfg.ConnectTimeout {
		t.Fatal("pool configuration was not applied")
	}
}
