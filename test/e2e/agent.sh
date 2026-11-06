#!/usr/bin/env bash
# End-to-end: omegacd agent, through the real binary (M2-01 to M2-05; M2 tickets extend it as the Agent learns to deploy).
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ea.XXXX")
trap 'rm -rf "$T"' EXIT
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
agent() { env -i PATH="$PATH" HOME="$T/home" "$@" "$BIN" agent 2>&1; }

help=$("$BIN" agent --help)
for v in OMEGACD_AGENT_CONTROL_PLANE OMEGACD_AGENT_INVITE OMEGACD_AGENT_STATE_DIR OMEGACD_AGENT_WORKDIR DOCKER_HOST \
  OMEGACD_AGENT_CACHE_REVISIONS OMEGACD_AGENT_SECRET_GROUP OMEGACD_AGENT_ALLOW_ROOT; do
  check "--help documents $v" 'echo "$help" | grep -q "^  $v$"'
done

out=$(agent XDG_RUNTIME_DIR="$T/run"); rc=$?
key1=$(echo "$out" | grep -o "hostKey=age1[a-z0-9]*") # from the first start, compared with the second below
check "starts as a user, with the user defaults" '[ $rc -eq 0 ] && echo "$out" | grep -q "state=.*/home/.local/share/omegacd-agent "'
check "state dir created owner-only" '[ "$(ls -ld "$T/home/.local/share/omegacd-agent" | cut -c1-10)" = drwx------ ]'
check "work dir in the user's runtime dir" '[ -d "$T/run/omegacd" ]'
check "its database is created in the state dir (M2-02)" '[ -s "$T/home/.local/share/omegacd-agent/agent.db" ]'
check "and its Revision cache (M2-03)" '[ -d "$T/home/.local/share/omegacd-agent/cache/tmp" ]'
out=$(agent XDG_RUNTIME_DIR="$T/run"); rc=$?
check "and opened again on the next start" '[ $rc -eq 0 ] && echo "$out" | grep -q "apps=0"'
check "its Host Key is created owner-only, and kept (M2-05)" '[ -n "$key1" ] && echo "$out" | grep -q "$key1" && [ "$(ls -l "$T/home/.local/share/omegacd-agent/host.key" | cut -c1-10)" = -rw------- ]'
out=$(agent OMEGACD_AGENT_SECRET_GROUP=no-such-group-xyz XDG_RUNTIME_DIR="$T/run"); rc=$?
check "an unknown OMEGACD_AGENT_SECRET_GROUP fails and names it" '[ $rc -ne 0 ] && echo "$out" | grep -q OMEGACD_AGENT_SECRET_GROUP'

out=$(agent STATE_DIRECTORY="$T/var-lib" OMEGACD_AGENT_WORKDIR="$T/services"); rc=$?
check "as a systemd service: its StateDirectory" '[ $rc -eq 0 ] && [ -d "$T/var-lib" ] && [ -d "$T/services" ]'

out=$(agent OMEGACD_AGENT_CACHE_REVISIONS=none XDG_RUNTIME_DIR="$T/run"); rc=$?
check "a bad setting fails and names it" '[ $rc -ne 0 ] && echo "$out" | grep -q OMEGACD_AGENT_CACHE_REVISIONS'
out=$(agent OMEGACD_AGENT_CONTROL_PLANE=cp.example.com XDG_RUNTIME_DIR="$T/run"); rc=$?
check "a Control Plane URL without scheme fails" '[ $rc -ne 0 ] && echo "$out" | grep -q "https://"'

# In a container the work dir must be the same path on the host (M2-04): Docker resolves bind mounts there
arch=$(docker version --format '{{.Server.Arch}}')
(cd "$(dirname "$0")/../.." && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$T/omegacd-linux" ./cmd/omegacd)
mkdir -p "$T/host-work"
incontainer() {
  docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$T/omegacd-linux:/omegacd:ro" \
    -e OMEGACD_AGENT_ALLOW_ROOT=true -e OMEGACD_AGENT_STATE_DIR=/state -e OMEGACD_AGENT_WORKDIR=/srv/work "$@" alpine:3 /omegacd agent 2>&1
}
out=$(incontainer -v "$T/host-work:/srv/work"); rc=$?
check "in a container, a work dir at another host path is refused" '[ $rc -ne 0 ] && echo "$out" | grep -q "paths must match"'
out=$(incontainer); rc=$?
check "and one not mounted from the host" '[ $rc -ne 0 ] && echo "$out" | grep -q "not mounted from the host"'
out=$(docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$T/omegacd-linux:/omegacd:ro" -v "$T/host-work:$T/host-work" \
  -e OMEGACD_AGENT_ALLOW_ROOT=true -e OMEGACD_AGENT_STATE_DIR=/state -e OMEGACD_AGENT_WORKDIR="$T/host-work" alpine:3 /omegacd agent 2>&1); rc=$?
check "the same path on both sides starts" '[ $rc -eq 0 ] && echo "$out" | grep -q "omegacd agent"'

[ $fail -eq 0 ] && echo "ALL PASSED"
exit $fail
