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
| `enact-model-inference`         | `POST /v1/inference`. Accepts `agent_id`; resolves the agent's model, loads the KB context files whole, retrieves top-k chunks from the agent's RAG collection, and augments the system prompt. |

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
