// Command enact-mcp-servers hosts the MCP servers the platform ships itself.
package main

import (
	"context"
	"os"

	"enact/internal/enactmcpservers"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactmcpservers.Config
	if err := service.Run(context.Background(), &cfg, enactmcpservers.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-mcp-servers").Error("service exited", "err", err)
		os.Exit(1)
	}
}
