#!/usr/bin/env bash
#
# Manage the enact platform's local infrastructure (OpenSearch + Redis + Tika)
# and the data structures the services depend on.
#
#   infrastructure.sh up        start containers, register index templates,
#                               create indices and the Redis stream/group
#   infrastructure.sh down      stop the containers (data is preserved)
#   infrastructure.sh clean     delete and recreate the indices and the Redis
#                               stream (containers keep running)
#   infrastructure.sh provision register templates, create indices and apply
#                               live mappings against an EXTERNAL OpenSearch
#                               (e.g. Aiven) — no containers, no Redis (the
#                               indexer creates its stream/group itself):
#
#                                 OPENSEARCH_ADDRESSES=https://host:port \
#                                 OPENSEARCH_USERNAME=... OPENSEARCH_PASSWORD=... \
#                                 OPENSEARCH_INSECURE_SKIP_VERIFY=false \
#                                 ./scripts/infrastructure.sh provision
#
# OpenSearch index mappings are owned by the composable index templates in
# the mappings/ directory; indices are created bare so the templates apply.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/deploy/docker-compose.infra.yml"
MAPPINGS_DIR="$ROOT_DIR/mappings"

# Load .env (if present) so this script and the Go services agree on settings.
if [ -f "$ROOT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$ROOT_DIR/.env"
  set +a
fi

# Defaults mirror the Go service defaults.
OPENSEARCH_ADDRESSES="${OPENSEARCH_ADDRESSES:-https://localhost:9200}"
OPENSEARCH_URL="${OPENSEARCH_ADDRESSES%%,*}"   # use the first address if a list
OPENSEARCH_USERNAME="${OPENSEARCH_USERNAME:-admin}"
OPENSEARCH_PASSWORD="${OPENSEARCH_PASSWORD:-9zT!mPq4#bRk7Fx2}"
REDIS_ADDR="${REDIS_ADDR:-localhost:6379}"
REDIS_PASSWORD="${REDIS_PASSWORD:-}"
REDIS_STREAM="${REDIS_STREAM:-enact-kb-documents}"
REDIS_GROUP="${REDIS_GROUP:-indexers}"
TIKA_URL="${TIKA_URL:-http://localhost:9998}"
BEDROCK_EMBEDDING_DIM="${BEDROCK_EMBEDDING_DIM:-1024}"

# Indices, paired with their template files in mappings/.
INDICES=(enact-knowledge-bases enact-agents enact-kb-documents enact-agent-rag-chunks enact-users enact-conversations enact-tool-servers enact-tool-cache enact-identities enact-identity-providers)

# --- helpers ---------------------------------------------------------------

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
err() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; }

# Resolve the docker compose command (plugin form vs legacy binary) on
# first use — `provision` runs against an external cluster and must work on
# machines without docker.
compose() {
  if [ -z "${DC+set}" ]; then
    if docker compose version >/dev/null 2>&1; then
      DC=(docker compose)
    elif command -v docker-compose >/dev/null 2>&1; then
      DC=(docker-compose)
    else
      err "docker compose / docker-compose not found on PATH"
      exit 1
    fi
  fi
  "${DC[@]}" -f "$COMPOSE_FILE" "$@"
}

# os <curl args...> : authenticated curl against OpenSearch. Certificate
# verification follows OPENSEARCH_INSECURE_SKIP_VERIFY (default true, for
# the self-signed local container; set false for Aiven/real certificates).
OPENSEARCH_INSECURE_SKIP_VERIFY="${OPENSEARCH_INSECURE_SKIP_VERIFY:-true}"
if [ "$OPENSEARCH_INSECURE_SKIP_VERIFY" = "false" ]; then
  OS_TLS_FLAG=""
else
  OS_TLS_FLAG="-k"
fi
os() { curl -s $OS_TLS_FLAG -u "$OPENSEARCH_USERNAME:$OPENSEARCH_PASSWORD" "$@"; }

# os_status <path> : print the HTTP status code for a GET against OpenSearch.
os_status() { os -o /dev/null -w '%{http_code}' "$OPENSEARCH_URL/$1"; }

# render <file> : substitute ${BEDROCK_EMBEDDING_DIM} into a mapping template.
render() { sed "s/\${BEDROCK_EMBEDDING_DIM}/${BEDROCK_EMBEDDING_DIM}/g" "$1"; }

# redis <args...> : run redis-cli inside the redis container.
redis() {
  if [ -n "$REDIS_PASSWORD" ]; then
    compose exec -T redis redis-cli -a "$REDIS_PASSWORD" "$@"
  else
    compose exec -T redis redis-cli "$@"
  fi
}

wait_for_opensearch() {
  log "Waiting for OpenSearch at $OPENSEARCH_URL ..."
  for _ in $(seq 1 60); do
    if [ "$(os_status '_cluster/health')" = "200" ]; then
      log "OpenSearch is ready."
      return 0
    fi
    sleep 2
  done
  err "OpenSearch did not become ready in time."
  exit 1
}

wait_for_redis() {
  log "Waiting for Redis ..."
  for _ in $(seq 1 60); do
    if [ "$(redis PING 2>/dev/null || true)" = "PONG" ]; then
      log "Redis is ready."
      return 0
    fi
    sleep 2
  done
  err "Redis did not become ready in time."
  exit 1
}

