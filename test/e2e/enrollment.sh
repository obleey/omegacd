#!/usr/bin/env bash
# End-to-end: an Agent enrolls with the Control Plane through the real binary (M3-02): Host added by name, single-use
# invite, the Agent's key registered, approved only with the fingerprint both sides show (Q22, Q28, Q73, Q74); then
# its key rotated, a copy of it locking the Host, unlocked, and revoked by deleting the Host (M3-03, Q75, Q76); its
# version recorded (M3-04); an allowlist (M3-05).
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
# waitfor FILE TEXT [SECONDS]: until FILE says TEXT (default 10 s)
waitfor() { for _ in $(seq $((${3:-10} * 10))); do grep -qF -- "$2" "$1" 2>/dev/null && return 0; sleep 0.1; done; return 1; }

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
# M3-04: the Agent's Hello is recorded: its version (checked against the Control Plane's; one minor behind and
# refusals are in the hub's tests, which can make up versions) and its engine
json=$("$BIN" host list -o json)
check "the Agent's version and engine are recorded" 'echo "$json" | python3 -c "
import json,sys; a=[h for h in json.load(sys.stdin) if h[\"name\"]==\"web\"][0][\"agent\"]
assert a[\"version\"] and a[\"engine\"] in (\"docker\",\"podman\") and a[\"engineVersion\"] and a[\"os\"] and a[\"arch\"] and not a.get(\"refused\"), a"'
check "host list shows the Agent's version" '"$BIN" host list | grep -q "^web .*agent [v(]"'

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

# M3-03: rotation, locking, revocation
KEY=$T/web/.local/share/omegacd-agent/signing.key
out=$("$BIN" host rotate web 2>&1); rc=$?
NEWFP=$(echo "$out" | grep -o 'SHA256:[A-Za-z0-9+/]*')
check "host rotate: the Agent makes a new key, its fingerprint printed" '[ $rc -eq 0 ] && [ -n "$NEWFP" ] && [ "$NEWFP" != "$FP" ]'
check "the Agent switched to it, on the same connection" 'waitfor "$T/web.log" "signing key rotated" && [ "$(grep -c "connected to the Control Plane" "$T/web.log")" = 1 ]'
check "the key file is the new key, owner-only" 'grep -qF "# fingerprint: $NEWFP" "$KEY" && [ "$(ls -l "$KEY" | cut -c1-10)" = -rw------- ]'
check "the Control Plane shows the new fingerprint, still approved" '"$BIN" host list | grep -qF "approved $NEWFP"'
kill $PID_web; wait $PID_web 2>/dev/null; : > "$T/web.log"
agent web
check "restarted, it connects with the new key" 'waitfor "$T/web.log" "connected to the Control Plane, approved" && grep -qF "fingerprint=$NEWFP" "$T/web.log"'

cp -Rp "$T/web" "$T/clone" # a copied state dir: someone has the key
agent clone
check "a second live connection with the same key locks the Host" 'waitfor "$T/clone.log" "Host web is locked: two connections used its key at once"'
check "the first connection is cut with the reason too" 'waitfor "$T/web.log" "Host web is locked"'
check "host list shows it locked, with both addresses" '"$BIN" host list | grep -q "^web .*LOCKED: two connections (127.0.0.1, 127.0.0.1)"'
kill $PID_clone; wait $PID_clone 2>/dev/null; : > "$T/web.log"
check "host unlock: the Agent connects again" '"$BIN" host unlock web | grep -q "unlocked" && waitfor "$T/web.log" "connected to the Control Plane, approved" 70'

# M3-05: an allowlist per Host (the proxy case, X-Forwarded-For from OMEGACD_TRUSTED_PROXIES, is in the API's tests)
: > "$T/web.log"
out=$("$BIN" host allow web 10.9.0.0/16 2>&1)
check "host allow: an allowlist without the Agent's address cuts its live connection" \
  'echo "$out" | grep -q "only from 10.9.0.0/16" && waitfor "$T/web.log" "may connect only from 10.9.0.0/16, not from 127.0.0.1"'
check "host list shows the allowlist" '"$BIN" host list | grep -q "^web .*only from 10.9.0.0/16"'
: > "$T/web.log"
check "with its address in the list, the Agent connects again" \
  '"$BIN" host allow web 10.9.0.0/16 127.0.0.1 >/dev/null && waitfor "$T/web.log" "connected to the Control Plane, approved" 70'
check "an invalid entry is refused" '! "$BIN" host allow web 10.9.0.0/99 >/dev/null 2>&1'
check "no list: anywhere" '"$BIN" host allow web | grep -q "from anywhere" && ! "$BIN" host list | grep -q "^web .*only from"'

check "host delete revokes the key: the live connection closes" '"$BIN" host delete web | grep -q "key is revoked" && waitfor "$T/web.log" "no Host has the key" 70'

check "no invite in any log (Q62)" '! grep -qF "$INVITE" "$T/server.log" "$T/web.log" "$T/thief.log" "$T/stranger.log" "$T/clone.log"'
check "no signing key in any log (Q62)" '! grep -qF "$(tail -1 "$KEY")" "$T/server.log" "$T/web.log" "$T/clone.log"'
check "no password in the log (Q62)" '! grep -qF e2e-password-123 "$T/server.log"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { for l in server web thief stranger clone; do echo "--- $l log"; tail -15 "$T/$l.log"; done; }
exit $fail
