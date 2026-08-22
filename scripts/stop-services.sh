#!/usr/bin/env bash
#
# Stop enact platform services running locally. Services started by
# start-services.sh record their PID in dist/run/<service>.pid; stopping
# kills exactly that process tree and nothing else. Matching by name
# (pgrep -f) is deliberately NOT used: substring matches have killed
# innocent bystanders whose command line merely contained a service name.
#
#   stop-services.sh
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_DIR="$ROOT_DIR/dist/run"

# Derived from the pid files rather than a hardcoded list.
#
# A second copy of the service list is a copy that drifts: enact-mcp-servers
# was added to start-services.sh and not here, so `make stop` quietly left it
# running and every later `make restart` kept serving a stale binary. Whatever
# was started is what gets stopped.
SERVICES=()
for pidfile in "$PID_DIR"/*.pid; do
  [ -e "$pidfile" ] || continue
  SERVICES+=("$(basename "$pidfile" .pid)")
done

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

# kill_tree TERMinates a process and its descendants, deepest first, so the
# dlv -> debugserver -> service chain comes down in order.
kill_tree() {
  local pid=$1 child
  for child in $(pgrep -P "$pid" 2>/dev/null || true); do
    kill_tree "$child"
  done
  kill "$pid" 2>/dev/null || true
}

for svc in "${SERVICES[@]}"; do
  pidfile="$PID_DIR/$svc.pid"
  pid=$(cat "$pidfile")
  if ! kill -0 "$pid" 2>/dev/null; then
    log "$svc: stale pidfile (PID $pid not running); cleaning up"
    rm -f "$pidfile"
    continue
  fi
  log "$svc: stopping (PID $pid and its children)"
  kill_tree "$pid"
  rm -f "$pidfile"
done

if [ ${#SERVICES[@]} -eq 0 ]; then
  log "no running services (no pid files in $PID_DIR)"
fi
