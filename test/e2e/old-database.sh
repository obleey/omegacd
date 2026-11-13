#!/usr/bin/env bash
# End-to-end: a v0.1 database (or v0.2's from before its fresh schema) stops the server with what to do, and is left as
# it was: not a byte changed, no file next to it (Q90, Q105). A new data dir starts.
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eo.XXXX")
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
trap 'rm -rf "$T"' EXIT

age-keygen -o "$T/age.key" 2>/dev/null
mkdir -p "$T/data"
# what v0.1 left: its migrations counted to 16
python3 -c 'import sqlite3,sys; d=sqlite3.connect(sys.argv[1]); d.executescript("CREATE TABLE apps (name TEXT PRIMARY KEY); INSERT INTO apps VALUES (\"web\"); PRAGMA user_version = 16;"); d.close()' "$T/data/omegacd.db"
before=$(shasum -a 256 "$T/data/omegacd.db")
server() { OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$1 OMEGACD_AGE_KEY_FILE=$T/age.key OMEGACD_ADMIN_PASSWORD=e2e-password-123 exec "$BIN" "${@:2}"; }

server "$T/data" > "$T/old.log" 2>&1 & pid=$!
for _ in $(seq 50); do kill -0 $pid 2>/dev/null || break; sleep 0.2; done
kill $pid 2>/dev/null; wait $pid; rc=$?
check "the server stops on a v0.1 database" '[ $rc -ne 0 ] && grep -q "level=ERROR msg=fatal" "$T/old.log"'
check "saying what it is and what to do" 'grep -q "user_version 16" "$T/old.log" && grep -q "omegacd app export" "$T/old.log" && grep -q "new, empty data directory" "$T/old.log"'
check "the database is left as it was" '[ "$(shasum -a 256 "$T/data/omegacd.db")" = "$before" ] && [ "$(ls "$T/data")" = omegacd.db ]'

mkdir -p "$T/new"
server "$T/new" > "$T/new.log" 2>&1 & pid=$!
for _ in $(seq 50); do curl -sf http://127.0.0.1:18080/api/version >/dev/null && break; sleep 0.2; done
check "with a new data dir it starts" 'curl -sf http://127.0.0.1:18080/api/version >/dev/null'
kill $pid; wait $pid 2>/dev/null

touch "$T/a-file"
(server "$T/a-file/data") > "$T/nodir.log" 2>&1; rc=$?
check "a data dir it can't make stops it, saying which setting" '[ $rc -ne 0 ] && grep -q "data dir: mkdir .*a-file" "$T/nodir.log" && grep -q OMEGACD_DATA "$T/nodir.log"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- old"; tail -5 "$T/old.log"; echo "--- new"; tail -5 "$T/new.log"; }
exit $fail
