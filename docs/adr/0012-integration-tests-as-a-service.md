# ADR-0012: Integration tests as a platform service with lifecycle-structured cases

**Date**: 2026-08-05
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

The platform needed integration tests that exercise the real services over
HTTP — including the S2S enforcement of ADR-0011, which requires calls
signed as specific service identities. Plain `go test` runs synchronously in
a local process, has no API for triggering or observing runs remotely, and
holds no service credentials.

## Decision

Integration testing is itself a platform service, `enact-tests`
(`internal/enacttests`), supporting asynchronous execution over HTTP:
`POST /v1/execution {num_workers, tests, skip}` selects registered cases by
regex, runs them on a bounded worker pool, and returns an execution id
immediately; `GET /v1/execution?id=` reports completed cases
(SUCCESS/FAILED, with the first failure message) and the pending count.
Executions are held in memory — the service is a dev tool, restarts discard
history.

Test cases are structured, not free functions:

- Each case is a struct implementing the `TestCase` interface — `Name`,
  `Setup`, `Run`, `TearDown` — in its own file under
  `internal/enacttests/cases`. Fixtures live on the struct between phases.
- The runner drives the lifecycle per case on a fresh instance (factories,
  not shared instances): a failed Setup skips Run; **TearDown always runs**,
  and helpers are teardown-tolerant (empty-id no-ops, 404s accepted), so
  fixtures cannot leak regardless of where a case aborts.
- Shared tooling lives in `internal/enacttests/utils`: the `T` handle
  (assertions mirroring the testing package, `DoJSON`, `Eventually` for
  search-backed eventual-consistency assertions) and domain helpers.
  Dependency flow is one-way: service → cases → utils.
- Registration is explicit — a manually maintained manifest
  (`cases.All()`), no init() side effects.

Cases run under a dedicated user id (`integration-tests`) to isolate test
data, and impersonate real service identities via a key fleet: the service
receives every service's private key (`S2S_PRIVATE_KEYS`, assembled by the
start script) and signs requests as any caller, exercising production auth
paths rather than bypassing them. Cases that would invoke Bedrock are
restricted to validation paths, so runs cost no model tokens.

## Alternatives Considered

### Alternative 1: Standard `go test` with a build tag (`-tags integration`)
- **Pros**: zero new infrastructure; familiar tooling; `-run` regex selection
- **Cons**: synchronous, local-only, no remote trigger/status API; key material distribution to developers' shells; no async execution (the explicit requirement)
- **Why not**: the platform wanted test execution as a first-class, remotely drivable capability with the same operational surface as other services (S2S, tracing, structured logs, meta responses)

### Alternative 2: Free test functions with defer-based cleanup (initial implementation)
- **Pros**: less boilerplate than one struct per case
- **Cons**: cleanup correctness depends on defer placement discipline (a real leak window was found in review); no reusable fixture state; files accumulated multiple cases
- **Why not**: the Setup/Run/TearDown lifecycle makes guaranteed cleanup structural rather than disciplinary

### Alternative 3: External test tooling (Postman/Newman, k6, pytest)
- **Pros**: mature reporting ecosystems
- **Cons**: second language/toolchain; cannot reuse `internal/s2s` signing or the platform's Go domain types; drifts from the codebase
- **Why not**: Go cases reuse the platform's own building blocks and stay in the repository's single toolchain

## Consequences

### Positive
- Tests run against the real deployment, through real auth, with results retrievable by API from anywhere (CI, Bruno, curl)
- Trace-correlated: every test request carries trace context, so a failure's trace id leads straight to the cross-service story in Tempo/Loki
- Adding a case is mechanical: one file implementing TestCase + one manifest line

### Negative
- The tests service is more machinery than `go test` (runner, HTTP API, fleet) and must itself be kept running
- In-memory executions vanish on restart; concurrent cases share the test user, so assertions must be id-based, never count-based
- Holding every service's private key makes enact-tests the most sensitive process on the platform — acceptable locally, a real hardening concern before any shared deployment

### Risks
- Test-data leaks if TearDown helpers regress — mitigated structurally (always-run TearDown, tolerant deletes) and verified by post-run leftover audits
- The case manifest and ACLs must track new routes/services — default deny surfaces omissions as 403s at test time