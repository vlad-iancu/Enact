# enact — minimal agent platform

A small agent platform built on Amazon Bedrock, OpenSearch, Redis Streams,
and Apache Tika. Agents can be grounded in documents two ways, independently:

* **Context files** — documents uploaded to a *knowledge base*. Agents that
  reference the KB load every document's full text into the model context at
  inference time.
* **RAG** — every agent has exactly one RAG configuration of its own.
  Documents uploaded to it are chunked and embedded; the most relevant chunks
  are retrieved (k-NN) per query at inference time.

> Authentication/authorization is intentionally **not** implemented. Any
> request may impersonate any user via the `X-User-Id` header (defaults to
> `default`). Do not expose these services publicly as-is.

## Services

| Binary (`cmd/…`)                | Purpose |
|---------------------------------|---------|
| `enact-model-management`        | `GET /v1/models` — friendly model names mapped to Bedrock model ids. |
| `enact-kb-api`                  | Create/delete knowledge bases; upload **context documents**. Raw file bytes are base64-encoded and pushed onto the Redis queue, **not** processed inline. |
| `enact-kb-document-indexer`     | Consumes the queue and extracts text via Tika. KB context documents are stored whole; agent RAG documents are chunked + embedded (Bedrock Titan) into the k-NN index. No public API (health probe only). |
| `enact-agent-management-api`    | CRUD for agents (`model`, `system_prompt`, `knowledge_base_ids`); upload **RAG documents** to an agent (`POST /v1/agents/{id}/rag/documents`). |
| `enact-model-inference`         | `POST /v1/inference`. Accepts `agent_id`; resolves the agent's model, loads the KB context files whole, retrieves top-k chunks from the agent's RAG collection, and augments the system prompt. Agents with MCP `tools` run a Bedrock tool-use loop (max `MAX_TURNS`, default 10): tool calls execute through the registry's MCP proxy and stream as `toolCall` / `toolCallResult` SSE events. |
| `enact-external-identities`     | Credentials the platform holds on a user's behalf at third parties (Google Workspace, GitHub, JIRA). Registers providers (`POST /v1/providers/oauth` with well-known discovery, `POST /v1/providers/pat`), owns the OAuth consent flow (`/v1/oauth/authorize` + `/v1/oauth/callback`), serves usable credentials to services, and refreshes tokens on a background sweep. Credentials are AES-256-GCM sealed at rest (ADR-0013). |
| `enact-tool-registry`           | MCP servers on the platform: register (`POST /v1/servers/create`, health-checked), partial update, list, and a cached tool catalogue (`GET /v1/servers/tools`, refreshed every `REFRESH_AT`). `/v1/servers/{id}/mcp` and `/{id}/sse` are spec-compliant MCP proxies to the underlying server. |

Every service also exposes `GET /healthz`, a home page at `/`, and Swagger UI
at `/swagger-ui/` for free (via `internal/service`).

## Architecture

```
          upload context doc                 enqueue (base64 bytes, typed)
  client ───────────────────▶ enact-kb-api ────────────▶ Redis Stream
          upload RAG doc                                      │ consume
  client ──────────────▶ enact-agent-mgmt-api ──────────▶─────┤
                                   │                          ▼
                                   │ agent CRUD        enact-kb-document-indexer ──▶ Tika
                                   ▼                     │        (extract text) ◀───┘
                              OpenSearch ◀───────────────┤
                              (metadata,     kb_context: │ document stored whole
                               documents,    agent_rag:  │ chunks+vectors (Bedrock embed)
                               kNN vectors)
                                   ▲
                                   │ load context files + retrieve (kNN)
  client ──▶ enact-model-inference ┘  (loads KB docs whole, embeds query,
              (POST /v1/inference,     searches agent RAG collection,
               agent_id)               augments prompt, Converse → Bedrock)
```

### Grounding design

Two independent grounding mechanisms per agent:

* **Context files** (`knowledge_base_ids`): every document of every referenced
  KB is loaded whole into the system prompt. Best for small, always-relevant
  material; no retrieval step, no embeddings.
* **RAG** (one collection per agent, uploaded via
  `POST /v1/agents/{id}/rag/documents`): documents are chunked (overlapping
  character windows, `internal/rag`) and embedded with Amazon Bedrock **Titan
  Text Embeddings v2** (`amazon.titan-embed-text-v2:0`, 1024-dim by default)
  into OpenSearch `knn_vector` fields (HNSW, cosine), filtered by `agent_id`
  (and `user_id`). At inference the query is embedded and the top-k chunks are
  added to the prompt.

