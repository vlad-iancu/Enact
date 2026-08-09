// Package ses wraps AWS Simple Email Service for the platform's
// transactional email (currently account verification), mirroring the
// bedrock/s3 packages' shape: an env-driven Config and a thin Client.
package ses

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsses "github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// Config holds the environment-driven SES settings. Credentials come from
// the standard AWS chain. The sender address must be a verified SES
// identity; in SES sandbox mode, recipients must be verified too.
type Config struct {
	// Region of the SES identity; shares AWS_REGION with the other AWS
	// clients.
	Region string `env:"AWS_REGION"`
	// From is the sender address (a verified SES identity). Required when
	// email verification is enabled.
	From string `env:"SES_FROM_EMAIL"`
}

// Client sends email through SES.
type Client struct {
	api  *awsses.Client
	from string
}

// NewClient loads AWS configuration and returns a Client. Construction does
// not touch the network; identity/credential problems surface on first send.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("ses: load aws config: %w", err)
	}
	return &Client{api: awsses.NewFromConfig(awsCfg), from: cfg.From}, nil
}

// Send delivers a plain-text email to one recipient.
func (c *Client) Send(ctx context.Context, to, subject, body string) error {
	_, err := c.api.SendEmail(ctx, &awsses.SendEmailInput{
		FromEmailAddress: aws.String(c.from),
		Destination:      &types.Destination{ToAddresses: []string{to}},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{Data: aws.String(subject)},
				Body:    &types.Body{Text: &types.Content{Data: aws.String(body)}},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("ses: send to %s: %w", to, err)
	}
	return nil
}
