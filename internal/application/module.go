package application

import "go.uber.org/fx"

var Module = fx.Module("usecases", fx.Provide(NewWithMetrics))
