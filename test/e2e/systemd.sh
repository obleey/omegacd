#!/usr/bin/env bash
# End-to-end: the Agent's systemd units from packaging/ (M6-03) as an operator installs them, this checkout's binary in
# a drop-in: the system unit as the omegacd-agent user in the docker group on rootful Docker (Q242), the user unit on the
# test user's rootless Docker and rootless Podman (Q244, Q246). Each enrolls, deploys an app with a restart policy that
# mounts a decrypted file, keeps its work dir across an Agent restart (Q243), and comes through a simulated reboot: the
# Agent and engine stopped, the RAM work dir emptied, the engine started, which runs the restore unit first, so the
# container starts again with its file (Q59, Q248). Linux with systemd and passwordless sudo; the system unit changes the
# machine (a user, /etc/omegacd, units), so it runs on the test servers and CI's runners.
#
# OMEGACD_E2E_REBOOT=clean removes what an aborted run left. =setup leaves the system unit and the rootless Docker user unit installed with an app each, for a
# real reboot (test/linux.sh, Q249); =check then checks both apps came back with their files, and removes it all.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BIN=${OMEGACD_BIN:-$ROOT/bin/omegacd}
REBOOT=${OMEGACD_E2E_REBOOT:-}
if [ "$(uname)" != Linux ] || ! command -v systemctl >/dev/null; then echo "SKIP  no systemd (Linux only)"; exit 0; fi
if ! sudo -n true 2>/dev/null; then
  [ -z "${OMEGACD_TEST_ALL:-}" ] || { echo "FAIL  passwordless sudo, which installing the system unit needs (Q248)"; exit 1; }
  echo "SKIP  no passwordless sudo"; exit 0
