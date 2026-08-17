// Package enacttests implements the integration-test service. It holds a
// registry of test cases — Go functions that exercise the real platform
// APIs — and runs them asynchronously on request:
//
//	POST /v1/execution {num_workers, tests, skip}  -> {exec_id}
//	GET  /v1/execution?id=<exec_id>                -> {completed, pending}
//
// Because it verifies S2S enforcement, the service holds the private keys
// of EVERY service (S2S_PRIVATE_KEYS) and impersonates them via Fleet.
package enacttests

import (
	"context"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/enacttests/cases"
	"enact/internal/enacttests/utils"
	"enact/internal/logging"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config wires the runtime, the platform base URLs the test cases target,
// and the S2S material. S2S.Config authenticates callers of THIS service;
// PrivateKeys lets the test cases impersonate every other service.
type Config struct {
	service.Config
	S2S s2s.Config

	// PrivateKeys is the YAML document holding every service's private key
	// (the counterpart of the JWKS), assembled by scripts/start-services.sh.
	PrivateKeys string `env:"S2S_PRIVATE_KEYS"`
	// PrivateKeysFile is a path to that document (written by s2s-keygen as
	// s2s/private-keys.yaml), read when PrivateKeys is empty.
	PrivateKeysFile string `env:"S2S_PRIVATE_KEYS_FILE"`

	AgentAPIURL     string `env:"AGENT_API_URL, default=http://localhost:8084"`
	KBAPIURL        string `env:"KB_API_URL, default=http://localhost:8082"`
	InferenceAPIURL string `env:"INFERENCE_API_URL, default=http://localhost:8080"`
	ModelsAPIURL    string `env:"MODELS_API_URL, default=http://localhost:8081"`
	MainAPIURL      string `env:"MAIN_API_URL, default=http://localhost:8000"`
	ToolRegistryURL string `env:"TOOL_REGISTRY_API_URL, default=http://localhost:8007"`
	IdentitiesURL   string `env:"EXTERNAL_IDENTITIES_API_URL, default=http://localhost:8008"`
	RBACURL         string `env:"RBAC_API_URL, default=http://localhost:8009"`

	// MCPFixtureListen is where the embedded MCP fixture server listens;
	// MCPFixtureURL is that server as the REGISTRY reaches it (differs in
	// docker compose, where it is this container's hostname).
	MCPFixtureListen string `env:"TEST_MCP_LISTEN, default=:8099"`
	MCPFixtureURL    string `env:"TEST_MCP_URL, default=http://localhost:8099"`
	// The OAuth fixture provider, same listen-vs-reachable-URL split.
	OAuthFixtureListen string `env:"TEST_OAUTH_LISTEN, default=:8098"`
	OAuthFixtureURL    string `env:"TEST_OAUTH_URL, default=http://localhost:8098"`

	// TestUserID isolates test data from real users.
	TestUserID string `env:"TEST_USER_ID, default=integration-tests"`
	// TestTimeout bounds each HTTP call made by a test case.
	TestTimeout time.Duration `env:"TEST_HTTP_TIMEOUT, default=15s"`
}

// Build constructs the tests service: S2S runtime for its own endpoints,
// the impersonation fleet for the cases, and the async runner.
func Build(cfg *Config) service.Builder {
	return func(_ context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}

		env := &utils.Env{
			AgentAPIURL:     cfg.AgentAPIURL,
			KBAPIURL:        cfg.KBAPIURL,
			InferenceAPIURL: cfg.InferenceAPIURL,
			ModelsAPIURL:    cfg.ModelsAPIURL,
			MainAPIURL:      cfg.MainAPIURL,
			ToolRegistryURL: cfg.ToolRegistryURL,
			IdentitiesURL:   cfg.IdentitiesURL,
			RBACURL:         cfg.RBACURL,
			MCPFixtureURL:   cfg.MCPFixtureURL,
			OAuthFixtureURL: cfg.OAuthFixtureURL,
			UserID:          cfg.TestUserID,
			AdminEmail:      cfg.AdminEmail,
			Timeout:         cfg.TestTimeout,
		}
		// The MCP fixture is part of the service, not a case: registry and
		// agent cases need a live spec-compliant server to point at.
		utils.StartMCPFixture(cfg.MCPFixtureListen, logger)
		utils.StartOAuthFixture(cfg.OAuthFixtureListen, logger)
		// The fleet requires the platform's private keys; without S2S there
		// is nothing to sign, so the cases run with plain clients.
		if s2sRuntime.Enabled() {
			privateKeys, err := s2s.ResolveContent(cfg.PrivateKeys, cfg.PrivateKeysFile, "S2S_PRIVATE_KEYS_FILE")
			if err != nil {
				logger.Error("failed to read fleet private keys", "err", err)
				return nil, err
			}
			fleet, err := utils.NewFleet(privateKeys, cfg.S2S.TokenTTL)
			if err != nil {
				logger.Error("failed to load impersonation fleet", "err", err)
				return nil, err
			}
			env.Fleet = fleet
		} else {
			logger.Warn("s2s disabled: test cases will call services without tokens")
		}

		runner, err := NewRunner(env, cases.All(), logger)
		if err != nil {
			logger.Error("failed to build test runner", "err", err)
			return nil, err
		}
		logger.Info("tests service initialized",
			"registered_cases", runner.CaseCount(),
			"agent_api", cfg.AgentAPIURL,
			"kb_api", cfg.KBAPIURL,
			"inference_api", cfg.InferenceAPIURL,
			"models_api", cfg.ModelsAPIURL,
			"s2s_enabled", s2sRuntime.Enabled(),
		)
		ws := newTestsAPI(runner, logger).WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
