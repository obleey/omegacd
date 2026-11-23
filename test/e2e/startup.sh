#!/usr/bin/env bash
# End-to-end: the server, the Source Server and the Agent refusing to start on what they can't use (Q147): each exits
# with an Error line saying what is wrong and, where it can, what to do. The Agent with no engine to talk to starts and
# says so.
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eu.XXXX")
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
cleanup() { kill $HOLD 2>/dev/null; wait $HOLD 2>/dev/null; rm -rf "$T"; }
trap cleanup EXIT

# refuses NAME WANT ENV... -- ARGS: omegacd, started with ENV and ARGS, exits non-zero with WANT in an Error line
refuses() {
  local name=$1 want=$2 envs=() out rc
  shift 2
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  out=$(env "${envs[@]}" "$BIN" "$@" 2>&1); rc=$?
  if [ $rc -ne 0 ] && printf '%s\n' "$out" | grep "level=ERROR" | grep -qF -- "$want"; then
    echo "PASS  $name"
  else
    echo "FAIL  $name (exit $rc)"; printf '%s\n' "$out" | tail -3 | sed 's/^/      /'; fail=1
  fi
}

age-keygen -o "$T/age.key" 2>/dev/null
touch "$T/file"
# a port something else holds
python3 -c 'import socket,time; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1",18089)); s.listen(); time.sleep(600)' & HOLD=$!
sleep 0.5
data() { mktemp -d "$T/data.XXXX"; }
server() { echo OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA="$(data)" OMEGACD_AGE_KEY_FILE="$T/age.key"; }

# shellcheck disable=SC2046 # word splitting of server's assignments is the point
refuses "the server: a poll interval that isn't one" "OMEGACD_POLL_INTERVAL must be a duration" $(server) OMEGACD_POLL_INTERVAL=often --
refuses "the server: a Source Server address that isn't https://host:port" "want https://host:port" $(server) OMEGACD_SOURCE_SERVER=ftp://source --
d=$(data); echo "not age" > "$d/credentials.age"
refuses "the server: Git credentials it can't open" "its key?" OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA="$d" OMEGACD_AGE_KEY_FILE="$T/age.key" --
d=$(data); mkdir "$d/initial-admin-password"
refuses "the server: the first admin password it can't write (Q300)" "write the initial admin password" OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA="$d" OMEGACD_AGE_KEY_FILE="$T/age.key" --
refuses "the server: trusted proxies that aren't addresses" "invalid" $(server) OMEGACD_TRUSTED_PROXIES=everyone --
refuses "the server: its port taken" "address already in use" OMEGACD_ADDR=127.0.0.1:18089 OMEGACD_DATA="$(data)" OMEGACD_AGE_KEY_FILE="$T/age.key" --

refuses "the Source Server: no certificates" "omegacd source-certs makes them" OMEGACD_SOURCE_TLS_DIR="$T/none" OMEGACD_DATA="$(data)" -- source-server
"$BIN" source-certs --out "$T/tls" >/dev/null 2>&1
d=$(data); echo "not age" > "$d/credentials.age"
refuses "the Source Server: Git credentials it can't open" "its key?" OMEGACD_SOURCE_TLS_DIR="$T/tls" OMEGACD_DATA="$d" -- source-server
refuses "the Source Server: its port taken" "address already in use" OMEGACD_SOURCE_TLS_DIR="$T/tls" OMEGACD_DATA="$(data)" OMEGACD_SOURCE_ADDR=127.0.0.1:18089 -- source-server

# the Agent, with an engine address nothing answers at: what is checked before it is asked fails first
agent() { echo DOCKER_HOST=tcp://127.0.0.1:1 OMEGACD_AGENT_STATE_DIR="$1" OMEGACD_AGENT_WORKDIR="$T/work"; }
refuses "the Agent: a state dir it can't create" "state dir: " $(agent "$T/file/state") --  agent
refuses "the Agent: a work dir it can't create" "work dir: " DOCKER_HOST=tcp://127.0.0.1:1 OMEGACD_AGENT_STATE_DIR="$T/s0" OMEGACD_AGENT_WORKDIR="$T/file/work" -- agent
mkdir -p "$T/s1" && echo "not a database" > "$T/s1/agent.db"
refuses "the Agent: a database that isn't one" "not a database" $(agent "$T/s1") -- agent
mkdir -p "$T/s2" && touch "$T/s2/cache"
refuses "the Agent: a cache it can't create" "cache" $(agent "$T/s2") -- agent
mkdir -p "$T/s3" && echo "not a key" > "$T/s3/host.key"
refuses "the Agent: a Host Key that isn't one" "host.key" $(agent "$T/s3") -- agent
mkdir -p "$T/s4" && env $(agent "$T/s4") "$BIN" agent rollback >/dev/null 2>&1 && chmod 644 "$T/s4/signing.key"
refuses "the Agent: a signing key others can read (Q77)" "chmod 600" $(agent "$T/s4") -- agent
mkdir -p "$T/s5" && env $(agent "$T/s5") "$BIN" agent rollback >/dev/null 2>&1
python3 -c 'import sqlite3,sys; d=sqlite3.connect(sys.argv[1]); d.execute("INSERT INTO apps (name, parent, revision, self_heal, prune, prune_volumes, updated_at) VALUES (\"web\", \"\", \"\", \"x\", 0, 0, 0)"); d.commit()' "$T/s5/agent.db"
refuses "the Agent: Applications it can't read" "s5/agent.db: sql: Scan error" $(agent "$T/s5") -- agent
refuses "the Agent: a CA file that isn't there" "OMEGACD_AGENT_CA_FILE" $(agent "$T/s6") OMEGACD_AGENT_CONTROL_PLANE=https://cp.example OMEGACD_AGENT_CA_FILE="$T/none.pem" -- agent

# no engine at the address: it starts all the same, says so, and stops when asked
env $(agent "$T/s7") "$BIN" agent > "$T/agent.log" 2>&1 & pid=$!
for _ in $(seq 50); do grep -q "omegacd agent running" "$T/agent.log" && break; sleep 0.2; done
kill $pid; wait $pid; rc=$?
check "the Agent without an engine to talk to says so, and runs" 'grep -q "level=WARN msg=\"no container engine reachable (DOCKER_HOST or the default socket)\"" "$T/agent.log" && grep -q "omegacd agent running" "$T/agent.log"'
check "and stops cleanly when asked" '[ $rc -eq 0 ]'

[ $fail -eq 0 ] && echo "ALL PASSED"
exit $fail
