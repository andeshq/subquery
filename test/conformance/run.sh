#!/usr/bin/env bash
#
# Replays the conformance cases against subquery and against stock PostgREST on an
# identical fixture database, then diffs the two recordings.
#
# Each pass starts from a freshly created database, so both servers see the same
# state and the same request sequence; write cases therefore affect both passes
# identically.
set -euo pipefail

cd "$(dirname "$0")/../.."

DB_URI="postgres://authenticator:secret@localhost:5432/app"
JWT_SECRET="reallyreallyreallyreallyverysafe!"
CASES="test/conformance/cases"
TARGET="/tmp/conformance-target.json"
REFERENCE="/tmp/conformance-reference.json"
SUBQUERY_PORT=3100
POSTGREST_PORT=3101

wait_for_db() {
  for _ in $(seq 1 60); do
    if docker compose -f test/conformance/compose.yaml exec -T db pg_isready -U postgres -d app >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "database did not become ready" >&2
  return 1
}

wait_for_http() {
  for _ in $(seq 1 120); do
    if curl -fsS "$1" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "no response from $1" >&2
  return 1
}

fresh_db() {
  docker compose -f test/conformance/compose.yaml down >/dev/null 2>&1 || true
  docker compose -f test/conformance/compose.yaml up -d db >/dev/null
  wait_for_db
}

echo "== pass 1: subquery =="
fresh_db
go build -o /tmp/subquery-conformance ./cmd/subquery
PGRST_DB_URI="$DB_URI" \
PGRST_DB_ANON_ROLE=anon \
PGRST_DB_SCHEMAS=public \
PGRST_JWT_SECRET="$JWT_SECRET" \
PGRST_DB_AGGREGATES_ENABLED=true \
PGRST_SERVER_PORT=$SUBQUERY_PORT \
PGRST_LOG_LEVEL=warn \
  /tmp/subquery-conformance &
SUBQUERY_PID=$!
trap 'kill $SUBQUERY_PID 2>/dev/null || true' EXIT
wait_for_http "http://localhost:$SUBQUERY_PORT/_health"
go run ./test/conformance replay -base "http://localhost:$SUBQUERY_PORT" -cases "$CASES" -out "$TARGET"
kill $SUBQUERY_PID 2>/dev/null || true
wait $SUBQUERY_PID 2>/dev/null || true

echo
echo "== pass 2: postgrest (the oracle) =="
fresh_db
docker compose -f test/conformance/compose.yaml up -d postgrest >/dev/null
wait_for_http "http://localhost:$POSTGREST_PORT/"
go run ./test/conformance replay -base "http://localhost:$POSTGREST_PORT" -cases "$CASES" -out "$REFERENCE"
docker compose -f test/conformance/compose.yaml stop postgrest >/dev/null

echo
echo "== comparison =="
go run ./test/conformance compare -ref "$REFERENCE" -target "$TARGET"

docker compose -f test/conformance/compose.yaml down >/dev/null 2>&1 || true
