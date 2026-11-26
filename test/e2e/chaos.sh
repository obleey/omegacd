#!/usr/bin/env bash
# End-to-end: no data loss when things die (M7-03, Q345–Q349). An Agent deploys two apps through a standalone Source
# Server: web, whose migration takes a few seconds and whose data lives in a volume, and still, which never changes and
# must never be recreated. The Control Plane is killed (SIGKILL) mid-Sync, and closes that Sync from the Agent's report
# once it is back; the Source Server is killed, a new Revision refused until it is back; the Agent is killed mid-deploy,
# its Sync Running until it is back and then Unknown, and restarted it keeps its apps with their data; the Source
# Server's repo cache is overwritten with garbage, and the next Sync works: a fetch brings what the cache lost, or the
# repair of Q348 clones it again (which one depends on how go-git reads the damage; the unit tests cover the repair).
# After each, web keeps its data and still its container.
# (The Agent Host rebooted while offline is the gate's real reboot; the restore and the rebuild are recovery.sh.)
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
. "$(dirname "$0")/admin.sh"
T=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ech.XXXX")" && pwd -P)
REPO=$T/repo API=http://127.0.0.1:18080/api
mkdir -p "$REPO/web" "$REPO/still" "$T/data" "$T/src-data"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
waitfor() { local end=$((SECONDS + ${2:-60})); while [ $SECONDS -lt $end ]; do eval "$1" && return 0; sleep 0.3; done; return 1; }
SRC= SRV= AGENT=
cleanup() {
  for a in chaos-web chaos-still; do "$BIN" app delete $a --volumes --yes >/dev/null 2>&1; done
  kill -9 $SRV $SRC $AGENT 2>/dev/null; wait 2>/dev/null # first: a running Agent's Self-Heal would bring them back
  for p in chaos-web chaos-still; do
    docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=$p) >/dev/null 2>&1
    docker volume rm ${p}_data >/dev/null 2>&1; docker network rm ${p}_default >/dev/null 2>&1
  done
  [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT

"$BIN" source-certs --out "$T/tls" 2>/dev/null
start_source() {
  OMEGACD_SOURCE_ADDR=127.0.0.1:19090 OMEGACD_DATA=$T/src-data OMEGACD_SOURCE_TLS_DIR=$T/tls \
    "$BIN" source-server >> "$T/source.log" 2>&1 &
  SRC=$!
  for _ in $(seq 50); do curl -s --cacert "$T/tls/ca.pem" https://127.0.0.1:19090/ -o /dev/null; [ $? -ne 7 ] && break; sleep 0.2; done
}
start_control_plane() {
  OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key OMEGACD_POLL_INTERVAL=1h \
    OMEGACD_SOURCE_SERVER=https://127.0.0.1:19090 OMEGACD_SOURCE_TLS_DIR=$T/tls "$BIN" >> "$T/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
}
start_source
start_control_plane
admin_login "$API" "$T/data"
. "$(dirname "$0")/enroll.sh"; enroll edge
start_agent() { # the Agent again, its state as it is
  env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/edge" XDG_RUNTIME_DIR="$T/edge/run" \
    ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER "$BIN" agent >> "$AGENT_LOG" 2>&1 &
  AGENT=$!
}

cat > "$REPO/web/compose.yaml" <<'EOF'
services:
  migrate:
    image: busybox:1.36
    command: ["sh", "-c", "sleep 6"]
  main:
    image: busybox:1.36
    command: ["sh", "-c", "[ -f /data/first ] || cat /conf.txt > /data/first; cat /conf.txt; sleep 3600"]
    volumes: ["./conf.txt:/conf.txt:ro", "data:/data"]
    depends_on: {migrate: {condition: service_completed_successfully}}
volumes:
  data: {}
EOF
cat > "$REPO/still/compose.yaml" <<'EOF'
services:
  main:
    image: busybox:1.36
    command: ["sh", "-c", "date > /data/started; sleep 3600"]
    volumes: ["data:/data"]
volumes:
  data: {}
EOF
echo "web v1" > "$REPO/web/conf.txt"
gitc init -q -b main && gitc add -A && gitc commit -qm v1
for a in web still; do "$BIN" app create chaos-$a --repo "$REPO" --path $a --host edge >/dev/null; done
"$BIN" app sync chaos-still --wait --timeout 3m >/dev/null 2>&1
out=$("$BIN" app sync chaos-web --wait --timeout 3m 2>&1); rc=$?
ctr() { docker ps -q --no-trunc --filter label=com.docker.compose.project=chaos-$1 --filter label=com.docker.compose.service=main; }
data() { docker run --rm -v chaos-web_data:/data busybox:1.36 cat /data/first 2>/dev/null; }
serves() { docker logs "$(ctr web)" 2>&1 | tail -1 | grep -qx "$1"; }
newest() { "$BIN" app history chaos-web -o json | python3 -c "import json,sys; o=json.load(sys.stdin)[0]; print(o['id'], o['phase'])"; }
deploying() { grep "msg=deploy " "$AGENT_LOG" | grep -q "op=$1 app=chaos-web"; }
check "both apps deploy, web with its data written once" '[ $rc -eq 0 ] && waitfor "serves \"web v1\"" && [ "$(data)" = "web v1" ] || { echo "$out" >&2; false; }'
STILL=$(ctr still)
kept() { [ "$(data)" = "web v1" ] && [ "$(ctr still)" = "$STILL" ]; } # web's data, still's container
next() { # next VERSION: a new Revision of web, committed and seen
  echo "web $1" > "$REPO/web/conf.txt"; gitc commit -qam "$1"; "$BIN" app refresh chaos-web >/dev/null 2>&1
}

# --- the Control Plane killed mid-Sync (Q346)
next v2
read -r BEFORE _ <<< "$(newest)"
"$BIN" app sync chaos-web >/dev/null 2>&1
waitfor 'read -r OP _ <<< "$(newest)"; [ "$OP" -gt "$BEFORE" ]'
check "a Sync is deploying" '[ "$OP" -gt "$BEFORE" ] && waitfor "deploying $OP"'
kill -9 $SRV; wait $SRV 2>/dev/null
check "the Control Plane killed mid-Sync, the Agent finishes it" 'waitfor "serves \"web v2\""'
start_control_plane
check "restarted, it closes that Sync from the Agent's report: Succeeded (Q346)" 'waitfor "[ \"\$(newest)\" = \"$OP Succeeded\" ]"'
check "web keeps its data, still its container" 'kept && "$BIN" app get chaos-web | grep -q "Synced"'

# --- the Source Server killed (Q347)
kill -9 $SRC; wait $SRC 2>/dev/null
next v3
out=$("$BIN" app sync chaos-web --wait --timeout 1m 2>&1); rc=$?
check "with the Source Server killed, a new Revision is refused, saying why" '[ $rc -ne 0 ] && echo "$out" | grep -qi "source server" || { echo "$out" >&2; false; }'
check "the apps run on untouched" 'serves "web v2" && kept'
start_source
# found again within seconds: the Control Plane retries a restarted Source Server at most 5 s apart (Q124)
waitfor 'out=$("$BIN" app sync chaos-web --wait --timeout 3m 2>&1); rc=$?; ! echo "$out" | grep -q "Source Server unreachable"' 30
check "back, the next Sync works" '[ $rc -eq 0 ] && waitfor "serves \"web v3\"" && kept || { echo "$out" >&2; false; }'

# --- the Agent killed mid-deploy (Q347)
next v4
read -r BEFORE _ <<< "$(newest)"
"$BIN" app sync chaos-web >/dev/null 2>&1
waitfor 'read -r OP _ <<< "$(newest)"; [ "$OP" -gt "$BEFORE" ]'
check "a new Sync is deploying" '[ "$OP" -gt "$BEFORE" ] && waitfor "deploying $OP"'
kill -9 $AGENT; wait $AGENT 2>/dev/null
op() { "$BIN" app history chaos-web -o json | python3 -c "import json,sys; o=[o for o in json.load(sys.stdin) if o['id']==$1][0]; print(o['phase'], o['message'])"; }
sleep 2
check "the Agent killed mid-deploy: its Sync waits Running, as the Agent may yet report it" '[ "$(op $OP | cut -d" " -f1)" = Running ]'
start_agent
check "back, the Agent neither runs it nor reported it: Unknown, saying so" \
  'waitfor "op $OP | grep -q \"^Unknown result unknown: .*neither runs it nor reported how it ended.*its Agent was stopped during it\""'
check "restarted, it keeps its apps running with their data" 'waitfor "[ -n \"\$(ctr web)\" ]" && kept'
out=$("$BIN" app sync chaos-web --wait --timeout 3m 2>&1); rc=$?
check "and a new Sync succeeds" '[ $rc -eq 0 ] && waitfor "serves \"web v4\"" && kept || { echo "$out" >&2; false; }'

# --- the Source Server's repo cache overwritten with garbage while it runs (Q348)
for p in $(find "$T/src-data/repos" -name '*.pack'); do # go-git writes them read-only
  chmod u+w "$p" && head -c "$(wc -c < "$p")" /dev/zero | tr '\0' '\253' > "$p"
done
check "its repo cache is overwritten" '[ -n "$(find "$T/src-data/repos" -name "*.pack")" ] && ! grep -lq PACK $(find "$T/src-data/repos" -name "*.pack")'
next v5
out=$("$BIN" app sync chaos-web --wait --timeout 3m 2>&1); rc=$?
check "with its repo cache overwritten, the next Sync works (fetched or cloned again, Q348)" \
  '[ $rc -eq 0 ] && waitfor "serves \"web v5\"" && kept || { echo "$out" >&2; tail -5 "$T/source.log" >&2; false; }'

check "no password in any log (Q62)" '! grep -qF -e "$ADMIN_FIRST_PW" -e "$ADMIN_PW" "$T"/*.log'
[ $fail -eq 0 ] && echo "ALL PASSED" || for f in "$T"/*.log; do echo "--- $f"; tail -12 "$f"; done
exit $fail
