#!/usr/bin/env bash
# End-to-end: payloads without the Source Server (M4-07, Q120–Q128). Synced through a standalone Source Server, then
# with it stopped: a new Revision is refused with the reason and no Sync Operation, a Rollback deploys what was
# rendered, and an Agent reinstalled with its keys (database, cache, work dir and containers gone) gets its Application
# back from the Control Plane's store, its Encrypted Secret decrypting. The Source Server back, the new Revision syncs.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ep.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api
mkdir -p "$REPO/web" "$T/data" "$T/src-data"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
SRC= SRV= AGENT=
stop() { kill "$@" 2>/dev/null; wait "$@" 2>/dev/null; }
gone() { # the Host reinstalled: its containers go too
  docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=pay-web) >/dev/null 2>&1
  docker network rm pay-web_default >/dev/null 2>&1
}
cleanup() { gone; stop $SRV $SRC $AGENT; [ -n "${KEEP:-}" ] || rm -rf "$T"; }
trap cleanup EXIT

"$BIN" source-certs --out "$T/tls" 2>/dev/null
start_source() {
  OMEGACD_SOURCE_ADDR=127.0.0.1:19090 OMEGACD_DATA=$T/src-data OMEGACD_SOURCE_TLS_DIR=$T/tls \
    "$BIN" source-server >> "$T/source.log" 2>&1 &
  SRC=$!
  for _ in $(seq 50); do curl -s --cacert "$T/tls/ca.pem" https://127.0.0.1:19090/ -o /dev/null; [ $? -ne 7 ] && break; sleep 0.2; done
}
mkdir -p "$T/bad" && touch "$T/bad/payloads"
out=$(OMEGACD_DATA=$T/bad OMEGACD_AGE_KEY_FILE=$T/bad.key "$BIN" 2>&1)
check "a store for the files it delivers it can't make stops the start, naming the setting" 'echo "$out" | grep -q "data dir: .*payloads.*(OMEGACD_DATA)"'
start_source
start_control_plane() { # extra settings as NAME=value arguments
  env "$@" OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key OMEGACD_ADMIN_PASSWORD=e2e-password-123 \
    OMEGACD_POLL_INTERVAL=1h OMEGACD_LOG_LEVEL=debug OMEGACD_SOURCE_SERVER=https://127.0.0.1:19090 OMEGACD_SOURCE_TLS_DIR=$T/tls \
    "$BIN" >> "$T/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
}
start_control_plane
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
. "$(dirname "$0")/enroll.sh"; enroll edge
waitfor() { local end=$((SECONDS + 60)); while [ $SECONDS -lt $end ]; do eval "$1" && return 0; sleep 0.3; done; return 1; }
ctr() { docker ps -q --filter label=com.docker.compose.project=pay-web; }
says() { docker logs $(ctr) 2>&1 | tail -1 | grep -qx "$1"; }
newest() { "$BIN" app history pay-web -o json | python3 -c "import json,sys; o=json.load(sys.stdin)[0]; print(o['trigger'], o['phase'], o['message'])"; }
ops() { "$BIN" app history pay-web -o json | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'; }

PW=$(openssl rand -hex 12) # a throwaway value, made up per run
cat > "$REPO/web/compose.yaml" <<'EOF2'
services:
  main:
    image: busybox:1.36
    command: ["sh", "-c", "cat /conf.txt; sleep 3600"]
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    volumes: ["./conf.txt:/conf.txt:ro"]
EOF2
printf 'DB_PASSWORD=%s\n' "$PW" > "$T/plain"
SOPS_AGE_RECIPIENTS=$RCPT sops --encrypt --input-type dotenv --output-type dotenv "$T/plain" > "$REPO/web/.sops.env"
rm "$T/plain"
echo "web v1" > "$REPO/web/conf.txt"; gitc init -q -b main && gitc add -A && gitc commit -qm v1
"$BIN" app create pay-web --repo "$REPO" --path web --host edge >/dev/null
"$BIN" app sync pay-web --wait --timeout 2m >/dev/null 2>&1
V1=$("$BIN" app history pay-web -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
echo "web v2" > "$REPO/web/conf.txt"; gitc commit -qam v2
"$BIN" app sync pay-web --wait --timeout 2m >/dev/null 2>&1
check "synced to v2 through the Source Server" 'says "web v2"'
check "the Control Plane keeps the files it delivered (Q121)" '[ "$(find "$T/data/payloads" -type f ! -path "$T/data/payloads/tmp/*" | wc -l)" -ge 4 ]'
echo "web v3" > "$REPO/web/conf.txt"; gitc commit -qam v3

stop $SRC # --- the Source Server down
out=$("$BIN" app sync pay-web 2>&1); rc=$?
check "a new Revision is refused, saying why (Q124)" '[ $rc -ne 0 ] && echo "$out" | grep -q "Source Server unreachable (OMEGACD_SOURCE_SERVER)"'
check "with no Sync Operation" '[ "$(ops)" = 2 ]'
check "logged as a 503" 'grep -q "status=503" "$T/server.log"'
"$BIN" app refresh pay-web >/dev/null 2>&1
check "the status says the deployed Revision keeps running" '"$BIN" app get pay-web -o json | grep -q "keeps running"'
"$BIN" app rollback pay-web "$V1" --wait --timeout 2m >/dev/null 2>&1
check "a Rollback deploys what was rendered (Q122)" 'says "web v1" && newest | grep -q "^rollback Succeeded"'

stop $AGENT # --- reinstalled, its keys backed up and put back (Q127)
STATE=$T/edge/.local/share/omegacd-agent
gone; find "$STATE" -mindepth 1 -maxdepth 1 ! -name signing.key ! -name host.key -exec rm -rf {} +; rm -rf "$T/edge/run"
check "only its keys are left" '[ "$(ls "$STATE" | sort | tr "\n" " ")" = "host.key signing.key " ]'
mv "$AGENT_LOG" "$T/edge-before.log"
env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" \
  ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER "$BIN" agent >> "$AGENT_LOG" 2>&1 &
AGENT=$!
check "it reconnects without an enrollment" 'waitfor "grep -q \"connected to the Control Plane, approved\" \"$AGENT_LOG\"" && ! grep -q "enrolled anew" "$AGENT_LOG"'
check "its Application is deployed again, from the Control Plane's store (Q123)" \
  'waitfor "newest | grep -q \"^restore Succeeded .*reinstalled\"" && says "web v1"'
check "its Encrypted Secret decrypts with the Host Key put back" '[ "$(docker exec $(ctr) printenv DB_PASSWORD)" = "$PW" ]'

start_source # --- back: found within seconds (its client's backoff is at most 5 s)
check "the Source Server back, the new Revision syncs" \
  'waitfor "\"\$BIN\" app sync pay-web --wait --timeout 2m >/dev/null 2>&1" && says "web v3"'
# --- pruned with the history (Q125): at start, past the files' hour of grace, keeping only the newest Sync Operation
stop $SRV
find "$T/data/payloads" -type f ! -path "$T/data/payloads/tmp/*" -exec touch -t 202001010000 {} +
start_control_plane OMEGACD_HISTORY_KEEP=1
kept() { local h; h=$(printf '%s\n' "$1" | openssl dgst -sha256 | awk '{print $NF}'); [ -f "$T/data/payloads/${h:0:2}/$h" ]; }
check "the payloads no kept Sync Operation uses are pruned, logged" \
  'waitfor "grep -q \"msg=\\\"payloads pruned\\\" component=history renders=2 files=2\" \"$T/server.log\"" && [ "$(ops)" = 1 ]'
check "v1's and v2's files gone, the deployed v3's kept" '! kept "web v1" && ! kept "web v2" && kept "web v3"'
check "it warns that webhooks have no secret (Q134)" 'grep -q "OMEGACD_WEBHOOK_SECRET is unset" "$T/server.log"'
check "stopped with its Agent connected, it records the Agent leaving before closing its database (M4-10a)" \
  '! grep -q "record the agent leaving" "$T/server.log"'
check "no secret in any log (Q62)" '! grep -qF -e "$PW" -e e2e-password-123 "$T/server.log" "$T/source.log" "$AGENT_LOG" "$T/edge-before.log"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -25 "$T/server.log"; echo "--- agent log"; tail -15 "$AGENT_LOG"; }
exit $fail
