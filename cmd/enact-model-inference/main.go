package main

import (
	"context"
	"os"

	"enact/internal/enactmodelinference"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactmodelinference.Config
	if err := service.Run(context.Background(), &cfg, enactmodelinference.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-model-inference").Error("service exited", "err", err)
		os.Exit(1)
	}
}
