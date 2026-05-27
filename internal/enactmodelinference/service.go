// Package enactmodelinference exposes the Bedrock-backed model inference
// service. Build is a service.Builder suitable to hand straight to
// service.Run from a cmd's main().
package enactmodelinference

import (
	"context"
	"os"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal"
	"enact/internal/service"
)

// Build constructs the inference web service. It loads BedrockConfig from the
// environment, bridges BEDROCK_API_KEY into AWS_BEARER_TOKEN_BEDROCK for the
// AWS SDK, initialises the Bedrock client, and returns the WebService.
func Build(ctx context.Context) ([]*restful.WebService, error) {
	var cfg internal.BedrockConfig
	if err := service.Load(&cfg); err != nil {
		return nil, err
	}
	// The AWS SDK reads bedrock bearer-token credentials from this variable.
	if os.Getenv("AWS_BEARER_TOKEN_BEDROCK") == "" {
		_ = os.Setenv("AWS_BEARER_TOKEN_BEDROCK", cfg.BedrockAPIKey)
	}

	client, err := newBedrockClient(ctx, cfg.Region)
	if err != nil {
		return nil, err
	}
	return []*restful.WebService{newInferenceAPI(client).WebService()}, nil
}
