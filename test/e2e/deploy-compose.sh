#!/usr/bin/env bash
# End-to-end: deploy/compose.yaml and deploy/agent.yaml as install.sh sets them up (M6-02, M6-01, Q269), with the image
# the Dockerfile makes around this checkout's binary: the Control Plane and its standalone Source Server, never root, their mTLS pair
# made once (Q231, Q236); the Agent in a container on host networking, as the omegacd user in the socket's group,
# reaching the Control Plane over loopback (Q238), finding its container without its hostname (Q235), taking back a
# work dir Docker made as root (Q237), refusing one mounted at another path (Q21); and, with the user's rootless
# Podman and rootless Docker (over https, Q252), the rootless variant as container root (Q240). The binary is
# OMEGACD_BIN on Linux; elsewhere a Linux build. install.sh is this checkout's, fetching from it; running it again
# moves to another image and keeps the keys (Q267); its development copy refuses to run.
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
cp_() { docker compose -p $CP -f "$T/cp/compose.yaml" -f "$T/cp-cover.yaml" "$@"; }
ag() { docker compose -p $AG -f "$T/agent/agent.yaml" -f "$T/agent-cover.yaml" "$@"; }
# setup PROJECT DIR FILE OVERRIDES SETTING=VALUE…: install.sh, this checkout's, its compose project and overrides given
setup() {
  local p=$1 d=$2 f=$3 o=$4; shift 4
  env COMPOSE_PROJECT_NAME="$p" COMPOSE_FILE="$d/$f:$o" OMEGACD_VERSION=v0.0.0-e2e OMEGACD_RAW="file://$ROOT/deploy" \
    OMEGACD_IMAGE=$IMAGE "$@" sh "$ROOT/deploy/install.sh" > "$T/install.out" 2>&1 </dev/null
}
agentlog() { docker logs $AG-agent-1 2>&1; }
waitfor() { for _ in $(seq $((${2:-30} * 5))); do eval "$1" && return 0; sleep 0.2; done; return 1; } # CMD [SECONDS]
PODMAN=unix://${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock DOCKERR=unix://${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/docker.sock
cleanup() {
  "$BIN" app delete web --yes >/dev/null 2>&1; "$BIN" app delete webp --yes >/dev/null 2>&1
  for p in web e2e-parity; do docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$p) >/dev/null 2>&1; done
  ag down -v >/dev/null 2>&1; cp_ down -v >/dev/null 2>&1; docker rmi omegacd-e2e:compose2 >/dev/null 2>&1
  "$BIN" app delete webd --yes >/dev/null 2>&1; docker rm -f e2e-tlsfront >/dev/null 2>&1
  for sock in "$PODMAN" "$DOCKERR"; do # the rootless variants: the user's Podman (p) and Docker (d)
    [ -S "${sock#unix://}" ] || continue
    n=p; [ "$sock" = "$DOCKERR" ] && n=d
    DOCKER_HOST=$sock docker rm -f $(DOCKER_HOST=$sock docker ps -aq --filter label=com.docker.compose.project=web$n) >/dev/null 2>&1
    DOCKER_HOST=$sock docker compose -p e2e-agent$n -f "$T/agent$n/agent.yaml" down -v >/dev/null 2>&1
    DOCKER_HOST=$sock docker rmi $IMAGE >/dev/null 2>&1
    rm -rf "${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/omegacd-e2e$n"
  done
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
{
  printf 'services:\n  agent:\n    environment: {OMEGACD_LOG_LEVEL: debug%s}\n' "${GOCOVERDIR:+, GOCOVERDIR: $GOCOVERDIR}"
  [ -z "${GOCOVERDIR:-}" ] || printf '    volumes: ["%s:%s"]\n' "$GOCOVERDIR" "$GOCOVERDIR"
} > "$T/agent-cover.yaml"

out=$(env -u OMEGACD_VERSION OMEGACD_MODE=agent sh "$ROOT/deploy/install.sh" 2>&1 </dev/null); rc=$?
check "install.sh's development copy refuses, pointing at a release's (Q266)" '[ $rc -ne 0 ] && echo "$out" | grep -q "development copy: use the install.sh of a release"'

setup $CP "$T/cp" compose.yaml "$T/cp-cover.yaml" OMEGACD_MODE=full OMEGACD_DIR="$T/cp" OMEGACD_PORT=18080
check "install.sh full brings up deploy/compose.yaml's Control Plane and Source Server, naming the next step (Q233)" \
  'waitfor "curl -sf $API/version >/dev/null" && grep -q "add a Host" "$T/install.out" || { cat "$T/install.out"; false; }'
check "neither runs as root: the image's omegacd user (Q236)" \
  '[ "$(docker exec $CP-omegacd-1 id -u)" = 10001 ] && [ "$(docker exec $CP-source-server-1 id -u)" = 10001 ]'
