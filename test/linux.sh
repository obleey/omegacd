#!/usr/bin/env bash
# Runs the whole test suite (make test-all) on the Linux Hosts OmegaCD supports (Q61): Ubuntu 24.04 and Debian 13,
# amd64 and arm64, each a Hetzner server with the same tests and engines; the gate before every push.
#   test/linux.sh [--only "CMD"] [TARGET...]   (default: all at once; each one's log in $TMPDIR)
# The servers come from ../omegacd-testinfra (OMEGACD_TESTINFRA): OpenTofu creates them, Ansible sets them up (Docker,
# Compose, Podman, Go, sops, the test user omegacd); `make up`, `make ip` (after this Mac's address changed) and
# `make down` there. This script copies the repository to each one (rsync, ~/omegacd) and runs the suite there.
# --only runs CMD (in the repository) instead of the whole suite: to rerun the part that failed, on the target it
# failed on, while fixing. Before a push, the whole suite on every target.
# A target that fails is stopped (and cleaned) at its first failure; the others go on. Each run cleans up before and
# after itself: everything but Forgejo and the image cache.
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
infra=${OMEGACD_TESTINFRA:-$(dirname "$repo")/omegacd-testinfra}
all="ubuntu2404-amd64 ubuntu2404-arm64 debian13-amd64 debian13-arm64"
engines="rootful rootless podman host-podman"

# on TARGET COMMAND...: runs a command on the target's server as the test user; asroot as root. Each word is quoted for
# the remote shell, so a command arrives as written
on() { local t=$1; shift; ssh -F "$infra/ssh_config" -o ServerAliveInterval=30 "omegacd-test-$t" "$(printf '%q ' "$@")"; }
asroot() { local t=$1; shift; ssh -F "$infra/ssh_config" -o ServerAliveInterval=30 -l root "omegacd-test-$t" "$(printf '%q ' "$@")"; }

# stop ends any test run on the target, one aborted or left over, and clears what tests leave in the engines: every
# Compose project but Forgejo's, the remote-Host tests' docker:dind containers, the rootless engines (recreated fresh by
# engines.sh), unused networks and volumes, the tests' temp dirs, the containers of the user's own Podman (before every
# run, and after it, passed or failed). Kept: Forgejo and the image cache. The pkill patterns are bracketed so they don't match this command itself.
stop() {
  asroot "$1" bash -c '
    pkill -x make; pkill -f "[t]est/e2e/run.sh"; pkill -f "^/usr/local/go/bin/[g]o (test|vet|build)"
    pkill -f "[.]test -test[.]" # the compiled test binaries go test runs: they outlive go test itself
    pkill -f "bin/[o]megacd-(arm64|amd64)"; sleep 1
    for id in $(docker ps -aq --filter label=com.docker.compose.project); do
      [ "$(docker inspect -f "{{index .Config.Labels \"com.docker.compose.project\"}}" "$id")" = forgejo ] || docker rm -f "$id"
    done
    docker ps -aq --filter ancestor=docker:29-dind | xargs -r docker rm -f
    docker rm -f omegacd-it-rootless omegacd-it-podman
    docker network prune -f --filter "label!=com.docker.compose.project=forgejo" # a stopped Forgejo keeps its network
    docker volume ls -q | grep -v "^forgejo_" | xargs -r docker volume rm -f
    rm -rf /tmp/omegacd-engines /tmp/omegacd-e2e* /tmp/Test* /tmp/agent-it-*
    true' >/dev/null 2>&1
  on "$1" bash -c 'export XDG_RUNTIME_DIR=/run/user/$(id -u); podman rm -af; podman volume prune -f; podman network prune -f; true' >/dev/null 2>&1
}

run() {
  local t=$1
  if ! grep -qx "Host omegacd-test-$t" "$infra/ssh_config" 2>/dev/null; then
    echo "no server omegacd-test-$t: make up in $infra" >&2; return 1
  fi
  on "$t" true 2>/dev/null || { echo "can't reach omegacd-test-$t: make ip in $infra (this Mac's address changed?)" >&2; return 1; }
  stop "$t"
  # the repository as it is here, built web UI included; not node_modules, binaries, or what tests left
  rsync -a --delete --exclude /web/node_modules/ --exclude /bin/ --exclude /cover/ -e "ssh -F $infra/ssh_config" "$repo/" "omegacd-test-$t:omegacd/"
  local cmd="make test-all ENGINES='$engines'"
  [ -n "$only" ] && cmd=$only
  echo "=== $t: $cmd"
  # a login shell: Go on the PATH; TMPDIR on the server's disk; the user's runtime dir for its rootless Podman
  on "$t" bash -lc "export TMPDIR=/tmp XDG_RUNTIME_DIR=/run/user/\$(id -u) ENGINES='$engines'; cd ~/omegacd && $cmd"
}

only=""
while [ $# -gt 0 ]; do
  case $1 in
  --only) only=${2:?--only needs a command}; shift 2 ;;
  *) break ;;
  esac
done
targets=${*:-$all}
for t in $targets; do
  case " $all " in *" $t "*) ;; *) echo "unknown target $t; targets: $all" >&2; exit 2 ;; esac
done
logs=${TMPDIR:-/tmp}
# A monitor, not a wait: every few seconds it reads each running target's log; a target's first failure (a test, a
# package, an e2e script, make) stops that target at once, inside its machine too, and the others go on.
failed='^--- FAIL|^FAIL|^✗ |^make: \*\*\*'
running=() queue=($targets)
# a new file each run: processes left from an aborted one keep writing to the old one, not into this run's log
launch() { rm -f "$logs/omegacd-test-$1.log"; { trap - INT TERM; run "$1"; } > "$logs/omegacd-test-$1.log" 2>&1 & running+=("$1:$!"); }
fill() { while [ ${#queue[@]} -gt 0 ]; do launch "${queue[0]}"; queue=("${queue[@]:1}"); done; }
failures=()
fail() { # target $1 (pid $2) failed: it stops, the others go on
  echo "=== $1: FAILED (log: $logs/omegacd-test-$1.log): $3"
  kill "$2" 2>/dev/null || true # it may have ended by itself already (make stops after a failed step)
  wait "$2" 2>/dev/null || true
  stop "$1"
  failures+=("$1")
}
# Ctrl-C stops the servers' work too, not only the ssh sessions that started it (kill: Ctrl-C may have ended them)
trap 'echo "=== interrupted: stopping ${running[*]%%:*}"; for e in "${running[@]}"; do kill "${e#*:}" 2>/dev/null || true; stop "${e%%:*}" & done; wait; exit 130' INT TERM
fill
while [ ${#running[@]} -gt 0 ]; do
  sleep 5
  still=()
  for e in "${running[@]}"; do
    t=${e%%:*} pid=${e#*:} log=$logs/omegacd-test-${e%%:*}.log
    if hit=$(grep -aE -m1 "$failed" "$log"); then fail "$t" "$pid" "$hit"; continue; fi
    if kill -0 "$pid" 2>/dev/null; then still+=("$e"); continue; fi
    if wait "$pid"; then echo "=== $t: passed (log: $log)"; stop "$t"; else fail "$t" "$pid" "exited with $?"; fi
  done
  running=("${still[@]}")
  fill
done
[ ${#failures[@]} -eq 0 ] || { echo "=== failed: ${failures[*]}; fix with --only, then the whole suite again before a push"; exit 1; }
