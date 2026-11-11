# shellcheck shell=bash disable=SC2034 # sets variables for the script that sources it
# Sourced by the e2e scripts that deploy to the Control Plane's own machine: it is a Host like any other (Q104), its
# Agent beside the server, enrolled by hand: host add, the invite, the fingerprint approved.
# Needs BIN, T, OMEGACD_SERVER and OMEGACD_TOKEN. enroll NAME starts it and sets AGENT (its pid), AGENT_LOG,
# AGENT_WORK (its work dir) and RCPT (its Host Key's recipient, for SOPS); kill $AGENT in the script's cleanup.
enroll() {
  local name=$1 invite fp
  AGENT_LOG=$T/$name.log AGENT_WORK=$T/$name/run/omegacd
  invite=$("$BIN" host add "$name" | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
  env -i PATH="$PATH" ${GOCOVERDIR:+GOCOVERDIR=$GOCOVERDIR} HOME="$T/$name" XDG_RUNTIME_DIR="$T/$name/run" \
    ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} OMEGACD_LOG_LEVEL=debug OMEGACD_AGENT_CONTROL_PLANE=$OMEGACD_SERVER \
    OMEGACD_AGENT_INVITE="$invite" "$BIN" agent >> "$AGENT_LOG" 2>&1 &
  AGENT=$!
  for _ in $(seq 100); do grep -q "waiting for an admin to approve" "$AGENT_LOG" 2>/dev/null && break; sleep 0.1; done
  fp=$(grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' "$AGENT_LOG" | head -1 | cut -d= -f2)
  "$BIN" host approve "$name" "$fp" >/dev/null
  for _ in $(seq 100); do grep -q "connected to the Control Plane, approved" "$AGENT_LOG" && break; sleep 0.1; done
  RCPT=$("$BIN" host list -o json | python3 -c "import json,sys; print([h for h in json.load(sys.stdin) if h['name']=='$name'][0]['ageRecipient'])")
}
