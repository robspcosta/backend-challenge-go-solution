package referenceworker

import (
	"context"
	"errors"
	"example.com/wagering/internal/application"
	"go.uber.org/fx"
	"log/slog"
	"time"
)

func Register(lc fx.Lifecycle, service *application.Service) {
	var cancel context.CancelFunc
	var done chan struct{}
	lc.Append(fx.Hook{OnStart: func(context.Context) error {
		ctx, stop := context.WithCancel(context.Background())
		cancel = stop
		done = make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				workCtx, workCancel := context.WithTimeout(ctx, 8*time.Second)
				_, err := service.ProcessDue(workCtx, 32)
				workCancel()
				if err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("reference processing failed", "error", err)
				}
			}
		}()
		return nil
	}, OnStop: func(ctx context.Context) error {
		if cancel != nil {
			cancel()
		}
		if done == nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
}

var Module = fx.Module("reference_worker", fx.Invoke(Register))
