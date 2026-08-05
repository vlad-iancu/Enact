package main

import (
	"context"
	"os"

	"enact/internal/enacttests"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enacttests.Config
	if err := service.Run(context.Background(), &cfg, enacttests.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-tests").Error("service exited", "err", err)
		os.Exit(1)
	}
}