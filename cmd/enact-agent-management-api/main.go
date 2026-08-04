package main

import (
	"context"
	"os"

	"enact/internal/enactagentapi"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactagentapi.Config
	if err := service.Run(context.Background(), &cfg, enactagentapi.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-agent-management-api").Error("service exited", "err", err)
		os.Exit(1)
	}
}