The embedding model's output dimension **must** match `BEDROCK_EMBEDDING_DIM`,
which is used to create the k-NN index mapping. Change both together.

## Storage layout (OpenSearch indices)

* `enact-knowledge-bases` — KB metadata (`id`, `user_id`, `created_at`).
* `enact-agents` — agent records.
* `enact-kb-documents` — extracted full-text context documents of KBs.
* `enact-agent-rag-chunks` — RAG chunks with `embedding` (`knn_vector`),
  scoped by `agent_id`.

Each index's mapping is defined once as an OpenSearch composable **index
template** under `mappings/` (e.g. `mappings/enact-agent-rag-chunks.json`).
`make infrastructure-up` registers these templates and creates the indices
bare so the templates apply; the RAG chunk template's `dimension` is filled
from `BEDROCK_EMBEDDING_DIM` at apply time. The services do not define
mappings — they only verify the indices exist.

Knowledge bases and agents are identified by id only (no friendly names);
only models have friendly names (see `internal/models`).

## Running locally

1. **Configure:** `cp .env.example .env` and set `BEDROCK_API_KEY` /
   `AWS_REGION` (and edit `internal/models/registry.go` so the friendly model
   names map to models enabled in your account/region).

2. **Start infrastructure.** This starts the OpenSearch + Redis + Tika
   containers and creates the OpenSearch indices (from the index templates in
   `mappings/`) and the Redis stream + consumer group if they don't already
   exist:

   ```sh
   make infrastructure-up
   ```

   | Target                   | Effect |
   |--------------------------|--------|
   | `make infrastructure-up`    | Start containers; register index templates; create indices and the Redis stream/group if missing. |
   | `make infrastructure-down`  | Stop the containers (data is preserved). |
   | `make infrastructure-clean` | Delete the OpenSearch indices and the Redis stream, then recreate them empty (containers keep running). |