fi
export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
T=/var/tmp/omegacd-e2ey # one path, on disk, which a real reboot's check finds again
[ "$REBOOT" = check ] || { sudo rm -rf "$T"; mkdir -p "$T/repo" "$T/data"; }
REPO=$T/repo API=http://127.0.0.1:18080/api EBIN=/usr/local/bin/omegacd-e2e
SD=/etc/systemd/system UD=$HOME/.config/systemd/user USTATE=$HOME/.local/share/omegacd-agent
DSOCK=$XDG_RUNTIME_DIR/docker.sock PSOCK=$XDG_RUNTIME_DIR/podman/podman.sock
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
waitfor() { for _ in $(seq $((${2:-30} * 5))); do eval "$1" && return 0; sleep 0.2; done; return 1; } # CMD [SECONDS]
syslog() { sudo journalctl -u omegacd-agent -o cat --since "@$START" 2>/dev/null; }
ulog() { journalctl --user -u omegacd-agent -o cat --since "@$START" 2>/dev/null; }
ctr() { DOCKER_HOST=$2 docker ps -q --filter label=com.docker.compose.project="$1"; } # APP ENGINE
# the file the app's container reads: the decrypted secret, restored (a folder Docker made can't be cat)
shows() { [ "$(DOCKER_HOST=$2 docker exec "$(ctr "$1" "$2")" cat /run/secrets/token 2>/dev/null)" = "TOKEN=$SECRET" ]; }
mono() { "$@" | tr -dc 0-9; } # a unit's monotonic timestamp, from systemctl show -p … --value
uninstall() {
  sudo systemctl disable --now omegacd-agent.service omegacd-agent-restore.service >/dev/null 2>&1
  sudo rm -rf $SD/omegacd-agent.service $SD/omegacd-agent-restore.service $SD/omegacd-agent.service.d \
    $SD/omegacd-agent-restore.service.d /etc/omegacd/agent.env /var/lib/omegacd-agent /run/omegacd
  sudo rmdir /etc/omegacd 2>/dev/null
  sudo systemctl daemon-reload
  id omegacd-agent >/dev/null 2>&1 && sudo userdel omegacd-agent
  uunits
  rm -f "$HOME/.config/containers/containers.conf.d/omegacd-agent.conf"
  for s in "unix://$DSOCK" "unix://$PSOCK" ""; do
    for p in e2e-sys e2e-rl e2e-pm; do
      ids=$(DOCKER_HOST=$s docker ps -aq --filter label=com.docker.compose.project=$p 2>/dev/null)
      [ -z "$ids" ] || DOCKER_HOST=$s docker rm -f $ids >/dev/null 2>&1
    done
  done
  sudo rm -f "$EBIN"
}
uunits() { # the user unit gone, and its state, before the next engine's
  systemctl --user disable --now omegacd-agent.service omegacd-agent-restore.service >/dev/null 2>&1
  rm -rf "$UD/omegacd-agent.service" "$UD/omegacd-agent-restore.service" "$UD/omegacd-agent.service.d" \
    "$UD/omegacd-agent-restore.service.d" "$HOME/.config/omegacd/agent.env" "$USTATE" "$XDG_RUNTIME_DIR/omegacd"
  systemctl --user daemon-reload 2>/dev/null
}
cleanup() {
  [ -z "${SRV:-}" ] || { kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; }
  [ "$REBOOT" = setup ] && [ $fail -eq 0 ] && return # left for the reboot
  # coverage the units' processes wrote, as their users, where this run collects it
  [ -z "${GOCOVERDIR:-}" ] || cp "$T"/cov/* "$GOCOVERDIR"/ 2>/dev/null
  uninstall; sudo rm -rf "$T"
}
trap cleanup EXIT

if [ "$REBOOT" = clean ]; then uninstall; sudo rm -rf "$T"; trap - EXIT; exit 0; fi # test/linux.sh's stop
if [ "$REBOOT" = check ]; then
  SECRET=$(cat "$T/secret")
  check "after a real reboot the system unit's app runs with its file, restored before Docker started it" \
    'waitfor "shows e2e-sys \"\"" 120'
  check "and the rootless Docker user unit's" 'waitfor "shows e2e-rl unix://$DSOCK" 120'
  check "the restore units ran before the engines" \
    '[ "$(systemctl show -p ActiveState --value omegacd-agent-restore)" != failed ] && [ "$(mono systemctl show -p ExecMainExitTimestampMonotonic --value omegacd-agent-restore)" -lt "$(mono systemctl show -p ExecMainStartTimestampMonotonic --value docker)" ] && [ "$(mono systemctl --user show -p ExecMainExitTimestampMonotonic --value omegacd-agent-restore)" -lt "$(mono systemctl --user show -p ExecMainStartTimestampMonotonic --value docker)" ]'
  [ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- system unit"; sudo journalctl -b -u omegacd-agent -u omegacd-agent-restore -o cat | tail -15; }
  exit $fail
fi

uninstall # what an aborted run left
sudo install -m 0755 "$BIN" "$EBIN" # the binary where the Agent's user can run it (homes are closed to others)
mkdir -p "$T/cov" && chmod 0755 "$T" && chmod 1777 "$T/cov"
# the units as shipped; a drop-in only swaps the binary, logs at debug and collects coverage
dropin() { printf '[Service]\nExecStart=\nExecStart=%s %s\nEnvironment=OMEGACD_LOG_LEVEL=debug GOCOVERDIR=%s\n' "$EBIN" "$1" "$T/cov"; }

SECRET=$(openssl rand -hex 12) && echo "$SECRET" > "$T/secret"
PW=$(openssl rand -hex 12)
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key OMEGACD_ADMIN_PASSWORD=$PW \
  OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
waitfor "curl -sf $API/version >/dev/null"
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}" $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
gitc init -q -b main && gitc commit -q --allow-empty -m init

# approve HOST LOG: the fingerprint its Agent logged
approve() {
  local fp; fp=$($2 | grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' | head -1 | cut -d= -f2)
  "$BIN" host approve "$1" "$fp" >/dev/null
}
# deploy APP HOST: an app with a restart policy and a Compose secret, a file decrypted from Git with its Host's key
deploy() {
  local rcpt; rcpt=$("$BIN" host list -o json | python3 -c "import json,sys; print([h for h in json.load(sys.stdin) if h['name']=='$2'][0]['ageRecipient'])")
  mkdir -p "$REPO/$1"
  printf 'services:\n  web:\n    image: busybox:1.38\n    restart: always\n    command: ["sh", "-c", "cat /run/secrets/token; sleep 3600"]\n    secrets: [token]\nsecrets:\n  token:\n    file: ./secret.env\n' > "$REPO/$1/compose.yaml"
  printf 'TOKEN=%s\n' "$SECRET" | SOPS_AGE_RECIPIENTS=$rcpt sops --encrypt --input-type dotenv --output-type dotenv /dev/stdin > "$REPO/$1/secret.sops.env"
  gitc add -A && gitc commit -qm "$1"
  "$BIN" app create "$1" --repo "$REPO" --path "$1" --host "$2" >/dev/null
  "$BIN" app sync "$1" --wait --timeout 3m > "$T/sync-$1.out" 2>&1
}
connected() { $1 | grep -c "connected to the Control Plane, approved"; }

# --- the system unit, rootful Docker (Q242)
START=$(date +%s)
sudo useradd --system --home-dir /var/lib/omegacd-agent --shell /usr/sbin/nologin --groups docker omegacd-agent
INVITE=$("$BIN" host add sys | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
sudo install -d -m 0755 /etc/omegacd
printf 'OMEGACD_AGENT_CONTROL_PLANE=%s\nOMEGACD_AGENT_INVITE=%s\n' http://127.0.0.1:18080 "$INVITE" |
  sudo install -m 0640 -g omegacd-agent /dev/stdin /etc/omegacd/agent.env # as the README has it
sudo cp "$ROOT/packaging/omegacd-agent.service" "$ROOT/packaging/omegacd-agent-restore.service" $SD/
sudo mkdir -p $SD/omegacd-agent.service.d $SD/omegacd-agent-restore.service.d
dropin agent | sudo tee $SD/omegacd-agent.service.d/e2e.conf >/dev/null
dropin "agent restore --files" | sudo tee $SD/omegacd-agent-restore.service.d/e2e.conf >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable omegacd-agent-restore.service >/dev/null 2>&1 # with Docker; nothing to restore yet: skipped
sudo systemctl enable --now omegacd-agent.service >/dev/null 2>&1
check "the system unit starts the Agent as omegacd-agent, not root, and it enrolls" \
  'waitfor "syslog | grep -q \"waiting for an admin to approve\"" && [ "$(ps -o uid= -p "$(systemctl show -p MainPID --value omegacd-agent)" | tr -d " ")" = "$(id -u omegacd-agent)" ]'
approve sys syslog
check "and is approved" 'waitfor "syslog | grep -q \"connected to the Control Plane, approved\""'
deploy e2e-sys sys
check "it deploys an app with a restart policy and a decrypted file into /run/omegacd" 'waitfor "shows e2e-sys \"\"" || { cat "$T/sync-e2e-sys.out"; syslog | tail -20; false; }'
check "its keys in /var/lib/omegacd-agent, owner-only" '[ "$(sudo stat -c "%U %a" /var/lib/omegacd-agent)" = "omegacd-agent 700" ] && sudo test -f /var/lib/omegacd-agent/host.key'

sudo systemctl restart omegacd-agent
check "restarting the Agent keeps its work dir under the running app (Q243)" \
  'sudo test -f /run/omegacd/e2e-sys/secret.env && shows e2e-sys ""'

if [ "$REBOOT" = setup ]; then :; else
  # a reboot, simulated: the Agent and Docker stopped, the RAM work dir gone, Docker started; the containers that ran
  # (Forgejo) started again after, as they have no restart policy
  n=$(connected syslog)
  was=$(docker ps -q)
  sudo systemctl stop omegacd-agent docker.socket docker
  sudo rm -rf /run/omegacd
  sudo systemctl start docker
  [ -z "$was" ] || docker start $was >/dev/null 2>&1
  check "Docker starting runs the restore unit first: the app restarted by Docker with its decrypted file, not a folder (Q59)" \
    'waitfor "shows e2e-sys \"\"" 60'
  check "the restore unit finished before Docker started" \
    '[ "$(mono systemctl show -p ExecMainExitTimestampMonotonic --value omegacd-agent-restore)" -lt "$(mono systemctl show -p ExecMainStartTimestampMonotonic --value docker)" ] && [ "$(systemctl show -p Result --value omegacd-agent-restore)" = success ]'
  sudo systemctl start omegacd-agent
  check "and the Agent, started, reconnects" 'waitfor "[ \$(connected syslog) -gt $n ]" 90'
fi

# --- the user unit, ENGINE NAME SOCKET: rootless Docker or Podman (Q244)
user_unit() {
  local engine=$1 app=$2 sock=$3
  START=$(date +%s)
  mkdir -p "$UD/omegacd-agent.service.d" "$UD/omegacd-agent-restore.service.d" "$HOME/.config/omegacd"
  cp "$ROOT/packaging/user/omegacd-agent.service" "$ROOT/packaging/user/omegacd-agent-restore.service" "$UD/"
  dropin agent > "$UD/omegacd-agent.service.d/e2e.conf"
  dropin "agent restore --files" > "$UD/omegacd-agent-restore.service.d/e2e.conf"
  INVITE=$("$BIN" host add "$app" | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
  (umask 077 && printf 'OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18080\nOMEGACD_AGENT_INVITE=%s\nDOCKER_HOST=unix://%s\n' \
    "$INVITE" "$sock" > "$HOME/.config/omegacd/agent.env")
  systemctl --user daemon-reload
  systemctl --user enable omegacd-agent-restore.service >/dev/null 2>&1
  systemctl --user enable --now omegacd-agent.service >/dev/null 2>&1
  check "$engine: the user unit starts the Agent as the user, and it enrolls" \
    'waitfor "ulog | grep -q \"waiting for an admin to approve\"" && ulog | grep -q "rootless=true"'
  approve "$app" ulog
  waitfor "ulog | grep -q \"connected to the Control Plane, approved\""
  deploy "$app" "$app"
  check "$engine: it deploys the app into \$XDG_RUNTIME_DIR/omegacd" 'waitfor "shows $app unix://$sock" && [ -f "$XDG_RUNTIME_DIR/omegacd/$app/secret.env" ]'
}

# the user units need the user's systemd (lingering), which the test servers have and CI's runners don't
usersd() { [ -n "${OMEGACD_TEST_ALL:-}" ] || { [ -S "$1" ] && systemctl --user show-environment >/dev/null 2>&1; }; }
if usersd "$DSOCK"; then
  user_unit "rootless Docker" e2e-rl "$DSOCK"
  if [ "$REBOOT" != setup ]; then
    n=$(connected ulog)
    systemctl --user stop omegacd-agent docker
    rm -rf "$XDG_RUNTIME_DIR/omegacd"
    systemctl --user start docker
    check "rootless Docker: its docker.service starting runs the user's restore unit first, the app back with its file" \
      'waitfor "shows e2e-rl unix://$DSOCK" 60 && [ "$(mono systemctl --user show -p ExecMainExitTimestampMonotonic --value omegacd-agent-restore)" -lt "$(mono systemctl --user show -p ExecMainStartTimestampMonotonic --value docker)" ]'
    systemctl --user start omegacd-agent
    check "rootless Docker: the Agent reconnects" 'waitfor "[ \$(connected ulog) -gt $n ]" 90'
    "$BIN" app delete e2e-rl --yes >/dev/null 2>&1
    uunits
  fi
fi

if [ "$REBOOT" != setup ] && usersd "$PSOCK"; then
  # Q246: the drop-in in the user's containers.conf.d; the API service reads it when it starts next
  mkdir -p "$HOME/.config/containers/containers.conf.d"
  cp "$ROOT/packaging/containers.conf.d/omegacd-agent.conf" "$HOME/.config/containers/containers.conf.d/"
  systemctl --user stop podman.service 2>/dev/null
  user_unit "rootless Podman" e2e-pm "$PSOCK"
  check "rootless Podman: the containers.conf drop-in caps the app's log at 10 MiB" \
    'ps -eo args | grep "[c]onmon" | grep "$(ctr e2e-pm unix://$PSOCK)" | grep -q -- "--log-size-max 10485760"'
  n=$(connected ulog)
  systemctl --user stop omegacd-agent
  DOCKER_HOST=unix://$PSOCK docker stop -t 1 "$(ctr e2e-pm unix://$PSOCK)" >/dev/null
  rm -rf "$XDG_RUNTIME_DIR/omegacd"
  systemctl --user restart podman-restart.service
  check "rootless Podman: podman-restart.service runs the user's restore unit first, the app back with its file" \
    'waitfor "shows e2e-pm unix://$PSOCK" 60 && [ "$(mono systemctl --user show -p ExecMainExitTimestampMonotonic --value omegacd-agent-restore)" -lt "$(mono systemctl --user show -p ExecMainStartTimestampMonotonic --value podman-restart)" ]'
  systemctl --user start omegacd-agent
  check "rootless Podman: the Agent reconnects" 'waitfor "[ \$(connected ulog) -gt $n ]" 90'
  "$BIN" app delete e2e-pm --yes >/dev/null 2>&1
  systemctl --user stop podman-restart.service
  uunits
fi

check "no secret or password in the logs (Q62)" \
  '! { START=0; syslog; journalctl --user -u omegacd-agent -o cat; cat "$T/server.log"; } | grep -qF -e "$SECRET" -e "$PW"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- system unit"; syslog | tail -15; echo "--- user unit"; ulog | tail -15; echo "--- server log"; tail -10 "$T/server.log"; }
exit $fail
