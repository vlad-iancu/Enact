// Package docs embeds the product documentation enact-main serves.
//
// It is embedded rather than read from disk so the documentation ships with
// the binary: the runtime image is distroless with nothing in it but /app,
// and one Dockerfile builds every service, so a COPY would have to be either
// service-conditional or applied to images that have no use for it. Embedding
// also means a deployment cannot start with its documentation missing.
//
// The trade is that editing a page requires a rebuild — which is the right
// way round for documentation that describes the code it ships with.
//
// This file lives here, rather than the markdown living under internal/,
// because go:embed cannot reach outside its own package directory.
package docs

import "embed"

// FS holds every file under app/. Only *.md is served; see
// internal/enactmain/docs.go.
//
//go:embed app
var FS embed.FS
