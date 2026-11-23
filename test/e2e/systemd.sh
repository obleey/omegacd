#!/usr/bin/env bash
# End-to-end: the .deb (M6-09) with this checkout's binary, and the Agent's systemd units in it (M6-03) as an operator
# uses them, a drop-in only logging at debug and collecting coverage: the system unit as the omegacd-agent user in the
# docker group on rootful Docker (Q242), the user unit on the test user's rootless Docker and rootless Podman (Q244,
# Q246). Each enrolls, deploys an app with a restart policy that mounts a decrypted file (as a Compose secret and a bind
# mount, M6-10) and a plain file from Git, keeps its work dir across an Agent restart (Q243), and comes through a
# simulated reboot: the Agent and engine stopped, the RAM work dir emptied, the engine started, which runs the restore
# unit first, so the container starts again with its files (Q59, Q248). Then the
# package: an upgrade restarts the Agents under the running app (Q256), remove keeps the state, a reinstall starts the
# Agent again, the Control Plane's unit runs, and purge leaves nothing of the Agent's (Q257, Q260). Linux with systemd,
# apt and passwordless sudo; it changes the machine (a package, users), so it runs on the test servers and CI's runners.
#
# OMEGACD_E2E_REBOOT=clean removes what an aborted run left. =setup leaves the system unit and the rootless Docker user unit installed with an app each, for a
# real reboot (test/linux.sh, Q249); =check then checks both apps came back with their files, and removes it all.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BIN=${OMEGACD_BIN:-$ROOT/bin/omegacd}
REBOOT=${OMEGACD_E2E_REBOOT:-}
if [ "$(uname)" != Linux ] || ! command -v systemctl >/dev/null; then echo "SKIP  no systemd (Linux only)"; exit 0; fi
if ! command -v apt-get >/dev/null; then
  [ -z "${OMEGACD_TEST_ALL:-}" ] || { echo "FAIL  apt, which installs the .deb (Q260)"; exit 1; }
  echo "SKIP  no apt"; exit 0
fi
if ! sudo -n true 2>/dev/null; then
  [ -z "${OMEGACD_TEST_ALL:-}" ] || { echo "FAIL  passwordless sudo, which installing the system unit needs (Q248)"; exit 1; }
  echo "SKIP  no passwordless sudo"; exit 0
