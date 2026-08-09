// Package cloudfront resolves storage keys to their public CDN URLs. The
// platform serves stored objects (avatars) through a CloudFront distribution
// in front of the S3 bucket; objects are publicly readable but their keys
// contain UUIDs, so URLs are unguessable rather than access-controlled.
package cloudfront

import "strings"

// Config holds the environment-driven CloudFront settings.
type Config struct {
	// BaseURL is the distribution's base URL (e.g.
	// https://d1234abcd.cloudfront.net). Empty means no CDN is configured;
	// callers fall back to direct S3 URLs.
	BaseURL string `env:"CLOUDFRONT_BASE_URL"`
}

// Resolver maps storage keys to public URLs.
type Resolver struct {
	baseURL string
}

// New returns a Resolver for the configured distribution.
func New(cfg Config) *Resolver {
	return &Resolver{baseURL: strings.TrimRight(cfg.BaseURL, "/")}
}

// Configured reports whether a distribution base URL is set.
func (r *Resolver) Configured() bool { return r.baseURL != "" }

// URL returns the public URL for a storage key.
func (r *Resolver) URL(key string) string {
	return r.baseURL + "/" + strings.TrimLeft(key, "/")
}
