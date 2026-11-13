#!/usr/bin/env bash
# End-to-end: disaster recovery (M4-06, Q30, Q112–Q119). From a backup: omegacd backup taken during a Sync, the data
# dir lost, restored by the README's steps; the Agent just reconnects, the Sync the restored database still shows
# Running is closed as Unknown, the Git credential is back, no container recreated. From nothing: a new Control Plane
# makes its own Master Key, the Agent is enrolled again (dropping the reports it kept), its apps show as not managed,
# and the App of Apps takes them over: the unchanged one without deploying, the rolled back one with a normal Sync.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2er.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api
mkdir -p "$REPO/apps" "$REPO/web" "$REPO/db"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
SRV= AGENT=
cleanup() {
  for p in rec-web rec-db; do
    docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$p) >/dev/null 2>&1
    docker network rm ${p}_default >/dev/null 2>&1
  done
  kill $SRV $AGENT 2>/dev/null; wait 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT

for app in web db; do
  cat > "$REPO/$app/compose.yaml" <<'EOF'
services:
  main:
    image: busybox:1.36
    command: ["sh", "-c", "cat /conf.txt; sleep 3600"]
    volumes: ["./conf.txt:/conf.txt:ro"]
EOF
  echo "$app v1" > "$REPO/$app/conf.txt"
  printf 'name: rec-%s\nrepoURL: %s\npath: %s\nhost: edge\nautoSync: true\n' $app "$REPO" $app > "$REPO/apps/$app.yaml"
done
gitc init -q -b main && gitc add -A && gitc commit -qm v1
V1=$(git -C "$REPO" rev-parse HEAD)

server() { # data dir, Master Key file
  OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$1 OMEGACD_AGE_KEY_FILE=$2 OMEGACD_ADMIN_PASSWORD=e2e-password-123 \
    OMEGACD_POLL_INTERVAL=1h "$BIN" >> "$T/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
  curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
  export OMEGACD_SERVER=http://127.0.0.1:18080
  export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
    -d '{"name":"e2e-'$RANDOM'"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
}
stop() { kill "$@" 2>/dev/null; wait "$@" 2>/dev/null; }
. "$(dirname "$0")/enroll.sh"
approved() { [ "$(grep -c "connected to the Control Plane, approved" "$AGENT_LOG")" -ge "$1" ]; }
waitfor() { local end=$((SECONDS + 30)); while [ $SECONDS -lt $end ]; do eval "$1" && return 0; sleep 0.2; done; return 1; }
ctr() { docker ps -q --no-trunc --filter label=com.docker.compose.project=$1; }
history() { "$BIN" app history $1 -o json; }
api() { curl -sf -H "Authorization: Bearer $OMEGACD_TOKEN" -H 'X-Requested-With: omegacd' -H 'Content-Type: application/json' "$@"; }

touch "$T/file"
out=$(OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/file/age.key "$BIN" 2>&1)
check "a Master Key it can't make stops the start, naming the setting" 'echo "$out" | grep -q "master key: .*(OMEGACD_AGE_KEY_FILE)"'
out=$(OMEGACD_DATA=$T/data OMEGACD_HISTORY_KEEP=0 "$BIN" 2>&1)
check "so does a retention that keeps nothing (Q112)" 'echo "$out" | grep -q OMEGACD_HISTORY_KEEP'
server "$T/data" "$T/age.key"
check "the first start makes the Master Key itself, saying where (Q115)" '[ -s "$T/age.key" ] && grep -q "created the Master Key" "$T/server.log"'
enroll edge
"$BIN" app create rec --repo "$REPO" --path apps --kind apps --auto-sync >/dev/null
waitfor '"$BIN" app wait rec-web --sync --timeout 5s >/dev/null 2>&1 && "$BIN" app wait rec-db --sync --timeout 5s >/dev/null 2>&1'
check "the App of Apps' children run on the Agent's Host" '[ -n "$(ctr rec-web)" ] && [ -n "$(ctr rec-db)" ]'
DB=$(ctr rec-db)
GITPW=$(openssl rand -hex 12) # a throwaway value, made up per run
api -o /dev/null -d '{"urlPrefix":"https://git.example.com/","username":"me","password":"'"$GITPW"'"}' $API/settings/repo-credentials

# --- from a backup, taken while a Sync runs (busybox ignores SIGTERM: stopping it takes 10 s)
echo "web v2" > "$REPO/web/conf.txt"; gitc commit -qam v2
"$BIN" app refresh rec-web >/dev/null 2>&1
waitfor 'history rec-web | grep -q "\"Running\""'
out=$("$BIN" backup "$T/backup.tar" 2>&1)
check "omegacd backup writes an owner-only tar during a Sync" '[ "$(stat -c %a "$T/backup.tar" 2>/dev/null || stat -f %Lp "$T/backup.tar")" = 600 ]'
check "with the database and the encrypted Git credentials, no key (Q114)" \
  '[ "$(tar -tf "$T/backup.tar" | sort | tr "\n" " ")" = "credentials.age omegacd.db " ] && ! tar -xOf "$T/backup.tar" credentials.age | grep -q "$GITPW"'
