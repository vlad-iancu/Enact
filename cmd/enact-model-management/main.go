package main

import (
	"context"
	"os"

	"enact/internal/enactmodelmanagement"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactmodelmanagement.Config
	if err := service.Run(context.Background(), &cfg, enactmodelmanagement.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-model-management").Error("service exited", "err", err)
		os.Exit(1)
	}
}
