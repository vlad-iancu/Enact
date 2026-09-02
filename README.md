# enact — minimal agent platform

A small agent platform built on Amazon Bedrock, OpenSearch, Redis Streams,
and Apache Tika. Agents are grounded in *knowledge bases*, of which there are
two kinds:

* **Context** (`"kind": "context"`) — documents are stored whole. An agent
  referencing the KB loads every document's full text into the model context
  at inference time. An agent may reference any number of them.
* **Retrieval** (`"kind": "rag"`) — documents are chunked and embedded, and
  the most relevant chunks are retrieved (k-NN) per query. An agent may attach
  **exactly one**, through `rag_knowledge_base_id`.

Both are ordinary knowledge bases: created, shared and permissioned the same
way (`enact:kb:…`), and independent of the agents that use them.

> Authentication/authorization is intentionally **not** implemented. Any
> request may impersonate any user via the `X-User-Id` header (defaults to
> `default`). Do not expose these services publicly as-is.

## Services

| Binary (`cmd/…`)                | Purpose |
|---------------------------------|---------|
| `enact-model-management`        | `GET /v1/models` — friendly model names mapped to Bedrock model ids. |
| `enact-kb-api`                  | Create/delete knowledge bases of either kind; upload **documents** (`POST /v1/knowledge-bases/{id}/documents`) — the KB's kind decides whether they are stored whole or chunked and embedded. Raw file bytes are base64-encoded and pushed onto the Redis queue, **not** processed inline. |
| `enact-kb-document-indexer`     | Consumes the queue and extracts text via Tika. A context KB's documents are stored whole; a retrieval KB's are chunked + embedded (Bedrock Titan) into the k-NN index. No public API (health probe only). |
| `enact-agent-management-api`    | CRUD for agents (`model`, `system_prompt`, `knowledge_base_ids`, `rag_knowledge_base_id`, `tools`, `output_schema`). Documents live in the KB service; this validates that a referenced KB exists and, for `rag_knowledge_base_id`, that it is of kind `rag`. |
| `enact-model-inference`         | `POST /v1/inference`. Accepts `agent_id`; resolves the agent's model, loads the context KBs' files whole, retrieves top-k chunks from its retrieval KB, and augments the system prompt. Agents with MCP `tools` run a Bedrock tool-use loop (max `MAX_TURNS`, default 10): tool calls execute through the registry's MCP proxy and stream as `toolCall` / `toolCallResult` SSE events. An agent's `output_schema` becomes the Converse `OutputConfig`, constraining the reply to JSON; a `malformed_model_output` stop fails the request rather than returning prose the caller would parse as JSON. |
| `enact-external-identities`     | Credentials the platform holds on a user's behalf at third parties (Google Workspace, GitHub, JIRA). Registers providers (`POST /v1/providers/oauth` with well-known discovery, `POST /v1/providers/pat`), owns the OAuth consent flow (`/v1/oauth/authorize` + `/v1/oauth/callback`), serves usable credentials to services, and refreshes tokens on a background sweep. Credentials are AES-256-GCM sealed at rest (ADR-0013). |
| `enact-rbac`                    | Organizations, memberships, roles and the rules they grant. `GET /v1/effective` is the hot path — a user's organization and every rule they hold, which services cache and evaluate locally. Also organization requests and their approval, and `POST /v1/grants`, the ownership bookkeeping every service writes when it creates a resource. |
| `enact-workflows`               | Workflow authoring and execution intake. A workflow-level `input_schema` is enforced at the trigger; a code step's `output_schema` is enforced by the runner (agent steps take their shape from the agent's own `output_schema`). `GET /v1/workflows/{id}/shapes` resolves both, plus the per-step context schema an editor completes `ctx` against. A workflow is an ordered list of **agent** steps (an existing agent, prompt templated with Go templates), **code** steps (JavaScript over the step context) and **google-docs** / **google-sheets** / **google-slides** steps (export to a file, create, and — for Sheets — append rows), the last three of which name an identity **provider** and act as the triggering user. `POST /v1/workflows/{id}/executions` validates `use`, writes an execution record and queues it — it never executes anything itself. |
| `enact-workflow-runner`         | Consumes queued executions and runs their steps in order **as the triggering user**, updating the record after each. Agent steps call `enact-model-inference`, which re-checks `enact:agent:use:{id}` — so a workflow cannot run an agent its triggerer may not. Code steps run in goja with no host access and a wall-clock interrupt. No public API (health probe only). |
| `enact-crawls`                  | Focused-crawl authoring and run intake. A crawl pairs a natural-language query with seed URLs and an **empty retrieval knowledge base** (checked at creation: a crawl becomes that KB's sole writer). `POST /v1/crawls/{id}/runs` writes a run record and queues it — it never crawls anything itself. |
| `enact-crawl-orchestrator`      | Schedules and executes crawls. A ticker queues crawls whose `next_run_at` has passed; a consumer runs them. Each run disambiguates the query against BabelNet, expands it, then crawls best-first — scoring every page with the Mihalcea semantic measure blended with BM25 — and syncs what it keeps into the knowledge base incrementally. No public API (health probe only). |
| `enact-tool-registry`           | MCP servers on the platform: register (`POST /v1/servers/create`, health-checked), partial update, list, and a cached tool catalogue (`GET /v1/servers/tools`, refreshed every `REFRESH_AT`). `/v1/servers/{id}/mcp` and `/{id}/sse` are spec-compliant MCP proxies to the underlying server. |

Programmatic access: `POST /auth/keys` on enact-main issues a per-user API key (`enact_sk_…`, returned once, stored only as a SHA-256). Presented as `Authorization: Bearer` or `X-Enact-Api-Key` (the former is consumed and stripped by the auth filter so the S2S filter, which reads the same header, never sees it), it authenticates the agent, knowledge-base, MCP-server, conversation, model and inference routes as that user — organizations, identities/providers, admin and key management stay session-only, so a key can neither escalate nor mint another.

Every service also exposes `GET /healthz`, a home page at `/`, and Swagger UI
at `/swagger-ui/` for free (via `internal/service`).

## Architecture

```
          upload document (either kind)      enqueue (base64 bytes, typed)
  client ───────────────────▶ enact-kb-api ────────────▶ Redis Stream
                                   │                          │ consume
          agent CRUD               │ KB CRUD                  ▼
  client ──▶ enact-agent-mgmt-api ─┤            enact-kb-document-indexer ──▶ Tika
                                   │              │        (extract text) ◀───┘
                              OpenSearch ◀────────┤
                              (metadata,  context: │ document stored whole
                               documents,   rag:   │ chunks+vectors (Bedrock embed)
                               kNN vectors)
                                   ▲
                                   │ load context files + retrieve (kNN)
  client ──▶ enact-model-inference ┘  (loads context KB docs whole, embeds
              (POST /v1/inference,      query, searches the agent's retrieval
               agent_id)                KB, augments prompt, Converse → Bedrock)
```

### Grounding design

Two independent grounding mechanisms per agent, both backed by knowledge
bases:

* **Context files** (`knowledge_base_ids`): every document of every referenced
  context KB is loaded whole into the system prompt. Best for small,
  always-relevant material; no retrieval step, no embeddings.
* **Retrieval** (`rag_knowledge_base_id`, exactly one): documents are chunked
  (overlapping character windows, `internal/rag`) and embedded with Amazon
  Bedrock **Titan Text Embeddings v2** (`amazon.titan-embed-text-v2:0`,
  1024-dim by default) into OpenSearch `knn_vector` fields (HNSW, cosine),
  filtered by `kb_id`. At inference the query is embedded and the top-k chunks
  are added to the prompt.

A retrieval knowledge base's `chunk_size` and `chunk_overlap` (in runes) are
set **at creation and never after** — `POST /v1/knowledge-bases` takes them,
`PUT` refuses them. Chunking happens at upload time, so a value changed
mid-life would apply only to later documents and leave one KB holding two
incompatible chunkings with nothing recording which is which. Size must be
100–8000 and overlap strictly less than size. Omitted, they are **recorded**
as the platform defaults (1000/150) rather than left blank, so moving
`RAG_CHUNK_SIZE` later cannot silently re-chunk what somebody uploads to an
existing knowledge base tomorrow. They are rejected outright on a context KB,
which stores documents whole. The embedding model stays global — a KB on a
different-dimension model would need its own index.

One retrieval KB, not many, is deliberate: k-NN ranks passages by distance
*within* one collection, so searching several and merging their scores
compares numbers that were never comparable — in practice one corpus quietly
crowds out another. Combine sources by putting them in the same knowledge
base, where they are embedded the same way. Context KBs have no such limit
because nothing is ranked.

The embedding model's output dimension **must** match `BEDROCK_EMBEDDING_DIM`,
which is used to create the k-NN index mapping. Change both together.

## Storage layout (OpenSearch indices)

* `enact-knowledge-bases` — KB metadata (`id`, `user_id`, `kind`, `created_at`).
* `enact-agents` — agent records. `output_schema` is `enabled: false`: it is arbitrary user JSON, so it is kept in `_source` but never mapped.
* `enact-kb-documents` — extracted full-text documents of context KBs.
* `enact-agent-rag-chunks` — retrieval chunks with `embedding` (`knn_vector`),
  scoped by `kb_id`. The index name predates knowledge-base kinds and is kept
  so existing deployments' data stays where it is.
* `enact-crawls` — focused-crawl definitions. `pages` (the URL-to-document
  map that makes re-crawling incremental) is `enabled: false`: its keys are
  arbitrary URLs and would otherwise create one mapping field per page ever
  crawled.
* `enact-crawl-runs` — one record per run, carrying the report: the
  disambiguated query, its expansion, and the crawl graph. All `enabled: false`.
* `enact-babelnet-cache` — cached sense lookups, and the per-day request
  counter. Never expires and is **not** org-scoped: it holds public lexical
  facts, and sharing one cache across every crawl is what makes a
  1000-request daily allowance workable.
* `enact-organizations`, `enact-organization-requests`, `enact-memberships`,
  `enact-roles` — the authorization model. A membership's document id is the
  user id, so belonging to exactly one organization is structural rather than
  enforced.
* `enact-workflows` — workflow definitions; `steps` is `enabled: false` (user-authored prompts and JavaScript).
* `enact-workflow-executions` — one record per run: the trigger input, the step definitions **as they were when it ran**, and each step's input, output and timing. All of it `enabled: false`.
* `enact-users` — accounts, keyed by normalized email. `api_keys[].key_hash`
  is an indexed keyword because authentication resolves a user *from* a
  presented key; the key itself is never stored, only its SHA-256.

Every resource carries `organization_id`, and identity providers and MCP
servers are additionally **keyed** by it (`<organization>:<name>` and
`<organization>:<id>`) because their names are chosen by people rather than
generated — see [Organizations and permissions](#organizations-and-permissions).

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

3. **Fetch WordNet** (only needed for focused crawls). `enact-crawl-orchestrator`
   will not start without it — it backs Wu-Palmer similarity, lemmatisation and
   page-side sense lookup. Downloads ~36 MB into `dist/` and is idempotent:

   ```sh
   make wordnet
   ```

   Crawls also want `BABELNET_API_KEY` in `.env` (a free key from
   babelnet.org). Without one the orchestrator runs but every crawl fails at
   query analysis.

   | Target                   | Effect |
   |--------------------------|--------|
   | `make infrastructure-up`    | Start containers; register index templates; create indices and the Redis stream/group if missing. |
   | `make infrastructure-down`  | Stop the containers (data is preserved). |
   | `make infrastructure-clean` | Delete the OpenSearch indices and the Redis stream, then recreate them empty (containers keep running). |

4. **Build / vet / test:**

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

5. **Run the services** (each on its own port; the queue/store defaults point
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

## Organizations and permissions

Every user belongs to exactly one **organization**, every resource belongs to
exactly one, and nothing crosses. A user who belongs to none can log in, see
their profile and ask for an organization — nothing else. Organizations are
created by an administrator approving a request; the requester becomes the
first owner. See ADR-0019.

**Permissions are four colon-separated segments**, matched segment by segment
with `*` as a single-segment wildcard. There is no regex:

```
enact:<resource-type>:<action>:<resource-id>

enact:kb:edit:9f2a…      one knowledge base
enact:agent:view:*       every agent
enact:kb:*               every action on every knowledge base
```

A rule may stop early and cover everything below it. A rule with *more*
segments than the question never matches, so a grant on one resource is never
inherited by a question about all of them.

`resource-type` is one of `kb agent mcp-server provider identity conversation
user role organization`; `action` is one of `view edit delete create use`.
`use` is the one that is not record management: running an agent, retrieving
from a knowledge base, calling a server's tools, connecting an account through
a provider. A viewer typically holds `view` without `use`.

**Roles hold rules and live inside an organization.** A user's own id doubles
as a hidden role name: creating a resource grants `enact:<type>:*:<id>` under
the role named after its creator, so ownership uses the same grammar as
everything else. Hidden roles never appear in role listings, cannot be edited,
and their names are reserved so no visible role can claim one.

**Owners may do anything inside their own organization** and nothing outside
it. The bypass exists because a new organization's first owner holds no rules
at all, and because an owner edits the roles anyway — enforcing rules against
them would add a step, not a safeguard.

### What the browser sees

```
GET /organizations/me            organization, name, owner flag, roles, rules
GET /organizations/me/roles      the caller's own roles, with what each grants
POST /organizations/requests     ask for an organization
GET  /organizations/{id}/members members, with display names and avatars
POST /organizations/{id}/roles   define a role and its rules   (owners)
POST /organizations/{id}/roles/{name}/assign|unassign          (owners)
POST /organizations/{id}/users   create an account in the organization (owners)
```

Listings of agents, knowledge bases, MCP servers and providers return
`editable`, `deletable` and `usable` per item, computed from the caller's
rules with the same matcher the services enforce with — so a control the UI
shows and the answer a later call gives cannot disagree. They are hints: the
gate is the call itself.

### Two deliberate asymmetries

**Refusals read as 404, not 403,** whenever a resource is named: "not yours"
must be indistinguishable from "does not exist". A caller with no organization
at all gets 403 instead, because that says nothing about what exists and is
the one case they can act on.

**Conversations carry no rules.** They are private to one person, checked by
comparing the stored user id, and no permission is consulted — so nobody can
be granted access to someone else's, including an owner. A conversation holds
its author's words and the verbatim output of every tool call made on their
behalf, which is a different thing from an organization's shared resources.

## Per-user credentials for MCP tools

A registered MCP server can declare, per tool, which third-party credentials
the tool needs and how to present them. At call time the inference service
resolves the **calling user's** credentials, renders them, and injects them;
if the user has not connected the account yet the call is refused and the UI
is told what to connect (ADR-0014).

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

Providers belong to an **organization**, not to the platform. The document is
keyed by `<organization>:<name>`, so two organizations may each register their
own `github` with different clients and scopes, and neither can see the
other's. Registration is on the ordinary user surface —
`POST /identities/providers/oauth|pat`, `DELETE /identities/providers/{name}` —
gated by the rule `enact:provider:create` rather than by being the platform
administrator. An organization owner holds it by owner bypass and may delegate
it through a role; registering records ownership of that provider, so the
registrar can also delete it. (These endpoints used to live under `/admin`,
which stopped making sense once a provider stopped being platform-wide.)

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

**When a credential is missing the call fails**, immediately. The stream
emits `toolCallAuthorizationRequired`, then the call's ordinary
`toolCallResult` with `is_error: true` — so the model learns it failed and
can say so, rather than the turn stalling:

```json
{ "server_id": "github-mcp", "tool": "list_issues", "tool_use_id": "…",
  "missing": [{ "provider": "github", "access_level": "read", "reason": "not_connected" }] }
```

It carries coordinates, not a URL — the inference service does not know the
frontend's origin. The UI builds
`/identities/connect?provider=…&access_level=…` (OAuth) or posts
`/identities/pat`, and the user asks again. **Nothing retries on their
behalf**: a parked call would hold a model turn, an SSE stream and a Bedrock
context open on the chance somebody finishes an OAuth flow in another tab.

To avoid the failure entirely, ask
`GET /agents/{id}/required-identities` before starting — it answers the same
question up front, so the UI can prompt for the account before the user
sends a message rather than after.

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

# 2. Create a context knowledge base ("kind" defaults to "context")
KB=$(curl -s -X POST localhost:8082/v1/knowledge-bases \
  -H 'Content-Type: application/json' -d '{"name":"notes"}' | jq -r .id)

# 3. Upload a context document to the KB (multipart file; processed async).
#    The raw bytes are base64-encoded onto the queue and Tika extracts the
#    text — PDFs, Office docs, HTML, plain text, etc. all work. Agents that
#    reference this KB load the whole document into context at inference time.
echo "Enact is a minimal agent platform. It uses OpenSearch for vector storage." > notes.txt
curl -s -X POST localhost:8082/v1/knowledge-bases/$KB/documents \
  -F 'file=@notes.txt'

# 4. (Optional) Create a RETRIEVAL knowledge base and upload to it. Its
#    documents are chunked + embedded async and retrieved per query.
#    chunk_size/chunk_overlap are optional and creation-only (defaults 1000/150).
RAGKB=$(curl -s -X POST localhost:8082/v1/knowledge-bases \
  -H 'Content-Type: application/json' \
  -d '{"name":"handbook","kind":"rag","chunk_size":800,"chunk_overlap":100}' | jq -r .id)
curl -s -X POST localhost:8082/v1/knowledge-bases/$RAGKB/documents \
  -F 'file=@handbook.pdf'

# 5. Create an agent: the context KB is loaded whole, the retrieval KB is
#    searched per query. An agent may attach exactly one of the latter.
AGENT=$(curl -s -X POST localhost:8084/v1/agents \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"assistant\",\"model\":\"claude-3-5-sonnet\",\"system_prompt\":\"You are a helpful assistant.\",\"knowledge_base_ids\":[\"$KB\"],\"rag_knowledge_base_id\":\"$RAGKB\"}" \
  | jq -r .id)

# 6. Invoke the agent (context loading + retrieval happen server-side)
curl -s -X POST localhost:8080/v1/inference \
  -H 'Content-Type: application/json' \
  -d "{\"agent_id\":\"$AGENT\",\"messages\":[{\"role\":\"user\",\"content\":\"What does enact use for vector storage?\"}]}"
```

`POST /v1/inference` still works without an agent — supply `model` and
`messages` directly (set `"stream": true` for SSE streaming). Agent requests
may set `"retrieval_top_k"` (1–50) to control how many chunks are retrieved
from the agent's retrieval knowledge base per query (default 5).

Sampling is tunable per request with `"temperature"` and `"top_p"`, both
0–1 and both rejected as a 400 outside it. **Set one or the other, not
both** — the Anthropic models refuse a request carrying both, so enact
rejects it up front rather than letting it come back as a Bedrock
`ValidationException`. They apply to agent invocations
too: naming an agent fixes its model, prompt and output schema, not how its
replies are sampled. Omit them and the model's own defaults apply — enact
sends nothing it was not given, so there is no platform default to inherit.
All three travel through enact-main as well, on `POST /inference` and on
`POST /conversations/{id}/messages`; nothing about them is persisted, so
they are set per call. `max_tokens` is accepted by the inference service
directly but is not forwarded by enact-main.

### Focused crawling

A **crawl** fills a retrieval knowledge base from the web and keeps it current.
It pairs a natural-language query with seed URLs and an empty retrieval KB, and
walks outwards taking the most promising unvisited link first.

Relevance is knowledge-based rather than statistical, and is computed per page:

```
score = alpha * semantic + (1 - alpha) * BM25
```

* **semantic** — Mihalcea, Corley & Strapparava (2006) text-to-text similarity
  over Wu-Palmer concept distance, computed on **synsets**: the query is
  POS-tagged, disambiguated, and expanded across the semantic graph with
  decaying weights (synonym 1.0, one hop 0.6, two hops 0.3). Instance relations
  are not followed — every named river is an instance of "river", and following
  them turns a topical expansion into a gazetteer.

The **query** is disambiguated by ant colony optimisation over the extended
Lesk objective (Banerjee & Pedersen 2002), not by choosing each word's best
sense in turn. Word-by-word Lesk has no way to notice that its answers
contradict one another: measured on "opensearch indices, security, syntax and
usage" it returned the software sense of `opensearch` beside the *semiotics*
sense of `index` and the *collateral* sense of `security` — each defensible
alone, together a reading of no text that exists. The colony scores a whole
assignment at once (every pair of chosen senses, plus their agreement with the
surrounding text), so senses that support one another win. It costs no extra
BabelNet requests: the same glosses are fetched once and the search is
arithmetic over them. Pages keep the greedy version — there are hundreds of
them, and a page is its own context.

The **starting pages are part of that context.** They are fetched before the
query is read and their most characteristic lemmas join the context bag, which
is the difference between disambiguating five words against each other and
disambiguating them against the corpus they were written about. They are handed
to the crawl afterwards through `Options.Prefetched`, so the site is asked for
them once. `analysis.seed_context` in the report is the vocabulary that was
used.
* **BM25** — over the expanded query **and the query's own words**, normalised
  against the query's ten strongest terms so the ceiling is one a real document
  reaches. It covers what the sense inventory has never heard of: product
  names, jargon, people.

**Names are weighted above ordinary words** (`CRAWL_ENTITY_WEIGHT`, default 3).
A term counts as a name on evidence from the text, never from a dictionary.
Absence from WordNet used to stand in for "is a name" and it was a bad proxy:
WordNet is a 2006 general-English lexicon, so it also lacks `rebalancing` and
every typo — measured, it marked `documetation` and `databse` as products and
gave them triple weight, and because that weight enters BM25's normalising
ceiling a typo dragged down every page in the crawl. The signals now are:

* prose's entity extractor found a name there;
* the tagger called it a capitalised proper noun, away from the first token
  where a capital means only that a sentence began;
* it is **spelled** like one — a capital inside the word (`OpenSearch`,
  `gRPC`), letters mixed with digits (`IPv6`, `S3`), or an all-capital run
  (`ORM`, `CDC`). These are conventions the industry uses precisely because
  they mark a name;
* **a model says so.** Optional, off by default: a BERT token classifier
  (`Xenova/bert-base-NER`, int8, ~109 MB) run through ONNX Runtime via
  `onnxruntime_go`. `make ner-model` fetches it; `NER_ENABLED=true` turns it
  on. It reads the seed pages, and what it finds joins the rules rather than
  replacing them — it is cased and newswire-trained, with no software class, so
  it misses names a capital letter in mid-word gives away for free. Its
  measured advantage is **precision**: on a real crawled page it returned 7
  names against the rules' 68, the extra 61 being page furniture that would
  otherwise have been weighted as products in the query. This is the only
  native dependency in the tree — the binary does not link against ONNX
  Runtime, it opens it at runtime only when enabled, so a deployment that
  leaves it off needs nothing installed;
* **the seed pages say so.** None of the above fires on a lowercase query, and
  people type `opensearch database documentation` in lowercase. The page the
  crawl was pointed at writes it `OpenSearch`, where both extraction and
  capitalisation work — so the query's names are read off the corpus the query
  is about. `analysis.names` in the report is the result, and a crawl that
  drifted onto a competitor's pages usually has a name missing from it.

Extraction is on for queries **and** pages. It roughly doubles tagging (59ms to
129ms on a 5kB page) and is invisible at crawl scale, where fetching dominates;
it earns that because it is the only part of the pipeline that sees a name as a
*unit*. `Amazon OpenSearch Service` is emitted as one term alongside its three
words, on both sides, so a page about Amazon Web Services no longer matches it
as well as a page about the thing being asked for. What extraction does **not**
do is worth knowing: prose's model is driven by capitalisation, so on a
lowercase query it finds nothing and the dictionary-absence rule is still what
catches the name. The reasoning is structural rather than cosmetic: an ordinary word
is scored twice, once through its synset and once lexically, while a name has
no synset and is scored once, so equal lexical weight leaves it permanently
behind. Measured on "opensearch database documentation, syntax and query
language", a PostgreSQL article matched five of the six words and missed only
the one that decided anything.

**A page that mentions none of the query's names is halved**
(`CRAWL_NAME_MISS_PENALTY`, 1 disables it). Weighting names is a nudge, not a
filter — one name at weight 3 beside five ordinary words is 37% of the query,
so a long page saturating the other 63% wins without it. Measured on
"opensearch database documentation, syntax and query language", an article
about building a document search tool with React and Supabase, never
mentioning OpenSearch in its text, scored 0.706 lexically against 0.517 for a
real OpenSearch article — on length alone. A query that names something is
asking about that thing.

The same fact corrects **coverage**. It measured the share of the query's
*concepts* the semantic half could judge — but a word with no sense produces no
concept, so it was absent from the fraction entirely and coverage read 1.00
while the semantic half was blind to the subject of the query. It is now scaled
by the share of the query's *weight* that resolved to a sense at all, which is
what makes `wsd.Combine` hand the decision to BM25 when the answer is "almost
none of it".

The two sense inventories are deliberately different. The **query** is
disambiguated against **BabelNet**, whose vocabulary includes named entities
and domain jargon, and which is affordable because a query is short and its
senses are cached forever. Every **page** is disambiguated against the local
**WordNet**, because there are hundreds per run and the free BabelNet tier
allows 1000 requests a day. They are comparable because a BabelNet sense
derived from WordNet carries its original offset; a BabelNet-only sense scores
0 semantically and is carried by BM25, with `coverage` on every score
recording how much of the query the semantic half could judge.

The score is not only a filter — it drives the search. Links are queued in a
best-first frontier priority-ordered by their parent page's score, their anchor
text and their URL path tokens; when the best remaining candidate falls below
`score_threshold`, the crawl is finished. A run bounded out by pages, time or
the daily sense allowance ends `partial` with its frontier persisted, and the
next run resumes from it.

**`DISABLE_BABELNET=true`** takes BabelNet out of the process entirely: the
query is disambiguated against the same local WordNet the pages use, and
nothing is ever sent to babelnet.io. Runs become free, offline and
reproducible — useful for development, for air-gapped deployments, and while
an allowance is spent. It is a configured mode, not a failure, so runs are
**not** marked `degraded`; `degraded` keeps its meaning of "the rich inventory
was expected and was unavailable".

The cost is worth knowing before setting it. WordNet has no entry for
*OpenSearch*, no database sense of *index* and no computing sense of
*security*, so a query about named technology is read in whatever everyday
senses exist — and no disambiguation algorithm can choose a sense it was never
offered. Fine for topics in ordinary English, poor for jargon.

When the BabelNet allowance is spent the **query** does not stop the run: it is
re-analysed against the local WordNet, wholesale, and the run proceeds with
`analysis.degraded` set in its report. The vocabulary is smaller — WordNet has
no encyclopaedic senses, so a query naming a product loses it — but a crawl
that runs on a poorer analysis beats one that does not run at all. Whatever the
abandoned attempt did resolve stays cached for the next run.

Every run produces a report: the disambiguated and expanded query, and a graph
of every document reached with its score (semantic and lexical separately),
its links, and the frontier nodes where the search stopped, each with a reason.

#### Diagnosing a disambiguation

```sh
make wsd-diag ARGS='-query "opensearch indices, security, syntax and usage" \
                    -seed https://dev.to/t/opensearch \
                    -expect n06491786,n00823316'
```

`scripts/wsd-diag` reproduces the query analysis exactly — same context bag,
same colony, same objective, calling into `wsd.Diagnose` rather than a copy —
and adds the two things a running service cannot afford: an **exhaustive**
search of every assignment, and a **decomposition** of the winning score into
the sense pairs that produced it and the words they agreed on.

That combination answers the only question worth asking about a wrong sense,
which is not "is it wrong" but "which half is wrong":

```
=== SEARCH  (405 assignments)
  colony F = 0.2203 == optimum 0.2203
  THE SEARCH IS FINE. A wrong sense here is the objective's fault.

=== WHAT THE SCORE IS MADE OF
  security x usage = 0.0606  on: written
  index    x syntax = 0.0476 on: relating,words
  context 0.0000 (0%)   pairs 0.2203 (100%)
```

A colony that reaches the optimum and still returns nonsense cannot be fixed by
tuning ants, cycles or evaporation, and the decomposition says what to fix
instead. Every defect found this way so far has been in the objective:
function words scoring as agreement, WordNet's example sentences leaking into
the comparison, and long glosses winning on bulk. All three are pinned by tests
in `internal/wsd/aco_test.go`.

`-inventory babelnet` runs the same diagnosis against the metered inventory;
the permanent cache means a query a real crawl already analysed costs nothing
to re-examine.

`-score <urls>` continues past disambiguation into scoring, which is a
different failure: a query can be understood perfectly and still crawl badly,
because the words the dictionary could not resolve are invisible to the
semantic half. It prints the lexical vocabulary with each word's weight, how
much of the query the semantic half cannot see, and — for every page given —
the term counts behind the score, next to what the same page would have scored
with names weighted like ordinary words:

```
   60% of the query's weight is INVISIBLE to the semantic half,
   so coverage is scaled by 0.40 and BM25 carries that much more of the decision.

   "OpenSearch isn't trying to be a better Elasticsearch anymore"
     ** opensearch   x9    weight 3.0
        database     x0    weight 1.0
     semantic 0.468  lexical 0.163  TOTAL 0.224
     with names weighted 1.0:       TOTAL 0.200  (+0.024)

   "Inside PostgreSQL MATCH Queries: Syntax, Paths, and Parameters"
     ** opensearch   x0    weight 3.0
        database     x2    weight 1.0
     semantic 0.511  lexical 0.121  TOTAL 0.199
     with names weighted 1.0:       TOTAL 0.259  (-0.060)
```

That last pair is the whole argument for weighting names: unweighted, the
PostgreSQL article outranks a page that says "OpenSearch" nine times, because
it happens to say "database" twice and the OpenSearch page never does.

Pages whose text the general-purpose extractor cannot find — a JIRA ticket, a
wiki, anything built from `div`s with no `<main>` — are handled by per-crawl
**extraction rules**: a URL wildcard and a set of CSS selectors, first match
wins. They replace the TEXT only; links are still collected document-wide, so a
selector chosen for a ticket's description does not also decide where the crawl
may go. A rule that matches the URL but selects nothing falls back to the
inferred text rather than emptying the page, and `selected` on each graph node
says which pages a rule actually supplied. Selectors are compiled at creation,
so a typo is a 400 rather than a week of silently empty documents.

Re-crawling is incremental. The crawl keeps a URL-to-document map, so an
unchanged page costs nothing, a changed one replaces its predecessor, and a
page missing for several consecutive runs is removed.

A crawl explores a **source**, not necessarily the web. `internal/source`
defines `Reference` / `Retrieve` / `Allows` / `Parse`; `crawler.WebSource` and
`jira.Source` implement it, and the loop cannot tell them apart — the test of
that being that `Options` no longer mentions HTTP. JIRA references are issue
keys, retrieval is `GET /rest/api/3/issue/{key}` with Basic `email:token`, and
new references are the parent, subtasks and explicit issue links. Mentions in
text are not followed. Traversal is bounded by `jira.max_depth` (default 2,
ceiling 4) as well as the crawl's own depth: an issue graph fans out
reciprocally, so each hop multiplies rather than adds, and that limit — not a
rate cap — is what bounds a JIRA crawl.

Crawls may carry **credentials**: per-crawl request headers, scoped to a URL
pattern whose host must be concrete (`https://*` is refused at validation —
a crawl follows links, and links leave sites). Values are sealed with
AES-256-GCM under `CRAWL_ENCRYPTION_KEY`, separate from the identity service's
key, blanked on every API response — there is no read path — and dropped on
redirect hops that do not match the pattern. That last one is enforced in a
`RoundTripper` rather than on the outgoing request, because `http.Client`
copies headers onto redirect hops and strips only `Authorization`, `Cookie` and
`WWW-Authenticate`, only across a different *domain*: a custom `X-Api-Token`
is never stripped, and a hop between ports of one host is not a domain change.
Measured with a plain client, both headers reached the redirect target.
`User-Agent`, `Host` and the hop-by-hop headers cannot be set, so a crawl
cannot disguise what it is after robots.txt was consulted for a crawler.

Politeness is enforced rather than advisory, because requests go out under the
platform's name: robots.txt (including `Crawl-delay`), a per-host delay, a
concurrency cap, an identifying User-Agent, and same-registrable-domain
scoping by default. Seed URLs are untrusted input fetched from the platform's
own network position, so the fetcher refuses private, loopback and link-local
addresses **in the dialer**, checking the resolved IP — a hostname check is
defeated by a DNS record pointing at 127.0.0.1, and a pre-flight resolution is
defeated by DNS rebinding.

## Notes & limitations

* No auth: ownership is by `X-User-Id` header only.
* Word sense disambiguation is still imperfect on short queries, even with the
  colony and the seed pages behind it. A sense the dictionary does not have
  cannot be chosen: WordNet has no database sense of `index` and no computing
  sense of `security`, so on the WordNet fallback those words resolve to a
  numeric scale and to collateral no matter how coherent the rest of the
  reading is. That is what BabelNet is for, and it is the case to check when a
  report looks wrong. The run report is the only place any of this is visible,
  which is most of why it exists.
* Crawls are English-only in practice: `prose`'s POS tagger is, even though
  BabelNet is not.
* The crawl scheduler assumes a single orchestrator replica — there is no
  leader election, matching the platform's other background sweeps.
* A knowledge base's `kind`, `chunk_size` and `chunk_overlap` are fixed at
  creation. Changing the kind would leave existing documents stored in a form
  nothing reads; changing the chunking would split later documents differently
  from earlier ones within the same KB.
* Context files are loaded **whole** into the prompt — every document of every
  context KB the agent references. Large KBs can exceed the model's context
  window; use a retrieval knowledge base for large corpora.
* The indexer ack's a message only after all chunks index successfully;
  failures leave it pending in the Redis consumer group. The consumer sweeps
  the pending list every `REDIS_RECLAIM_INTERVAL` (30s), reclaiming deliveries
  idle longer than `REDIS_RECLAIM_MIN_IDLE` (2m) — including ones stranded by
  a dead consumer — and drops a message after `REDIS_MAX_DELIVERIES` (5)
  failed attempts so a poison message cannot block the queue.
* The friendly→Bedrock model map defaults to `eu.*` inference profiles for
  `eu-north-1`; adjust `internal/models/registry.go` for your region.
