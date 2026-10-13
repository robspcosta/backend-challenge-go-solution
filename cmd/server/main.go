package main

import (
	"example.com/wagering/internal/app"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"log/slog"
	"os"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	fx.New(app.Options(), fx.WithLogger(func() fxevent.Logger { return &fxevent.SlogLogger{Logger: logger} })).Run()
}
