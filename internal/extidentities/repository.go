package extidentities

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/opensearch"
)

// Repository persists provider records and identities in OpenSearch. It is
// the single choke point for encryption: envelopes are sealed on the way in
// and opened on the way out, so no caller and no Provider ever handles the
// key.
type Repository struct {
	os         *opensearch.Client
	identities string
	providers  string
	vault      *Vault
}

// NewRepository returns a Repository over the indices in cfg, sealing with
// vault.
func NewRepository(os *opensearch.Client, cfg Config, vault *Vault) *Repository {
	return &Repository{os: os, identities: cfg.IdentitiesIndex, providers: cfg.ProvidersIndex, vault: vault}
}

// Vault exposes the repository's vault so the service can seal provider
// client secrets with the same key.
func (r *Repository) Vault() *Vault { return r.vault }

// EnsureIndices verifies both indices exist; they are created by
// infrastructure provisioning, never by services.
func (r *Repository) EnsureIndices(ctx context.Context) error {
	for _, index := range []string{r.identities, r.providers} {
		exists, err := r.os.IndexExists(ctx, index)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("extidentities: required index %q is missing; run `make infrastructure-up` to create it", index)
		}
	}
	return nil
}

// --- providers -------------------------------------------------------------

// SaveProvider persists a provider record (create or full replace).
func (r *Repository) SaveProvider(ctx context.Context, rec ProviderRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.providers, rec.Name, body)
}

// GetProvider fetches a provider by name. The boolean reports existence.
func (r *Repository) GetProvider(ctx context.Context, name string) (ProviderRecord, bool, error) {
	var rec ProviderRecord
	found, err := r.os.GetSource(ctx, r.providers, name, &rec)
	return rec, found, err
}

// DeleteProvider removes a provider record.
func (r *Repository) DeleteProvider(ctx context.Context, name string) error {
	return r.os.DeleteDoc(ctx, r.providers, name)
}

