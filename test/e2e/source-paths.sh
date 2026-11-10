#!/usr/bin/env bash
# End-to-end: every way a change in Git reaches an Agent goes through a standalone Source Server (M4-03, Q46): an App
# of Apps' definitions come from its Render, a webhook makes it fetch again (polling 1h), and polling asks it to
# Resolve. The Control Plane clones nothing.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ep.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api PARENT=e2e-paths CHILD=e2e-paths-web
SECRET=$(openssl rand -hex 16) # the webhook secret, made up per run
mkdir -p "$REPO/apps" "$REPO/web" "$T/data" "$T/src-data" "$T/work"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
PIDS=() SRV=
cleanup() {
  for p in $CHILD $CHILD-2; do
    docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$p) >/dev/null 2>&1
    docker network rm ${p}_default >/dev/null 2>&1
  done
  kill $SRV ${PIDS[@]+"${PIDS[@]}"} 2>/dev/null; wait 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT

cat > "$REPO/web/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "cat /conf.txt; sleep 3600"]
    volumes: ["./conf.txt:/conf.txt:ro"]
EOF
echo "conf v1" > "$REPO/web/conf.txt"
cat > "$REPO/apps/web.yaml" <<EOF
name: $CHILD
repoURL: $REPO
path: web
host: edge
autoSync: true
EOF
gitc init -q -b main && gitc add -A && gitc commit -qm v1

"$BIN" source-certs --out "$T/tls" 2>/dev/null
age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_SOURCE_ADDR=127.0.0.1:19090 OMEGACD_DATA=$T/src-data OMEGACD_SOURCE_TLS_DIR=$T/tls "$BIN" source-server > "$T/source.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -s --cacert "$T/tls/ca.pem" https://127.0.0.1:19090/ -o /dev/null; [ $? -ne 7 ] && break; sleep 0.2; done
control_plane() { # poll interval
  OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
    OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=$1 OMEGACD_WEBHOOK_SECRET=$SECRET \
    OMEGACD_SOURCE_SERVER=https://127.0.0.1:19090 OMEGACD_SOURCE_TLS_DIR=$T/tls "$BIN" >> "$T/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
}
control_plane 1h
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
waitfor() { for _ in $(seq $((${3:-10} * 10))); do grep -qF -- "$2" "$1" 2>/dev/null && return 0; sleep 0.1; done; return 1; }
INVITE=$("$BIN" host add edge | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} \
  OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER OMEGACD_AGENT_INVITE="$INVITE" "$BIN" agent >> "$T/agent.log" 2>&1 &
PIDS+=($!)
waitfor "$T/agent.log" "waiting for an admin to approve"
"$BIN" host approve edge "$(grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' "$T/agent.log" | head -1 | cut -d= -f2)" >/dev/null
waitfor "$T/agent.log" "connected to the Control Plane, approved"

conf() { cat "$T/edge/run/omegacd/$PARENT/$1/conf.txt" 2>/dev/null; } # children's folders are in their parent's
# until the Agent's copy of the child's conf.txt says $2 (or 30 s), then its Sync has finished: the Agent writes the
# files before compose starts the containers
deployed() {
  for _ in $(seq 60); do
    [ "$(conf "$1")" = "$2" ] && { "$BIN" app wait "$1" --sync --timeout 1m >/dev/null 2>&1; return; }
    sleep 0.5
  done
  return 1
}

"$BIN" app create $PARENT --repo "$REPO" --path apps --kind apps --auto-sync >/dev/null
check "the App of Apps' child, defined in Git, is deployed by the Agent" 'deployed $CHILD "conf v1"'
check "it runs on the Agent's Host" '[ -n "$(docker ps -q --filter label=com.docker.compose.project=$CHILD)" ]'
check "the Source Server cloned the repository, the Control Plane did not" '[ -d "$T/src-data/repos" ] && [ ! -d "$T/data/repos" ]'

# a push the webhook announces (Gitea's format, signed): with polling at 1h only it can make it deploy
hook() { # body; prints the HTTP code
  sig=$(printf '%s' "$1" | openssl dgst -sha256 -hmac "${2:-$SECRET}" | sed 's/.*= //')
  curl -s -o /dev/null -w '%{http_code}' -H 'X-Gitea-Event: push' -H "X-Gitea-Signature: $sig" -d "$1" $API/webhooks/gitea
}
echo "conf v2" > "$REPO/web/conf.txt"
sed "s/$CHILD/$CHILD-2/" "$REPO/apps/web.yaml" > "$REPO/apps/web-2.yaml"
gitc add -A && gitc commit -qm v2
code=$(hook "{\"repository\":{\"clone_url\":\"$REPO\"}}" wrong-secret)
check "a webhook signed with another secret is refused ($code)" '[ "$code" = 401 ]'
code=$(hook "{\"repository\":{\"clone_url\":\"$REPO\"}}")
check "the signed webhook is accepted ($code)" '[ "$code" = 202 ]'
check "the push reaches the Agent through it" 'deployed $CHILD "conf v2"'
check "a child added in Git by the same push is created and deployed" 'deployed $CHILD-2 "conf v2"'

# any other caller (a CI step, a Git host without a format of its own) uses the generic one, the secret its bearer token (Q99)
generic() { curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -d "$2" $API/webhooks/generic; }
echo "conf v2b" > "$REPO/web/conf.txt"; gitc commit -qam v2b
code=$(generic wrong-secret "{\"repoURL\":\"$REPO\"}")
check "a generic webhook without the secret is refused ($code)" '[ "$code" = 401 ]'
code=$(generic "$SECRET" '{"repository":"not its format"}')
check "one without a repoURL is refused as invalid ($code)" '[ "$code" = 400 ]'
code=$(generic "$SECRET" "{\"repoURL\":\"$REPO\"}")
check "the generic webhook is accepted ($code) and the push reaches the Agent" '[ "$code" = 202 ] && deployed $CHILD "conf v2b"'

# polling: the Control Plane asks the Source Server to Resolve; no webhook this time
kill $SRV; wait $SRV 2>/dev/null
control_plane 2s
echo "conf v3" > "$REPO/web/conf.txt"; gitc commit -qam v3
check "a push nobody announces is found by polling" 'deployed $CHILD "conf v3"'
check "still no clone on the Control Plane's side" '[ ! -d "$T/data/repos" ] && [ -z "$(find "$T/data" "$T/work" -name HEAD -path "*.git*" 2>/dev/null)" ]'
check "the log is at debug and holds no secret (Q62)" 'grep -q level=DEBUG "$T/source.log" && ! grep -qF -e "$SECRET" -e e2e-password-123 "$T/server.log" "$T/source.log" "$T/agent.log"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -20 "$T/server.log"; echo "--- source server log"; tail -10 "$T/source.log"; echo "--- agent log"; tail -10 "$T/agent.log"; }
exit $fail
