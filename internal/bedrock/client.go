// Package bedrock wraps Amazon Bedrock's Converse / ConverseStream APIs
// with a small Go-friendly client. It owns the AWS SDK setup, including
// bridging Bedrock API keys into the SDK's AWS_BEARER_TOKEN_BEDROCK env var.
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
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

// Message is a single conversational turn passed to Converse. Beyond plain
// text, an assistant turn may carry the tool calls the model requested
// (ToolUses) and a user turn may carry their outcomes (ToolResults) — the
// shapes a Converse tool-use loop threads back into the conversation.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
	// ToolUses are the model's tool invocations (assistant turns).
	ToolUses []ToolUse
	// ToolResults answer earlier tool uses (user turns).
	ToolResults []ToolResult
}

// ToolSpec advertises one callable tool to the model.
type ToolSpec struct {
	// Name must satisfy Bedrock's tool-name rules (letters, digits, _ -).
	Name        string
	Description string
	// InputSchema is the tool's JSON Schema, passed to the model verbatim.
	InputSchema json.RawMessage
}

// ToolUse is one tool invocation requested by the model.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the outcome of one tool invocation, fed back to the model.
//
// Content and Structured are what the tool actually returned — MCP servers
// may send text, a structured object, or both, and both are forwarded as
// their own content blocks so the model reads JSON as JSON rather than as a
// string that happens to look like it.
type ToolResult struct {
	ToolUseID  string
	Content    string
	Structured json.RawMessage
	IsError    bool
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
	// Tools the model may call; a "tool_use" stop reason then carries the
	// requested invocations.
	Tools       []ToolSpec
	MaxTokens   int32
	Temperature *float32
	TopP        *float32
}

// ConverseResponse is the high-level output from Converse.
type ConverseResponse struct {
	Content      string
	ToolUses     []ToolUse
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
	toolConfig      *types.ToolConfiguration
}

// rawToDocument converts raw JSON into the SDK's document representation
// (marshaling a json.RawMessage directly would base64 the bytes).
func rawToDocument(raw json.RawMessage) document.Interface {
	var v any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &v)
	}
	if v == nil {
		v = map[string]any{}
	}
	return document.NewLazyDocument(v)
}

// documentToRaw extracts the JSON of an SDK document.
func documentToRaw(doc document.Interface) json.RawMessage {
	if doc == nil {
		return nil
	}
	raw, err := doc.MarshalSmithyDocument()
	if err != nil {
		return nil
	}
	return raw
}

