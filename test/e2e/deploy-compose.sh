#!/usr/bin/env bash
# End-to-end: deploy/compose.yaml and deploy/agent.yaml as an operator runs them (M6-02), with the image the Dockerfile
# makes around this checkout's binary: the Control Plane and its standalone Source Server, never root, their mTLS pair
# made once (Q231, Q236); the Agent in a container on host networking, as the omegacd user in the socket's group,
# reaching the Control Plane over loopback (Q238), finding its container without its hostname (Q235), taking back a
# work dir Docker made as root (Q237), refusing one mounted at another path (Q21); and, with the user's rootless
# Podman, the rootless variant as container root (Q240). The binary is OMEGACD_BIN on Linux; elsewhere a Linux build.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BIN=${OMEGACD_BIN:-$ROOT/bin/omegacd}
D=${TMPDIR:-/tmp} T=$(mktemp -d "${D%/}/omegacd-e2ed.XXXX") # no //: the Agent's messages have the path cleaned
REPO=$T/repo IMAGE=omegacd-e2e:compose CP=e2e-cp AG=e2e-agent API=http://127.0.0.1:18080/api
FJ=${OMEGACD_E2E_FORGEJO:-forgejo-forgejo-1} U=omegacd-e2ed # the Git server, as webhook.sh's: the image has no git
docker inspect "$FJ" >/dev/null 2>&1 || { echo "SKIP  no Forgejo container $FJ (set OMEGACD_E2E_FORGEJO)"; exit 0; }
mkdir -p "$REPO/web" "$T/bc"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
cp_() { docker compose -p $CP -f "$ROOT/deploy/compose.yaml" -f "$T/cp-cover.yaml" "$@"; }
ag() { docker compose -p $AG -f "$ROOT/deploy/agent.yaml" -f "$T/agent-cover.yaml" --env-file "$T/agent.env" "$@"; }
agentlog() { docker logs $AG-agent-1 2>&1; }
waitfor() { for _ in $(seq $((${2:-30} * 5))); do eval "$1" && return 0; sleep 0.2; done; return 1; } # CMD [SECONDS]
PODMAN=unix://${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock
cleanup() {
  "$BIN" app delete web --yes >/dev/null 2>&1; "$BIN" app delete webp --yes >/dev/null 2>&1
  for p in web e2e-parity; do docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$p) >/dev/null 2>&1; done
  ag down -v >/dev/null 2>&1; cp_ down -v >/dev/null 2>&1
  if [ -S "${PODMAN#unix://}" ]; then
    DOCKER_HOST=$PODMAN docker rm -f $(DOCKER_HOST=$PODMAN docker ps -aq --filter label=com.docker.compose.project=webp) >/dev/null 2>&1
    DOCKER_HOST=$PODMAN docker compose -p e2e-agentp -f "$ROOT/deploy/agent.yaml" -f "$T/agent-cover.yaml" --env-file "$T/agentp.env" down -v >/dev/null 2>&1
    DOCKER_HOST=$PODMAN docker rmi $IMAGE >/dev/null 2>&1
    rm -rf "${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/omegacd-e2e"
  fi
  docker rmi $IMAGE >/dev/null 2>&1
  docker exec -u git "$FJ" forgejo admin user delete --username $U --purge >/dev/null 2>&1
  docker run --rm -v "$T:$T" busybox:1.38 rm -rf "$T/work" >/dev/null 2>&1 # Docker made it as root
  rm -rf "$T"
}
trap cleanup EXIT

# the image: the Dockerfile's last stage around this checkout's Linux binary, built with -cover when the suite measures
LBIN=$BIN
if [ "$(uname)" != Linux ]; then
  LBIN=$T/omegacd-linux
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$(docker version -f '{{.Server.Arch}}') go build -o "$LBIN" ./cmd/omegacd) ||
    { echo "FAIL  build a Linux binary"; exit 1; }
fi
cp "$LBIN" "$T/bc/omegacd"
docker pull -q busybox:1.38 >/dev/null
check "the image builds from the Dockerfile" 'docker buildx build -q --load --build-context go="$T/bc" -t $IMAGE "$ROOT" >/dev/null 2>&1'