3. **Build / vet / test:**

   ```sh
   go mod tidy      # fetches opensearch-go, go-redis, uuid, OpenTelemetry and fills go.sum
   make all         # vet + test + build all binaries into dist/
   ```

   ### Debugging

   Set `DEBUG=1` on `make start` / `make restart` to run every service under a
   headless [Delve](https://github.com/go-delve/delve) debugger you can attach
   your IDE to (GoLand/IntelliJ: *Go Remote* run configuration; VS Code:
   `mode: "remote"` attach). `DEBUG=1` also builds the binaries with
   `-gcflags=all="-N -l"` (no optimizations/inlining), and switching the flag
   on or off forces a rebuild, so the running binaries always match the mode:

   ```sh
   make restart DEBUG=1   # rebuild for debugging + start under Delve
   make restart           # back to optimized binaries, no debugger
   ```

   Delve listens on `127.0.0.1`, one port per service:

   | Service                      | Debug port |
   |------------------------------|------------|
   | `enact-agent-management-api` | 40001      |
   | `enact-kb-api`               | 40002      |
   | `enact-kb-document-indexer`  | 40003      |
   | `enact-model-inference`      | 40004      |
   | `enact-model-management`     | 40005      |

   Requires Delve on your PATH: `go install github.com/go-delve/delve/cmd/dlv@latest`.

4. **Run the services** (each on its own port; the queue/store defaults point
   at the compose services). The KB, indexer, and agent services verify their
   indices exist at startup, so run `make infrastructure-up` first:

   ```sh
   SERVICE_PORT=8081 SERVICE_NAME=enact-model-management   go run ./cmd/enact-model-management
   SERVICE_PORT=8082 SERVICE_NAME=enact-kb-api             go run ./cmd/enact-kb-api
   SERVICE_PORT=8083 SERVICE_NAME=enact-kb-document-indexer go run ./cmd/enact-kb-document-indexer
   SERVICE_PORT=8084 SERVICE_NAME=enact-agent-management-api go run ./cmd/enact-agent-management-api
   SERVICE_PORT=8080 SERVICE_NAME=enact-model-inference     go run ./cmd/enact-model-inference
   ```

## Observability (LGTM)

Every service is instrumented with OpenTelemetry and exports the three signals
directly (OTLP/HTTP) to a local **LGTM** stack — **L**oki (logs), **G**rafana
(UI), **T**empo (traces), **M**imir (metrics):

| Signal  | Backend | Endpoint (default)                          |
|---------|---------|---------------------------------------------|
| Traces  | Tempo   | `http://localhost:4318/v1/traces`           |
| Metrics | Mimir   | `http://localhost:9009/otlp/v1/metrics`     |
| Logs    | Loki    | `http://localhost:3100/otlp/v1/logs`        |

Bring the stack up and open Grafana (anonymous admin, no login) at
`http://localhost:3000`:

```sh
make observability-up     # docker compose -f deploy/docker-compose.lgtm.yml up -d
# ... run the services (their OTEL_* defaults already point here) ...
make observability-down
```

Datasources are pre-provisioned and cross-linked, so in *Explore* you can move
from a Loki log line to its Tempo trace (via the `trace_id` field) and from a
span to Mimir RED metrics and a service graph. Tuning knobs live under
`OTEL_*` in `.env` (see `.env.example`); set `OTEL_SDK_DISABLED=true` to turn
telemetry off when the stack isn't running.

**Tracing across services.** The tracing middleware lives in
`internal/requesthelper`. Its server half — `requesthelper.TracingFilter`,
registered container-wide by the shared `service` runtime — extracts the
incoming W3C trace context, starts a span, and records RED metrics. Its client
half — `requesthelper.WrapClient` / `NewTransport` — injects the active trace
context into outbound requests so a call from one service continues the same
trace in the next. Use a wrapped `*http.Client` for any new service-to-service
call to keep the trace connected (the Tika client already does).

## Deploying with Docker

The seven services ship as one image each, built from the single root
`Dockerfile` (`--build-arg SERVICE=<cmd-dir>`, distroless, static Go
binaries). `deploy/docker-compose.app.yml` is the full platform for a VM:
the services **plus the infrastructure** — OpenSearch (with Dashboards on
loopback :5601), Redis, and Tika — with data in named volumes. Only AWS
(Bedrock/S3/SES/CloudFront) remains external. The images contain **no**
dotenv files — container environment is the only configuration source (so
the local `.env` overload behavior cannot interfere). Infrastructure
connection settings live in the compose file itself (`x-infra-env`); the
env files carry AWS credentials and platform settings. Plan ~4 GB of RAM on
the VM (OpenSearch runs with a 512 MB heap by default; raise
`OPENSEARCH_JAVA_OPTS` on larger hosts).

Build and push (from the dev machine; images target `linux/amd64`):

```sh
docker login ghcr.io                     # or your registry
make docker-push REGISTRY=ghcr.io/<owner>   # tags :<git-sha> and :latest
```

One-time VM setup:

```sh
# On the VM: install docker + the compose plugin, then
sudo mkdir -p /opt/enact && sudo chown $USER /opt/enact
docker login ghcr.io

# From the dev machine: generate DEPLOYMENT S2S material (prefer fresh keys
# over reusing local-dev ones; run from a clean checkout or temp dir) and
# copy the deployment files over:
make s2s-keygen        # also writes s2s/private-keys.yaml (enact-tests bundle)
rsync -av deploy/docker-compose.app.yml deploy/*.example s2s <vm>:/opt/enact/

# On the VM: the s2s mount must be readable by the distroless nonroot uid
sudo chown -R 65532:65532 /opt/enact/s2s && sudo chmod -R u=rX,go= /opt/enact/s2s

# On the VM: fill in the env files (gitignored; never commit the real ones)
cd /opt/enact
cp app.env.example app.env               # AWS creds + platform settings
cp enact-main.env.example enact-main.env # OAuth/frontend/SES/S3
echo 'ENACT_REGISTRY=ghcr.io/<owner>' > .env
echo 'ENACT_TAG=<git-sha>' >> .env
echo 'OPENSEARCH_PASSWORD=<strong-password>' >> .env  # in-stack cluster admin

# First boot: start OpenSearch alone, create the indices (idempotent),
# then bring up everything (the services verify their indices at startup):
docker compose -f docker-compose.app.yml up -d opensearch
OPENSEARCH_ADDRESSES=https://127.0.0.1:9200 OPENSEARCH_PASSWORD=<password> \
  ./scripts/infrastructure.sh provision
docker compose -f docker-compose.app.yml up -d
```

Run and operate:

```sh
docker compose -f docker-compose.app.yml up -d
docker compose -f docker-compose.app.yml ps       # all Up; enact-main on :8000
docker compose -f docker-compose.app.yml logs -f enact-main
```

Only `enact-main` (8000) is published; `enact-tests` listens on
`127.0.0.1:8006` (reach it via `ssh -L 8006:127.0.0.1:8006 <vm>` to trigger
the integration suite).

### Hosting the frontend on the same machine

The stack ships an optional web entry point (`--profile web`): a Caddy
container that serves the SPA build and proxies `/api/*` to enact-main —
one origin, so CORS never comes into play and cookies stay `SameSite=lax`.
With a domain in the Caddyfile, Let's Encrypt TLS is automatic (open ports
80+443 in the EC2 security group).

