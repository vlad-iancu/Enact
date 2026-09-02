// Command enact-crawls serves the focused-crawl API: authoring crawl
// definitions and queueing runs. The crawling itself is done by
// enact-crawl-orchestrator.
package main

import (
	"context"
	"os"

	"enact/internal/enactcrawls"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactcrawls.Config
	if err := service.Run(context.Background(), &cfg, enactcrawls.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-crawls").Error("service exited", "err", err)
		os.Exit(1)
	}
}
