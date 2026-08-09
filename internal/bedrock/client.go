// Package bedrock wraps Amazon Bedrock's Converse / ConverseStream APIs
// with a small Go-friendly client. It owns the AWS SDK setup, including
// bridging Bedrock API keys into the SDK's AWS_BEARER_TOKEN_BEDROCK env var.
package bedrock

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// Client is a thin wrapper around bedrockruntime.Client exposing Converse /
// ConverseStream over a small, JSON-friendly request shape.
type Client struct {
	api *bedrockruntime.Client
}

// NewClient initialises an AWS SDK client for Bedrock Runtime using cfg. If
// cfg.APIKey is set and AWS_BEARER_TOKEN_BEDROCK is not already in the
// environment, the API key is exported there so the SDK picks it up as the
// Bedrock bearer token.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.APIKey != "" && os.Getenv("AWS_BEARER_TOKEN_BEDROCK") == "" {
		_ = os.Setenv("AWS_BEARER_TOKEN_BEDROCK", cfg.APIKey)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config: %w", err)
	}
	return &Client{api: bedrockruntime.NewFromConfig(awsCfg)}, nil
}

// Message is a single conversational turn passed to Converse.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
}

// Document is a raw file the model reads natively via a Bedrock Converse
// DocumentBlock. Data carries the file's exact bytes — no extraction happens
// on our side. Format must be one of the Converse-supported formats (pdf,
// csv, doc, docx, xls, xlsx, html, txt, md); Name must satisfy Bedrock's
// document-name character rules (callers sanitize).
type Document struct {
	Name   string
	Format string
	Data   []byte
}

// ConverseRequest is the high-level input to Converse / ConverseStream.
// Documents are attached to the LAST user message (Bedrock requires
// documents to ride in a user turn).
type ConverseRequest struct {
	Model        string
	Messages     []Message
	SystemPrompt string
	Documents    []Document
	MaxTokens    int32
	Temperature  *float32
	TopP         *float32
}

// ConverseResponse is the high-level output from Converse.
type ConverseResponse struct {
	Content      string
	StopReason   string
	InputTokens  int32
	OutputTokens int32
}

// StreamChunk is a single event emitted on a streaming Converse call. JSON
// tags are provided so callers can forward chunks straight to an SSE stream.
type StreamChunk struct {
	Delta        string `json:"delta,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	InputTokens  int32  `json:"input_tokens,omitempty"`
	OutputTokens int32  `json:"output_tokens,omitempty"`
	Done         bool   `json:"done,omitempty"`
}

type converseParams struct {
	modelID         *string
	messages        []types.Message
	system          []types.SystemContentBlock
	inferenceConfig *types.InferenceConfiguration
}

func buildConverseParams(req *ConverseRequest) converseParams {
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

	// Documents ride in the last user message, per the Converse API's
	// contract; inference validates that one exists when documents are sent.
	if len(req.Documents) > 0 {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != types.ConversationRoleUser {
				continue
			}
			for _, d := range req.Documents {
				msgs[i].Content = append(msgs[i].Content, &types.ContentBlockMemberDocument{
					Value: types.DocumentBlock{
						Format: types.DocumentFormat(d.Format),
						Name:   aws.String(d.Name),
						Source: &types.DocumentSourceMemberBytes{Value: d.Data},
					},
				})
			}
			break
		}
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

// Converse runs a non-streaming Bedrock conversation turn.
func (c *Client) Converse(ctx context.Context, req *ConverseRequest) (*ConverseResponse, error) {
	p := buildConverseParams(req)
	out, err := c.api.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:         p.modelID,
		Messages:        p.messages,
		System:          p.system,
		InferenceConfig: p.inferenceConfig,
	})
	if err != nil {
		return nil, err
	}

	resp := &ConverseResponse{StopReason: string(out.StopReason)}
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

// ConverseStream runs a streaming Bedrock conversation turn, invoking onChunk
// for each event. A final chunk with Done=true is emitted after the upstream
// stream closes cleanly.
func (c *Client) ConverseStream(ctx context.Context, req *ConverseRequest, onChunk func(StreamChunk) error) error {
	p := buildConverseParams(req)
	out, err := c.api.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
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
