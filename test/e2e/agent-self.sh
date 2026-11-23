#!/usr/bin/env bash
# End-to-end: an Agent running in a container, as a Compose project of its own, refuses an Application of that project
# (Q109) and deploys any other, the Control Plane's included (Q104); after a reboot it reclaims its work dir from
# Docker's placeholders and restores (Q143). The binary is OMEGACD_BIN on Linux; elsewhere a
# Linux build of this checkout for the engine's architecture.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
. "$(dirname "$0")/admin.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2es.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api SELF=e2e-selfagent
mkdir -p "$REPO/self" "$REPO/other" "$T/data" "$T/work" "$T/state"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
agentlog() { docker logs $SELF-agent-1 2>&1; }
waitlog() { for _ in $(seq 100); do agentlog | grep -qF -- "$1" && return 0; sleep 0.2; done; return 1; }
cleanup() {
  for a in $SELF e2e-other; do "$BIN" app delete $a --yes >/dev/null 2>&1; done
  docker compose -p e2e-other down >/dev/null 2>&1
  docker compose -p $SELF -f "$T/agent.yaml" down >/dev/null 2>&1
  docker rmi omegacd-e2e-agent:self >/dev/null 2>&1
  # the Agent ran as root: what it made goes the same way
  docker run --rm -v "$T:$T" busybox:1.38 rm -rf "$T/work" "$T/state" >/dev/null 2>&1
  kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; rm -rf "$T"
}
trap cleanup EXIT

LBIN=$BIN
if [ "$(uname)" != Linux ]; then
  LBIN=$T/omegacd-linux
  (cd "$(dirname "$0")/../.." && CGO_ENABLED=0 GOOS=linux GOARCH=$(docker version -f '{{.Server.Arch}}') go build -o "$LBIN" ./cmd/omegacd) ||
    { echo "FAIL  build a Linux binary"; exit 1; }
fi

age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
admin_login "$API" "$T/data"

# the Agent as a Compose project: the engine's socket, its work dir at the same path both sides (Q21), coverage written
# where the e2e binary writes it. Its own network, so its hostname is its container ID; a sidecar in its network
# namespace forwards its loopback to the Control Plane on this machine (plain http only over loopback, Q95)
INVITE=$("$BIN" host add box | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
# an image of its own, which this test can take away
docker pull -q busybox:1.38 >/dev/null && docker tag busybox:1.38 omegacd-e2e-agent:self
cat > "$T/agent.yaml" <<EOF
services:
  agent:
    image: omegacd-e2e-agent:self
    command: ["/omegacd", "agent"]
    extra_hosts: ["host.docker.internal:host-gateway"]
    environment:
      OMEGACD_AGENT_CONTROL_PLANE: http://127.0.0.1:18080
      OMEGACD_AGENT_INVITE: $INVITE
      OMEGACD_AGENT_STATE_DIR: $T/state
      OMEGACD_AGENT_WORKDIR: $T/work
      OMEGACD_AGENT_ALLOW_ROOT: "true"
      OMEGACD_LOG_LEVEL: debug
${GOCOVERDIR:+      GOCOVERDIR: $GOCOVERDIR}
    volumes:
      - $LBIN:/omegacd:ro
      - /var/run/docker.sock:/var/run/docker.sock
      - $T/work:$T/work
      - $T/state:$T/state
${GOCOVERDIR:+      - $GOCOVERDIR:$GOCOVERDIR}
  forward:
    image: alpine/socat:1.8.0.3
    network_mode: service:agent
    command: ["tcp-listen:18080,fork,reuseaddr", "tcp:host.docker.internal:18080"]
EOF
docker compose -p $SELF -f "$T/agent.yaml" up -d --quiet-pull >/dev/null 2>&1
check "the Agent starts in its container and enrolls" 'waitlog "waiting for an admin to approve"'
FP=$(agentlog | grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' | head -1 | cut -d= -f2)
"$BIN" host approve box "$FP" >/dev/null
check "and is approved" 'waitlog "connected to the Control Plane, approved"'

printf 'services:\n  app:\n    image: busybox:1.38\n    command: ["sleep", "3600"]\n' > "$REPO/self/compose.yaml"
# a file from Git mounted: after a reboot Docker makes it a root-owned folder if it restarts the app first (Q143)
printf 'services:\n  app:\n    image: busybox:1.38\n    command: ["sh", "-c", "cat /conf.txt; sleep 3600"]\n    volumes: ["./conf.txt:/conf.txt:ro"]\n' > "$REPO/other/compose.yaml"
echo "conf from git" > "$REPO/other/conf.txt"
gitc init -q -b main && gitc add -A && gitc commit -qm init

"$BIN" app create $SELF --repo "$REPO" --path self --host box --adopt >/dev/null
out=$("$BIN" app sync $SELF --wait --timeout 2m 2>&1)
check "an Application of the project the Agent runs in is refused (Q109)" 'echo "$out" | grep -q "the Compose project this Agent runs in"'
check "and the Agent still runs, untouched" '[ "$(docker inspect -f "{{.State.Running}}" $SELF-agent-1)" = true ] && ! agentlog | grep -q "msg=deploy .*app=$SELF"'

"$BIN" app create e2e-other --repo "$REPO" --path other --host box >/dev/null
out=$("$BIN" app sync e2e-other --wait --timeout 3m 2>&1); rc=$?
check "any other it deploys" '[ $rc -eq 0 ] && [ -n "$(docker ps -q --filter label=com.docker.compose.project=e2e-other)" ]'

# Q143: the Host rebooted, its RAM work dir empty, and Docker restarted the app before the Agent: the file it mounts is
# a folder Docker made as root. The containerised Agent reclaims the work dir through Docker with its own image, then
# restores the app from its cache.
docker compose -p $SELF -f "$T/agent.yaml" stop agent >/dev/null 2>&1
docker run --rm -v "$T:$T" busybox:1.38 rm -rf "$T/work/e2e-other"
# it fails to start (a folder can't be mounted where the file was), having made the folder: as after a reboot
docker restart $(docker ps -aq --filter label=com.docker.compose.project=e2e-other) >/dev/null 2>&1
check "Docker made the missing file a folder" '[ -d "$T/work/e2e-other/conf.txt" ]'
docker rmi -f omegacd-e2e-agent:self >/dev/null # its image pruned meanwhile: it can't reclaim
docker compose -p $SELF -f "$T/agent.yaml" start agent >/dev/null 2>&1
check "an Agent whose own image is gone says it can't reclaim the work dir" 'waitlog "restore failed; fix the cause" && agentlog | grep "restore failed" | grep -q "reclaim .*/work/e2e-other: .*No such image"'
docker compose -p $SELF -f "$T/agent.yaml" stop agent >/dev/null 2>&1
docker tag busybox:1.38 omegacd-e2e-agent:self
docker compose -p $SELF -f "$T/agent.yaml" start agent >/dev/null 2>&1 # restarted, as the error says
running() { [ -f "$T/work/e2e-other/conf.txt" ] && docker logs "$(docker ps -q --filter label=com.docker.compose.project=e2e-other)" 2>&1 | grep -q "conf from git"; }
check "with its image back it reclaims the work dir from Docker's placeholders and restores the app: its file back, read by its container" 'for _ in $(seq 150); do running && break; sleep 0.2; done; running'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- agent log"; agentlog | tail -15; echo "--- server log"; tail -10 "$T/server.log"; }
exit $fail
