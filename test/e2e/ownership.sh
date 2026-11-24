#!/usr/bin/env bash
# End-to-end: two OmegaCDs on one Docker (M7-09, Q335–Q341). Each has its own Agent there and an Application named
# shared. The first deploys it with data in a volume; the second's Sync is refused, naming the first as its owner, and
# its delete with volumes leaves the first's container and data as they were. The first's Agent, pointed at the second
# OmegaCD with an invite of its own, refuses it before enrolling (the invite stays unused, the refusal shown on the
# Host) and connects to its own OmegaCD again.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
. "$(dirname "$0")/admin.sh"
T=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eow.XXXX")" && pwd -P)
REPO=$T/repo APP=e2e-shared
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
SRVS="" AGENTS=""
serve() { # serve PORT DATA: an OmegaCD
  age-keygen -o "$T/$2.key" 2>/dev/null
  OMEGACD_ADDR=127.0.0.1:$1 OMEGACD_DATA=$T/$2 OMEGACD_AGE_KEY_FILE=$T/$2.key OMEGACD_POLL_INTERVAL=1h \
    "$BIN" > "$T/$2.log" 2>&1 &
  SRVS="$SRVS $!"
  for _ in $(seq 50); do curl -sf "http://127.0.0.1:$1/api/version" >/dev/null && break; sleep 0.2; done
}
as() { export OMEGACD_SERVER=$1 OMEGACD_TOKEN=$2; } # the CLI talks to that OmegaCD
cleanup() {
  as "$URL_A" "$TOKEN_A"; "$BIN" app delete $APP --volumes --yes >/dev/null 2>&1
  kill $SRVS $AGENTS 2>/dev/null; wait $SRVS $AGENTS 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT
serve 18080 a
serve 18081 b
admin_login http://127.0.0.1:18080/api "$T/a"; URL_A=$OMEGACD_SERVER TOKEN_A=$OMEGACD_TOKEN PWS="$ADMIN_FIRST_PW $ADMIN_PW"
admin_login http://127.0.0.1:18081/api "$T/b"; URL_B=$OMEGACD_SERVER TOKEN_B=$OMEGACD_TOKEN PWS="$PWS $ADMIN_FIRST_PW $ADMIN_PW"
. "$(dirname "$0")/enroll.sh"

mkdir -p "$REPO/app"
cat > "$REPO/app/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "[ -f /data/owner ] || echo first > /data/owner; sleep 3600"]
    volumes: [data:/data]
volumes:
  data: {}
EOF
gitc init -q -b main && gitc add -A && gitc commit -qm app
ctr() { docker ps -q --filter label=com.docker.compose.project=$APP; }
data() { docker run --rm -v ${APP}_data:/data busybox:1.36 cat /data/owner 2>/dev/null; }

# the first OmegaCD deploys it, with data in its volume
as "$URL_A" "$TOKEN_A"; enroll a-box; AGENT_A=$AGENT; AGENTS="$AGENTS $AGENT"
"$BIN" app create $APP --repo "$REPO" --path app --host a-box >/dev/null
out=$("$BIN" app sync $APP --wait --timeout 3m 2>&1); rc=$?
check "the first OmegaCD deploys its app, which writes its data" '[ $rc -eq 0 ] && sleep 1 && [ "$(data)" = first ] || { echo "$out" >&2; false; }'
FIRST=$(ctr)
OWNER=$(docker inspect -f '{{index .Config.Labels "dev.omegacd.owner"}}' "$FIRST")
check "its container and volume carry its Control Plane as owner (Q335, Q341)" \
  '[ ${#OWNER} -eq 32 ] && [ "$(docker volume inspect -f "{{index .Labels \"dev.omegacd.owner\"}}" ${APP}_data)" = "$OWNER" ]'

# the second OmegaCD, its own Agent on the same Docker, an Application of the same name
as "$URL_B" "$TOKEN_B"; enroll b-box; AGENTS="$AGENTS $AGENT"
"$BIN" app create $APP --repo "$REPO" --path app --host b-box >/dev/null
out=$("$BIN" app sync $APP --wait --timeout 3m 2>&1); rc=$?
check "the second OmegaCD's Sync is refused, naming the owner (Q336)" \
  '[ $rc -ne 0 ] && echo "$out" | grep -q "belongs to another OmegaCD ($OWNER): rename the Application, or set adopt: true" || { echo "$out" >&2; false; }'
out=$("$BIN" app delete $APP --volumes --yes 2>&1); rc=$?
check "its delete with volumes is done, removing nothing of the first's" \
  '[ $rc -eq 0 ] && sleep 1 && [ "$(ctr)" = "$FIRST" ] && [ "$(data)" = first ] || { echo "$out" >&2; false; }'

# the first OmegaCD's Agent, pointed at the second with an invite of its own (Q340)
INVITE=$("$BIN" host add moved | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
kill $AGENT_A; wait $AGENT_A 2>/dev/null
agent() { # agent URL [INVITE]: the first's Agent, its state as enroll left it
  env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/a-box" XDG_RUNTIME_DIR="$T/a-box/run" \
    ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$1 \
    OMEGACD_AGENT_INVITE="${2:-}" "$BIN" agent >> "$T/a-box.log" 2>&1 &
  AGENT_A=$!; AGENTS="$AGENTS $AGENT_A"
}
agent "$URL_B" "$INVITE"
for _ in $(seq 100); do grep -q "refused a Control Plane other than the one that approved this Agent" "$T/a-box.log" && break; sleep 0.1; done
check "the Agent refuses another OmegaCD before enrolling, saying so" \
  'grep -q "refused a Control Plane other than the one that approved this Agent" "$T/a-box.log"'
check "which shows it on the Host, its invite unused" \
  '"$BIN" host list | grep "^moved " | grep -q "invited, REFUSED: its Agent belongs to another OmegaCD" &&
   "$BIN" host list -o json | python3 -c "import json,sys; h=[h for h in json.load(sys.stdin) if h[\"name\"]==\"moved\"][0]; sys.exit(0 if not h.get(\"fingerprint\") and \"$OWNER\" in h[\"refused\"] else 1)"'
kill $AGENT_A; wait $AGENT_A 2>/dev/null
n=$(grep -c "connected to the Control Plane, approved" "$T/a-box.log")
agent "$URL_A"
for _ in $(seq 100); do [ "$(grep -c "connected to the Control Plane, approved" "$T/a-box.log")" -gt "$n" ] && break; sleep 0.1; done
check "and connects to its own OmegaCD again, its app running on" \
  '[ "$(grep -c "connected to the Control Plane, approved" "$T/a-box.log")" -gt "$n" ] && [ "$(ctr)" = "$FIRST" ]'
nopw() { for p in $PWS; do ! grep -qF "$p" "$T"/*.log || return 1; done; }
check "no password in any log (Q62)" nopw

[ $fail -eq 0 ] && echo "ALL PASSED" || for f in "$T"/*.log; do echo "--- $f"; tail -12 "$f"; done
exit $fail
