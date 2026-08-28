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
	"enact/internal/cloudfront"
	"enact/internal/conversations"
	"enact/internal/extidentities"
	"enact/internal/files"
	"enact/internal/inference"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/models"
	"enact/internal/opensearch"
	"enact/internal/rbac"
	"enact/internal/s2s"
	"enact/internal/s3"
	"enact/internal/service"
	"enact/internal/ses"
	"enact/internal/tools"
	"enact/internal/users"
	"enact/internal/workflows"
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
	ToolRegistry  tools.ClientConfig
	RBAC          rbac.ClientConfig
	Identities    extidentities.ClientConfig
	Workflows     workflows.ClientConfig
	Files         files.Config
	S2S           s2s.Config
	Storage       s3.Config
	CDN           cloudfront.Config
	SES           ses.Config
	Sessions      SessionsConfig

	// VerificationEnabled requires local accounts to verify their email
	// before they can log in (unverified = unauthenticated). Requires
	// SES_FROM_EMAIL. Disable only for local development without AWS email.
	VerificationEnabled bool `env:"EMAIL_VERIFICATION_ENABLED, default=true"`
	// VerificationTTL bounds how long an emailed verification link is valid.
	VerificationTTL time.Duration `env:"VERIFICATION_TTL, default=24h"`
	// PublicBaseURL is this service's browser-reachable base URL, used to
	// build the emailed verification links.
	PublicBaseURL string `env:"PUBLIC_BASE_URL, default=http://localhost:8000"`

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
		// Avatar storage: S3 for the objects, CloudFront (when configured)
		// for the public URLs. Credential problems surface on first upload,
		// not at startup, so the service runs without AWS configured.
		storage, err := s3.NewClient(ctx, cfg.Storage)
		if err != nil {
			logger.Error("failed to create s3 client", "err", err)
			return nil, err
		}
		cdn := cloudfront.New(cfg.CDN)
		// Email verification fails closed: enabling it without a sender
		// identity would strand every new registration unverified.
		var mailer *ses.Client
		if cfg.VerificationEnabled {
			if cfg.SES.From == "" {
				logger.Error("EMAIL_VERIFICATION_ENABLED requires SES_FROM_EMAIL (a verified SES identity), or set EMAIL_VERIFICATION_ENABLED=false")
				return nil, fmt.Errorf("enactmain: email verification enabled without SES_FROM_EMAIL")
			}
			mailer, err = ses.NewClient(ctx, cfg.SES)
			if err != nil {
				logger.Error("failed to create ses client", "err", err)
				return nil, err
			}
		}
		google, err := newGoogleAuth(ctx, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.OAuthRedirectURL)
		if err != nil {
			logger.Error("failed to initialize google auth", "err", err)
			return nil, err
		}
		// Sessions live in Redis when an address is configured, so a restart
		// or a deploy no longer logs everybody out and two replicas can share
		// them. With no address they stay in the process, which keeps a bare
		// checkout runnable.
		sessions := NewSessionStore(cfg.SessionTTL)
		sessionStore := "memory"
		if cfg.Sessions.RedisAddr != "" {
			redisSessions, err := NewRedisSessionStore(ctx, cfg.Sessions, cfg.SessionTTL, logger)
			if err != nil {
				// Fails closed. Quietly falling back to memory would restore
				// the very behaviour this replaces, and look like a working
				// deployment until the first restart.
				logger.Error("failed to connect to the session store", "addr", cfg.Sessions.RedisAddr, "err", err)
				return nil, err
			}
			sessions = redisSessions
			sessionStore = "redis"
		}
		sameSite, secure, err := parseSameSite(cfg.CookieSameSite, cfg.SecureCookies)
		if err != nil {
			logger.Error("invalid cookie configuration", "err", err)
			return nil, err
		}

		logger.Info("main service initialized",
			"storage_bucket", cfg.Storage.Bucket,
			"cdn_configured", cdn.Configured(),
			"verification_enabled", cfg.VerificationEnabled,
			"ses_from", cfg.SES.From,
			"public_base_url", cfg.PublicBaseURL,
			"redirect_url", cfg.OAuthRedirectURL,
			"session_ttl", cfg.SessionTTL,
			"secure_cookies", secure,
			"cookie_samesite", cfg.CookieSameSite,
			"frontend_url", cfg.FrontendURL,
			"admin_email", cfg.AdminEmail,
			"s2s_key_id", cfg.S2S.KeyID,
			"session_store", sessionStore,
		)
		api := newMainAPI(userRepo, sessions, google, convRepo, inferenceClient, modelsClient, agentsClient, kbClient, storage, cdn,
			cookieSettings{Secure: secure, SameSite: sameSite}, cfg.FrontendURL, logger)
		api.mailer = mailer
		api.verificationEnabled = cfg.VerificationEnabled
		api.verificationTTL = cfg.VerificationTTL
		api.publicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
		api.adminEmail = users.NormalizeEmail(cfg.AdminEmail)
		// Loaded before anything is served: a page missing its title heading
		// is a mistake in content that ships with the binary, so it should
		// stop the service here rather than quietly vanish from navigation.
		pages, err := loadDocs()
		if err != nil {
			logger.Error("failed to load the documentation", "err", err)
			return nil, err
		}
		api.docs = pages
		logger.Info("documentation loaded", "pages", len(pages))
		api.toolRegistry = tools.NewClient(cfg.ToolRegistry, s2sRuntime.Transport(nil, "enact-tool-registry"))
		api.rbac = rbac.NewClient(cfg.RBAC, s2sRuntime.Transport(nil, "enact-rbac"))
		api.identities = extidentities.NewClient(cfg.Identities, s2sRuntime.Transport(nil, "enact-external-identities"))
		api.workflows = workflows.NewClient(cfg.Workflows, s2sRuntime.Transport(nil, "enact-workflows"))
		fileStore, err := files.NewFS(cfg.Files)
		if err != nil {
			logger.Error("failed to open the workflow file store", "err", err)
			return nil, err
		}
		logger.Info("workflow file store opened", "root", fileStore.Root(), "configured", cfg.Files.Root != "")
		api.files = fileStore
		services := api.WebServices()
		if s2sRuntime.Enabled() {
			// Appended LAST, and that matters: go-restful runs a WebService's
			// filters in registration order, so requireCaller (attached inside
			// WebServices) runs first and takes an API key out of the
			// Authorization header before the S2S filter reads it. Register
			// this earlier and every API key becomes "invalid service token" —
			// see TestConsumeAPIKeyClearsAuthorization.
			for _, ws := range services {
				ws.Filter(s2sRuntime.Filter)
			}
		}
		return services, nil
	}
}
