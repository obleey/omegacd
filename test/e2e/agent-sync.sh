#!/usr/bin/env bash
# End-to-end: a Sync through an Agent (M4-01). The Control Plane renders from Git and sends the Desired State; the
# enrolled Agent fetches the files, decrypts the SOPS secret with its own Host Key (nothing decrypted, or written, on
# the Control Plane's side) and deploys; one Sync is followed by its op from the Control Plane's log into the Agent's;
# a rollback; a Sync to an offline Host waits for it (Q81); an Agent refuses plain http to another machine (Q95).
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eg.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api APP=e2e-agentsync
mkdir -p "$REPO/web" "$T/data" "$T/work"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
PIDS=()
cleanup() {
  docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$APP) >/dev/null 2>&1
  docker network rm ${APP}_default >/dev/null 2>&1
  kill ${PIDS[@]+"${PIDS[@]}"} 2>/dev/null; wait 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT

age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
waitfor() { for _ in $(seq $((${3:-10} * 10))); do grep -qF -- "$2" "$1" 2>/dev/null && return 0; sleep 0.1; done; return 1; }
agent() {
  env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} \
    OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER "$@" "$BIN" agent >> "$T/agent.log" 2>&1 &
  AGENT=$!
  PIDS+=($AGENT)
}

INVITE=$("$BIN" host add edge | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
agent OMEGACD_AGENT_INVITE="$INVITE"
waitfor "$T/agent.log" "waiting for an admin to approve"
FP=$(grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' "$T/agent.log" | head -1 | cut -d= -f2)
"$BIN" host approve edge "$FP" >/dev/null
waitfor "$T/agent.log" "connected to the Control Plane, approved"
RCPT=$("$BIN" host list -o json | python3 -c 'import json,sys; print([h for h in json.load(sys.stdin) if h["name"]=="edge"][0]["ageRecipient"])')

# the secret is encrypted for the Agent's Host Key only: the Control Plane couldn't decrypt it if it tried
cat > "$REPO/web/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "cat /conf.txt; echo secret=$$SECRET; sleep 3600"]
    environment:
      SECRET: ${SECRET}
    volumes: ["./conf.txt:/conf.txt:ro"]
EOF
echo "conf v1" > "$REPO/web/conf.txt"
printf 'SECRET=only-the-agent-reads-this\n' > "$T/plain"
SOPS_AGE_RECIPIENTS=$RCPT sops --encrypt --input-type dotenv --output-type dotenv "$T/plain" > "$REPO/web/.sops.env" 2> "$T/sops.err"
rm "$T/plain"
gitc init -q -b main && gitc add -A && gitc commit -qm v1

ctr() { docker ps -q --filter label=com.docker.compose.project=$APP; }
"$BIN" app create $APP --repo "$REPO" --path web --host edge > "$T/create.out" 2>&1
out=$("$BIN" app sync $APP --wait --timeout 3m 2>&1); rc=$?
check "a Sync through the Agent succeeds, --wait waiting for the Health its Agent reports (M4-02)" '[ $rc -eq 0 ] && echo "$out" | grep -q Healthy'
check "its container runs on the Agent's Host, the secret decrypted by the Agent" \
  '[ -n "$(ctr)" ] && docker logs $(ctr) 2>&1 | grep -q "secret=only-the-agent-reads-this" && docker logs $(ctr) 2>&1 | grep -q "conf v1"'
check "the Agent's work dir has the app's files" '[ -f "$T/edge/run/omegacd/$APP/conf.txt" ]'
check "the Control Plane wrote no file of it" '[ -z "$(find "$T/work" -path "*$APP*" 2>/dev/null)" ]'
check "and decrypted nothing: the secret is nowhere on its side" '! grep -rqaF only-the-agent-reads-this "$T/data" "$T/work" "$T/server.log"'
check "nor in the Agent's log (Q62)" '! grep -qF only-the-agent-reads-this "$T/agent.log"'
OP=$("$BIN" app history $APP -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
check "one Sync followed by its op from the Control Plane's log into the Agent's (Q62)" \
  'grep "op=$OP " "$T/server.log" | grep -q "sync started" && grep "op=$OP " "$T/agent.log" | grep -q "msg=deploy"'
check "Synced" '"$BIN" app get $APP -o json | python3 -c "import json,sys; v=json.load(sys.stdin); assert v[\"syncStatus\"]==\"Synced\", v[\"syncStatus\"]"'
# M4-02: the views come from the Agent's reports
api() { curl -sf -H "Authorization: Bearer $OMEGACD_TOKEN" "$API/apps/$APP/$1"; }
check "Live State and resources as the Agent reported them, the decrypted .env marked secret" 'api resources | python3 -c "
import json,sys; v=json.load(sys.stdin); c=v[\"containers\"]; r={(x[\"kind\"],x[\"name\"]):x for x in v[\"resources\"]}
assert len(c)==1 and c[0][\"state\"]==\"running\" and r[(\"env_file\",\".env\")].get(\"secret\"), v"'
check "a file's content from the rendered Revision; the secret's stays on the Host" \
  'api "resource?kind=bind&name=conf.txt" | grep -q "conf v1" && ! api "resource?kind=env_file&name=.env" | grep -q only-the-agent-reads-this'
check "app logs through the Agent" '"$BIN" app logs $APP --tail 20 2>&1 | grep -q "secret=only-the-agent-reads-this"'
check "host list: the Host connected, its engine's version" '"$BIN" host list -o json | python3 -c "
import json,sys; h=[h for h in json.load(sys.stdin) if h[\"name\"]==\"edge\"][0]; assert h[\"status\"][\"connected\"] and h[\"status\"][\"serverVersion\"], h[\"status\"]"'

echo "conf v2" > "$REPO/web/conf.txt"; gitc commit -qam v2
"$BIN" app refresh $APP >/dev/null 2>&1
# M4-11: the Diff of the new Revision, computed by the Agent without deploying it
out=$("$BIN" app diff $APP 2>&1); rc=$?
check "app diff of a new Revision through the Agent: the service would change" '[ $rc -eq 1 ] && echo "$out" | grep -q "~ web"'
check "nothing deployed by it, and no staged file left on the Host" \
  'docker logs $(ctr) 2>&1 | tail -3 | grep -q "conf v1" && [ -z "$(ls -A "$T/edge/run/omegacd/.omegacd-diff" 2>/dev/null)" ]'
"$BIN" app sync $APP --wait --timeout 3m >/dev/null 2>&1
check "a new Revision deploys" 'sleep 2; docker logs $(ctr) 2>&1 | grep -q "conf v2"'
out=$("$BIN" app rollback $APP "$OP" --wait --timeout 3m 2>&1); rc=$?
check "a rollback through the Agent" '[ $rc -eq 0 ] && sleep 2 && docker logs $(ctr) 2>&1 | grep -q "conf v1" && ! docker logs $(ctr) 2>&1 | grep -q "conf v2"'

kill $AGENT; wait $AGENT 2>/dev/null
out=$("$BIN" app sync $APP --wait --no-health --timeout 1m 2>&1); rc=$?
check "app sync --wait stops at a Sync waiting for its Host, saying so" '[ $rc -ne 0 ] && echo "$out" | grep -q "Waiting: waiting for Host edge"'
check "a Sync to an offline Host waits for it (Q81)" '"$BIN" app history $APP -o json | python3 -c "
import json,sys; op=json.load(sys.stdin)[0]; assert op[\"phase\"]==\"Waiting\" and \"waiting for Host edge\" in op[\"message\"], op"'
check "its containers keep running meanwhile" '[ -n "$(ctr)" ]'
check "offline, the app shows its Host offline since when, and the last known state from when (Q38)" '"$BIN" app get $APP -o json | python3 -c "
import json,sys; v=json.load(sys.stdin); m=v.get(\"syncMessage\",\"\")
assert \"Host edge offline since\" in m and \"last known state from\" in m and v[\"health\"]==\"Healthy\", v"'
check "its resources still show, as last known" 'api resources | grep -q "\"state\":\"running\""'
check "and logs say the Host is offline" '! "$BIN" app logs $APP --tail 5 >/dev/null 2>&1'
out=$("$BIN" app diff $APP 2>&1)
check "offline, app diff shows the last Diff its Agent computed, saying so (M4-11)" 'echo "$out" | grep -q "warning: Host edge offline: the services. changes as its Agent computed them at"'

# M3-06: back online, the Sync that waited runs; a rollback run on the Host while offline arrives exactly once
hist() { "$BIN" app history $APP -o json | python3 -c "import json,sys; ops=json.load(sys.stdin); $1"; }
WAIT=$(hist 'print(ops[0]["id"])')
: > "$T/agent.log"; agent
for _ in $(seq 120); do [ "$(hist "print([o for o in ops if o['id']==$WAIT][0]['phase'])")" = Succeeded ] && break; sleep 1; done
check "the Sync that waited runs when its Agent is back, as the same Sync Operation (Q81)" \
  '[ "$(hist "print([o for o in ops if o[\"id\"]==$WAIT][0][\"phase\"])")" = Succeeded ] && sleep 2 && docker logs $(ctr) 2>&1 | grep -q "conf v2"'
REV1=$(hist "print([o for o in ops if o['id']==$OP][0]['revision'])")
rollbacks() { hist 'print(sum(1 for o in ops if o["trigger"]=="rollback"))'; }
BEFORE=$(rollbacks) # the rollback through the Control Plane above
kill $AGENT; wait $AGENT 2>/dev/null
out=$(env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} "$BIN" agent rollback $APP "$REV1" 2>&1); rc=$?
check "offline, a rollback on the Host itself (Q25)" '[ $rc -eq 0 ] && sleep 2 && docker logs $(ctr) 2>&1 | tail -3 | grep -q "conf v1"'
agent
for _ in $(seq 60); do [ "$(rollbacks)" = $((BEFORE + 1)) ] && break; sleep 1; done
check "back online, the Agent reports it from its outbox" '[ "$(rollbacks)" = $((BEFORE + 1)) ]'
check "the Control Plane shows it rolled back on the Host, Auto-Sync off" '"$BIN" app get $APP -o json | python3 -c "
import json,sys; v=json.load(sys.stdin); assert v[\"rolledBack\"] and not v[\"autoSync\"], v"'
kill $AGENT; wait $AGENT 2>/dev/null; : > "$T/agent.log"; agent
waitfor "$T/agent.log" "connected to the Control Plane, approved"; sleep 3
check "exactly once: another connection doesn't report it again" '[ "$(rollbacks)" = $((BEFORE + 1)) ]'

out=$(env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/other" OMEGACD_AGENT_CONTROL_PLANE=http://cp.example.com "$BIN" agent 2>&1); rc=$?
check "an Agent refuses plain http to another machine (Q95)" '[ $rc -ne 0 ] && echo "$out" | grep -q "https only"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -20 "$T/server.log"; echo "--- agent log"; tail -20 "$T/agent.log"; }
exit $fail