func buildConverseParams(req *ConverseRequest) converseParams {
	msgs := make([]types.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := types.ConversationRoleUser
		if m.Role == "assistant" {
			role = types.ConversationRoleAssistant
		}
		var content []types.ContentBlock
		if m.Content != "" || (len(m.ToolUses) == 0 && len(m.ToolResults) == 0) {
			content = append(content, &types.ContentBlockMemberText{Value: m.Content})
		}
		for _, tu := range m.ToolUses {
			content = append(content, &types.ContentBlockMemberToolUse{
				Value: types.ToolUseBlock{
					ToolUseId: aws.String(tu.ID),
					Name:      aws.String(tu.Name),
					Input:     rawToDocument(tu.Input),
				},
			})
		}
		for _, tr := range m.ToolResults {
			status := types.ToolResultStatusSuccess
			if tr.IsError {
				status = types.ToolResultStatusError
			}
			// A result block carries a list, so a tool that returned both
			// prose and data is represented as both — no flattening, and no
			// duplicating one as the other.
			var blocks []types.ToolResultContentBlock
			if tr.Content != "" {
				blocks = append(blocks, &types.ToolResultContentBlockMemberText{Value: tr.Content})
			}
			if len(tr.Structured) > 0 {
				blocks = append(blocks, &types.ToolResultContentBlockMemberJson{Value: rawToDocument(tr.Structured)})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, &types.ToolResultContentBlockMemberText{Value: tr.Content})
			}
			content = append(content, &types.ContentBlockMemberToolResult{
				Value: types.ToolResultBlock{
					ToolUseId: aws.String(tr.ToolUseID),
					Status:    status,
					Content:   blocks,
				},
			})
		}
		msgs = append(msgs, types.Message{Role: role, Content: content})
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
	if len(req.Tools) > 0 {
		specs := make([]types.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			spec := types.ToolSpecification{
				Name:        aws.String(t.Name),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: rawToDocument(t.InputSchema)},
			}
			if t.Description != "" {
				spec.Description = aws.String(t.Description)
			}
			specs = append(specs, &types.ToolMemberToolSpec{Value: spec})
		}
		p.toolConfig = &types.ToolConfiguration{Tools: specs}
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
		ToolConfig:      p.toolConfig,
	})
	if err != nil {
		return nil, err
	}

	resp := &ConverseResponse{StopReason: string(out.StopReason)}
	if outputMsg, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
		for _, block := range outputMsg.Value.Content {
			switch b := block.(type) {
			case *types.ContentBlockMemberText:
				resp.Content += b.Value
			case *types.ContentBlockMemberToolUse:
				resp.ToolUses = append(resp.ToolUses, ToolUse{
					ID:    aws.ToString(b.Value.ToolUseId),
					Name:  aws.ToString(b.Value.Name),
					Input: documentToRaw(b.Value.Input),
				})
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

// StreamResult is the accumulated outcome of one streaming turn: the full
// assistant text, any tool invocations the model requested, and the turn's
// stop reason and token usage. Callers running a tool loop feed Content +
// ToolUses back as the assistant message.
type StreamResult struct {
	Content      string
	ToolUses     []ToolUse
	StopReason   string
	InputTokens  int32
	OutputTokens int32
}

// ConverseStream runs one streaming Bedrock conversation turn, invoking
// onChunk for each text delta / stop / usage event, and returns the
// accumulated turn. It does NOT emit a Done chunk — a tool loop spans
// several turns, so end-of-response is the caller's call.
func (c *Client) ConverseStream(ctx context.Context, req *ConverseRequest, onChunk func(StreamChunk) error) (*StreamResult, error) {
	p := buildConverseParams(req)
	out, err := c.api.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:         p.modelID,
		Messages:        p.messages,
		System:          p.system,
		InferenceConfig: p.inferenceConfig,
		ToolConfig:      p.toolConfig,
	})
	if err != nil {
		return nil, err
	}
	stream := out.GetStream()
	defer stream.Close()

	result := &StreamResult{}
	var content strings.Builder
	// One tool-use block accumulates between ContentBlockStart and
	// ContentBlockStop; its input JSON arrives as string fragments.
	var currentTool *ToolUse
	var currentInput strings.Builder

	for event := range stream.Events() {
		switch e := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockStart:
			if ts, ok := e.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
				currentTool = &ToolUse{
					ID:   aws.ToString(ts.Value.ToolUseId),
					Name: aws.ToString(ts.Value.Name),
				}
				currentInput.Reset()
			}
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			switch td := e.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				content.WriteString(td.Value)
				if err := onChunk(StreamChunk{Delta: td.Value}); err != nil {
					return nil, err
				}
			case *types.ContentBlockDeltaMemberToolUse:
				if currentTool != nil && td.Value.Input != nil {
					currentInput.WriteString(*td.Value.Input)
				}
			}
		case *types.ConverseStreamOutputMemberContentBlockStop:
			if currentTool != nil {
				input := currentInput.String()
				if input == "" {
					input = "{}"
				}
				currentTool.Input = json.RawMessage(input)
				result.ToolUses = append(result.ToolUses, *currentTool)
				currentTool = nil
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			result.StopReason = string(e.Value.StopReason)
			if err := onChunk(StreamChunk{StopReason: result.StopReason}); err != nil {
				return nil, err
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
			result.InputTokens += chunk.InputTokens
			result.OutputTokens += chunk.OutputTokens
			if err := onChunk(chunk); err != nil {
				return nil, err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	result.Content = content.String()
	return result, nil
}
