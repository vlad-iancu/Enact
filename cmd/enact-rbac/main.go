// enact-rbac is the authorization service: organizations, the requests to
// create them, memberships, roles and the rules they grant. Every other
// service asks it "may this user do this?" through internal/rbac's client.
package main

import (
	"context"
	"os"

	"enact/internal/enactrbac"
	"enact/internal/logging"
	"enact/internal/service"
)

func main() {
	var cfg enactrbac.Config
	if err := service.Run(context.Background(), &cfg, enactrbac.Build(&cfg)); err != nil {
		logging.New().WithFields("service", "enact-rbac").Error("service exited", "err", err)
		os.Exit(1)
	}
}
