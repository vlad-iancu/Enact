package enactmodelinference

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

type bedrockClient struct {
	api *bedrockruntime.Client
}

func newBedrockClient(ctx context.Context, region string) (*bedrockClient, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &bedrockClient{api: bedrockruntime.NewFromConfig(cfg)}, nil
}

type converseParams struct {
	modelID         *string
	messages        []types.Message
	system          []types.SystemContentBlock
	inferenceConfig *types.InferenceConfiguration
}

func buildConverseParams(req *InferenceRequest) converseParams {
	msgs := make([]types.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := types.ConversationRoleUser
		if m.Role == "assistant" {
			role = types.ConversationRoleAssistant
		}
		msgs = append(msgs, types.Message{
			Role:    role,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}},
		})
	}

	p := converseParams{
		modelID:  aws.String(req.Model),
		messages: msgs,
	}
	if req.SystemPrompt != "" {
		p.system = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: req.SystemPrompt}}
	}

	cfg := &types.InferenceConfiguration{}
	hasCfg := false
	if req.MaxTokens > 0 {
		cfg.MaxTokens = aws.Int32(req.MaxTokens)
		hasCfg = true
	}
	if req.Temperature != nil {
		cfg.Temperature = req.Temperature
		hasCfg = true
	}
	if req.TopP != nil {
		cfg.TopP = req.TopP
		hasCfg = true
	}
	if hasCfg {
		p.inferenceConfig = cfg
	}
	return p
}

func (b *bedrockClient) Converse(ctx context.Context, req *InferenceRequest) (*InferenceResponse, error) {
	p := buildConverseParams(req)
	out, err := b.api.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:         p.modelID,
		Messages:        p.messages,
		System:          p.system,
		InferenceConfig: p.inferenceConfig,
	})
	if err != nil {
		return nil, err
	}

	resp := &InferenceResponse{
		Model:      req.Model,
		StopReason: string(out.StopReason),
	}
	if outputMsg, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
		for _, block := range outputMsg.Value.Content {
			if textBlock, ok := block.(*types.ContentBlockMemberText); ok {
				resp.Content += textBlock.Value
			}
		}
	}
	if out.Usage != nil {
		if out.Usage.InputTokens != nil {
			resp.InputTokens = *out.Usage.InputTokens
		}
		if out.Usage.OutputTokens != nil {
			resp.OutputTokens = *out.Usage.OutputTokens
		}
	}
	return resp, nil
}

func (b *bedrockClient) ConverseStream(ctx context.Context, req *InferenceRequest, onChunk func(StreamChunk) error) error {
	p := buildConverseParams(req)
	out, err := b.api.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:         p.modelID,
		Messages:        p.messages,
		System:          p.system,
		InferenceConfig: p.inferenceConfig,
	})
	if err != nil {
		return err
	}
	stream := out.GetStream()
	defer stream.Close()

	for event := range stream.Events() {
		switch e := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			if td, ok := e.Value.Delta.(*types.ContentBlockDeltaMemberText); ok {
				if err := onChunk(StreamChunk{Delta: td.Value}); err != nil {
					return err
				}
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			if err := onChunk(StreamChunk{StopReason: string(e.Value.StopReason)}); err != nil {
				return err
			}
		case *types.ConverseStreamOutputMemberMetadata:
			chunk := StreamChunk{}
			if e.Value.Usage != nil {
				if e.Value.Usage.InputTokens != nil {
					chunk.InputTokens = *e.Value.Usage.InputTokens
				}
				if e.Value.Usage.OutputTokens != nil {
					chunk.OutputTokens = *e.Value.Usage.OutputTokens
				}
			}
			if err := onChunk(chunk); err != nil {
				return err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	return onChunk(StreamChunk{Done: true})
}
