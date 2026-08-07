// Package enactmain implements the platform's main service: the backend for
// the UI. It owns authentication — "Login with Google" (authorization code +
// PKCE, ID token validation) and local email+password accounts (bcrypt) —
// and cookie sessions guarding the app pages. User records live in the
// enact-users index via the users domain package.
package enactmain

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/agents"
	"enact/internal/conversations"
	"enact/internal/inference"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/models"
	"enact/internal/opensearch"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/users"
)

// Config wires the runtime, OpenSearch (user records), S2S, and the Google
// OAuth client settings (from the Google Cloud console; the redirect URL
// must exactly match a registered redirect URI).
type Config struct {
	service.Config
	OpenSearch    opensearch.Config
	Users         users.Config
	Conversations conversations.Config
	Inference     inference.ClientConfig
	Models        models.ClientConfig
	Agents        agents.ClientConfig
	KB            kb.ClientConfig
	S2S           s2s.Config

	GoogleClientID     string `env:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `env:"GOOGLE_CLIENT_SECRET"`
	OAuthRedirectURL   string `env:"OAUTH_REDIRECT_URL, default=http://localhost:8000/google/oauth/callback"`

	// SessionTTL is how long a login lasts.
	SessionTTL time.Duration `env:"SESSION_TTL, default=24h"`
	// SecureCookies marks auth cookies Secure (HTTPS-only). Off by default
	// for plain-HTTP local development; MUST be on behind TLS.
	SecureCookies bool `env:"SECURE_COOKIES"`
	// CookieSameSite is "lax" (default) or "none". Use "none" ONLY when the
	// frontend lives on a different registrable domain than this service —
	// browsers then require Secure, which this service enforces, so the
	// deployment must be HTTPS. FRONTEND_URL itself (CORS + redirects) is
	// part of the embedded service.Config.
	CookieSameSite string `env:"COOKIE_SAMESITE, default=lax"`
}

// parseSameSite maps the config value to the http constant; "none" forces
// Secure per browser rules.
func parseSameSite(v string, secure bool) (http.SameSite, bool, error) {
	switch strings.ToLower(v) {
	case "", "lax":
		return http.SameSiteLaxMode, secure, nil
	case "none":
		return http.SameSiteNoneMode, true, nil
	default:
		return 0, false, fmt.Errorf("enactmain: COOKIE_SAMESITE must be \"lax\" or \"none\", got %q", v)
	}
}

// Build constructs the main service. Google OIDC discovery runs once at
// startup, so building requires reaching accounts.google.com.
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		userRepo := users.NewRepository(osClient, cfg.Users)
		if err := userRepo.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify users index", "err", err)
			return nil, err
		}
		convRepo := conversations.NewRepository(osClient, cfg.Conversations)
		if err := convRepo.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify conversations index", "err", err)
			return nil, err
		}
		// Chat messages are answered by the inference service; calls are
		// signed as this service and carry the logged-in user's identity.
		inferenceClient := inference.NewClient(cfg.Inference, s2sRuntime.Transport(nil, "enact-model-inference"))
		modelsClient := models.NewClient(cfg.Models, s2sRuntime.Transport(nil, "enact-model-management"))
		agentsClient := agents.NewClient(cfg.Agents, s2sRuntime.Transport(nil, "enact-agent-management-api"))
		kbClient := kb.NewClient(cfg.KB, s2sRuntime.Transport(nil, "enact-kb-api"))
		google, err := newGoogleAuth(ctx, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.OAuthRedirectURL)
		if err != nil {
			logger.Error("failed to initialize google auth", "err", err)
			return nil, err
		}
		sessions := NewSessionStore(cfg.SessionTTL)
		sameSite, secure, err := parseSameSite(cfg.CookieSameSite, cfg.SecureCookies)
		if err != nil {
			logger.Error("invalid cookie configuration", "err", err)
			return nil, err
		}

		logger.Info("main service initialized",
			"redirect_url", cfg.OAuthRedirectURL,
			"session_ttl", cfg.SessionTTL,
			"secure_cookies", secure,
			"cookie_samesite", cfg.CookieSameSite,
			"frontend_url", cfg.FrontendURL,
			"s2s_key_id", cfg.S2S.KeyID,
		)
		api := newMainAPI(userRepo, sessions, google, convRepo, inferenceClient, modelsClient, agentsClient, kbClient,
			cookieSettings{Secure: secure, SameSite: sameSite}, cfg.FrontendURL, logger)
		services := api.WebServices()
		if s2sRuntime.Enabled() {
			for _, ws := range services {
				ws.Filter(s2sRuntime.Filter)
			}
		}
		return services, nil
	}
}
