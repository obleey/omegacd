#!/usr/bin/env bash
# End-to-end: an Agent enrolls with the Control Plane through the real binary (M3-02): Host added by name, single-use
# invite, the Agent's key registered, approved only with the fingerprint both sides show (Q22, Q28, Q73, Q74).
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2en.XXXX")
API=http://127.0.0.1:18080/api
mkdir -p "$T/data" "$T/work"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
PIDS=()
trap 'kill ${PIDS[@]+"${PIDS[@]}"} 2>/dev/null; wait 2>/dev/null; rm -rf "$T"' EXIT

age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')

# agent NAME [VAR=value]...: starts an Agent with its own state dir, connected to the Control Plane; log in $T/NAME.log
agent() {
  local name=$1
  shift
  env -i PATH="$PATH" HOME="$T/$name" XDG_RUNTIME_DIR="$T/$name/run" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} \
    OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER "$@" "$BIN" agent >> "$T/$name.log" 2>&1 &
  PIDS+=($!)
  eval "PID_$name=$!"
}
# waitfor FILE TEXT: until FILE says TEXT (10 s)
waitfor() { for _ in $(seq 100); do grep -qF -- "$2" "$1" 2>/dev/null && return 0; sleep 0.1; done; return 1; }

out=$("$BIN" host add web 2>&1)
INVITE=$(echo "$out" | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
check "host add prints the command that enrolls the Host, with its invite" \
  'echo "$out" | grep -qF "OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER OMEGACD_AGENT_INVITE=$INVITE omegacd agent" && [ ${#INVITE} -gt 40 ]'
check "the Control Plane keeps only the invite's hash" '! grep -rqaF "$INVITE" "$T/data"'
check "the Host is listed as invited" '"$BIN" host list | grep -q "^web .*invited"'

agent web OMEGACD_AGENT_INVITE="$INVITE"
check "the Agent enrolls and waits for approval" 'waitfor "$T/web.log" "waiting for an admin to approve this Agent"'
FP=$(grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' "$T/web.log" | head -1 | cut -d= -f2)
check "the Agent prints its key's fingerprint at start" '[ -n "$FP" ] && grep "msg=\"omegacd agent\"" "$T/web.log" | grep -qF "fingerprint=$FP"'
check "the fingerprint the Control Plane shows is the Agent's" '"$BIN" host list | grep -qF "to approve $FP"'
check "and so is the Host Key the Agent registered" \
  '"$BIN" host list | grep -q "^web .*$(grep -o "hostKey=age1[a-z0-9]*" "$T/web.log" | head -1 | cut -d= -f2)"'
check "its signing key is owner-only in its state dir" '[ "$(ls -l "$T/web/.local/share/omegacd-agent/signing.key" | cut -c1-10)" = -rw------- ]'

out=$("$BIN" host approve web SHA256:not-the-agents-key 2>&1); rc=$?
check "approving another fingerprint is refused, naming the Agent's" '[ $rc -ne 0 ] && echo "$out" | grep -qF "$FP"'
check "and the Agent still waits" '! grep -q "approved" "$T/web.log"'
check "the Agent's own fingerprint approves it" '"$BIN" host approve web "$FP" | grep -q "Host web approved"'
check "the connected Agent hears it at once" 'waitfor "$T/web.log" "connected to the Control Plane, approved"'
check "and is listed approved" '"$BIN" host list | grep -qF "approved $FP"'

kill $PID_web; wait $PID_web 2>/dev/null; : > "$T/web.log"
agent web # its invite is used; it needs none any more
check "restarted without the invite, it connects approved" 'waitfor "$T/web.log" "connected to the Control Plane, approved"'

agent thief OMEGACD_AGENT_INVITE="$INVITE"
check "a used invite is refused" 'waitfor "$T/thief.log" "the invite is unknown, used or expired"'
agent stranger
check "an Agent with no invite and an unknown key is refused" 'waitfor "$T/stranger.log" "no Host has the key"'
check "the refused Agents enrolled nowhere" '[ "$("$BIN" host list | grep -c "SHA256:")" = 1 ]'

out=$("$BIN" host add web 2>&1); rc=$?
check "an enrolled Host gets no new invite" '[ $rc -ne 0 ] && echo "$out" | grep -q "enrolled for Host web already"'
out=$("$BIN" host add local 2>&1); rc=$?
check "the built-in Host can't be invited" '[ $rc -ne 0 ] && echo "$out" | grep -q "built-in Host"'

check "no invite in any log (Q62)" '! grep -qF "$INVITE" "$T/server.log" "$T/web.log" "$T/thief.log" "$T/stranger.log"'
check "no password in the log (Q62)" '! grep -qF e2e-password-123 "$T/server.log"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { for l in server web thief stranger; do echo "--- $l log"; tail -15 "$T/$l.log"; done; }
exit $fail
