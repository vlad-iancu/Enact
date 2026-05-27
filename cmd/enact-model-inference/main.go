package main

import (
	"context"
	"log"

	"enact/internal/enactmodelinference"
	"enact/internal/service"
)

func main() {
	if err := service.Run(context.Background(), enactmodelinference.Build); err != nil {
		log.Fatal(err)
	}
}
