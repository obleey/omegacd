#!/usr/bin/env bash
# End-to-end: what the Control Plane does through an Agent besides Syncs (M4-08). A private registry's credentials go
# to the Agent only when it pulls, age-encrypted to its Host Key: the password is in no log, on no disk of the Agent,
# and never in plaintext on the wire (the Agent connects through a proxy that records every byte). Live Sync progress
# reaches the UI's event stream. Deletes go through the Agent: with volumes, keeping containers, waiting for an offline
# Host and done when it's back, and --forget for a Host gone for good (Q83-Q87, Q92).
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eo.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api REG=127.0.0.1:15000 PRE=e2e-ops
mkdir -p "$REPO/priv" "$REPO/vol" "$REPO/keep" "$REPO/later" "$REPO/gone" "$T/data" "$T/work" "$T/auth"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
PIDS=()
cleanup() {
  for a in priv vol keep later gone; do
    docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$PRE-$a) >/dev/null 2>&1
    docker network rm $PRE-${a}_default >/dev/null 2>&1
    docker volume rm $PRE-${a}_data >/dev/null 2>&1
  done
  docker rm -f $PRE-registry >/dev/null 2>&1
  docker rmi $REG/private/busybox:1 >/dev/null 2>&1
  kill ${PIDS[@]+"${PIDS[@]}"} 2>/dev/null; wait 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT

# a private registry with a made-up password, and an image only it has
PW=$(openssl rand -hex 16)
docker run --rm --entrypoint htpasswd docker.io/library/httpd:2.4-alpine -Bbn puller "$PW" > "$T/auth/htpasswd" 2>/dev/null
docker run -d --name $PRE-registry -p $REG:5000 -v "$T/auth:/auth" -e REGISTRY_AUTH=htpasswd \
  -e REGISTRY_AUTH_HTPASSWD_REALM=e2e -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd docker.io/library/registry:2 >/dev/null
for _ in $(seq 50); do curl -s -o /dev/null "http://$REG/v2/" && break; sleep 0.2; done
export DOCKER_CONFIG=$T/docker-cli # the test's own login, not the user's ~/.docker
docker pull -q docker.io/library/busybox:1.36 >/dev/null
docker tag docker.io/library/busybox:1.36 $REG/private/busybox:1
echo "$PW" | docker login -u puller --password-stdin $REG >/dev/null 2>&1
docker push -q $REG/private/busybox:1 >/dev/null
docker rmi $REG/private/busybox:1 >/dev/null # the Agent must pull it
unset DOCKER_CONFIG

age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
H=(-H "Authorization: Bearer $OMEGACD_TOKEN" -H 'X-Requested-With: omegacd' -H 'Content-Type: application/json')
waitfor() { for _ in $(seq $((${3:-10} * 10))); do grep -qF -- "$2" "$1" 2>/dev/null && return 0; sleep 0.1; done; return 1; }

# the Agent's connection runs through a proxy that records every byte, both ways
cat > "$T/tap.py" <<'EOF'
import socket, sys, threading
out = open(sys.argv[1], "ab", buffering=0)
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 18081)); srv.listen()
def pipe(a, b):
    try:
        while (d := a.recv(65536)):
            out.write(d); b.sendall(d)
    except OSError:
        pass
    a.close(); b.close()
