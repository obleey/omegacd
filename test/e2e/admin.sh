# shellcheck shell=bash disable=SC2034 # sets variables for the script that sources it
# Sourced by the e2e scripts: the admin's login, as a person makes the first one (Q298, Q302). admin_login API DATA
# reads the generated password from DATA/initial-admin-password (or, DATA empty, takes the one the caller put in
# ADMIN_FIRST_PW) and changes it to ADMIN_PW, as the first login must; after that (a restart on the same data) it logs
# in with ADMIN_PW. Either way it exports OMEGACD_SERVER and OMEGACD_TOKEN, a new API token. Needs T; the session's
# cookie jar is $T/j. The checks that no password is in a log look for both.
admin_login() {
  local api=$1 file=$2/initial-admin-password
  if [ -n "$2" ] && [ -s "$file" ]; then # a new admin, also on new data after a restart
    ADMIN_FIRST_PW=$(cat "$file") ADMIN_PW=
  fi
  if [ -z "${ADMIN_PW:-}" ]; then
    ADMIN_PW=$(openssl rand -hex 16)
    curl -sf -c "$T/j" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$ADMIN_FIRST_PW\"}" "$api/auth/login" >/dev/null
    curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
      -d "{\"currentPassword\":\"$ADMIN_FIRST_PW\",\"newPassword\":\"$ADMIN_PW\"}" "$api/auth/password" >/dev/null
  else
    curl -sf -c "$T/j" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}" "$api/auth/login" >/dev/null
  fi
  export OMEGACD_SERVER=${api%/api}
  export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
    -d "{\"name\":\"e2e-$RANDOM$RANDOM\"}" "$api/settings/tokens" | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
}