```sh
# On the VM, next to the compose file:
cp Caddyfile.example Caddyfile   # then pick the domain or bare-IP block

# From the frontend repo on the dev machine:
npm run build
rsync -av --delete dist/ user@vm:/opt/enact/www/

# Point the browser-facing URLs at the proxy in enact-main.env:
#   PUBLIC_BASE_URL=https://<domain>/api
#   OAUTH_REDIRECT_URL=https://<domain>/api/google/oauth/callback   (register
#     this exact URL in the Google Cloud console)
#   FRONTEND_URL=https://<domain>
#   SECURE_COOKIES=true
# ...and set the frontend's API base URL to /api when building it.

docker compose -f docker-compose.app.yml --profile web up -d
```

`make docker-deploy` detects a `Caddyfile` on the VM and enables the
profile automatically. Frontend updates are just a rebuild + rsync of
`www/` — no container restart needed (Caddy serves the files live). Once
the proxy fronts everything, consider re-binding enact-main's port to
loopback (`127.0.0.1:8000:8000` in the compose file) so nothing bypasses
TLS.

To update: `make docker-push REGISTRY=...` on the dev machine, bump
`ENACT_TAG` in `/opt/enact/.env`, then `docker compose pull && docker compose
up -d` on the VM (`make docker-deploy VM=user@host` automates the
sync+pull+up path). Services shut down gracefully on SIGTERM.

The stack can also run against an external OpenSearch/Redis (e.g. Aiven)
instead of the in-stack containers: override the `x-infra-env` values in the
compose file and run `infrastructure.sh provision` against that cluster
(`OPENSEARCH_INSECURE_SKIP_VERIFY=false` for real certificates). On Aiven's
free tier the cluster powers off when idle; the services then crash-loop
(`restart: unless-stopped`) until it is powered back on — expected recovery
behavior, not a bug.

## Per-user credentials for MCP tools

A registered MCP server can declare, per tool, which third-party credentials
the tool needs and how to present them. At call time the inference service
resolves the **calling user's** credentials, renders them, and injects them;
if the user has not connected the account yet the call parks and the UI is
told (ADR-0014).

```json
{
  "id": "github-mcp",
  "url": "https://api.githubcopilot.com/mcp/",
  "transport_type": "streamable-http",
  "tool_access_requirements": {
    "list_issues": [{ "provider": "github", "access_level": "read" }]
  },
  "tool_authorizations": {
    "list_issues": {
      "headers_authorization": [
        { "header_name": "Authorization", "header_template": "Bearer {{ (cred \"github\").Credentials }}" }
      ],
      "param_authorization": [
        { "param_name": "api_key", "param_template": "{{ (cred \"github\").Credentials }}" }
      ]
    }
  }
}
```

**`"*"` stands for every tool the server offers** — the ordinary case, where
one server talks to one third party and every tool needs the same
credential. It works in both maps, and a key naming a tool outright beats it
for that tool, *whole*: the specific entry replaces the wildcard's rather
than adding to it, so what a tool sends is always one list read from one
place. A tool needing the wildcard's credentials plus its own restates both.
The two maps are resolved independently, so a tool may declare its own
requirements and still inherit the wildcard's authorization — registration
validates that combination by rendering the inherited templates against that
tool's own requirements.

```json
"tool_access_requirements": { "*": [{ "provider": "github", "access_level": "read" }] },
"tool_authorizations": { "*": { "headers_authorization": [
  { "header_name": "Authorization", "header_template": "Bearer {{ (cred \"github\").Credentials }}" }
]}}
```

A tool may require several providers but **one access level per provider**.
`access_level` may be **omitted**, and then the requirement is only "the user
has connected this provider" with no coverage check — which is the whole
contract for a credential that has nothing to say about scope, such as a
plain API key.

Provider registration differs by type. A **PAT** provider needs nothing but a
name: the user pastes a token whose scope the platform can neither see nor
influence, so `access_levels` there is optional labelling. An **OAuth**
provider must declare `scopes` or `access_levels` — consent is a list of
permissions shown to a user, so a provider naming none describes nothing a
caller could request.
Credentials belong to the **provider**, not to the server: a user connects
GitHub once and every server that declares a GitHub requirement uses that
one identity — so registering a server is enough to reach a user's
credential for any provider it declares, and disconnecting a provider
revokes it everywhere at once.

