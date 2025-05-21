package shared

import (
	"github.com/DODOEX/web3rpcproxy/internal/app/database"
	"go.uber.org/fx"
)

var NewSharedModule = fx.Options(
	fx.Provide(NewConfInstance),
	fx.Provide(
		fx.Annotate(
			NewConfInstance,
			fx.ResultTags(`name:"config"`),
		),
	),
	fx.Provide(NewLogger),
	fx.Provide(
		fx.Annotate(
			NewLogger,
			fx.ResultTags(`name:"logger"`),
		),
	),
	fx.Provide(NewEtcdClient),
	fx.Provide(NewTransport),
	fx.Provide(NewWatcherClientInstance),
	fx.Provide(database.NewDatabase),
	fx.Provide(NewRedisClient),
	fx.Provide(
		fx.Annotate(
			NewRedisClient,
			fx.ResultTags(`name:"redis"`),
		),
	),
	fx.Provide(NewRedisScripts),
	fx.Provide(NewRabbitMQ),
)
