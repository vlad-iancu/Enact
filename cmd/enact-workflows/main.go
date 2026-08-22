package main

import (
	"context"
	"os"

	"enact/internal/enactworkflows"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactworkflows.Config
	if err := service.Run(context.Background(), &cfg, enactworkflows.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-workflows").Error("service exited", "err", err)
		os.Exit(1)
	}
}
