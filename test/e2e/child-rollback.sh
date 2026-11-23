#!/usr/bin/env bash
# End-to-end: a Child Application rolled back as any Application, through an Agent (M5-09, Q171): its App of Apps'
# Syncs leave it rolled back, a manual Sync of it ends the rollback and the next Sync of its App of Apps applies Git's
# settings again (Q221); rolled back and removed from Git, it is pruned like any child (Q222); a child adopting a
# running stack waits for its first manual Sync (Q167).
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
. "$(dirname "$0")/admin.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ec.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api
mkdir -p "$REPO/defs" "$REPO/web" "$T/data"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
cleanup() {
  for p in web held; do
    docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$p) >/dev/null 2>&1
    docker network rm ${p}_default >/dev/null 2>&1
  done
  kill $SRV ${AGENT:-} 2>/dev/null; wait $SRV ${AGENT:-} 2>/dev/null; rm -rf "$T"
}
trap cleanup EXIT

PW=$(python3 -c 'import secrets; print(secrets.token_urlsafe(12))')
age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
admin_login "$API" "$T/data"
. "$(dirname "$0")/enroll.sh"; enroll box

cat > "$REPO/web/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "cat /conf.txt; sleep 3600"]
    volumes: ["./conf.txt:/conf.txt:ro"]
EOF
web() { printf 'name: web\nrepoURL: %s\npath: web\nhost: box\nautoSync: true\n' "$REPO"; }
web > "$REPO/defs/apps.yaml"
echo "conf v1" > "$REPO/web/conf.txt"
gitc init -q -b main && gitc add -A && gitc commit -qm v1
shown() { docker logs $(docker ps -q --filter label=com.docker.compose.project=web) 2>&1 | tail -1; }
field() { "$BIN" app get web -o json | python3 -c "import json,sys; print(json.load(sys.stdin)[sys.argv[1]])" "$1"; }
ops() { "$BIN" app history web -o json | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'; }

"$BIN" app create platform --repo "$REPO" --path defs --kind apps --prune >/dev/null
"$BIN" app sync platform --wait --timeout 3m >/dev/null 2>&1
"$BIN" app wait web --timeout 2m >/dev/null 2>&1
check "its App of Apps deploys the child" '[ "$(shown)" = "conf v1" ]'
V1=$("$BIN" app history web -o json | python3 -c 'import json,sys; print([o["id"] for o in json.load(sys.stdin) if o["phase"]=="Succeeded"][0])')

echo "conf v2" > "$REPO/web/conf.txt"; gitc commit -qam v2
"$BIN" app refresh platform >/dev/null 2>&1; "$BIN" app sync platform --wait --timeout 3m >/dev/null 2>&1
for _ in $(seq 60); do [ "$(shown)" = "conf v2" ] && break; sleep 1; done
check "a new Revision reaches it" '[ "$(shown)" = "conf v2" ]'

out=$("$BIN" app rollback web "$V1" --wait --no-health --timeout 2m 2>&1); rc=$?
check "the child is rolled back, through its Agent (Q171)" '[ $rc -eq 0 ] && sleep 2 && [ "$(shown)" = "conf v1" ]'
check "rolled back, its Auto-Sync off" '[ "$(field rolledBack)" = True ] && [ "$(field autoSync)" = False ]'

N=$(ops)
echo "conf v3" > "$REPO/web/conf.txt"; gitc commit -qam v3
"$BIN" app refresh platform >/dev/null 2>&1; "$BIN" app sync platform --wait --timeout 3m >/dev/null 2>&1; sleep 3
check "a Sync of its App of Apps leaves it rolled back, unsynced" \
  '[ "$(shown)" = "conf v1" ] && [ "$(field rolledBack)" = True ] && [ "$(field autoSync)" = False ] && [ "$(ops)" = "$N" ]'
check "and the Control Plane says why, at debug" 'grep "child application not synced with its App of Apps" "$T/server.log" | grep -q "app=web"'

"$BIN" app sync web --wait --no-health --timeout 2m >/dev/null 2>&1; sleep 2
check "a manual Sync of it ends the rollback, Auto-Sync still off (Q221)" \
  '[ "$(shown)" = "conf v3" ] && [ "$(field rolledBack)" = False ] && [ "$(field autoSync)" = False ]'
"$BIN" app sync platform --wait --timeout 3m >/dev/null 2>&1
check "the next Sync of its App of Apps applies Git's autoSync again" '[ "$(field autoSync)" = True ]'

"$BIN" app rollback web "$V1" --wait --no-health --timeout 2m >/dev/null 2>&1
: > "$REPO/defs/apps.yaml"; gitc commit -qam "web removed"
"$BIN" app refresh platform >/dev/null 2>&1; "$BIN" app sync platform --wait --timeout 3m >/dev/null 2>&1
for _ in $(seq 60); do "$BIN" app get web >/dev/null 2>&1 || break; sleep 1; done
check "rolled back and removed from Git, it is pruned like any child (Q222)" \
  '! "$BIN" app get web >/dev/null 2>&1 && [ -z "$(docker ps -aq --filter label=com.docker.compose.project=web)" ]'

# Q167: a child adopting a running stack waits for its first manual Sync, after its Diff
mkdir -p "$REPO/held" && cp "$REPO/web/compose.yaml" "$REPO/held/" && echo "held" > "$REPO/held/conf.txt"
(cd "$REPO/held" && docker compose -p held up -d >/dev/null 2>&1)
first=$(docker ps -q --filter label=com.docker.compose.project=held)
printf 'name: held\nrepoURL: %s\npath: held\nhost: box\nadopt: true\n' "$REPO" > "$REPO/defs/apps.yaml"; gitc add -A; gitc commit -qm held
"$BIN" app refresh platform >/dev/null 2>&1; "$BIN" app sync platform --wait --timeout 3m >/dev/null 2>&1; sleep 3
check "an adopted child is created but not synced by its App of Apps (Q167)" \
  '"$BIN" app get held >/dev/null 2>&1 && [ "$("$BIN" app history held -o json)" = "[]" ] && [ "$(docker ps -q --filter label=com.docker.compose.project=held)" = "$first" ]'
"$BIN" app sync held --wait --no-health --timeout 2m >/dev/null 2>&1
check "its first manual Sync takes the stack over" '"$BIN" app history held -o json | grep -q "\"Succeeded\""'

check "no password in the logs (Q62)" '! grep -qF "$PW" "$T/server.log" "$AGENT_LOG"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -15 "$T/server.log"; echo "--- agent log"; tail -15 "$AGENT_LOG"; }
exit $fail
