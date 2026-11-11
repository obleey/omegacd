#!/usr/bin/env bash
# End-to-end: a database migration as Compose's init pattern (M2-11), through a real server and Postgres: the app
# starts after its migration, each Sync runs it once (idempotent), and a failing one fails the Sync with its reason and
# leaves the running app as it was.
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2em.XXXX")
REPO=$T/repo; API=http://127.0.0.1:18080/api; APP=e2e-mig
mkdir -p "$REPO/mig" "$T/data"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
compose() { # $1: web's V, so a change recreates it
  cat > "$REPO/mig/compose.yaml" <<EOF
services:
  db:
    image: postgres:17-alpine
    environment: {POSTGRES_PASSWORD: e2e, POSTGRES_DB: app}
    healthcheck: {test: ["CMD", "pg_isready", "-U", "postgres", "-d", "app"], interval: 1s, retries: 60}
  migrate:
    image: postgres:17-alpine
    environment: {PGPASSWORD: e2e}
    command: ["psql", "-h", "db", "-U", "postgres", "-d", "app", "-v", "ON_ERROR_STOP=1", "-f", "/migrations/schema.sql"]
    volumes: ["./schema.sql:/migrations/schema.sql:ro"]
    depends_on: {db: {condition: service_healthy}}
  web:
    image: postgres:17-alpine
    environment: {PGPASSWORD: e2e, V: "$1"}
    command: ["sh", "-c", "echo runs=\$\$(psql -h db -U postgres -d app -tAc 'select count(*) from runs'); sleep 3600"]
    depends_on: {migrate: {condition: service_completed_successfully}}
EOF
}
compose 1
cat > "$REPO/mig/schema.sql" <<'EOF'
CREATE TABLE IF NOT EXISTS runs (id serial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now());
INSERT INTO runs DEFAULT VALUES;
EOF
gitc init -q -b main && gitc add -A && gitc commit -qm init

age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() { "$BIN" app delete $APP --volumes --yes >/dev/null 2>&1; kill $SRV ${AGENT:-} 2>/dev/null; wait $SRV ${AGENT:-} 2>/dev/null; rm -rf "$T"; }
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
. "$(dirname "$0")/enroll.sh"; enroll box # the Control Plane's machine, a Host like any other (Q104)
ctr() { docker ps -aq --filter label=com.docker.compose.project=$APP --filter label=com.docker.compose.service=$1; }
runs() { docker exec "$(ctr db)" psql -U postgres -d app -tAc 'select count(*) from runs' 2>/dev/null; }
field() { "$BIN" app get $APP -o json | python3 -c "import json,sys; v=json.load(sys.stdin); print($1)"; }

"$BIN" app create $APP --repo "$REPO" --path mig --host box >/dev/null
"$BIN" app sync $APP --wait --timeout 4m >/dev/null 2>&1
check "the app syncs, Healthy with its one-shot completed" '[ "$(field "v[\"syncStatus\"], v[\"health\"]")" = "Synced Healthy" ]'
check "the migration exited 0" '[ "$(docker inspect -f "{{.State.Status}} {{.State.ExitCode}}" "$(ctr migrate)")" = "exited 0" ]'
check "the app started after it: it saw the table, with one run" 'docker logs "$(ctr web)" 2>&1 | grep -q "^runs=1$"'

printf 'ALTER TABLE runs ADD COLUMN IF NOT EXISTS note text;\n' >> "$REPO/mig/schema.sql"; gitc commit -qam "migration 2"
"$BIN" app refresh $APP >/dev/null 2>&1
"$BIN" app sync $APP --wait --timeout 4m >/dev/null 2>&1
check "a new Revision runs the migration once more (idempotent)" '[ "$(runs)" = 2 ]'
check "and it changed the schema" 'docker exec "$(ctr db)" psql -U postgres -d app -tAc "select note from runs limit 1" >/dev/null 2>&1'

web=$(ctr web)
compose 2; printf 'THIS IS NOT SQL;\n' >> "$REPO/mig/schema.sql"; gitc commit -qam "a broken migration, and a new web"
"$BIN" app refresh $APP >/dev/null 2>&1
"$BIN" app sync $APP --wait --timeout 4m >/dev/null 2>&1
check "a failing migration fails the Sync, saying which" '[ "$(field "v[\"lastOperation\"][\"phase\"]")" = Failed ] && field "v[\"lastOperation\"][\"message\"]" | grep -q "migrate exited"'
check "and leaves the running app as it was" '[ "$(ctr web)" = "$web" ] && [ "$(docker inspect -f "{{.State.Running}}" "$web")" = true ]'

check "deployed by the Agent of the Control Plane's machine, as its log says (Q104)" 'grep -q "msg=deploy" "$AGENT_LOG"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- agent log"; tail -15 "$AGENT_LOG"; echo "--- server log"; tail -15 "$T/server.log"; }
exit $fail
