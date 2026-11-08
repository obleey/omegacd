#!/usr/bin/env bash
# End-to-end: omegacd agent, through the real binary (M2-01 to M2-07; M2 tickets extend it as the Agent learns to deploy).
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ea.XXXX")
trap 'rm -rf "$T"' EXIT
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
# agent runs the Agent with the given settings until it says it runs (then stops it: exit 0) or exits on its own (its
# exit code); its log is the output
agent() {
  env -i PATH="$PATH" HOME="$T/home" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} ${OMEGACD_LOG_LEVEL:+OMEGACD_LOG_LEVEL=$OMEGACD_LOG_LEVEL} \
    "$@" "$BIN" agent > "$T/agent.log" 2>&1 &
  local pid=$! rc
  for _ in $(seq 100); do
    grep -q "omegacd agent running" "$T/agent.log" && { kill $pid; wait $pid; cat "$T/agent.log"; return 0; }
    kill -0 $pid 2>/dev/null || break
    sleep 0.1
  done
  wait $pid; rc=$?
  cat "$T/agent.log"
  return $rc
}

help=$("$BIN" agent --help)
for v in OMEGACD_AGENT_CONTROL_PLANE OMEGACD_AGENT_INVITE OMEGACD_AGENT_STATE_DIR OMEGACD_AGENT_WORKDIR DOCKER_HOST \
  OMEGACD_AGENT_CACHE_REVISIONS OMEGACD_AGENT_SECRET_GROUP OMEGACD_AGENT_ALLOW_ROOT OMEGACD_LOG_LEVEL OMEGACD_LOG_FORMAT \
  OMEGACD_AGENT_WORKDIR_ALLOW; do
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
check "the container engine is detected and reported (M2-06)" 'echo "$out" | grep -q "msg=\"container engine\" engine=docker version=[0-9]"'
if [ "$(uname)" = Darwin ]; then
  check "on macOS it warns: development only (Q61)" 'echo "$out" | grep -q "macOS is for development only"'
else
  check "on Linux no macOS warning" '! echo "$out" | grep -q "macOS is for development only"'
