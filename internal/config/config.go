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
	HTTP                HTTP
	Database            Database
	LogLevel            slog.Level
	DocumentStoragePath string
	ScoringURL          string
	JWTSecret           string
	DemoOTPCode         string
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
	// Reserved settings: stage 1 does not enable authentication or contact scoring.
	cfg.JWTSecret = get("JWT_SECRET", "")
	cfg.DemoOTPCode = get("DEMO_OTP_CODE", "1234")
	if len(cfg.DemoOTPCode) != 4 || strings.IndexFunc(cfg.DemoOTPCode, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		invalid("DEMO_OTP_CODE", "must contain four digits")
	}
	return cfg, firstErr
}
