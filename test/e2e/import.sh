#!/usr/bin/env bash
# End-to-end: omegacd import through the Host's Agent (M6-05, Q317–Q326). On each engine there is (Docker; on Linux also
# rootless Podman and rootless Docker) an Agent and two stacks started by hand: one reading an env file with a secret and
# a config file from a folder beside its own (issue #9), one whose folder is inside a volume. import refuses to encrypt
# for the Host alone, then imports the first (its files copied in without the ../, the secret encrypted on the Host for
# the Host Key and a person's key) and refuses the second, naming the volume; the App of Apps takes the first over.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
. "$(dirname "$0")/admin.sh"
T=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eim.XXXX")" && pwd -P) # the resolved path: Compose records it so
chmod 755 "$T"                                                            # a rootless engine's helper reads the stacks
API=http://127.0.0.1:18080/api
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$T/repo" -c user.name=e2e -c user.email=e2e@local "$@"; }
age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key OMEGACD_POLL_INTERVAL=1h \
  "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
AGENTS="" ENGINES=""
cleanup() {
  for e in $ENGINES; do "$BIN" app delete "imp${e%%=*}" --volumes --yes >/dev/null 2>&1; done
  kill $SRV $AGENTS 2>/dev/null; wait $SRV $AGENTS 2>/dev/null # before what's left goes: their Self-Heal
  for e in $ENGINES; do
    sock=${e#*=} n=${e%%=*}
    DOCKER_HOST=$sock docker compose -p imp$n down -v >/dev/null 2>&1
    DOCKER_HOST=$sock docker rm -f imp-store$n-web >/dev/null 2>&1
    DOCKER_HOST=$sock docker volume rm imp-store$n >/dev/null 2>&1
  done
  [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
admin_login "$API" "$T/data"
. "$(dirname "$0")/enroll.sh"
mkdir -p "$T/repo" && gitc init -q -b main && gitc remote add origin "$T/repo" && gitc commit -q --allow-empty -m start

# a person's key, on the key list after the first import is refused
export XDG_CONFIG_HOME=$T/me
"$BIN" keygen >/dev/null 2>&1
KEYS=$T/me/sops/age/keys.txt ME=$(grep -o 'age1[a-z0-9]*' "$KEYS" | head -1)
SECRET=$(openssl rand -hex 12)

# imports ENGINE N SOCKET: the engine's Agent box$N and its stacks imp$N and imp-store$N
imports() {
  local engine=$1 n=$2 sock=$3 out rc mp
  ENGINES="$ENGINES $n=$sock"
  export DOCKER_HOST=$sock
  enroll box$n
  AGENTS="$AGENTS $AGENT"
  local S=$T/stacks$n
  mkdir -p "$S/imp$n/data" "$S/shared"
  cat > "$S/imp$n/compose.yaml" <<'EOF'
services:
  web:
    image: nginx:1.29-alpine
    env_file: ../shared/app.env
    volumes:
      - ../shared/site.conf:/etc/nginx/conf.d/default.conf:ro
      - ./data:/data
    restart: unless-stopped
EOF
  printf 'API_TOKEN=%s\n' "$SECRET" > "$S/shared/app.env"
  printf 'server { listen 80; location / { return 200 "imported\\n"; } }\n' > "$S/shared/site.conf"
  chmod -R a+rX "$S"
  (cd "$S/imp$n" && docker compose -p imp$n up -d --quiet-pull >/dev/null 2>&1)
  # a stack whose folder is inside a volume, as another deploy tool's checkout is (issue #9): what Compose labels
  docker volume create imp-store$n >/dev/null
  mp=$(docker volume inspect -f '{{.Mountpoint}}' imp-store$n)
  docker run --rm -v imp-store$n:/v busybox:1.36 sh -c \
    'mkdir -p /v/app && printf "services:\n  web:\n    image: busybox:1.36\n    volumes: [./data:/data]\n" > /v/app/compose.yaml'
  docker run -d --name imp-store$n-web --label com.docker.compose.project=imp-store$n \
    --label com.docker.compose.project.working_dir="$mp/app" --label com.docker.compose.project.config_files="$mp/app/compose.yaml" \
    busybox:1.36 sleep 3600 >/dev/null

  if [ -z "$n" ]; then # before anyone's key is on the list
    out=$(cd "$T/repo" && "$BIN" import --host box$n imp$n 2>&1); rc=$?
    check "$engine: a stack with secrets is refused when only the Host could open them (Q201, Q320)" \
      '[ $rc -ne 0 ] && echo "$out" | grep -q "or pass --host-only" && [ ! -e "$T/repo/apps/imp$n" ] || { echo "$out" >&2; false; }'
    "$BIN" secrets key add me "$ME" >/dev/null # for all Hosts
  fi
  out=$(cd "$T/repo" && "$BIN" import --host box$n --dry-run 2>&1); rc=$?
  check "$engine: --dry-run reports the stack, writing nothing" \
    'echo "$out" | grep -q "✓ imp$n would go to apps/imp$n" && [ ! -e "$T/repo/apps/imp$n" ] || { echo "$out" >&2; false; }'
  check "$engine: a stack whose data is inside a volume is refused, naming it (Q325)" \
    '[ $rc -ne 0 ] && echo "$out" | grep -q "✗ imp-store$n: data is $mp/app/data, inside the volume imp-store$n" || { echo "$out" >&2; false; }'
  out=$(cd "$T/repo" && "$BIN" import --host box$n imp$n 2>&1); rc=$?
  local A=$T/repo/apps/imp$n
  check "$engine: import writes the stack through its Agent, the files beside it copied in without the ../ (Q323)" \
    '[ $rc -eq 0 ] && grep -q "env_file: ./shared/app.env" "$A/compose.yaml" && grep -q "./shared/site.conf:/etc/nginx/conf.d/default.conf:ro" "$A/compose.yaml" &&
     grep -q "$S/imp$n/data:/data" "$A/compose.yaml" && cmp -s "$A/shared/site.conf" "$S/shared/site.conf" || { echo "$out" >&2; false; }'
  check "$engine: the secret encrypted on the Host, the person's key opening it (Q230, Q194)" \
    '! grep -q "$SECRET" "$A/shared/app.sops.env" && [ ! -e "$A/shared/app.env" ] &&
     SOPS_AGE_KEY_FILE=$KEYS sops --decrypt "$A/shared/app.sops.env" | grep -q "API_TOKEN=$SECRET"'
  gitc add -A && gitc commit -qm "import imp$n"
}

imports Docker "" "${DOCKER_HOST:-}"
if [ "$(uname)" = Linux ]; then
  RT=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
  if [ -S "$RT/podman/podman.sock" ] || [ -n "${OMEGACD_TEST_ALL:-}" ]; then imports "rootless Podman" p "unix://$RT/podman/podman.sock"; fi
  if [ -S "$RT/docker.sock" ] || [ -n "${OMEGACD_TEST_ALL:-}" ]; then imports "rootless Docker" d "unix://$RT/docker.sock"; fi
fi

# the takeover, as the import's last lines say: an App of Apps over platform/, each Application's Diff, then its Sync
"$BIN" app create platform --repo "$T/repo" --path platform --kind apps >/dev/null
"$BIN" app sync platform --wait --timeout 3m >/dev/null 2>&1
for e in $ENGINES; do
  n=${e%%=*} sock=${e#*=}
  out=$("$BIN" app sync imp$n --wait --timeout 3m 2>&1); rc=$?
  check "imp$n: taken over by the App of Apps' Application, still serving its config" \
    '[ $rc -eq 0 ] && [ "$(DOCKER_HOST=$sock docker exec $(DOCKER_HOST=$sock docker ps -q --filter label=dev.omegacd.application=imp$n) wget -qO- http://127.0.0.1/ 2>&1)" = imported ] || { echo "$out" >&2; false; }'
  check "imp$n: with the secret, decrypted by its Agent" \
    'DOCKER_HOST=$sock docker exec $(DOCKER_HOST=$sock docker ps -q --filter label=dev.omegacd.application=imp$n) printenv API_TOKEN | grep -qx "$SECRET"'
done
check "no secret in the Control Plane's or the Agents' logs (Q62)" \
  '! grep -qF -e "$SECRET" -e "$ADMIN_FIRST_PW" -e "$ADMIN_PW" "$T/server.log" "$T"/box*.log'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -15 "$T/server.log"; for f in "$T"/box*.log; do echo "--- $f"; tail -15 "$f"; done; }
exit $fail
