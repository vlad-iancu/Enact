// enact-external-identities is the external identity service: it stores the
// credentials the platform holds on a user's behalf at third parties,
// drives the OAuth consent flow, and refreshes tokens before they expire.
package main

import (
	"context"
	"os"

	"enact/internal/enactexternalidentities"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactexternalidentities.Config
	if err := service.Run(context.Background(), &cfg, enactexternalidentities.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-external-identities").Error("service exited", "err", err)
		os.Exit(1)
	}
}
