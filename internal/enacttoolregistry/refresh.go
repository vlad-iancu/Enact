package enacttoolregistry

import (
	"context"
	"time"
)

// refreshLoop re-fetches every registered server's tool list on the
// configured interval, keeping the enact-tool-cache index in step with what
// the servers actually advertise. An unreachable server keeps its last
// cached tools — a transient outage must not blank an agent's toolbox.
func (a *RegistryAPI) refreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	a.logger.Info("tool cache refresh loop started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.logger.Info("tool cache refresh loop stopped")
			return
		case <-ticker.C:
			a.refreshAll(ctx)
		}
	}
}

// refreshAll refreshes the cache of every registered server.
func (a *RegistryAPI) refreshAll(ctx context.Context) {
	servers, err := a.repo.ListServers(ctx, nil, "")
	if err != nil {
		a.logger.Error("refresh: failed to list servers", "err", err)
		return
	}
	a.logger.Info("refreshing tool caches", "servers", len(servers))
	for _, server := range servers {
		a.refreshServer(ctx, server.ID)
	}
}

// refreshServer re-fetches one server's tools and replaces its cache entry.
func (a *RegistryAPI) refreshServer(ctx context.Context, id string) {
	server, found, err := a.repo.GetServer(ctx, id)
	if err != nil || !found {
		a.logger.Warn("refresh: server lookup failed", "id", id, "found", found, "err", err)
		return
	}
	// As the owner: a gated server needs credentials to answer at all, and
	// the sweep has no user of its own to borrow.
	defs, err := a.probeTools(ctx, server)
	if err != nil {
		a.logger.Warn("refresh: mcp server probe failed; keeping cached tools", "id", id, "url", server.URL, "err", err)
		return
	}
	if err := a.repo.ReplaceTools(ctx, server.ID, defs); err != nil {
		a.logger.Error("refresh: failed to replace cached tools", "id", id, "err", err)
		return
	}
	if server.ToolCount != len(defs) {
		server.ToolCount = len(defs)
		server.UpdatedAt = time.Now().UTC()
		if err := a.repo.SaveServer(ctx, server); err != nil {
			a.logger.Warn("refresh: failed to update tool count", "id", id, "err", err)
		}
	}
	a.logger.Info("tool cache refreshed", "id", id, "tool_count", len(defs))
}