fi
# the warning follows the filesystem: /tmp is a tmpfs on some Linux systems, disk on others and on macOS
case $(stat -f -c %T "$T/run/omegacd" 2>/dev/null || echo disk) in
tmpfs | ramfs) check "a work dir in RAM isn't warned about (M2-07)" '! echo "$out" | grep -q "a work dir is not in RAM"' ;;
*) check "a work dir on disk is warned about: decrypted secrets would stay there (M2-07)" 'echo "$out" | grep -q "a work dir is not in RAM"' ;;
esac
out=$(env -i PATH="$PATH" HOME="$T/home" XDG_RUNTIME_DIR="$T/run" "$BIN" agent restore --files 2>&1); rc=$?
check "omegacd agent restore --files runs without an engine and exits" '[ $rc -eq 0 ]'
out=$(env -i PATH="$PATH" HOME="$T/home" "$BIN" agent restore 2>&1); rc=$?
check "an unknown agent command is refused with its usage" '[ $rc -ne 0 ] && echo "$out" | grep -q "usage: omegacd agent"'
# per-Application work dirs (M2-09): the roots the Agent allows besides its own; deploying into them is in the
# Agent's integration tests on every engine (the binary has no Control Plane to deploy from until M4-01)
out=$(agent XDG_RUNTIME_DIR="$T/run" OMEGACD_AGENT_WORKDIR_ALLOW=srv/apps); rc=$?
check "a relative OMEGACD_AGENT_WORKDIR_ALLOW is refused, named" '[ $rc -ne 0 ] && echo "$out" | grep -q "OMEGACD_AGENT_WORKDIR_ALLOW"'
mkdir -p "$T/disk-root"
out=$(agent XDG_RUNTIME_DIR="$T/run" OMEGACD_AGENT_WORKDIR_ALLOW="$T/disk-root"); rc=$?
case $(stat -f -c %T "$T/disk-root" 2>/dev/null || echo disk) in
tmpfs | ramfs) check "an allowed root in RAM starts without a warning" '[ $rc -eq 0 ] && ! echo "$out" | grep -q "workdir=[^ ]*/disk-root"' ;;
*) check "an allowed root on disk is warned about, like the work dir" '[ $rc -eq 0 ] && echo "$out" | grep -q "a work dir is not in RAM.*workdir=[^ ]*/disk-root"' ;;
esac
# offline rollback (M2-08): what the binary reaches without a Control Plane to deploy from (that comes with M4-01); the
# rollback itself, beside a running Agent with Self-Heal, is in the Agent's integration tests on every engine
ra() { env -i PATH="$PATH" HOME="$T/home" XDG_RUNTIME_DIR="$T/run" ${DOCKER_HOST:+DOCKER_HOST=$DOCKER_HOST} "$BIN" agent rollback "$@" 2>&1; }
out=$(ra); rc=$?
check "omegacd agent rollback lists the cached Revisions: none yet" '[ $rc -eq 0 ] && echo "$out" | grep -q "no Applications deployed on this Host"'
out=$(ra web); rc=$?
check "and refuses an Application it doesn't have, naming it" '[ $rc -ne 0 ] && echo "$out" | grep -q "no Application web on this Host"'
out=$(ra web abc123); rc=$?
check "also when rolling back" '[ $rc -ne 0 ] && echo "$out" | grep -q "no Application web on this Host"'
out=$(ra web abc123 extra); rc=$?
check "too many arguments: its usage" '[ $rc -ne 0 ] && echo "$out" | grep -q "rollback \[APP \[REVISION\]\]"'
check "--help documents rollback" 'echo "$help" | grep -q "rollback APP REVISION"'
out=$(agent XDG_RUNTIME_DIR="$T/run"); rc=$?
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
out=$(incontainer -v "$T/host-work:$T/host-work" -e OMEGACD_AGENT_WORKDIR="$T/host-work" -e OMEGACD_AGENT_WORKDIR_ALLOW=/srv/apps); rc=$?
check "an allowed root needs path parity too (M2-09)" '[ $rc -ne 0 ] && echo "$out" | grep -q "the work dir /srv/apps is not mounted from the host"'
cid=$(docker run -d -v /var/run/docker.sock:/var/run/docker.sock -v "$T/omegacd-linux:/omegacd:ro" -v "$T/host-work:$T/host-work" \
  -e OMEGACD_AGENT_ALLOW_ROOT=true -e OMEGACD_AGENT_STATE_DIR=/state -e OMEGACD_AGENT_WORKDIR="$T/host-work" alpine:3 /omegacd agent)
for _ in $(seq 100); do docker logs "$cid" 2>&1 | grep -q "omegacd agent running" && break; sleep 0.1; done
out=$(docker logs "$cid" 2>&1); docker rm -f "$cid" >/dev/null
check "the same path on both sides starts" 'echo "$out" | grep -q "omegacd agent running"'

# logging (M2-10)
out=$(agent XDG_RUNTIME_DIR="$T/run" OMEGACD_LOG_LEVEL=debug OMEGACD_LOG_FORMAT=json); rc=$?
check "OMEGACD_LOG_FORMAT=json: every line is one JSON object with a level" '[ $rc -eq 0 ] &&
  echo "$out" | python3 -c "import json,sys; e=[json.loads(l) for l in sys.stdin]; sys.exit(0 if e and all(\"level\" in x for x in e) else 1)"'
check "at debug its log shows the Host Key's public half, never the private one (Q62)" 'echo "$out" | grep -q "age1" && ! echo "$out" | grep -q AGE-SECRET-KEY'
out=$(agent XDG_RUNTIME_DIR="$T/run" OMEGACD_LOG_LEVEL=loud); rc=$?
check "an unknown OMEGACD_LOG_LEVEL is refused and named" '[ $rc -ne 0 ] && echo "$out" | grep -q "OMEGACD_LOG_LEVEL=loud"'
out=$(agent XDG_RUNTIME_DIR="$T/run" OMEGACD_LOG_FORMAT=xml); rc=$?
check "and so is an unknown OMEGACD_LOG_FORMAT" '[ $rc -ne 0 ] && echo "$out" | grep -q "OMEGACD_LOG_FORMAT=xml"'

[ $fail -eq 0 ] && echo "ALL PASSED"
exit $fail
