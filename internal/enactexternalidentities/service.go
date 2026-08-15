// Package enactexternalidentities implements the external identity service:
// the source of truth for the credentials the platform holds on a user's
// behalf at third parties (Google Workspace, GitHub, JIRA, …).
//
// It owns three things no other service may do: sealing credentials at rest,
// driving the OAuth consent flow, and refreshing tokens before they expire.
// Consumers ask it for a usable credential and never see a refresh token.
package enactexternalidentities

import (
	"context"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/extidentities"
	"enact/internal/identityevents"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/requesthelper"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config wires the runtime, OpenSearch, the encryption key, S2S, and the
// refresh cadence.
//
// The env names are deliberately prefixed: deploy/app.env is shared by every
// container, where REFRESH_AT already belongs to the tool registry and
// PUBLIC_BASE_URL to enact-main.
type Config struct {
	service.Config
	OpenSearch     opensearch.Config
	Identities     extidentities.Config
	Crypto         extidentities.CryptoConfig
	IdentityEvents identityevents.Config
	S2S            s2s.Config

	// RefreshAt is how often the sweep runs.
	RefreshAt time.Duration `env:"IDENTITIES_REFRESH_AT, default=5m"`
	// RefreshWindow is how far ahead of expiry a credential is renewed. It
	// must exceed RefreshAt or a token can expire between sweeps; Build
	// raises it to 3×RefreshAt when configured lower.
	RefreshWindow time.Duration `env:"IDENTITIES_REFRESH_WINDOW, default=15m"`
	// ProviderTimeout bounds one exchange or refresh against a provider.
	ProviderTimeout time.Duration `env:"IDENTITIES_PROVIDER_TIMEOUT, default=20s"`
	// FlowTTL bounds how long an started OAuth flow may sit unfinished.
	FlowTTL time.Duration `env:"IDENTITIES_FLOW_TTL, default=10m"`
	// PublicURL is this service as the BROWSER reaches it; the OAuth
	// redirect URI is derived from it and must match, byte for byte, the
	// value registered in each provider's console.
	PublicURL string `env:"IDENTITIES_PUBLIC_URL, default=http://localhost:8008"`
}

// Build constructs the service and starts the refresh sweep.
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		// Fail closed: without a usable key this service would either store
		// credentials in the clear or serve unreadable ones. Neither is an
		// acceptable degraded mode.
		vault, err := extidentities.NewVault(cfg.Crypto)
		if err != nil {
			logger.Error("failed to initialize the credential vault; set IDENTITIES_ENCRYPTION_KEY to base64 of 32 random bytes (openssl rand -base64 32)", "err", err)
			return nil, err
		}
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		repo := extidentities.NewRepository(osClient, cfg.Identities, vault)
		if err := repo.EnsureIndices(ctx); err != nil {
			logger.Error("failed to verify identity indices", "err", err)
			return nil, err
		}

		refreshWindow := cfg.RefreshWindow
		if minWindow := 3 * cfg.RefreshAt; refreshWindow < minWindow {
			// A window narrower than the sweep interval lets tokens expire
			// between sweeps; widen rather than silently under-refresh.
			logger.Warn("refresh window is too narrow for the sweep interval; widening",
				"configured", refreshWindow, "interval", cfg.RefreshAt, "using", minWindow)
			refreshWindow = minWindow
		}

		api := &IdentitiesAPI{
			repo:         repo,
			flows:        newFlowStore(cfg.FlowTTL),
			providerHTTP: &http.Client{Transport: requesthelper.NewTransport(nil), Timeout: cfg.ProviderTimeout},
			// Announces newly stored credentials so a tool call parked on a
			// missing one resumes. Constructing it opens no connection, so a
			// Redis outage cannot stop this service from starting.
			events:        identityevents.NewPublisher(cfg.IdentityEvents),
			refreshWindow: refreshWindow,
			publicURL:     strings.TrimRight(cfg.PublicURL, "/"),
			logger:        logger,
		}
		logger.Info("external identities initialized",
			"identities_index", cfg.Identities.IdentitiesIndex,
			"providers_index", cfg.Identities.ProvidersIndex,
			"encryption_key_fingerprint", vault.KeyFingerprint(),
			"refresh_at", cfg.RefreshAt,
			"refresh_window", refreshWindow,
			"provider_timeout", cfg.ProviderTimeout,
			"flow_ttl", cfg.FlowTTL,
			"public_url", api.publicURL,
			"identity_events_channel", api.events.Channel(),
			"s2s_key_id", cfg.S2S.KeyID,
		)

		go api.refreshLoop(ctx, cfg.RefreshAt)

		services := api.WebServices()
		if s2sRuntime.Enabled() {
			for _, ws := range services {
				ws.Filter(s2sRuntime.Filter)
			}
		}
		return services, nil
	}
}