while True:
    c, _ = srv.accept()
    u = socket.create_connection(("127.0.0.1", 18080))
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()
EOF
python3 "$T/tap.py" "$T/wire.bin" & PIDS+=($!)
agent() {
  env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} \
    OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18081 "$@" "$BIN" agent >> "$T/agent.log" 2>&1 &
  AGENT=$!
  PIDS+=($AGENT)
}
INVITE=$("$BIN" host add edge | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
agent OMEGACD_AGENT_INVITE="$INVITE"
waitfor "$T/agent.log" "waiting for an admin to approve"
"$BIN" host approve edge "$(grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' "$T/agent.log" | head -1 | cut -d= -f2)" >/dev/null
waitfor "$T/agent.log" "connected to the Control Plane, approved"

cat > "$REPO/priv/compose.yaml" <<EOF
services:
  app:
    image: $REG/private/busybox:1
    command: ["sh", "-c", "echo pulled-from-the-private-registry; sleep 3600"]
EOF
for a in vol keep later gone; do
  cat > "$REPO/$a/compose.yaml" <<'EOF'
services:
  app:
    image: busybox:1.36
    command: ["sh", "-c", "date > /data/started; sleep 3600"]
    volumes: ["data:/data"]
volumes:
  data: {}
EOF
done
gitc init -q -b main && gitc add -A && gitc commit -qm apps
ctr() { docker ps -q --filter label=com.docker.compose.project=$PRE-$1; }
synced() { "$BIN" app sync $PRE-$1 --wait --no-health --timeout 3m >/dev/null 2>&1; }
for a in priv vol keep later gone; do "$BIN" app create $PRE-$a --repo "$REPO" --path $a --host edge >/dev/null; done

# Q85-Q87: a private registry through the Agent
check "without its credentials the private image doesn't pull" '! synced priv'
curl -sf "${H[@]}" -d "{\"registry\":\"$REG\",\"username\":\"puller\",\"password\":\"$PW\"}" $API/settings/registry-credentials >/dev/null
curl -sN -H "Authorization: Bearer $OMEGACD_TOKEN" $API/events > "$T/events" & PIDS+=($!)
sleep 1
check "with them, the Agent pulls it: they reach it when it pulls (Q85)" 'synced priv && docker logs $(ctr priv) 2>&1 | grep -q pulled-from-the-private-registry'
check "the password is in no log (Q86)" '! grep -qF "$PW" "$T/server.log" "$T/agent.log"'
check "on no disk of the Agent: its state, work dir and home" '! grep -rqaF "$PW" "$T/edge"'
# what travels in the clear (the registry's name in the request, the files' names) is readable in the recording: no
# compression or framing hides text from it, so a password sent in plaintext would show
check "the recording reads the Agent's traffic: names in the clear are there" \
  'grep -qaF "$REG" "$T/wire.bin" && grep -qaF compose.yaml "$T/wire.bin" && ! grep -qaF permessage-deflate "$T/wire.bin"'
check "and never the password in plaintext on the wire: it went age-encrypted to the Host Key" '! grep -qaF "$PW" "$T/wire.bin"'
OP=$("$BIN" app history $PRE-priv -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
check "the Sync's live progress from the Agent reached the UI's event stream (Q87)" \
  'grep -A1 "^event: progress" "$T/events" | grep -q "\"opID\":$OP"'

# Q83: deletes through the Agent
for a in vol keep later gone; do synced $a; done
check "the apps for the deletes run" '[ -n "$(ctr vol)" ] && [ -n "$(ctr keep)" ] && [ -n "$(ctr later)" ] && [ -n "$(ctr gone)" ]'
"$BIN" app delete $PRE-vol --volumes --yes >/dev/null 2>&1
check "delete with its volumes: the Agent removes containers and volume" \
  '[ -z "$(ctr vol)" ] && ! docker volume inspect $PRE-vol_data >/dev/null 2>&1 && ! "$BIN" app get $PRE-vol >/dev/null 2>&1'
"$BIN" app delete $PRE-keep --cascade=false --yes >/dev/null 2>&1
check "delete keeping containers: they run on, the Agent forgets it" \
  '[ -n "$(ctr keep)" ] && ! "$BIN" app get $PRE-keep >/dev/null 2>&1 && grep -q "keepContainers=true" "$T/agent.log"'

kill $AGENT; wait $AGENT 2>/dev/null
out=$("$BIN" app delete $PRE-later --yes 2>&1)
check "to an offline Host a delete waits, saying so" 'echo "$out" | grep -q "deleting, waiting for the Host edge"'
check "the app shows it is being deleted" '"$BIN" app get $PRE-later -o json | python3 -c "import json,sys; assert json.load(sys.stdin)[\"deleting\"]==\"containers\""'
check "its containers run on meanwhile" '[ -n "$(ctr later)" ]'
out=$("$BIN" app delete $PRE-gone --forget --yes 2>&1)
check "--forget removes an app from OmegaCD only, asking no Agent (Q92)" \
  '! "$BIN" app get $PRE-gone >/dev/null 2>&1 && [ -n "$(ctr gone)" ]'
: > "$T/agent.log"; agent
for _ in $(seq 60); do "$BIN" app get $PRE-later >/dev/null 2>&1 || break; sleep 1; done
check "its Agent back, the waiting delete is done" '! "$BIN" app get $PRE-later >/dev/null 2>&1 && [ -z "$(ctr later)" ]'
check "and the forgotten app's containers were left alone" '[ -n "$(ctr gone)" ]'
check "the password is still nowhere (Q86)" '! grep -qF "$PW" "$T/server.log" "$T/agent.log" "$T/wire.bin" && ! grep -rqaF "$PW" "$T/edge"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -20 "$T/server.log"; echo "--- agent log"; tail -20 "$T/agent.log"; }
exit $fail