PW=$(python3 -c 'import secrets; print(secrets.token_urlsafe(12))')
# coverage where the e2e binary writes it, by the containers' users too
cover() {
  [ -n "${GOCOVERDIR:-}" ] || return 0
  chmod a+rwx "$GOCOVERDIR"
  printf '    environment: {GOCOVERDIR: %s}\n    volumes: ["%s:%s"]\n' "$GOCOVERDIR" "$GOCOVERDIR" "$GOCOVERDIR"
}
{
  echo "services:"
  echo "  source-certs:"; cover
  echo "  source-server:"; cover; echo '    extra_hosts: ["git.localhost:host-gateway"]'
  printf '  omegacd:\n    environment: {OMEGACD_ADMIN_PASSWORD: "%s", OMEGACD_LOG_LEVEL: debug, OMEGACD_POLL_INTERVAL: 1h%s}\n' \
    "$PW" "${GOCOVERDIR:+, GOCOVERDIR: $GOCOVERDIR}"
  [ -z "${GOCOVERDIR:-}" ] || printf '    volumes: ["%s:%s"]\n' "$GOCOVERDIR" "$GOCOVERDIR"
} > "$T/cp-cover.yaml"
{ echo "services:"; echo "  agent:"; cover; } > "$T/agent-cover.yaml"
[ -n "${GOCOVERDIR:-}" ] || echo "services: {}" > "$T/agent-cover.yaml"

export OMEGACD_IMAGE=$IMAGE OMEGACD_PORT=18080
cp_ up -d --wait >/dev/null 2>&1
check "deploy/compose.yaml brings up the Control Plane and the Source Server" 'waitfor "curl -sf $API/version >/dev/null"'
check "neither runs as root: the image's omegacd user (Q236)" \
  '[ "$(docker exec $CP-omegacd-1 id -u)" = 10001 ] && [ "$(docker exec $CP-source-server-1 id -u)" = 10001 ]'
check "the Control Plane uses the standalone Source Server (Q231)" 'cp_ logs omegacd 2>&1 | grep -q "using the standalone source server.*url=https://source-server:9090"'
check "and made its Master Key itself, in its keys volume (Q115)" 'cp_ logs omegacd 2>&1 | grep -q "created the Master Key.*path=/etc/omegacd/age.key"'
CA=$(docker run --rm -v ${CP}_source-tls:/c busybox:1.38 cat /c/ca.pem | cksum)

curl -sf -c "$T/j" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}" $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')