fi
export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
T=/var/tmp/omegacd-e2ey # one path, on disk, which a real reboot's check finds again
[ "$REBOOT" = check ] || { sudo rm -rf "$T"; mkdir -p "$T/repo" "$T/data"; }
REPO=$T/repo API=http://127.0.0.1:18080/api
COV=/var/lib/omegacd-e2e-cov # where the units' processes write coverage: the Control Plane's has its own /var/tmp
SD=/etc/systemd/system UD=$HOME/.config/systemd/user USTATE=$HOME/.local/share/omegacd-agent
DSOCK=$XDG_RUNTIME_DIR/docker.sock PSOCK=$XDG_RUNTIME_DIR/podman/podman.sock
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
waitfor() { for _ in $(seq $((${2:-30} * 5))); do eval "$1" && return 0; sleep 0.2; done; return 1; } # CMD [SECONDS]
syslog() { sudo journalctl -u omegacd-agent -o cat --since "@$START" 2>/dev/null; }
ulog() { journalctl --user -u omegacd-agent -o cat --since "@$START" 2>/dev/null; }
ctr() { DOCKER_HOST=$2 docker ps -q --filter label=com.docker.compose.project="$1"; } # APP ENGINE
# the files the app's container reads, restored (a folder Docker made can't be cat): the decrypted file as a Compose
# secret and as a bind mount (Q330), and a plain file from Git as a bind mount
shows() {
  [ "$(DOCKER_HOST=$2 docker exec "$(ctr "$1" "$2")" cat /run/secrets/token /secret.env /plain.conf 2>/dev/null)" = \
    "$(printf 'TOKEN=%s\nTOKEN=%s\nPLAIN=git' "$SECRET" "$SECRET")" ]
}
mono() { "$@" | tr -dc 0-9; } # a unit's monotonic timestamp, from systemctl show -p … --value
# a system user, which the test removes after itself: the test servers' own login is omegacd too (the package's
# Control Plane then runs as it), never removed
sysuser() { local uid; uid=$(id -u "$1" 2>/dev/null) && [ "$uid" -lt 1000 ]; }
apt() { sudo DEBIAN_FRONTEND=noninteractive apt-get -y -q "$@" >> "$T/apt.log" 2>&1; }
uninstall() {
  uunits
  sudo rm -rf $SD/omegacd.service.d $SD/omegacd-agent.service.d $SD/omegacd-agent-restore.service.d
  sudo DEBIAN_FRONTEND=noninteractive apt-get -y -q purge omegacd >/dev/null 2>&1
  # what's left: the Control Plane's data and Master Key, kept by purge; units and a user of M6-03's manual install
  sudo rm -rf $SD/omegacd-agent.service $SD/omegacd-agent-restore.service /etc/omegacd /var/lib/omegacd \
    /var/lib/omegacd-agent /run/omegacd
  sudo systemctl daemon-reload
  for u in omegacd-agent omegacd; do ! sysuser $u || sudo userdel $u; done
  rm -f "$HOME/.config/containers/containers.conf.d/omegacd-agent.conf"
  systemctl --user disable --now podman-restart.service >/dev/null 2>&1 # install.sh enabled it (Q265)
  for s in "unix://$DSOCK" "unix://$PSOCK" ""; do
    for p in e2e-sys e2e-rl e2e-pm; do
      ids=$(DOCKER_HOST=$s docker ps -aq --filter label=com.docker.compose.project=$p 2>/dev/null)
      [ -z "$ids" ] || DOCKER_HOST=$s docker rm -f $ids >/dev/null 2>&1
    done
  done
  sudo rm -f /usr/local/bin/omegacd-e2e # M6-03's
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
  [ -z "${GOCOVERDIR:-}" ] || cp $COV/* "$GOCOVERDIR"/ 2>/dev/null
  uninstall; sudo rm -rf "$T" $COV
}
trap cleanup EXIT

if [ "$REBOOT" = clean ]; then uninstall; sudo rm -rf "$T" $COV; trap - EXIT; exit 0; fi # test/linux.sh's stop
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
sudo rm -rf $COV && sudo install -d -m 1777 $COV
# this checkout as a package, twice (a newer version to upgrade to), as releases publish it for install.sh (Q266)
ARCH=$(dpkg --print-architecture)
mkdir -p "$T/dl"
for v in 1 2; do
  make -s -C "$ROOT" deb DEB_BIN="$BIN" VERSION=v0.0.1-e2e.$v DEB_OUT="$T/dl/omegacd_v0.0.1-e2e.${v}_linux_$ARCH.deb" >> "$T/apt.log" 2>&1 ||
    { echo "FAIL  make deb"; tail -5 "$T/apt.log"; exit 1; }
done
(cd "$T/dl" && sha256sum *.deb > SHA256SUMS)
# inst VERSION ENGINE [SETTING=VALUE…]: install.sh's agent mode with systemd, this checkout's copy (Q269)
inst() {
  local v=$1 engine=$2; shift 2
  env OMEGACD_VERSION=v0.0.1-e2e.$v OMEGACD_DOWNLOAD="file://$T/dl" OMEGACD_MODE=agent OMEGACD_AGENT_INSTALL=systemd \
    OMEGACD_AGENT_ENGINE="$engine" "$@" sh "$ROOT/deploy/install.sh" > "$T/install.out" 2>&1 </dev/null
}
# the units as shipped; a drop-in only logs at debug and collects coverage
dropin() { printf '[Service]\nEnvironment=OMEGACD_LOG_LEVEL=debug GOCOVERDIR=%s\n' $COV; }

SECRET=$(openssl rand -hex 12) && echo "$SECRET" > "$T/secret"
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
waitfor "curl -sf $API/version >/dev/null"
. "$(dirname "$0")/admin.sh"; admin_login "$API" "$T/data"
gitc init -q -b main && gitc commit -q --allow-empty -m init

# approve HOST LOG: the fingerprint its Agent logged
approve() {
  local fp; fp=$($2 | grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' | head -1 | cut -d= -f2)
  "$BIN" host approve "$1" "$fp" >/dev/null
}
# deploy APP HOST: an app with a restart policy, a file decrypted from Git with its Host's key as a Compose secret and
# as a bind mount (M6-10), and a plain file from Git bind-mounted
deploy() {
  local rcpt; rcpt=$("$BIN" host list -o json | python3 -c "import json,sys; print([h for h in json.load(sys.stdin) if h['name']=='$2'][0]['ageRecipient'])")
  mkdir -p "$REPO/$1"
  printf 'services:\n  web:\n    image: busybox:1.38\n    restart: always\n    command: ["sh", "-c", "cat /run/secrets/token; sleep 3600"]\n    secrets: [token]\n    volumes:\n      - ./secret.env:/secret.env:ro\n      - ./plain.conf:/plain.conf:ro\nsecrets:\n  token:\n    file: ./secret.env\n' > "$REPO/$1/compose.yaml"
  printf 'PLAIN=git\n' > "$REPO/$1/plain.conf"
  printf 'TOKEN=%s\n' "$SECRET" | SOPS_AGE_RECIPIENTS=$rcpt sops --encrypt --input-type dotenv --output-type dotenv /dev/stdin > "$REPO/$1/secret.sops.env"
  gitc add -A && gitc commit -qm "$1"
  "$BIN" app create "$1" --repo "$REPO" --path "$1" --host "$2" >/dev/null
  "$BIN" app sync "$1" --wait --timeout 3m > "$T/sync-$1.out" 2>&1
}
connected() { $1 | grep -c "connected to the Control Plane, approved"; }

# --- the package, and its system unit on rootful Docker (Q242, Q255)
START=$(date +%s)
apt install "$T/dl/omegacd_v0.0.1-e2e.1_linux_$ARCH.deb"
check "the package installs: the Agent's user in the docker group, its units enabled, not started without agent.env" \
  'id -nG omegacd-agent | grep -qw docker && systemctl -q is-enabled omegacd-agent omegacd-agent-restore && ! systemctl -q is-active omegacd-agent && ! systemctl -q is-enabled omegacd || { tail -20 "$T/apt.log"; false; }'
INVITE=$("$BIN" host add sys | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
sudo mkdir -p $SD/omegacd-agent.service.d $SD/omegacd-agent-restore.service.d
dropin | sudo tee $SD/omegacd-agent.service.d/e2e.conf $SD/omegacd-agent-restore.service.d/e2e.conf >/dev/null
sudo systemctl daemon-reload
inst 1 docker OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18080 OMEGACD_AGENT_INVITE="$INVITE"
check "install.sh agent writes agent.env (0640, the Agent's group) and starts the system unit; it prints the fingerprint" \
  '[ "$(sudo stat -c "%U:%G %a" /etc/omegacd/agent.env)" = "root:omegacd-agent 640" ] && grep -q "Approve it in OmegaCD.s UI (Hosts) if its fingerprint is: SHA256:" "$T/install.out" || { cat "$T/install.out"; false; }'
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

# --- the user unit, ENGINE NAME SOCKET INSTALL-ENGINE VERSION: rootless Docker or Podman (Q244)
user_unit() {
  local engine=$1 app=$2 sock=$3
  START=$(date +%s)
  mkdir -p "$UD/omegacd-agent.service.d" "$UD/omegacd-agent-restore.service.d"
  dropin | tee "$UD/omegacd-agent.service.d/e2e.conf" "$UD/omegacd-agent-restore.service.d/e2e.conf" >/dev/null
  local INVITE; INVITE=$("$BIN" host add "$app" | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
  inst "$5" "$4" OMEGACD_AGENT_CONTROL_PLANE=http://127.0.0.1:18080 OMEGACD_AGENT_INVITE="$INVITE"
  check "$engine: install.sh agent sets up the user units, lingering, with the engine's socket in agent.env" \
    '[ "$(stat -c %a "$HOME/.config/omegacd/agent.env")" = 600 ] && grep -qx "DOCKER_HOST=unix://$sock" "$HOME/.config/omegacd/agent.env" && systemctl --user -q is-enabled omegacd-agent omegacd-agent-restore && [ -f "/var/lib/systemd/linger/$(id -un)" ] || { cat "$T/install.out"; false; }'
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
  user_unit "rootless Docker" e2e-rl "$DSOCK" rootless-docker 1
  if [ "$REBOOT" != setup ]; then
    n=$(connected ulog)
    systemctl --user stop omegacd-agent docker
    rm -rf "$XDG_RUNTIME_DIR/omegacd"
    systemctl --user start docker
    check "rootless Docker: its docker.service starting runs the user's restore unit first, the app back with its file" \
      'waitfor "shows e2e-rl unix://$DSOCK" 60 && [ "$(mono systemctl --user show -p ExecMainExitTimestampMonotonic --value omegacd-agent-restore)" -lt "$(mono systemctl --user show -p ExecMainStartTimestampMonotonic --value docker)" ]'
    systemctl --user start omegacd-agent
    check "rootless Docker: the Agent reconnects" 'waitfor "[ \$(connected ulog) -gt $n ]" 90'
  fi
fi

if [ "$REBOOT" != setup ]; then
  # Q256: an upgrade restarts the running Agents on the new binary, the lingering user's too; the app runs on
  pid() { $1 show -p MainPID --value omegacd-agent; }
  sp=$(pid systemctl) up=$(pid "systemctl --user") app=$(ctr e2e-sys "") n=$(connected syslog)
  usersd "$DSOCK" && un=$(connected ulog)
  inst 2 docker # a rerun, with no settings: the new version, agent.env kept (Q267)
  check "install.sh run again upgrades the package, keeping agent.env" \
    'sudo grep -q "^OMEGACD_AGENT_INVITE=$INVITE\$" /etc/omegacd/agent.env || { cat "$T/install.out"; false; }'
  check "an upgrade restarts the system Agent, which reconnects with its key, while the app's container runs on" \
    '[ "$(dpkg-query -W -f "\${Version}" omegacd)" = 0.0.1~e2e.2 ] && [ "$(pid systemctl)" != "$sp" ] && waitfor "[ \$(connected syslog) -gt $n ]" 90 && [ "$(ctr e2e-sys "")" = "$app" ] && shows e2e-sys ""'
  if usersd "$DSOCK"; then
    check "and the lingering user's Agent" '[ "$(pid "systemctl --user")" != "$up" ] && waitfor "[ \$(connected ulog) -gt $un ]" 90'
    "$BIN" app delete e2e-rl --yes >/dev/null 2>&1
    uunits
  fi
fi

if [ "$REBOOT" != setup ] && usersd "$PSOCK"; then
  # Q246, Q265: install.sh copies the package's drop-in, which the API service reads when it starts next
  user_unit "rootless Podman" e2e-pm "$PSOCK" podman 2 # after the upgrade
  check "rootless Podman: install.sh enables podman-restart.service, and the drop-in caps the app's log at 10 MiB" \
    'systemctl --user -q is-enabled podman-restart.service && ps -eo args | grep "[c]onmon" | grep "$(ctr e2e-pm unix://$PSOCK)" | grep -q -- "--log-size-max 10485760"'
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
fi

if [ "$REBOOT" != setup ]; then
  # Q257: remove stops the Agent and keeps its state; installed again, it starts by itself with it
  n=$(connected syslog)
  apt remove omegacd
  check "apt remove stops the Agents, the user's too, and takes the binary, keeping its user, state and agent.env" \
    '! systemctl -q is-active omegacd-agent && ! systemctl --user -q is-active omegacd-agent && [ ! -e /usr/bin/omegacd ] && [ ! -e /usr/bin/omegacd-reset-admin-password ] && id omegacd-agent >/dev/null && sudo test -f /var/lib/omegacd-agent/host.key && sudo test -f /etc/omegacd/agent.env'
  uunits
  apt install "$T/dl/omegacd_v0.0.1-e2e.2_linux_$ARCH.deb"
  check "installed again, the Agent starts by itself and reconnects with its key" 'waitfor "[ \$(connected syslog) -gt $n ]" 90'

  # the Control Plane's unit (Q253, Q254): not enabled by the package; enabled, it makes its Master Key in its folder
  sudo mkdir -p $SD/omegacd.service.d && dropin | sudo tee $SD/omegacd.service.d/e2e.conf >/dev/null
  printf 'OMEGACD_ADDR=127.0.0.1:18081\nOMEGACD_POLL_INTERVAL=1h\n' | sudo install -m 0600 /dev/stdin /etc/omegacd/omegacd.env
  sudo systemctl daemon-reload && sudo systemctl enable --now omegacd >/dev/null 2>&1
  check "the Control Plane's unit runs as omegacd, its Master Key made in /etc/omegacd/keys" \
    'waitfor "curl -sf http://127.0.0.1:18081/api/version >/dev/null" && [ "$(sudo stat -c "%U %a" /etc/omegacd/keys/age.key)" = "omegacd 600" ] || { sudo journalctl -u omegacd -o cat | tail -10; false; }'
  PWF=/var/lib/omegacd/initial-admin-password
  check "its first admin password in the data dir's file, the service's own, owner-only (Q300)" '[ "$(sudo stat -c "%U %a" $PWF)" = "omegacd 600" ]'
  first=$(sudo cat $PWF)
  out=$(omegacd-reset-admin-password 2>&1); rc=$?
  check "omegacd-reset-admin-password asks for sudo (Q316)" '[ $rc -ne 0 ] && echo "$out" | grep -q "run it with sudo"'
  out=$(sudo omegacd-reset-admin-password 2>&1); rc=$?
  check "with sudo it writes a new one as the service's user, the database's files staying the service's (Q311, Q316)" \
    '[ $rc -eq 0 ] && echo "$out" | grep -q "A new admin password is in $PWF" && [ "$(sudo cat $PWF)" != "$first" ] && [ "$(sudo stat -c "%U %a" $PWF)" = "omegacd 600" ] && [ -z "$(sudo find /var/lib/omegacd -maxdepth 1 -user root)" ] || { echo "$out"; false; }'
  out=$(sudo env OMEGACD_DATA=/var/lib/omegacd OMEGACD_AGE_KEY_FILE=/etc/omegacd/keys/age.key /usr/bin/omegacd reset-admin-password 2>&1); rc=$?
  check "the command itself, as root over the service's database, refuses, naming the wrapper" '[ $rc -ne 0 ] && echo "$out" | grep -q "sudo omegacd-reset-admin-password"'

  sudo rm -rf $SD/omegacd.service.d $SD/omegacd-agent.service.d $SD/omegacd-agent-restore.service.d
  OMEGACD_UID=$(id -u omegacd) # a system user, or the test servers' own login
  out=$(sudo DEBIAN_FRONTEND=noninteractive apt-get -y -q purge omegacd 2>&1)
  check "apt purge leaves no unit or folder of the Agent's, and its users (Q261)" \
    '[ -z "$(find /usr/lib/systemd /etc/systemd/system /usr/share/omegacd /usr/lib/sysusers.d -name "*omegacd*" 2>/dev/null)" ] && sysuser omegacd-agent && [ "$(id -u omegacd)" = "$OMEGACD_UID" ] && sudo test ! -e /var/lib/omegacd-agent -a ! -e /etc/omegacd/agent.env -a ! -e /run/omegacd || { echo "$out"; false; }'
  check "and keeps the Control Plane's data and Master Key, saying so" \
    'sudo test -d /var/lib/omegacd -a -f /etc/omegacd/keys/age.key && echo "$out" | grep -q "kept /var/lib/omegacd"'
fi

check "no secret or password in the logs (Q62)" \
  '! { START=0; syslog; journalctl --user -u omegacd-agent -o cat; cat "$T/server.log"; } | grep -qF -e "$SECRET" -e "$ADMIN_FIRST_PW" -e "$ADMIN_PW"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- system unit"; syslog | tail -15; echo "--- user unit"; ulog | tail -15; echo "--- server log"; tail -10 "$T/server.log"; }
exit $fail
