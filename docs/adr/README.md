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