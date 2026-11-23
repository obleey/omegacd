#!/usr/bin/env bash
# End-to-end: *.sops.* copies leave the work dir, decrypted files stay masked, a changed secret reaches the container.
set -u  # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
. "$(dirname "$0")/admin.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2es.XXXX")
REPO=$T/repo; API=http://127.0.0.1:18080/api; W=
mkdir -p "$REPO/apps/box" "$T/data"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }

age-keygen -o "$T/age.key" 2>/dev/null

OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() { "$BIN" app delete e2e-sec --volumes --yes >/dev/null 2>&1; kill $SRV ${AGENT:-} 2>/dev/null; wait $SRV ${AGENT:-} 2>/dev/null; rm -rf "$T"; }
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
admin_login "$API" "$T/data"
. "$(dirname "$0")/enroll.sh"; enroll box # the Control Plane's machine, a Host like any other (Q104)
W=$AGENT_WORK/e2e-sec
api() { curl -sf -H "Authorization: Bearer $OMEGACD_TOKEN" "$API/apps/e2e-sec/$1"; }
ctr() { docker ps -q --filter label=dev.omegacd.application=e2e-sec; }

# encrypted for the Agent's Host Key, not the Master Key, with the .sops.yaml rule a Host card copies (M5-02, Q166):
# its public half from the keys endpoint, in the rule hostkeys.ts writes
KEYS=$(curl -sf -H "Authorization: Bearer $OMEGACD_TOKEN" $API/settings/hosts/box/keys)
HK=$(echo "$KEYS" | python3 -c 'import json,sys; print([k["recipient"] for k in json.load(sys.stdin) if k["current"]][0])')
check "the keys endpoint has the Host Key the Agent registered" '[ "$HK" = "$RCPT" ]'
cat > "$REPO/.sops.yaml" <<EOF
creation_rules:
  # Host box; the path is up to you: match the files of the Applications deployed to it
  - path_regex: apps/box/.*\\.sops\\.
    age: $HK
EOF
enc() { printf '%s\n' "$1" > "$2"; (cd "$REPO" && sops --encrypt --in-place "$2"); }
cat > "$REPO/apps/box/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sleep", "3600"]
    env_file: app.env
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
EOF
enc 'DB_PASSWORD=first-db-pw' "$REPO/apps/box/.sops.env"
enc 'API_TOKEN=first-token' "$REPO/apps/box/app.sops.env"
gitc init -q -b main && gitc add -A && gitc commit -qm init