check "the Control Plane uses the standalone Source Server (Q231)" 'cp_ logs omegacd 2>&1 | grep -q "using the standalone source server.*url=https://source-server:9090"'
check "and made its Master Key itself, in its keys volume (Q115)" 'cp_ logs omegacd 2>&1 | grep -q "created the Master Key.*path=/etc/omegacd/age.key"'
CA=$(docker run --rm -v ${CP}_source-tls:/c busybox:1.38 cat /c/ca.pem | cksum)
MK=$(docker run --rm -v ${CP}_omegacd-keys:/k busybox:1.38 cat /k/age.key | cksum)

curl -sf -c "$T/j" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}" $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')

# the Agent in a container, through install.sh: a .env with the Control Plane, the invite and the socket's group (the
# engine's view of it: OrbStack's differs from macOS's)
INVITE=$("$BIN" host add box | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
GID=$(docker run --rm -v /var/run/docker.sock:/s busybox:1.38 stat -c %g /s)
agent() { # SETTING=VALUE…: install.sh agent, the container on rootful Docker
  setup $AG "$T/agent" agent.yaml "$T/agent-cover.yaml" OMEGACD_MODE=agent OMEGACD_AGENT_INSTALL=container \
    OMEGACD_AGENT_ENGINE=docker OMEGACD_AGENT_DIR="$T/agent" OMEGACD_DOCKER_GID="$GID" OMEGACD_AGENT_WORKDIR="$T/work" "$@"
}
agent OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18080 OMEGACD_AGENT_INVITE="$INVITE"
check "install.sh agent writes the .env and prints the Agent's fingerprint" \
  'grep -qx "OMEGACD_AGENT_INVITE=$INVITE" "$T/agent/.env" && grep -q "if its fingerprint is: SHA256:" "$T/install.out" || { cat "$T/install.out"; false; }'
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
docker tag $IMAGE omegacd-e2e:compose2 # a newer release, as far as install.sh can tell
setup $CP "$T/cp" compose.yaml "$T/cp-cover.yaml" OMEGACD_MODE=full OMEGACD_DIR="$T/cp" OMEGACD_PORT=18080 OMEGACD_IMAGE=omegacd-e2e:compose2
check "install.sh full run again moves to the new image, keeping the Master Key (Q267)" \
  '[ "$(docker inspect -f "{{.Config.Image}}" $CP-omegacd-1)" = omegacd-e2e:compose2 ] && [ "$(docker run --rm -v ${CP}_omegacd-keys:/k busybox:1.38 cat /k/age.key | cksum)" = "$MK" ] || { cat "$T/install.out"; false; }'
check "recreated, the two keep their certificates (Q231)" '[ "$(docker run --rm -v ${CP}_source-tls:/c busybox:1.38 cat /c/ca.pem | cksum)" = "$CA" ]'
# a Sync while the Agent is still reconnecting is queued, and --wait doesn't wait for that; its backoff, not reset by a
# connection shorter than a minute, can be some 40 s by now
waitfor "[ \$(connected) -gt $n ]" 90
out=$("$BIN" app sync web --wait --timeout 3m 2>&1); rc=$?
check "and still trust each other: a Sync renders through the Source Server" '[ $rc -eq 0 ] || { echo "$out" >&2; false; }'

agent OMEGACD_IMAGE=omegacd-e2e:compose2 # run again without the settings: the new image, the .env's used invite kept
check "install.sh agent run again moves to the new image; with its used invite still in .env, it reconnects by its key" \
  'waitfor "[ \$(connected) -gt 0 ]" 90 && [ "$(docker inspect -f "{{.Config.Image}}" $AG-agent-1)" = omegacd-e2e:compose2 ] && grep -qx "OMEGACD_AGENT_INVITE=$INVITE" "$T/agent/.env" || { cat "$T/install.out"; false; }'

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

# Q240: the rootless variant, as container root, with the user's own rootless Podman and rootless Docker
RT=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
# rootless ENGINE N SOCKET URL INSTALL-ENGINE [OVERRIDE]: install.sh's Agent as container root there, approved, deploying
rootless() {
  local engine=$1 n=$2 sock=$3 url=$4 ie=$5 extra=${6:-}
  docker save $IMAGE | DOCKER_HOST=$sock docker load >/dev/null 2>&1
  local invite; invite=$("$BIN" host add box$n | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
  agr() { DOCKER_HOST=$sock docker compose -p e2e-agent$n -f "$T/agent$n/agent.yaml" -f "$T/agent-cover.yaml" ${extra:+-f "$extra"} "$@"; }
  rlog() { agr logs agent 2>&1; }
  setup e2e-agent$n "$T/agent$n" agent.yaml "$T/agent-cover.yaml${extra:+:$extra}" OMEGACD_MODE=agent OMEGACD_AGENT_INSTALL=container \
    OMEGACD_AGENT_ENGINE=$ie OMEGACD_AGENT_DIR="$T/agent$n" OMEGACD_AGENT_WORKDIR="$RT/omegacd-e2e$n" \
    OMEGACD_AGENT_CONTROL_PLANE="$url" OMEGACD_AGENT_INVITE="$invite"
  check "$engine: install.sh writes the rootless values, container root and the user's socket (Q240, Q265)" \
    'grep -qx OMEGACD_AGENT_USER=0:0 "$T/agent$n/.env" && grep -qx "OMEGACD_DOCKER_SOCK=${sock#unix://}" "$T/agent$n/.env" || { cat "$T/install.out"; false; }'
  check "$engine: the Agent runs as container root, the user on the Host (Q240)" \
    'waitfor "rlog | grep -q \"running as root in a rootless engine.s container\"" && waitfor "rlog | grep -q \"waiting for an admin to approve\"" 90 || { rlog | tail -8 >&2; false; }'
  "$BIN" host approve box$n "$(rlog | grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' | head -1 | cut -d= -f2)" >/dev/null
  waitfor "rlog | grep -q \"connected to the Control Plane, approved\"" 90 # else the Sync is queued, and --wait returns
  "$BIN" app create web$n --repo http://git.localhost/$U/deploy.git --path web --host box$n >/dev/null
  out=$("$BIN" app sync web$n --wait --timeout 3m 2>&1); rc=$?
  check "$engine: and deploys through the user's engine" '[ $rc -eq 0 ] && [ -f "$RT/omegacd-e2e$n/web$n/conf.txt" ] || { echo "$out" >&2; rlog | tail -5 >&2; false; }'
}
if [ "$(uname)" = Linux ] && { [ -S "${PODMAN#unix://}" ] || [ -n "${OMEGACD_TEST_ALL:-}" ]; }; then
  rootless "rootless Podman" p "$PODMAN" http://127.0.0.1:18080 podman
fi
# Q252: rootless Docker's host network has a loopback of its own, so its Agent takes the Control Plane's https:// URL,
# here a TLS front on this server's own address with a private CA
if [ "$(uname)" = Linux ] && { [ -S "${DOCKERR#unix://}" ] || [ -n "${OMEGACD_TEST_ALL:-}" ]; }; then
  IP=$(hostname -I | awk '{print $1}')
  mkdir -p "$T/tls" && (cd "$T/tls" &&
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=e2e-ca -keyout ca.key -out ca.pem &&
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=e2e -keyout front.key -out front.csr &&
    printf 'subjectAltName=IP:%s\n' "$IP" > san.cnf &&
    openssl x509 -req -in front.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 1 -extfile san.cnf -out front.crt &&
    cat front.crt front.key > front.pem && chmod 0644 front.pem ca.pem) >/dev/null 2>&1
  docker run -d --name e2e-tlsfront --network host -v "$T/tls:/tls:ro" alpine/socat:1.8.0.3 \
    "openssl-listen:18443,bind=$IP,fork,reuseaddr,cert=/tls/front.pem,verify=0" tcp:127.0.0.1:18080 >/dev/null 2>&1
  waitfor "curl -sf --cacert $T/tls/ca.pem https://$IP:18443/api/version >/dev/null" # listening, before the Agent dials
  printf 'services:\n  agent:\n    environment: {OMEGACD_AGENT_CA_FILE: /etc/omegacd-ca.pem}\n    volumes: ["%s:/etc/omegacd-ca.pem:ro"]\n' "$T/tls/ca.pem" > "$T/agentd-ca.yaml"
  out=$(env OMEGACD_VERSION=v0.0.0-e2e OMEGACD_MODE=agent OMEGACD_AGENT_INSTALL=container OMEGACD_AGENT_ENGINE=rootless-docker \
    OMEGACD_AGENT_DIR="$T/agentx" OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18080 sh "$ROOT/deploy/install.sh" 2>&1 </dev/null); rc=$?
  check "rootless Docker: install.sh refuses a Control Plane on loopback for the container, naming the user unit (Q252)" \
    '[ $rc -ne 0 ] && echo "$out" | grep -q "can.t reach http://127.0.0.1:18080.*OMEGACD_AGENT_INSTALL=systemd" && [ ! -e "$T/agentx" ]'
  rootless "rootless Docker" d "$DOCKERR" "https://$IP:18443" rootless-docker "$T/agentd-ca.yaml"
fi

check "no password in the logs (Q62)" '! { cp_ logs 2>&1; agentlog; } | grep -qF "$PW"'

[ $fail -eq 0 ] && echo "ALL PASSED" || {
  echo "--- agent log"; agentlog | tail -15; echo "--- control plane log"; cp_ logs omegacd 2>&1 | tail -10
  for n in p d; do [ ! -f "$T/agent$n/.env" ] || { echo "--- rootless agent $n"; [ $n = p ] && sock=$PODMAN || sock=$DOCKERR
    DOCKER_HOST=$sock docker compose -p e2e-agent$n -f "$T/agent$n/agent.yaml" logs agent 2>&1 | tail -15; }; done
  docker logs e2e-tlsfront 2>&1 | tail -5
}
exit $fail