# the Agent as the file's comment says: a .env with the Control Plane, the invite and the socket's group
INVITE=$("$BIN" host add box | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
GID=$(docker run --rm -v /var/run/docker.sock:/s busybox:1.38 stat -c %g /s)
cat > "$T/agent.env" <<EOF
OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18080
OMEGACD_AGENT_INVITE=$INVITE
OMEGACD_DOCKER_GID=$GID
OMEGACD_AGENT_WORKDIR=$T/work
OMEGACD_LOG_LEVEL=debug
EOF
ag up -d >/dev/null 2>&1
check "deploy/agent.yaml starts the Agent, on host networking, and it enrolls over loopback (Q238)" 'waitfor "agentlog | grep -q \"waiting for an admin to approve\""'
FP=$(agentlog | grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' | head -1 | cut -d= -f2)
"$BIN" host approve box "$FP" >/dev/null
check "and is approved" 'waitfor "agentlog | grep -q \"connected to the Control Plane, approved\""'
check "as the omegacd user, in the socket's group (Q236)" \
  '[ "$(docker exec $AG-agent-1 id -u)" = 10001 ] && docker exec $AG-agent-1 id -G | tr " " "\n" | grep -qx "$GID"'
check "it found its own container though its hostname is the Host's (Q235)" \
  '[ "$(docker inspect -f "{{.Config.Hostname}}" $AG-agent-1)" = "$(docker run --rm --network host busybox:1.38 hostname)" ] && ! agentlog | grep -q "can.t inspect this container"'

printf 'services:\n  web:\n    image: busybox:1.38\n    command: ["sh", "-c", "cat /conf.txt; sleep 3600"]\n    volumes: ["./conf.txt:/conf.txt:ro"]\n' > "$REPO/web/compose.yaml"
echo "conf from git" > "$REPO/web/conf.txt"
fj() { docker exec -u git "$FJ" forgejo "$@"; }
fj admin user delete --username $U --purge >/dev/null 2>&1
fj admin user create --username $U --password "$(openssl rand -hex 12)" --email e2ed@example.com --must-change-password=false >/dev/null 2>&1
FTOK=$(fj admin user generate-access-token --username $U --token-name e2e-$RANDOM --scopes write:repository,write:user --raw 2>/dev/null | tail -1)
curl -sf -H "Authorization: token $FTOK" -H 'Content-Type: application/json' -d '{"name":"deploy","default_branch":"main"}' \
  http://git.localhost/api/v1/user/repos >/dev/null
gitc init -q -b main && gitc add -A && gitc commit -qm init && gitc push -q "http://$U:$FTOK@git.localhost/$U/deploy.git" main
check "the test repository is on Forgejo" '[ -n "$FTOK" ] && git ls-remote "http://git.localhost/$U/deploy.git" main | grep -q main'
"$BIN" app create web --repo http://git.localhost/$U/deploy.git --path web --host box >/dev/null
out=$("$BIN" app sync web --wait --timeout 3m 2>&1); rc=$?
shown() { docker logs "$(docker ps -q --filter label=com.docker.compose.project=web)" 2>&1 | head -1; }
check "it deploys an Application with a bind-mounted file from Git" '[ $rc -eq 0 ] && waitfor "[ \"\$(shown)\" = \"conf from git\" ]"'

connected() { agentlog | grep -c "connected to the Control Plane, approved"; }
n=$(connected)
cp_ up -d --force-recreate --wait >/dev/null 2>&1
check "recreated, the two keep their certificates (Q231)" '[ "$(docker run --rm -v ${CP}_source-tls:/c busybox:1.38 cat /c/ca.pem | cksum)" = "$CA" ]'
# a Sync while the Agent is still reconnecting is queued, and --wait doesn't wait for that; its backoff, not reset by a
# connection shorter than a minute, can be some 40 s by now
waitfor "[ \$(connected) -gt $n ]" 90
out=$("$BIN" app sync web --wait --timeout 3m 2>&1); rc=$?
check "and still trust each other: a Sync renders through the Source Server" '[ $rc -eq 0 ] || { echo "$out" >&2; false; }'

n=$(connected)
ag restart >/dev/null 2>&1
check "restarted with its used invite still in .env, it reconnects by its key" \
  'waitfor "[ \$(connected) -gt $n ]" 90'

# Q237: after a reboot its RAM work dir is gone and Docker makes the bind mount's source again, owned by root
ag stop >/dev/null 2>&1
docker run --rm -v "$T:$T" busybox:1.38 rm -rf "$T/work"
ag start >/dev/null 2>&1
# as root through Docker: the work dir is the omegacd user's now, 0750, closed to this test's user
back() { docker run --rm -v "$T:$T" busybox:1.38 test -f "$T/work/web/conf.txt"; }
check "a work dir Docker made as root is given back to the Agent's user, and the app's files restored" 'waitfor back'
if [ "$(uname)" = Linux ]; then
  check "its owner the omegacd user, through Docker" '[ "$(stat -c %u "$T/work")" = 10001 ] && agentlog | grep -q "the work dir isn.t the Agent.s user.s"'
  # the helper runs the Agent's own image: one untagged since (pruned, say) can't, and the Agent says so
  docker run --rm -v "$T:$T" busybox:1.38 mkdir "$T/rootwork"
  docker tag $IMAGE omegacd-e2e:untagged
  id=$(docker create --network host --user omegacd --group-add "$GID" -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$T/rootwork:$T/rootwork" -e OMEGACD_AGENT_WORKDIR="$T/rootwork" -e OMEGACD_AGENT_STATE_DIR=/tmp/state \
    ${GOCOVERDIR:+-e GOCOVERDIR=$GOCOVERDIR -v $GOCOVERDIR:$GOCOVERDIR} omegacd-e2e:untagged agent)
  docker rmi omegacd-e2e:untagged >/dev/null
  out=$(docker start -a "$id" 2>&1); docker rm -f "$id" >/dev/null
  check "a work dir it can't give back is an error, the cause said" 'echo "$out" | grep -q "work dir $T/rootwork: own $T/rootwork: .*omegacd-e2e:untagged"'
fi

run() { docker run --rm --user omegacd --group-add "$GID" ${GOCOVERDIR:+-e GOCOVERDIR=$GOCOVERDIR -v $GOCOVERDIR:$GOCOVERDIR} \
  -e OMEGACD_AGENT_STATE_DIR=/tmp/state "$@" 2>&1; }
mkdir -p "$T/elsewhere"
out=$(run --network host -l com.docker.compose.project=e2e-parity -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$T/elsewhere:$T/parity" -e OMEGACD_AGENT_WORKDIR="$T/parity" $IMAGE agent)
check "a work dir mounted from another path is refused, the fix said (Q21)" 'echo "$out" | grep -q "the work dir $T/parity is $T/elsewhere on the host.*same path on both sides"'
out=$(run -e OMEGACD_AGENT_WORKDIR=/tmp/work $IMAGE agent)
check "without the engine's socket it says it can't check its work dir" 'echo "$out" | grep -q "can.t inspect this container"'
out=$(docker run --rm --user 0:0 -v /var/run/docker.sock:/var/run/docker.sock ${GOCOVERDIR:+-e GOCOVERDIR=$GOCOVERDIR -v $GOCOVERDIR:$GOCOVERDIR} $IMAGE agent 2>&1)
check "root in a rootful engine's container is refused (Q240)" 'echo "$out" | grep -q "refusing to run as root"'
out=$(docker run --rm --user 0:0 -e OMEGACD_LOG_LEVEL=debug ${GOCOVERDIR:+-e GOCOVERDIR=$GOCOVERDIR -v $GOCOVERDIR:$GOCOVERDIR} $IMAGE agent 2>&1)
check "and where it can't ask the engine whether it's rootless, logged" 'echo "$out" | grep -q "engine unreachable: not taken for rootless" && echo "$out" | grep -q "refusing to run as root"'

# Q240: the rootless variant, as container root, with the user's own rootless Podman
if [ "$(uname)" = Linux ] && { [ -S "${PODMAN#unix://}" ] || [ -n "${OMEGACD_TEST_ALL:-}" ]; }; then
  RT=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
  docker save $IMAGE | DOCKER_HOST=$PODMAN docker load >/dev/null 2>&1
  INVITE=$("$BIN" host add boxp | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
  cat > "$T/agentp.env" <<EOF
OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18080
OMEGACD_AGENT_INVITE=$INVITE
OMEGACD_AGENT_USER=0:0
OMEGACD_DOCKER_GID=0
OMEGACD_DOCKER_SOCK=${PODMAN#unix://}
OMEGACD_AGENT_WORKDIR=$RT/omegacd-e2e
OMEGACD_LOG_LEVEL=debug
EOF
  agp() { DOCKER_HOST=$PODMAN docker compose -p e2e-agentp -f "$ROOT/deploy/agent.yaml" -f "$T/agent-cover.yaml" --env-file "$T/agentp.env" "$@"; }
  plog() { agp logs agent 2>&1; }
  agp up -d >/dev/null 2>&1
  check "rootless Podman: the Agent runs as container root, the user on the Host (Q240)" \
    'waitfor "plog | grep -q \"running as root in a rootless engine.s container\"" && waitfor "plog | grep -q \"waiting for an admin to approve\""'
  FP=$(plog | grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' | head -1 | cut -d= -f2)
  "$BIN" host approve boxp "$FP" >/dev/null
  waitfor "plog | grep -q \"connected to the Control Plane, approved\"" # else the Sync is queued, and --wait returns
  "$BIN" app create webp --repo http://git.localhost/$U/deploy.git --path web --host boxp >/dev/null
  out=$("$BIN" app sync webp --wait --timeout 3m 2>&1); rc=$?
  check "and deploys through the user's Podman" '[ $rc -eq 0 ] && [ -f "$RT/omegacd-e2e/webp/conf.txt" ] || { echo "$out" >&2; false; }'
fi

check "no password in the logs (Q62)" '! { cp_ logs 2>&1; agentlog; } | grep -qF "$PW"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- agent log"; agentlog | tail -15; echo "--- control plane log"; cp_ logs omegacd 2>&1 | tail -10; }
exit $fail
