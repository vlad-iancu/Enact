# Architecture Decision Records

Decisions that shape the enact agent platform, in the order they were made.
See [template.md](template.md) to add one manually.

| ADR | Title | Status | Date |
|-----|-------|--------|------|
| [0001](0001-lgtm-observability-via-opentelemetry.md) | LGTM observability stack wired via OpenTelemetry | accepted | 2026-08-01 |
| [0002](0002-w3c-trace-context-propagation.md) | W3C trace context propagation via middleware and wrapped HTTP client | accepted | 2026-08-01 |
| [0003](0003-request-identity-via-context.md) | Request identity flows through context, not parameters | accepted | 2026-08-02 |
| [0004](0004-service-to-service-http-with-domain-clients.md) | Services are sources of truth, called over HTTP via domain-package clients | accepted | 2026-08-02 |
| [0005](0005-response-meta-block.md) | Every JSON response carries a `meta` block (trace_id + execution time) | accepted | 2026-08-02 |
| [0006](0006-queue-pending-reclaim-and-delivery-cap.md) | Queue consumers reclaim stalled deliveries and cap attempts | accepted | 2026-08-02 |
| [0007](0007-retrieval-top-k-per-request.md) | RAG retrieval top-k is a per-request API parameter, not service config | accepted | 2026-08-01 |
| [0008](0008-verbose-logging-on-all-code-paths.md) | All code paths log all non-sensitive parameters and relevant intermediary results | accepted | 2026-08-02 |
| [0009](0009-one-domain-per-service.md) | Each service owns exactly one domain and does not trespass | accepted | 2026-08-02 |
| [0010](0010-titan-v2-embeddings.md) | Amazon Titan Text Embeddings v2 as the embedding model | accepted | 2026-08-02 |
| [0011](0011-s2s-authentication.md) | Service-to-service authentication with per-service Ed25519 JWTs and YAML-distributed JWKS/ACLs | accepted | 2026-08-04 |
| [0012](0012-integration-tests-as-a-service.md) | Integration tests as a platform service with lifecycle-structured cases | accepted | 2026-08-05 |
| [0013](0013-external-identity-credentials-encrypted-at-rest.md) | External identity credentials are encrypted at rest with an application key | accepted | 2026-08-12 |
| [0014](0014-per-user-credentials-for-mcp-tools.md) | Per-user credentials for MCP tool calls | accepted | 2026-08-13 |
| [0015](0015-provider-scoped-identities.md) | Identities are scoped to a provider, not to a consumer | accepted | 2026-08-14 |
| [0016](0016-mcp-tool-registry-with-cache-and-proxy.md) | MCP servers are registered in a registry service that caches their tools and proxies their traffic | accepted | 2026-08-15 |
| [0017](0017-owner-scoped-probe-credentials.md) | A gated MCP server is reached with its owner's credentials | accepted | 2026-08-15 |
| [0018](0018-conversations-record-and-replay-tool-calls.md) | Conversations record tool calls and replay them into the model's context | accepted | 2026-08-15 |
| [0019](0019-organizations-as-the-isolation-boundary.md) | Organizations are the isolation boundary, and every resource stores which one it is in | accepted | 2026-08-16 |
| [0020](0020-workflows-run-queued-with-a-copied-definition.md) | Workflows run on a queue, against a copy of their own definition | accepted | 2026-08-21 |
