#!/usr/bin/env bash
#
# Start all enact platform services from the dist/ directory in the
# background. Each binary's stdout/stderr is appended to
# /tmp/<binary-name>.log. Services already running (matched by process name)
# are left alone, so the script is safe to re-run.
#
# The binaries are launched with dist/ as their working directory, so the
# shared .env and per-service "<binary-name>.env" files placed alongside the
# binaries are picked up automatically.
#
# With DEBUG=1 each service is started under a headless Delve debugger
# (dlv exec) listening on a fixed per-service port, so an IDE can attach a
# remote Go debug session (GoLand: "Go Remote"; VS Code: "connect" mode).
# Build the binaries with DEBUG=1 too (make build DEBUG=1) so they carry
# debug info and no optimizations — `make restart DEBUG=1` does both.
#
#   start-services.sh
#   DEBUG=1 start-services.sh
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
PID_DIR="$DIST_DIR/run"
LOG_DIR="/tmp"
DEBUG="${DEBUG:-0}"
S2S_DIR="$ROOT_DIR/s2s"

SERVICES=(
  enact-agent-management-api
  enact-kb-api
  enact-kb-document-indexer
  enact-main
  enact-model-inference
  enact-model-management
  enact-tests
)

# Delve listen ports per service, index-aligned with SERVICES.
DEBUG_PORTS=(
  40001
  40002
  40003
  40007
  40004
  40005
  40006
)

# assemble_private_keys emits a YAML document with every service's private
# key (the counterpart of the JWKS) for the enact-tests impersonation fleet.
assemble_private_keys() {
  echo "keys:"
  for f in "$S2S_DIR"/keys/*.key; do
    [ -f "$f" ] || continue
    printf '  - kid: %s\n    private_key: |\n' "$(basename "$f" .key)"
    sed 's/^/      /' "$f"
  done
}

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
err() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; }

if [ "$DEBUG" = "1" ] && ! command -v dlv >/dev/null 2>&1; then
  err "DEBUG=1 requires the Delve debugger; install it with:"
  err "  go install github.com/go-delve/delve/cmd/dlv@latest"
  exit 1
fi

# S2S material: every service receives the shared JWKS, its own ACL, and its
# own private key as environment variables holding the YAML/PEM content
# verbatim (services never read these files themselves). With
# S2S_ENABLED=false no material is needed — the services skip enforcement.
#
# Precedence: shell > dist/.env > default true. The explicit fallback to
# dist/.env matters because this script EXPORTS the resolved value — and an
# exported variable would otherwise beat dist/.env inside the services
# (godotenv.Load never overrides existing env).
if [ -z "${S2S_ENABLED:-}" ] && [ -f "$DIST_DIR/.env" ]; then
  S2S_ENABLED="$(sed -n 's/^S2S_ENABLED=//p' "$DIST_DIR/.env" | tail -1 | sed "s/[\"']//g")"
fi
S2S_ENABLED="${S2S_ENABLED:-true}"
if [ "$S2S_ENABLED" = "true" ] && [ ! -f "$S2S_DIR/jwks.yaml" ]; then
  err "s2s/jwks.yaml not found; generate the key material with: make s2s-keygen"
  err "(or start with S2S_ENABLED=false to run without service authentication)"
  exit 1
fi
S2S_JWKS_CONTENT=""
[ -f "$S2S_DIR/jwks.yaml" ] && S2S_JWKS_CONTENT="$(cat "$S2S_DIR/jwks.yaml")"

mkdir -p "$PID_DIR"
cd "$DIST_DIR"

for i in "${!SERVICES[@]}"; do
  svc="${SERVICES[$i]}"
  if [ ! -x "./$svc" ]; then
    err "$svc: binary not found in dist/ (run 'make build')"
    continue
  fi
  # Already-running is decided by our own pidfile, never by name matching:
  # pgrep -f substring checks have false-matched unrelated processes (any
  # command line containing the service name, including this script's caller).
  pidfile="$PID_DIR/$svc.pid"
  if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
    log "$svc: already running (PID $(cat "$pidfile"))"
    continue
  fi
  rm -f "$pidfile"
  logfile="$LOG_DIR/$svc.log"

  export S2S_ENABLED
  if [ "$S2S_ENABLED" = "true" ]; then
    for f in "$S2S_DIR/acl/$svc.yaml" "$S2S_DIR/keys/$svc.key"; do
      if [ ! -f "$f" ]; then
        err "$svc: missing $f (run 'make s2s-keygen' for keys; ACLs live in s2s/acl/)"
        continue 2
      fi
    done
    export S2S_JWKS="$S2S_JWKS_CONTENT"
    export S2S_ACL="$(cat "$S2S_DIR/acl/$svc.yaml")"
    export S2S_PRIVATE_KEY="$(cat "$S2S_DIR/keys/$svc.key")"
    export S2S_KEY_ID="$svc"
    # The tests service impersonates every other service and therefore
    # receives the whole fleet's private keys.
    if [ "$svc" = "enact-tests" ]; then
      export S2S_PRIVATE_KEYS="$(assemble_private_keys)"
    else
      unset S2S_PRIVATE_KEYS
    fi
  fi
  if [ "$DEBUG" = "1" ]; then
    port="${DEBUG_PORTS[$i]}"
    # --continue starts the service immediately instead of waiting for a
    # client; --accept-multiclient lets the IDE attach and detach at will.
    nohup dlv exec "./$svc" \
      --headless \
      --listen "127.0.0.1:$port" \
      --api-version 2 \
      --accept-multiclient \
      --continue \
      >> "$logfile" 2>&1 &
    disown
    echo $! > "$pidfile"
    log "$svc: started under delve (PID $!, debug port $port). Writing logs to $logfile"
  else
    nohup "./$svc" >> "$logfile" 2>&1 &
    disown
    echo $! > "$pidfile"
    log "$svc: started (PID $!). Writing logs to $logfile"
  fi
done

if [ "$DEBUG" = "1" ]; then
  log "Attach your IDE with a remote Go debug configuration (host 127.0.0.1):"
  for i in "${!SERVICES[@]}"; do
    log "  ${SERVICES[$i]}: port ${DEBUG_PORTS[$i]}"
  done
fi