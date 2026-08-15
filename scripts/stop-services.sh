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

SERVICES=(
  enact-agent-management-api
  enact-kb-api
  enact-kb-document-indexer
  enact-main
  enact-model-inference
  enact-model-management
  enact-tests
  enact-tool-registry
  enact-external-identities
)

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
  if [ ! -f "$pidfile" ]; then
    log "$svc: no pidfile (not started by start-services.sh, or already stopped)"
    continue
  fi
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