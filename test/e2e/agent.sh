#!/usr/bin/env bash
# End-to-end: omegacd agent, through the real binary (M2-01 to M2-03; M2 tickets extend it as the Agent learns to deploy).
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ea.XXXX")
trap 'rm -rf "$T"' EXIT
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
agent() { env -i PATH="$PATH" HOME="$T/home" "$@" "$BIN" agent 2>&1; }

help=$("$BIN" agent --help)
for v in OMEGACD_AGENT_CONTROL_PLANE OMEGACD_AGENT_INVITE OMEGACD_AGENT_STATE_DIR OMEGACD_AGENT_WORKDIR DOCKER_HOST \
  OMEGACD_AGENT_CACHE_REVISIONS OMEGACD_AGENT_ALLOW_ROOT; do
  check "--help documents $v" 'echo "$help" | grep -q "^  $v$"'
done

out=$(agent XDG_RUNTIME_DIR="$T/run"); rc=$?
check "starts as a user, with the user defaults" '[ $rc -eq 0 ] && echo "$out" | grep -q "state=.*/home/.local/share/omegacd-agent "'
check "state dir created owner-only" '[ "$(ls -ld "$T/home/.local/share/omegacd-agent" | cut -c1-10)" = drwx------ ]'
check "work dir in the user's runtime dir" '[ -d "$T/run/omegacd" ]'
check "its database is created in the state dir (M2-02)" '[ -s "$T/home/.local/share/omegacd-agent/agent.db" ]'
check "and its Revision cache (M2-03)" '[ -d "$T/home/.local/share/omegacd-agent/cache/tmp" ]'
out=$(agent XDG_RUNTIME_DIR="$T/run"); rc=$?
check "and opened again on the next start" '[ $rc -eq 0 ] && echo "$out" | grep -q "apps=0"'

out=$(agent STATE_DIRECTORY="$T/var-lib" OMEGACD_AGENT_WORKDIR="$T/services"); rc=$?
check "as a systemd service: its StateDirectory" '[ $rc -eq 0 ] && [ -d "$T/var-lib" ] && [ -d "$T/services" ]'

out=$(agent OMEGACD_AGENT_CACHE_REVISIONS=none XDG_RUNTIME_DIR="$T/run"); rc=$?
check "a bad setting fails and names it" '[ $rc -ne 0 ] && echo "$out" | grep -q OMEGACD_AGENT_CACHE_REVISIONS'
out=$(agent OMEGACD_AGENT_CONTROL_PLANE=cp.example.com XDG_RUNTIME_DIR="$T/run"); rc=$?
check "a Control Plane URL without scheme fails" '[ $rc -ne 0 ] && echo "$out" | grep -q "https://"'

[ $fail -eq 0 ] && echo "ALL PASSED"
exit $fail
