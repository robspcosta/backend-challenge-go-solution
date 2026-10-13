package postgres

import (
	"context"
	"example.com/wagering/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

func New(lc fx.Lifecycle, c config.Config) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.DatabaseURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return err
		}
		return nil
	}, OnStop: func(context.Context) error { pool.Close(); return nil }})
	return pool, nil
}

var Module = fx.Module("postgres", fx.Provide(New))
