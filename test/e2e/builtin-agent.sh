#!/usr/bin/env bash
# End-to-end: the built-in Agent (M4-04, Q26, Q35, Q88, Q100). On a machine with Docker, omegacd server manages it as
# the Host local through an Agent of its own, enrolled with no step; its secrets are encrypted to that Agent's Host
# Key, not the Master Key; in Kubernetes (auto) it is off and local deploys as before (Q101).
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eb.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api APP=e2e-builtin
mkdir -p "$REPO/web" "$T/data" "$T/work"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
SRV=
cleanup() {
  "$BIN" app delete $APP --volumes --yes >/dev/null 2>&1
  docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$APP) >/dev/null 2>&1
  kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT
age-keygen -o "$T/age.key" 2>/dev/null
server() { # extra env
  env "$@" OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
    OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" >> "$T/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
}
restart() { kill $SRV; wait $SRV 2>/dev/null; : > "$T/server.log"; server "$@"; }
waitfor() { for _ in $(seq $((${3:-10} * 10))); do grep -qF -- "$2" "$1" 2>/dev/null && return 0; sleep 0.1; done; return 1; }
local_host() { "$BIN" host list -o json | python3 -c "import json,sys; h=[h for h in json.load(sys.stdin) if h['name']=='local'][0]; print(h.get('$1') or h['status'].get('$1') or '')"; }

server
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')

check "the startup log says it is on, and why" 'grep "msg=\"built-in agent\"" "$T/server.log" | grep -q "on=true why=\"a container engine is reachable\""'
check "it connects to its own Control Plane, approved, with no enrollment step" 'waitfor "$T/server.log" "connected to the Control Plane, approved"'
check "local is listed once, connected, with the Agent's key" '[ "$(local_host connected)" = True ] && [ -n "$(local_host fingerprint)" ] && [ "$("$BIN" host list -o json | python3 -c "import json,sys; print(sum(h[\"name\"]==\"local\" for h in json.load(sys.stdin)))")" = 1 ]'
RCPT=$(local_host ageRecipient)
MASTER=$(grep -o 'age1[0-9a-z]*' "$T/age.key" | head -1)
check "its Host Key is its own, not the Master Key (Q100)" '[ -n "$RCPT" ] && [ "$RCPT" != "$MASTER" ] && [ -f "$T/data/agent/host.key" ]'

cat > "$REPO/web/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "echo secret=$$SECRET; sleep 3600"]
    environment:
      SECRET: ${SECRET}
EOF
printf 'SECRET=for-the-builtin-agent\n' > "$T/plain"
SOPS_AGE_RECIPIENTS=$RCPT sops --encrypt --input-type dotenv --output-type dotenv "$T/plain" > "$REPO/web/.sops.env"
rm "$T/plain"
gitc init -q -b main && gitc add -A && gitc commit -qm v1
"$BIN" app create $APP --repo "$REPO" --path web >/dev/null
out=$("$BIN" app sync $APP --wait --timeout 3m 2>&1); rc=$?
check "an app on local syncs through it" '[ $rc -eq 0 ] && grep "component=agent" "$T/server.log" | grep "app=$APP" | grep -q "msg=deploy"'
ctr() { docker ps -q --filter label=com.docker.compose.project=$APP; }
check "the built-in Agent decrypted its secret with its own key" 'docker logs $(ctr) 2>&1 | grep -q secret=for-the-builtin-agent'
check "the secret is in no log (Q62)" '! grep -qF for-the-builtin-agent "$T/server.log"'

FP=$(local_host fingerprint)
restart
check "a restart keeps its keys" 'waitfor "$T/server.log" "connected to the Control Plane, approved" && [ "$(local_host fingerprint)" = "$FP" ]'

restart KUBERNETES_SERVICE_HOST=10.0.0.1
check "in Kubernetes (auto) it is off, saying why" 'grep "msg=\"built-in agent\"" "$T/server.log" | grep -q "on=false why=\"in Kubernetes"'
check "and no Agent starts" '! grep -q "component=agent" "$T/server.log"'

kill $SRV; wait $SRV 2>/dev/null
OMEGACD_BUILTIN_AGENT=maybe OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  "$BIN" > "$T/bad.log" 2>&1; rc=$?
check "a setting that isn't auto, true or false stops the server" '[ $rc -ne 0 ] && grep -q "OMEGACD_BUILTIN_AGENT must be auto, true or false" "$T/bad.log"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -25 "$T/server.log"; }
exit $fail