check "saying which keys to keep apart, and why (Q115)" 'echo "$out" | grep -q "Master Key" && echo "$out" | grep -q "source.key"'
check "the backup is logged, never its contents" 'grep -q "msg=\"backup taken\" component=api via=token" "$T/server.log"'
"$BIN" app wait rec-web --sync --timeout 2m >/dev/null 2>&1
WEB=$(ctr rec-web)

stop $SRV
mv "$T/data" "$T/data.lost" # the disaster
mkdir -p "$T/data" && tar -xf "$T/backup.tar" -C "$T/data" && cp "$T/data.lost/source.key" "$T/data/" # the README's steps
server "$T/data" "$T/age.key"
check "restored, the Agent just reconnects" 'waitfor "approved 2"'
check "the Sync the backup caught running is closed as Unknown (Q116)" \
  'waitfor "history rec-web | python3 -c \"import json,sys; sys.exit(0 if any(o[\\\"phase\\\"]==\\\"Unknown\\\" and \\\"result unknown\\\" in o[\\\"message\\\"] for o in json.load(sys.stdin)) else 1)\""'
check "the Git credential is back" 'api $API/settings/repo-credentials | grep -q "git.example.com"'
check "rec-web Synced again at v2, rec-db untouched: no container recreated" \
  'waitfor "\"$BIN\" app wait rec-web --sync --timeout 5s >/dev/null 2>&1" && [ "$(ctr rec-web)" = "$WEB" ] && [ "$(ctr rec-db)" = "$DB" ]'

# --- from nothing: while no Control Plane runs, rec-web is rolled back on its Host (a report kept in the outbox)
stop $SRV $AGENT
env -i PATH="$PATH" HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} \
  ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} "$BIN" agent rollback rec-web "${V1:0:12}" >> "$AGENT_LOG" 2>&1
check "offline, rec-web rolled back on its Host" 'docker logs $(ctr rec-web) 2>&1 | grep -q "web v1"'
mv "$T/server.log" "$T/server-before.log" # each phase its own log
server "$T/data.new" "$T/new.key"
check "a new Control Plane makes a new Master Key" '[ -s "$T/new.key" ] && grep -q "created the Master Key" "$T/server.log"'
mv "$AGENT_LOG" "$T/edge-before.log" # its log from before: enroll waits for this enrollment's lines
enroll edge # the same Agent, its state kept, with a new invite
check "enrolled again, it drops the report kept for the Control Plane that is gone (Q117)" \
  'grep -q "enrolled anew: dropped the reports" "$AGENT_LOG" && grep "enrolled anew" "$AGENT_LOG" | grep -q "reports=1"'
unmanaged() { "$BIN" host list -o json | python3 -c 'import json,sys; print(" ".join([h for h in json.load(sys.stdin) if h["name"]=="edge"][0].get("unmanaged") or []))'; }
check "its apps show as on the Host, not managed (Q118)" 'waitfor "[ \"\$(unmanaged)\" = \"rec-db rec-web\" ]"'
check "host list says so" '"$BIN" host list | grep edge | grep -q "rec-db, rec-web"'

"$BIN" app create rec --repo "$REPO" --path apps --kind apps --auto-sync >/dev/null
waitfor '"$BIN" app wait rec-web --sync --timeout 5s >/dev/null 2>&1 && "$BIN" app wait rec-db --sync --timeout 5s >/dev/null 2>&1'
check "rec-db, unchanged, is taken over without deploying (Q119)" \
  'history rec-db | python3 -c "import json,sys; o=json.load(sys.stdin); sys.exit(0 if len(o)==1 and o[0][\"phase\"]==\"Succeeded\" and \"taken over\" in o[0][\"message\"] else 1)"'
check "its container untouched since the start" '[ "$(ctr rec-db)" = "$DB" ]'
check "rec-web, rolled back, Syncs to v2 normally" \
  'history rec-web | grep -q "synced to" && docker logs $(ctr rec-web) 2>&1 | grep -q "web v2"'
stop $AGENT
env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" \
  ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER "$BIN" agent >> "$AGENT_LOG" 2>&1 &
AGENT=$!
check "after its next resync nothing is unmanaged" 'waitfor "[ -z \"\$(unmanaged)\" ]"'
check "no secret in any log (Q62)" '! grep -qF -e "$GITPW" -e e2e-password-123 "$T/server.log" "$AGENT_LOG"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -25 "$T/server.log"; echo "--- agent log"; tail -15 "$AGENT_LOG"; }
exit $fail
