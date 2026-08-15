package enactexternalidentities

import (
	"context"
	"errors"
	"time"

	"enact/internal/extidentities"
	"enact/internal/logging"
)

// refreshLoop renews credentials ahead of their expiry.
//
// Refresh is a background sweep only: retrieval never blocks on a provider
// round-trip. The consequence is that the refresh WINDOW must be wider than
// the sweep INTERVAL, or a token could expire between two sweeps — Build
// enforces window >= 3x interval.
func (a *IdentitiesAPI) refreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	a.logger.Info("credential refresh loop started", "interval", interval, "window", a.refreshWindow)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.logger.Info("credential refresh loop stopped")
			return
		case <-ticker.C:
			a.refreshExpiring(ctx)
		}
	}
}

// refreshExpiring renews every refreshable credential expiring within the
// window.
func (a *IdentitiesAPI) refreshExpiring(ctx context.Context) {
	horizon := time.Now().UTC().Add(a.refreshWindow)
	due, err := a.repo.ListExpiring(ctx, horizon)
	if err != nil {
		a.logger.Error("refresh: failed to list expiring identities", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	a.logger.Info("refreshing expiring credentials", "count", len(due), "horizon", horizon)

	// Provider records are stable across a sweep; resolve each once.
	providers := map[string]extidentities.Provider{}
	for _, record := range due {
		provider, ok := providers[record.Provider]
		if !ok {
			rec, found, err := a.repo.GetProvider(ctx, record.Provider)
			if err != nil || !found {
				a.logger.Warn("refresh: provider unavailable", "provider", record.Provider, "found", found, "err", err)
				continue
			}
			provider, err = extidentities.NewProvider(rec, a.repo.Vault(), a.providerHTTP)
			if err != nil {
				a.logger.Warn("refresh: provider could not be built", "provider", record.Provider, "err", err)
				continue
			}
			providers[record.Provider] = provider
		}
		a.refreshOne(ctx, provider, record)
	}
}

// refreshOne renews a single identity. A transient failure keeps the old
// credential (it may still be valid) and is retried next sweep; a permanent
// refusal marks the identity revoked so the user is asked to re-authorize.
func (a *IdentitiesAPI) refreshOne(ctx context.Context, provider extidentities.Provider, record extidentities.Identity) {
	logger := a.logger.WithFields("id", record.ID, "user_id", record.UserID,
		"provider", record.Provider)

	envelope, err := a.repo.OpenEnvelope(record)
	if err != nil {
		logger.Error("refresh: stored credential could not be opened", "err", err)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, a.providerHTTP.Timeout)
	defer cancel()

	stored, refreshed, err := provider.Refresh(callCtx, envelope)
	if err != nil {
		var permanent *extidentities.PermanentRefreshError
		if errors.As(err, &permanent) {
			a.markRefreshOutcome(ctx, logger, record, extidentities.StatusRevoked, permanent.Error())
			return
		}
		a.markRefreshOutcome(ctx, logger, record, extidentities.StatusRefreshFailed, err.Error())
		return
	}
	if !refreshed {
		// Nothing to do (no refresh token): stop reconsidering it every
		// sweep by clearing the refreshable flag.
		logger.Info("refresh: nothing to refresh; clearing the refreshable flag")
		record.Refreshable = false
		record.UpdatedAt = time.Now().UTC()
		if err := a.repo.SaveIdentity(ctx, record, envelope); err != nil {
			logger.Error("refresh: failed to persist flag", "err", err)
		}
		return
	}

	now := time.Now().UTC()
	record.ExpiresAt = stored.ExpiresAt
	record.Refreshable = stored.Refreshable
	if len(stored.Access) > 0 {
		record.Access = stored.Access
	}
	record.Status = extidentities.StatusActive
	record.RefreshFailures = 0
	record.RefreshFailedAt = nil
	record.LastRefreshedAt = &now
	record.UpdatedAt = now
	if err := a.repo.SaveIdentity(ctx, record, stored.Envelope); err != nil {
		logger.Error("refresh: failed to store refreshed credential", "err", err)
		return
	}
	logger.Info("credential refreshed", "expires_at", record.ExpiresAt)
}

// markRefreshOutcome records a failed refresh without touching the stored
// credential — the previous one may still work until it expires.
func (a *IdentitiesAPI) markRefreshOutcome(ctx context.Context, logger *logging.Logger, record extidentities.Identity, status, reason string) {
	now := time.Now().UTC()
	record.Status = status
	record.RefreshFailures++
	record.RefreshFailedAt = &now
	record.UpdatedAt = now

	envelope, err := a.repo.OpenEnvelope(record)
	if err != nil {
		logger.Error("refresh: could not re-store status", "err", err)
		return
	}
	if err := a.repo.SaveIdentity(ctx, record, envelope); err != nil {
		logger.Error("refresh: failed to persist status", "err", err)
		return
	}
	if status == extidentities.StatusRevoked {
		logger.Warn("credential revoked by the provider; re-authorization required", "reason", reason)
		return
	}
	logger.Warn("credential refresh failed; keeping the previous credential",
		"reason", reason, "failures", record.RefreshFailures)
}
