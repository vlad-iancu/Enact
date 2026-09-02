// Command enact-crawl-orchestrator schedules focused crawls and executes
// them. It exposes no API beyond the standard health probe.
package main

import (
	"context"
	"os"

	"enact/internal/enactcrawlorchestrator"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactcrawlorchestrator.Config
	if err := service.Run(context.Background(), &cfg, enactcrawlorchestrator.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-crawl-orchestrator").Error("service exited", "err", err)
		os.Exit(1)
	}
}
