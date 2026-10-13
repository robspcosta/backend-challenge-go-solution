package app

import (
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/config"
	"example.com/wagering/internal/platform/auth"
	"example.com/wagering/internal/platform/httpserver"
	"example.com/wagering/internal/platform/messaging"
	"example.com/wagering/internal/platform/metrics"
	"example.com/wagering/internal/platform/postgres"
	"example.com/wagering/internal/platform/referenceworker"
	"example.com/wagering/internal/platform/repository"
	"go.uber.org/fx"
	"time"
)

func Options() fx.Option {
	return fx.Options(config.Module, postgres.Module, repository.Module, metrics.Module, application.Module, auth.Module, messaging.Module, referenceworker.Module, httpserver.Module, fx.StartTimeout(20*time.Second), fx.StopTimeout(20*time.Second))
}
