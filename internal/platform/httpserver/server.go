package httpserver

import (
	"context"
	"errors"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/config"
	"example.com/wagering/internal/platform/auth"
	"example.com/wagering/internal/platform/httpapi"
	"example.com/wagering/internal/platform/messaging"
	"example.com/wagering/internal/platform/metrics"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"log/slog"
	"net"
	"net/http"
	"time"
)

func Register(lc fx.Lifecycle, c config.Config, pool *pgxpool.Pool, sqs *messaging.Transport, identity *auth.Authenticator, shutdown fx.Shutdowner, service *application.Service, meter *metrics.Registry) {
	mux := http.NewServeMux()
	httpapi.New(service).Register(mux)
	mux.Handle("GET /metrics", meter)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if err := sqs.Ready(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if err := identity.Ready(); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Addr: c.HTTPAddress, Handler: identity.Wrap(mux, service), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan struct{})
	lc.Append(fx.Hook{OnStart: func(context.Context) error {
		listener, err := net.Listen("tcp", c.HTTPAddress)
		if err != nil {
			return err
		}
		go func() {
			defer close(done)
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("HTTP server stopped", "error", err)
				_ = shutdown.Shutdown(fx.ExitCode(1))
			}
		}()
		return nil
	}, OnStop: func(ctx context.Context) error {
		err := server.Shutdown(ctx)
		if err != nil {
			_ = server.Close()
		}
		select {
		case <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
}

var Module = fx.Module("http", fx.Invoke(Register))