wait_for_tika() {
  log "Waiting for Tika at $TIKA_URL ..."
  for _ in $(seq 1 60); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "$TIKA_URL/tika" 2>/dev/null || true)" = "200" ]; then
      log "Tika is ready."
      return 0
    fi
    sleep 2
  done
  err "Tika did not become ready in time."
  exit 1
}

register_templates() {
  for index in "${INDICES[@]}"; do
    local file="$MAPPINGS_DIR/$index.json"
    [ -f "$file" ] || { err "mapping template not found: $file"; exit 1; }
    log "Registering index template: $index"
    local code
    code=$(render "$file" | os -o /dev/null -w '%{http_code}' \
      -X PUT -H 'Content-Type: application/json' \
      "$OPENSEARCH_URL/_index_template/$index" --data-binary @-)
    case "$code" in
      200|201) ;;
      *) err "failed to register template $index (HTTP $code)"; exit 1 ;;
    esac
  done
}

# Templates only apply to indices created after registration; fields added
# to a template later must also be PUT onto the live index. Adding a new
# field to an existing mapping is an idempotent, always-allowed operation.
update_live_mappings() {
  put_live_mapping enact-agent-rag-chunks '{"properties":{"filename":{"type":"keyword"}}}'
  put_live_mapping enact-knowledge-bases '{"properties":{"name":{"type":"text"},"updated_at":{"type":"date"}}}'
  put_live_mapping enact-agents '{"properties":{"name":{"type":"text"},"tools":{"type":"keyword"}}}'
  put_live_mapping enact-conversations '{"properties":{"messages":{"properties":{"attachments":{"type":"keyword"},"tool_calls":{"type":"object","enabled":false}}}}}'
  put_live_mapping enact-tool-servers '{"properties":{"tool_access_requirements":{"type":"object","enabled":false},"tool_authorizations":{"type":"object","enabled":false}}}'
  put_live_mapping enact-identities '{"properties":{"access_level":{"type":"keyword"}}}'
  put_live_mapping enact-identity-providers '{"properties":{"access_levels":{"type":"object","enabled":false}}}'
  put_live_mapping enact-users '{"properties":{"avatar_key":{"type":"keyword"},"verification_token_hash":{"type":"keyword","index":false},"verification_expires_at":{"type":"date"}}}'
}

# put_live_mapping <index> <mapping-json>
put_live_mapping() {
  log "Updating live mapping: $1"
  local code
  code=$(os -o /dev/null -w '%{http_code}' \
    -X PUT -H 'Content-Type: application/json' \
    "$OPENSEARCH_URL/$1/_mapping" -d "$2")
  case "$code" in
    200|201) ;;
    404) log "Index $1 not created yet; template will cover it" ;;
    *) err "failed to update mapping for $1 (HTTP $code)"; exit 1 ;;
  esac
}

create_indices() {
  for index in "${INDICES[@]}"; do
    if [ "$(os_status "$index")" = "200" ]; then
      log "Index already exists: $index"
      continue
    fi
    log "Creating index: $index"
    local code
    code=$(os -o /dev/null -w '%{http_code}' -X PUT "$OPENSEARCH_URL/$index")
    case "$code" in
      200|201) ;;
      *) err "failed to create index $index (HTTP $code)"; exit 1 ;;
    esac
  done
}

delete_indices() {
  for index in "${INDICES[@]}"; do
    log "Deleting index: $index"
    os -o /dev/null -X DELETE "$OPENSEARCH_URL/$index" || true
  done
}

ensure_redis_stream() {
  log "Ensuring Redis stream '$REDIS_STREAM' and group '$REDIS_GROUP'"
  # MKSTREAM creates the stream if absent; BUSYGROUP means it already exists.
  redis XGROUP CREATE "$REDIS_STREAM" "$REDIS_GROUP" '$' MKSTREAM >/dev/null 2>&1 || true
}

delete_redis_stream() {
  log "Deleting Redis stream '$REDIS_STREAM'"
  redis DEL "$REDIS_STREAM" >/dev/null 2>&1 || true
}

# --- commands --------------------------------------------------------------

cmd_up() {
  log "Starting infrastructure containers ..."
  compose up -d
  wait_for_opensearch
  wait_for_redis
  wait_for_tika
  register_templates
  create_indices
  update_live_mappings
  ensure_redis_stream
  log "Infrastructure is up."
}

cmd_down() {
  log "Stopping infrastructure containers ..."
  compose stop
  log "Infrastructure stopped (data preserved)."
}

cmd_clean() {
  log "Cleaning OpenSearch indices and Redis stream ..."
  compose up -d >/dev/null
  wait_for_opensearch
  wait_for_redis
  delete_indices
  delete_redis_stream
  register_templates
  create_indices
  ensure_redis_stream
  log "Clean complete: indices and stream recreated empty."
}

# provision targets an external cluster: everything OpenSearch-side, nothing
# container- or Redis-side (the indexer's EnsureGroup creates the stream).
cmd_provision() {
  wait_for_opensearch
  register_templates
  create_indices
  update_live_mappings
  log "Provisioning complete for $OPENSEARCH_URL."
}

main() {
  local cmd="${1:-}"
  case "$cmd" in
    up)        cmd_up ;;
    down)      cmd_down ;;
    clean)     cmd_clean ;;
    provision) cmd_provision ;;
    *)
      err "usage: $0 {up|down|clean|provision}"
      exit 2
      ;;
  esac
}

main "$@"
