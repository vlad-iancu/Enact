package main

import (
	"context"
	"os"

	"enact/internal/enactmain"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactmain.Config
	if err := service.Run(context.Background(), &cfg, enactmain.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-main").Error("service exited", "err", err)
		os.Exit(1)
	}
}
