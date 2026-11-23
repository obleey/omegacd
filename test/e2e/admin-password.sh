#!/usr/bin/env bash
# End-to-end: the admin's first password (M6-11, Q298–Q315). Generated into the data dir's initial-admin-password
# (0600), only its path logged; OMEGACD_ADMIN_PASSWORD ignored; the first login must change it, which deletes the file;
# omegacd reset-admin-password beside the running server writes a new one, ends every session and leaves API tokens.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eap.XXXX")" && pwd) # no "//" from a TMPDIR ending in /: the paths are compared
API=http://127.0.0.1:18080/api FILE=$T/data/initial-admin-password
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
settings() { exec env OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key "$@"; }
start() { (settings "$@" "$BIN" >> "$T/server.log" 2>&1) & SRV=$!; for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done; }
stop() { kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; }
cleanup() { stop; [ -n "${KEEP:-}" ] || rm -rf "$T"; }
trap cleanup EXIT
login() { curl -s -o /dev/null -w '%{http_code}' -c "$T/${2:-j}" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$1\"}" $API/auth/login; }
as() { curl -s -o /dev/null -w '%{http_code}' -b "$T/$1" -H 'X-Requested-With: omegacd' "${@:2}"; }
change() { as "$1" -H 'Content-Type: application/json' -d "{\"currentPassword\":\"$2\",\"newPassword\":\"$3\"}" $API/auth/password; }

GIVEN=$(openssl rand -hex 12)
start OMEGACD_ADMIN_PASSWORD="$GIVEN" # Q301: no longer read
FIRST=$(cat "$FILE" 2>/dev/null)
check "the first start writes a generated password to the data dir's initial-admin-password, mode 0600 (Q300)" \
  '[ ${#FIRST} -eq 20 ] && [ "$(python3 -c "import os,sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o777))" "$FILE")" = 0o600 ]'
check "its log says where it is, never the password (Q300, Q312)" \
  'grep -q "the initial admin password is in this file.*path=$FILE" "$T/server.log" && ! grep -qF "$FIRST" "$T/server.log"'
sleep 2.1 # one failed login holds the client off for 2 s
check "OMEGACD_ADMIN_PASSWORD is ignored (Q301)" '[ "$(login "$GIVEN")" = 401 ]'
sleep 2.1
check "the file's password logs in" '[ "$(login "$FIRST")" = 200 ]'
check "and must be changed before anything else (Q298)" '[ "$(as j $API/apps)" = 403 ]'
NEW=$(openssl rand -hex 12)
check "the first login changes it" '[ "$(change j "$FIRST" "$NEW")" = 204 ] && [ "$(as j $API/apps)" = 200 ]'
check "which deletes the file, saying so (Q300)" '[ ! -e "$FILE" ] && grep -q "the initial password file is deleted" "$T/server.log"'
TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' -d '{"name":"ci"}' $API/settings/tokens |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')

stop; : > "$T/server.log"; start
check "a restart after the change says nothing of a first password (Q312)" '! grep -q "level=WARN.*initial admin password" "$T/server.log"'
login "$NEW" k >/dev/null
out=$( (settings "$BIN" reset-admin-password) 2>&1); rc=$?
RESET=$(cat "$FILE" 2>/dev/null)
check "reset-admin-password beside the running server writes a new one, naming the file, not the password (Q311)" \
  '[ $rc -eq 0 ] && echo "$out" | grep -qF "A new admin password is in $FILE" && [ ${#RESET} -eq 20 ] && ! echo "$out" | grep -qF "$RESET"'
check "every session ended" '[ "$(as k $API/auth/me)" = 401 ]'
check "an API token still works" '[ "$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" $API/apps)" = 200 ]'
check "the old password no longer logs in" '[ "$(login "$NEW")" = 401 ]'
sleep 2.1
check "the new one does, and must be changed" '[ "$(login "$RESET")" = 200 ] && [ "$(as j $API/apps)" = 403 ]'
stop; : > "$T/server.log"; start
check "until then every start says where it is (Q312)" 'grep -q "the initial admin password is in this file.*path=$FILE" "$T/server.log"'
out=$(OMEGACD_DATA=$T/none OMEGACD_AGE_KEY_FILE=$T/age.key "$BIN" reset-admin-password 2>&1); rc=$?
check "away from the server's data it says so" '[ $rc -ne 0 ] && echo "$out" | grep -q "no OmegaCD database"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -20 "$T/server.log"; }
exit $fail
