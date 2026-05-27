package service

import (
	"testing"
	"time"
)

type bedrockCfg struct {
	Region        string `env:"TEST_REGION, required"`
	BedrockAPIKey string `env:"TEST_BEDROCK_API_KEY, required"`
}

type fullCfg struct {
	Name    string        `env:"TEST_NAME, default=service"`
	Port    int           `env:"TEST_PORT, default=8080"`
	Debug   bool          `env:"TEST_DEBUG"`
	Timeout time.Duration `env:"TEST_TIMEOUT, default=5s"`
	Tags    []string      `env:"TEST_TAGS"`
}

func TestLoad_RequiredMissing(t *testing.T) {
	t.Setenv("TEST_REGION", "us-east-1")
	// TEST_BEDROCK_API_KEY intentionally not set.
	var cfg bedrockCfg
	if err := Load(&cfg); err == nil {
		t.Fatal("expected error for missing required variable")
	}
}

func TestLoad_RequiredPresent(t *testing.T) {
	t.Setenv("TEST_REGION", "us-east-1")
	t.Setenv("TEST_BEDROCK_API_KEY", "key-123")
	var cfg bedrockCfg
	if err := Load(&cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != "us-east-1" || cfg.BedrockAPIKey != "key-123" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
}

func TestLoad_TypesAndDefaults(t *testing.T) {
	t.Setenv("TEST_PORT", "9090")
	t.Setenv("TEST_DEBUG", "true")
	t.Setenv("TEST_TAGS", "alpha,beta,gamma")
	// TEST_NAME and TEST_TIMEOUT intentionally left to defaults.

	var cfg fullCfg
	if err := Load(&cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "service" {
		t.Errorf("Name default: got %q", cfg.Name)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port: got %d", cfg.Port)
	}
	if !cfg.Debug {
		t.Errorf("Debug: got false")
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout: got %v", cfg.Timeout)
	}
	if len(cfg.Tags) != 3 || cfg.Tags[0] != "alpha" || cfg.Tags[2] != "gamma" {
		t.Errorf("Tags: got %v", cfg.Tags)
	}
}
