// enact-tool-registry is the MCP tool registry service: MCP server records,
// the cached tool catalogue, and the MCP proxy.
package main

import (
	"context"
	"os"

	"enact/internal/enacttoolregistry"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enacttoolregistry.Config
	if err := service.Run(context.Background(), &cfg, enacttoolregistry.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-tool-registry").Error("service exited", "err", err)
		os.Exit(1)
	}
}
