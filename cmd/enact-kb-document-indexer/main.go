package main

import (
	"context"
	"os"

	"enact/internal/enactkbindexer"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactkbindexer.Config
	if err := service.Run(context.Background(), &cfg, enactkbindexer.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-kb-document-indexer").Error("service exited", "err", err)
		os.Exit(1)
	}
}
