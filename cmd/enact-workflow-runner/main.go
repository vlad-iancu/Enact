package main

import (
	"context"
	"os"

	"enact/internal/enactworkflowrunner"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactworkflowrunner.Config
	if err := service.Run(context.Background(), &cfg, enactworkflowrunner.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-workflow-runner").Error("service exited", "err", err)
		os.Exit(1)
	}
}
