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

SERVICES=(
  enact-agent-management-api
  enact-kb-api
  enact-kb-document-indexer
  enact-model-inference
  enact-model-management
)

# Delve listen ports per service, index-aligned with SERVICES.
DEBUG_PORTS=(
  40001
  40002
  40003
  40004
  40005
)

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
err() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; }

if [ "$DEBUG" = "1" ] && ! command -v dlv >/dev/null 2>&1; then
  err "DEBUG=1 requires the Delve debugger; install it with:"
  err "  go install github.com/go-delve/delve/cmd/dlv@latest"
  exit 1
fi

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