**Templates** are Go `text/template` over the resolved credentials, keyed by
provider name (`.Credentials`, `.Username`, `.TokenType`, `.AccessLevel`).
Use **`cred "name"`** rather than a field selector: provider names may
contain `-`, and `{{ .my-provider.Credentials }}` does not parse. `b64` is
available for Basic auth:
`Basic {{ b64 (printf "%s:%s" (cred "jira").Username (cred "jira").Credentials) }}`.
Registration validates every template by rendering it against sentinel
credentials, so a configuration that registers is one that will render.

**When a credential is missing**, the stream emits
`toolCallWaitingAuthorization` with `status` `waiting` → `resolved` |
`timeout`:

```json
{ "server_id": "github-mcp", "tool": "list_issues", "tool_use_id": "…",
  "status": "waiting",
  "missing": [{ "provider": "github", "access_level": "read", "reason": "not_connected" }] }
```

It carries coordinates, not a URL — the inference service does not know the
frontend's origin. The UI builds
`/identities/connect?provider=…&access_level=…` (OAuth) or posts
`/identities/pat`. The moment the credential lands, the identity
service publishes on Redis and the parked call resumes (measured at ~20ms);
`TOOL_AUTH_RECHECK_INTERVAL` is the fallback and `TOOL_AUTH_WAIT_TIMEOUT`
(default 5m) the ceiling.

**Tool calls are persisted** on the assistant message of a conversation
(`tool_calls`: server, tool, `tool_use_id`, the model's arguments, the result
text and `is_error`), so reopening a thread shows what the agent did, not
only what it concluded. The arguments stored are the model's own — credential
injection happens after the record is made, so a stored conversation never
carries a user's token. They are stored but **not indexed**: a tool's
arguments are arbitrary JSON, and dynamic mapping would turn every argument
name into an index field.

They are also **replayed into the model's context** on later turns, as a
transcript rather than a summary. Each record carries its `turn` (the
tool-loop round it belongs to), the assistant's own `text` in that round, and
the result as the tool returned it — prose in `content`, structure in
`structured_content`. Replay rebuilds the rounds in order:

```
assistant { text, tool_use }    round 1
user      { tool_result }
assistant { text, tool_use }    round 2 — made KNOWING round 1's results
user      { tool_result }
assistant { text }              the answer
```

Collapsing the rounds would tell the model something untrue about its own
reasoning, so they are kept apart. Structured results are sent as native
`ToolResultContentBlockMemberJson` blocks, so the model reads JSON as JSON;
a tool that returned both prose and data contributes both blocks. The
pairing is validated upstream — a `tool_use` with no matching `tool_result`
in the next message is rejected — so both halves are built from one record
and dropped together, and a call whose tool the agent no longer has is
skipped with only its text surviving. The cost is real: those tool results
are re-sent, and re-charged as input tokens, on every subsequent turn.

The UI can also ask **before** a conversation starts:
`GET /agents/{id}/required-identities` returns every credential the agent's
tools need, joined with whether the logged-in user has it
(`connected`, `reason`, plus `provider_type` and `access_levels` so the
screen knows whether to redirect into OAuth or show a token form), and a
top-level `satisfied` for a simple "can I run this agent?" check. Entries
are one per (provider, access level) across all of the agent's servers,
listing the `servers` and `tools` that need each — one prompt per account to
connect, not one per server.

**Re-connecting never narrows a credential.** Because one identity serves
every server, an OAuth authorization asks for the scopes the user already
granted *plus* the newly requested ones; connecting at `read` cannot strip
the `admin` access another server depends on.

**Disconnecting revokes.** `DELETE /identities/{provider}` asks the provider
to invalidate the tokens (RFC 7009, against the record's `revoke_url` —
taken from the well-known document's `revocation_endpoint` when registration
does not name one) *before* deleting the platform's copy, so disconnecting
ends the access rather than merely forgetting it. Revocation is best effort
by design: a PAT has no revocation API (the user deletes it at the provider),
and a provider outage must never leave a user unable to delete a credential —
so the local copy always goes, and the outcome is logged as
`revocation=revoked|unsupported|failed`.

**Deleting an account deletes its credentials.** `POST /admin/delete-user`
calls `DELETE /v1/identities/all` first, revoking each credential and
removing it; unlike the avatar cleanup this step is allowed to fail the whole
deletion, because a credential nobody can map back to a user is a live grant
that the refresh sweep would keep alive indefinitely.

