package bedrock

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// titanEmbedRequest is the InvokeModel request body for Amazon Titan Text
// Embeddings. Titan Embeddings v2 accepts an optional dimensions field; we
// leave it unset and rely on the model's default, which the caller must keep
// in sync with the OpenSearch knn_vector dimension.
type titanEmbedRequest struct {
	InputText string `json:"inputText"`
}

type titanEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed returns the embedding vector for text using the given Bedrock
// embedding model (e.g. "amazon.titan-embed-text-v2:0"), via InvokeModel.
func (c *Client) Embed(ctx context.Context, modelID, text string) ([]float32, error) {
	body, err := json.Marshal(titanEmbedRequest{InputText: text})
	if err != nil {
		return nil, fmt.Errorf("bedrock: marshal embed request: %w", err)
	}
	out, err := c.api.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelID),
		Body:        body,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke embedding model: %w", err)
	}
	var parsed titanEmbedResponse
	if err := json.Unmarshal(out.Body, &parsed); err != nil {
		return nil, fmt.Errorf("bedrock: decode embedding response: %w", err)
	}
	if len(parsed.Embedding) == 0 {
		return nil, fmt.Errorf("bedrock: embedding model returned no vector")
	}
	return parsed.Embedding, nil
}