// ListProviders returns registered providers, optionally filtered by type.
func (r *Repository) ListProviders(ctx context.Context, providerType string) ([]ProviderRecord, error) {
	query := map[string]any{"match_all": map[string]any{}}
	if providerType != "" {
		query = map[string]any{"term": map[string]any{"type": providerType}}
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": query,
		"sort":  []any{map[string]any{"name": map[string]any{"order": "asc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("extidentities: marshal providers query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.providers, body)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderRecord, 0, len(hits))
	for _, h := range hits {
		var rec ProviderRecord
		if err := json.Unmarshal(h.Source, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// --- identities ------------------------------------------------------------

// SaveIdentity seals the plaintext envelope and persists the identity. The
// caller passes the envelope a Provider produced; this method is the only
// place it becomes ciphertext.
func (r *Repository) SaveIdentity(ctx context.Context, id Identity, envelope string) error {
	sealed, err := r.vault.Seal(envelope)
	if err != nil {
		return err
	}
	id.Credentials = sealed
	body, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.identities, id.ID, body)
}

// GetIdentityMetadata fetches an identity WITHOUT opening its envelope, for
// callers that only need to know what is recorded (access, status, expiry).
// Credentials stays ciphertext. Use this rather than GetIdentity wherever
// the plaintext is not needed: it decrypts nothing, and it still answers for
// a record whose envelope can no longer be opened.
func (r *Repository) GetIdentityMetadata(ctx context.Context, userID, provider string) (Identity, bool, error) {
	var record Identity
	found, err := r.os.GetSource(ctx, r.identities, DocID(userID, provider), &record)
	if err != nil || !found {
		return Identity{}, found, err
	}
	return record, true, nil
}

// GetIdentity fetches an identity and opens its envelope. The returned
// Identity keeps the sealed form in Credentials; the plaintext envelope is
// the separate return value, so a caller cannot accidentally serialize it.
func (r *Repository) GetIdentity(ctx context.Context, userID, provider string) (Identity, string, bool, error) {
	record, found, err := r.GetIdentityMetadata(ctx, userID, provider)
	if err != nil || !found {
		return Identity{}, "", found, err
	}
	envelope, err := r.vault.Open(record.Credentials)
	if err != nil {
		return Identity{}, "", true, err
	}
	return record, envelope, true, nil
}

// DeleteIdentity removes an identity by its uniqueness pair.
func (r *Repository) DeleteIdentity(ctx context.Context, userID, provider string) error {
	return r.os.DeleteDoc(ctx, r.identities, DocID(userID, provider))
}

// ListIdentities returns identity metadata (never envelopes) filtered by
// any combination of user, provider and status.
func (r *Repository) ListIdentities(ctx context.Context, userID, provider, status string) ([]Identity, error) {
	var filters []any
	for field, value := range map[string]string{
		"user_id":  userID,
		"provider": provider,
		"status":   status,
	} {
		if value != "" {
			filters = append(filters, map[string]any{"term": map[string]any{field: value}})
		}
	}
	query := map[string]any{"match_all": map[string]any{}}
	if len(filters) > 0 {
		query = map[string]any{"bool": map[string]any{"filter": filters}}
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": query,
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("extidentities: marshal identities query: %w", err)
	}
	return r.searchIdentities(ctx, body)
}

// ListExpiring returns refreshable identities whose credentials expire
// before the given instant — the refresh sweep's work list. Revoked
// identities are skipped: only re-consent fixes those.
func (r *Repository) ListExpiring(ctx context.Context, before time.Time) ([]Identity, error) {
	body, err := json.Marshal(map[string]any{
		"size": 1000,
		"query": map[string]any{"bool": map[string]any{
			"filter": []any{
				map[string]any{"term": map[string]any{"refreshable": true}},
				map[string]any{"range": map[string]any{"expires_at": map[string]any{"lte": before.UTC().Format(time.RFC3339)}}},
			},
			"must_not": []any{
				map[string]any{"term": map[string]any{"status": StatusRevoked}},
			},
		}},
		"sort": []any{map[string]any{"expires_at": map[string]any{"order": "asc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("extidentities: marshal expiring query: %w", err)
	}
	return r.searchIdentities(ctx, body)
}

// DeleteIdentitiesByProvider removes every identity of one provider and
// reports how many went. It backs the forced provider deletion: a
// credential whose provider is gone cannot be opened by anyone, so leaving
// it behind stores an unreadable secret forever.
func (r *Repository) DeleteIdentitiesByProvider(ctx context.Context, provider string) (int, error) {
	count, err := r.CountByProvider(ctx, provider)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"provider": provider}},
	})
	if err != nil {
		return 0, err
	}
	if err := r.os.DeleteByQuery(ctx, r.identities, body); err != nil {
		return 0, err
	}
	return count, nil
}

// DeleteIdentitiesByUser removes every identity of one user, for account
// deletion. Without it a deleted account's credentials would stay sealed in
// the index forever, still refreshed by the sweep (which selects on expiry,
// not on whether the owner still exists).
func (r *Repository) DeleteIdentitiesByUser(ctx context.Context, userID string) (int, error) {
	if userID == "" {
		return 0, fmt.Errorf("extidentities: refusing to delete identities for an empty user id")
	}
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"user_id": userID}},
	})
	if err != nil {
		return 0, err
	}
	records, err := r.ListIdentities(ctx, userID, "", "")
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	if err := r.os.DeleteByQuery(ctx, r.identities, body); err != nil {
		return 0, err
	}
	return len(records), nil
}

// CountByProvider reports how many identities reference a provider — the
// guard against deleting a provider and stranding unreadable credentials.
func (r *Repository) CountByProvider(ctx context.Context, provider string) (int, error) {
	body, err := json.Marshal(map[string]any{
		"size":             0,
		"track_total_hits": true,
		"query":            map[string]any{"term": map[string]any{"provider": provider}},
	})
	if err != nil {
		return 0, err
	}
	result, err := r.os.SearchWithAggregations(ctx, r.identities, body)
	if err != nil {
		return 0, err
	}
	return result.Total, nil
}

func (r *Repository) searchIdentities(ctx context.Context, body []byte) ([]Identity, error) {
	hits, err := r.os.Search(ctx, r.identities, body)
	if err != nil {
		return nil, err
	}
	out := make([]Identity, 0, len(hits))
	for _, h := range hits {
		var record Identity
		if err := json.Unmarshal(h.Source, &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

// OpenEnvelope decrypts a stored identity's credentials. Used by the
// refresh sweep, which works from listed records rather than single gets.
func (r *Repository) OpenEnvelope(record Identity) (string, error) {
	return r.vault.Open(record.Credentials)
}
