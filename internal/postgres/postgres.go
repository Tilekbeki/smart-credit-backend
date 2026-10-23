// Package postgres owns the PostgreSQL connection pool lifecycle.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/config"
)

func Open(ctx context.Context, cfg config.Database) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx parse errors can include the original DSN. Never propagate it to logs.
		return nil, errors.New("invalid DATABASE_URL")
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, errors.New("cannot create PostgreSQL pool")
	}
	// NewWithConfig is lazy; explicitly verify that the database is reachable.
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, errors.New("cannot connect to PostgreSQL within DB_CONNECT_TIMEOUT")
	}
	return pool, nil
}
