package bedrock

// ClientConfig holds the environment-driven settings for connecting to
// Amazon Bedrock. APIKey is propagated to the AWS SDK via the
// AWS_BEARER_TOKEN_BEDROCK environment variable inside NewClient.
//
// The type is named ClientConfig (rather than the more obvious Config) so it
// can be embedded alongside service.Config without a field-name collision.
type ClientConfig struct {
	Region string `env:"AWS_REGION, required"`
	APIKey string `env:"BEDROCK_API_KEY, required"`
}
