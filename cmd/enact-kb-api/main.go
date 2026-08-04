package main

import (
	"context"
	"os"

	"enact/internal/enactkbapi"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactkbapi.Config
	if err := service.Run(context.Background(), &cfg, enactkbapi.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-kb-api").Error("service exited", "err", err)
		os.Exit(1)
	}
}
