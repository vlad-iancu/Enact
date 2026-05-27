package internal

type BedrockConfig struct {
	Region        string `env:"AWS_REGION,required"`
	BedrockAPIKey string `env:"BEDROCK_API_KEY,required"`
}