**Every deletion path revokes**, through one choke point (`revokeAll`): a
user disconnecting, an account being deleted, and an admin force-deleting a
provider — which revokes *before* the cascade, since the provider record it
is about to delete holds the revocation endpoint and the client secret.
Re-authorizing is the deliberate exception: replacing a credential does not
revoke the one it replaces, because most providers revoke the whole grant
and a re-authorization often returns the same refresh token.

**Servers that authenticate their handshake** — GitHub's answers 401 before
the JSON-RPC body is parsed, so `initialize` and `tools/list` are refused,
not just tool calls — are configured with two further keys, `"/initialize"`
and `"/list-tools"`. They lead with `/` because an MCP tool name never does.

```json
"tool_access_requirements": { "/initialize": [{ "provider": "github", "access_level": "read" }] },
"tool_authorizations": { "/initialize": { "headers_authorization": [
  { "header_name": "Authorization", "header_template": "Bearer {{ (cred \"github\").Credentials }}" }
]}}
```

These are resolved against the server's **owner**, everywhere: at
registration, on the background refresh where no user exists at all, and
inside a caller's agent session, where the owner's headers go *underneath*
the caller's — the tool's own headers win a name clash. They are the key to
the door; the caller's credentials decide who they are once inside. So a
gated server is usable by people who have no account at that third party,
which is the point, and it does mean an agent reaches it on the owner's
access.

Declaring one phase covers both. They never inherit `"*"` — a wildcard is
about tools, and inheriting it would quietly start spending the owner's
credential on platform traffic. Registration and any update that changes
them re-probes the server, so fixing a broken credential is proved rather
than assumed.

## End-to-end example

```sh
# 1. See available models
curl -s localhost:8081/v1/models

# 2. Create a knowledge base
KB=$(curl -s -X POST localhost:8082/v1/knowledge-bases | jq -r .id)

# 3. Upload a context document to the KB (multipart file; processed async).
#    The raw bytes are base64-encoded onto the queue and Tika extracts the
#    text — PDFs, Office docs, HTML, plain text, etc. all work. Agents that
#    reference this KB load the whole document into context at inference time.
echo "Enact is a minimal agent platform. It uses OpenSearch for vector storage." > notes.txt
curl -s -X POST localhost:8082/v1/knowledge-bases/$KB/documents \
  -F 'file=@notes.txt'

# 4. Create an agent that uses that KB as context
AGENT=$(curl -s -X POST localhost:8084/v1/agents \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"claude-3-5-sonnet\",\"system_prompt\":\"You are a helpful assistant.\",\"knowledge_base_ids\":[\"$KB\"]}" \
  | jq -r .id)

# 5. (Optional) Upload a document to the agent's RAG configuration; it is
#    chunked + embedded async and retrieved per query at inference time.
curl -s -X POST localhost:8084/v1/agents/$AGENT/rag/documents \
  -F 'file=@handbook.pdf'

# 6. Invoke the agent (context loading + RAG happen server-side)
curl -s -X POST localhost:8080/v1/inference \
  -H 'Content-Type: application/json' \
  -d "{\"agent_id\":\"$AGENT\",\"messages\":[{\"role\":\"user\",\"content\":\"What does enact use for vector storage?\"}]}"
```

`POST /v1/inference` still works without an agent — supply `model` and
`messages` directly (set `"stream": true` for SSE streaming). Agent requests
may set `"retrieval_top_k"` (1–50) to control how many RAG chunks are
retrieved per query (default 5).

## Notes & limitations

* No auth: ownership is by `X-User-Id` header only.
* Knowledge bases support create/delete only (no update), per spec.
* Context files are loaded **whole** into the prompt — every document of every
  KB the agent references. Large KBs can exceed the model's context window;
  use the agent's RAG configuration for large corpora.
* The indexer ack's a message only after all chunks index successfully;
  failures leave it pending in the Redis consumer group. The consumer sweeps
  the pending list every `REDIS_RECLAIM_INTERVAL` (30s), reclaiming deliveries
  idle longer than `REDIS_RECLAIM_MIN_IDLE` (2m) — including ones stranded by
  a dead consumer — and drops a message after `REDIS_MAX_DELIVERIES` (5)
  failed attempts so a poison message cannot block the queue.
* The friendly→Bedrock model map defaults to `eu.*` inference profiles for
  `eu-north-1`; adjust `internal/models/registry.go` for your region.
