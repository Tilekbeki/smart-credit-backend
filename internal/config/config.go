// Package config loads and validates process configuration from the environment.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type HTTP struct {
	Host              string
	Port              int
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

func (h HTTP) Address() string { return net.JoinHostPort(h.Host, strconv.Itoa(h.Port)) }

type Database struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	ConnectTimeout  time.Duration
	PingTimeout     time.Duration
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

type Config struct {
	AppEnv                   string
	HTTP                     HTTP
	Database                 Database
	LogLevel                 slog.Level
	DocumentStoragePath      string
	ScoringURL               string
	ScoringTimeout           time.Duration
	JWTSecret                string
	JWTSecretFile            string
	JWTIssuer                string
	JWTAudience              string
	AuthDBTimeout            time.Duration
	DemoOTPCode              string
	MaxActiveDraftsPerClient int
}

func Load() (Config, error) { return load(os.LookupEnv) }

func load(lookup func(string) (string, bool)) (Config, error) {
	var cfg Config
	var firstErr error
	get := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}
		return fallback
	}
	invalid := func(key, reason string) {
		if firstErr == nil {
			// Do not echo values: configuration can contain credentials.
			firstErr = fmt.Errorf("%s: %s", key, reason)
		}
	}
	integer := func(key, fallback string, min, max int64) int64 {
		n, err := strconv.ParseInt(get(key, fallback), 10, 64)
		if err != nil || n < min || n > max {
			invalid(key, fmt.Sprintf("must be an integer between %d and %d", min, max))
		}
		return n
	}
	duration := func(key, fallback string) time.Duration {
		d, err := time.ParseDuration(get(key, fallback))
		if err != nil || d <= 0 {
			invalid(key, "must be a positive duration, for example 5s")
		}
		return d
	}
	cfg.AppEnv = get("APP_ENV", "production")
	cfg.MaxActiveDraftsPerClient = int(integer("MAX_ACTIVE_DRAFTS_PER_CLIENT", "20", 1, 10000))
	if cfg.AppEnv != "demo" && cfg.AppEnv != "production" {
		invalid("APP_ENV", "must be demo or production")
	}
	cfg.HTTP = HTTP{
		Host:              get("HTTP_HOST", "127.0.0.1"),
		Port:              int(integer("HTTP_PORT", "8080", 1, 65535)),
		ReadHeaderTimeout: duration("HTTP_READ_HEADER_TIMEOUT", "5s"),
		ReadTimeout:       duration("HTTP_READ_TIMEOUT", "10s"),
		WriteTimeout:      duration("HTTP_WRITE_TIMEOUT", "15s"),
		IdleTimeout:       duration("HTTP_IDLE_TIMEOUT", "60s"),
		ShutdownTimeout:   duration("HTTP_SHUTDOWN_TIMEOUT", "15s"),
	}
	cfg.Database = Database{
		URL:             get("DATABASE_URL", ""),
		MaxConns:        int32(integer("DB_MAX_CONNS", "10", 1, 2147483647)),
		MinConns:        int32(integer("DB_MIN_CONNS", "2", 0, 2147483647)),
		ConnectTimeout:  duration("DB_CONNECT_TIMEOUT", "5s"),
		PingTimeout:     duration("DB_PING_TIMEOUT", "2s"),
		MaxConnLifetime: duration("DB_MAX_CONN_LIFETIME", "1h"),
		MaxConnIdleTime: duration("DB_MAX_CONN_IDLE_TIME", "15m"),
	}
	if strings.TrimSpace(cfg.Database.URL) == "" {
		invalid("DATABASE_URL", "is required")
	}
	if cfg.Database.MinConns > cfg.Database.MaxConns {
		invalid("DB_MIN_CONNS", "must not exceed DB_MAX_CONNS")
	}
	if cfg.Database.PingTimeout >= cfg.HTTP.WriteTimeout {
		invalid("DB_PING_TIMEOUT", "must be less than HTTP_WRITE_TIMEOUT")
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		invalid("LOG_LEVEL", "must be a slog level: debug, info, warn or error")
	}
	cfg.DocumentStoragePath = get("DOCUMENT_STORAGE_PATH", "./data/documents")
	if strings.TrimSpace(cfg.DocumentStoragePath) == "" {
		invalid("DOCUMENT_STORAGE_PATH", "must not be empty")
	}
	cfg.ScoringURL = get("SCORING_URL", "http://127.0.0.1:8001")
	u, err := url.Parse(cfg.ScoringURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		invalid("SCORING_URL", "must be an HTTP(S) base URL without credentials, query or fragment")
	}
	cfg.ScoringTimeout = duration("SCORING_TIMEOUT", "5s")
	if cfg.ScoringTimeout > time.Minute {
		invalid("SCORING_TIMEOUT", "must not exceed 1m")
	}
	cfg.JWTSecret = get("JWT_SECRET", "")
	if cfg.JWTSecret != "" && !validJWTSecret(cfg.JWTSecret) {
		invalid("JWT_SECRET", "must contain at least 32 bytes")
	}
	if cfg.JWTSecret == "" && cfg.AppEnv == "production" {
		invalid("JWT_SECRET", "is required in production and must contain at least 32 bytes")
	}
	cfg.JWTSecretFile = get("JWT_SECRET_FILE", "./data/secrets/jwt-secret")
	if strings.TrimSpace(cfg.JWTSecretFile) == "" {
		invalid("JWT_SECRET_FILE", "must not be empty")
	}
	cfg.JWTIssuer = get("JWT_ISSUER", "smart-credit-backend")
	if strings.TrimSpace(cfg.JWTIssuer) == "" {
		invalid("JWT_ISSUER", "must not be empty")
	}
	cfg.JWTAudience = get("JWT_AUDIENCE", "smart-credit-frontend")
	if strings.TrimSpace(cfg.JWTAudience) == "" {
		invalid("JWT_AUDIENCE", "must not be empty")
	}
	cfg.AuthDBTimeout = duration("AUTH_DB_TIMEOUT", "3s")
	if cfg.AuthDBTimeout >= cfg.HTTP.WriteTimeout {
		invalid("AUTH_DB_TIMEOUT", "must be less than HTTP_WRITE_TIMEOUT")
	}
	cfg.DemoOTPCode = get("DEMO_OTP_CODE", "1234")
	if len(cfg.DemoOTPCode) != 4 || strings.IndexFunc(cfg.DemoOTPCode, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		invalid("DEMO_OTP_CODE", "must contain four digits")
	}
	if firstErr != nil {
		return cfg, firstErr
	}
	// Initialize persistent demo state only after all configuration is valid.
	cfg.JWTSecret, err = resolveJWTSecret(cfg.AppEnv, cfg.JWTSecret, cfg.JWTSecretFile)
	return cfg, err
}
