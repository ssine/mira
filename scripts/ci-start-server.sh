#!/usr/bin/env bash
# Start a CI-only Mira Server worker in the background, wait for /healthz and
# print its PID on stdout. The worker inherits the caller's environment.
# Usage: server_pid=$(bash scripts/ci-start-server.sh BINARY LOG_FILE)
set -euo pipefail

binary=$1
log=$2

"$binary" server-worker >"$log" 2>&1 </dev/null &
pid=$!
for _ in $(seq 1 40); do
  if curl --fail --silent http://127.0.0.1:8787/healthz >/dev/null; then
    echo "$pid"
    exit 0
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
cat "$log" >&2
kill "$pid" >/dev/null 2>&1 || true
exit 1