"$BIN" app create e2e-sec --repo "$REPO" --path apps/box --host box >/dev/null
"$BIN" app sync e2e-sec --wait --timeout 2m >/dev/null 2>&1
check "work dir has the decrypted files" '[ -f "$W/.env" ] && [ -f "$W/app.env" ]'
check "and no *.sops.* copies" '[ -z "$(ls -A "$W" | grep "\.sops\.")" ]'
check "container got both secrets" '[ "$(docker exec $(ctr) printenv DB_PASSWORD)" = first-db-pw ] && [ "$(docker exec $(ctr) printenv API_TOKEN)" = first-token ]'
R=$(api resources)
check "its containers come with their start time, through the Agent, for uptime counted live (Q179)" 'echo "$R" | python3 -c "
import json,sys; cs=json.load(sys.stdin)[\"containers\"]
assert cs and all(c.get(\"startedAt\", \"\")[:4] > \"2000\" for c in cs), cs"'
check "Resources marks app.env as secret" 'echo "$R" | python3 -c "import json,sys; r=[x for x in json.load(sys.stdin)[\"resources\"] if x[\"name\"]==\"app.env\"]; sys.exit(0 if r and r[0].get(\"secret\") else 1)"'
D=$(api "resource?kind=env_file&name=app.env")
# its key names from the encrypted file in Git, never a value: the Control Plane never has the decrypted file (Q208, Q210)
check "it reads as its key names, API_TOKEN=••••, masked" 'echo "$D" | python3 -c "
import json,sys; d=json.load(sys.stdin); assert d[\"content\"]==\"API_TOKEN=••••\\n\" and d[\"masked\"] and \"app.sops.env\" in d[\"info\"][\"from\"], d"'
check "and .env as DB_PASSWORD=••••" 'api "resource?kind=env_file&name=.env" | grep -q "DB_PASSWORD=••••"'
check "no secret anywhere in the API" '! echo "$R$D$(api diff)" | grep -q "first-"'
check "nor in the Control Plane's data, which the Agent's reports fill (Q210)" '! grep -rqF -e first-db-pw -e first-token "$T/data"'

first=$(ctr)
enc 'API_TOKEN=second-token' "$REPO/apps/box/app.sops.env"; gitc commit -qam "rotate token"
"$BIN" app refresh e2e-sec >/dev/null 2>&1
check "changed secret makes the app OutOfSync" '"$BIN" app get e2e-sec -o json | grep -q "\"OutOfSync\""'
"$BIN" app sync e2e-sec --wait --timeout 2m >/dev/null 2>&1
check "after Sync the container has the new secret" '[ "$(docker exec $(ctr) printenv API_TOKEN)" = second-token ]'
check "it was recreated for it" '[ "$(ctr)" != "$first" ]'
check "still no *.sops.* copies" '[ -z "$(ls -A "$W" | grep "\.sops\.")" ]'
check "app is Synced again" '"$BIN" app get e2e-sec -o json | grep -q "\"Synced\""'

# M5-08: a file encrypted for another key than this Host's fails the Sync, saying which file, the key it's for, this
# Host's key in full and the fix; the message holds no secret (Q215, Q216, Q219)
age-keygen -o "$T/other.key" 2>/dev/null
OTHER=$(grep -o 'age1[a-z0-9]*' "$T/other.key" | head -1)
printf 'STRAY=%s\n' "$(openssl rand -hex 8)" > "$REPO/apps/box/stray.sops.env"
(cd "$REPO" && sops --encrypt --age "$OTHER" --in-place apps/box/stray.sops.env) && gitc add -A && gitc commit -qm "a file for another key"
"$BIN" app refresh e2e-sec >/dev/null 2>&1
out=$("$BIN" app sync e2e-sec --wait --timeout 2m 2>&1); rc=$?
check "app sync --wait fails, naming the file and this Host's key in full, with the fix" \
  '[ $rc -ne 0 ] && echo "$out" | grep -q "stray.sops.env can.t be decrypted on this Host: it.s encrypted for ${OTHER:0:8}…" && echo "$out" | grep -q "not for this Host.s key $HK" && echo "$out" | grep -q "run sops updatekeys on it"'
check "and the failed Sync Operation says the same in the API" '"$BIN" app history e2e-sec -o json | python3 -c "
import json,sys; op=json.load(sys.stdin)[0]; assert op[\"phase\"]==\"Failed\" and \"be decrypted on this Host\" in op[\"message\"] and sys.argv[1] in op[\"message\"], op" "$HK"'
check "the message holds no secret" '! "$BIN" app history e2e-sec -o json | grep -qF -e "$(grep AGE-SECRET-KEY "$T/other.key")" -e AGE-SECRET-KEY'
gitc rm -q apps/box/stray.sops.env && gitc commit -qm "remove it"

"$BIN" app delete e2e-sec --yes >/dev/null 2>&1
check "delete removes its folder, decrypted files and all" '[ ! -e "$W" ]'

op=$(grep -o 'msg="sync started" .* op=[0-9]*' "$T/server.log" | head -1 | grep -o '[0-9]*$')
check "a Sync is followed by its op from the controller through the Agent into the engine (Q62)" \
  '[ -n "$op" ] && grep "msg=\"compose up\"" "$AGENT_LOG" | grep "app=e2e-sec" | grep -q "op=$op "'
check "the logs are at debug and hold no secret (Q62)" 'grep -q level=DEBUG "$T/server.log" && ! grep -qF -e first-db-pw -e first-token -e second-token -e "$ADMIN_FIRST_PW" -e "$ADMIN_PW" -e "$OMEGACD_TOKEN" -e "$(grep AGE-SECRET-KEY "$T/age.key")" -e AGE-SECRET-KEY "$T/server.log" "$AGENT_LOG"'

check "deployed by the Agent of the Control Plane's machine, as its log says (Q104)" 'grep -q "msg=deploy" "$AGENT_LOG"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- agent log"; tail -15 "$AGENT_LOG"; echo "--- server log"; tail -15 "$T/server.log"; }
exit $fail
