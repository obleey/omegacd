#!/usr/bin/env bash
# End-to-end: the secrets helper (M6-08, Q286–Q297). A person's key from omegacd keygen, put on the key list; a
# repository prepared with omegacd secrets init deploys a SOPS secret to its Host, and that person opens and edits it
# with sops; a Host-only rule refused without --host-only; a file encrypted for an outdated set of keys warns; after
# the Host enrolls again with a new Host Key, omegacd secrets rekey makes it deployable and editable again.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2esh.XXXX")
REPO=$T/repo API=http://127.0.0.1:18080/api APP=e2e-sechelp
PW=$(openssl rand -hex 16)
mkdir -p "$REPO/apps/shop" "$T/data"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/master.key OMEGACD_ADMIN_PASSWORD=$PW OMEGACD_POLL_INTERVAL=1h \
  "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() {
  "$BIN" app delete $APP --volumes --yes >/dev/null 2>&1
  kill $SRV ${AGENT:-} 2>/dev/null; wait $SRV ${AGENT:-} 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$T"
}
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"'"$PW"'"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
. "$(dirname "$0")/enroll.sh"; enroll box
ctr() { docker ps -q --filter label=dev.omegacd.application=$APP; }
sopsme() { (cd "$REPO" && SOPS_AGE_KEY_FILE=$KEYS SOPS_AGE_KEY= sops "$@"); }

# a person's key, where sops reads it (Q290); its public half on the key list
export XDG_CONFIG_HOME=$T/me
out=$("$BIN" keygen 2>&1)
KEYS=$T/me/sops/age/keys.txt ME=$(grep -o 'age1[a-z0-9]*' "$KEYS" | head -1)
check "keygen writes a person's key where sops reads it, saying how to add its public half" \
  '[ -s "$KEYS" ] && echo "$out" | grep -qF "omegacd secrets key add NAME $ME"'
check "a second keygen keeps it" '"$BIN" keygen 2>&1 | grep -q "holds a key already" && [ "$(grep -c AGE-SECRET-KEY "$KEYS")" = 1 ]'

SECRET=$(openssl rand -hex 12)
cat > "$REPO/apps/shop/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "echo secret=$$SECRET; sleep 3600"]
    environment:
      SECRET: ${SECRET}
EOF
gitc init -q -b main && gitc remote add origin "$REPO" && gitc add -A && gitc commit -qm compose
"$BIN" app create $APP --repo "$REPO" --path apps/shop --host box > "$T/create.out" 2>&1

out=$(cd "$REPO" && "$BIN" secrets init --host box 2>&1); rc=$?
check "no person's key covers the Host: secrets init refuses a Host-only rule (Q201)" '[ $rc -ne 0 ] && echo "$out" | grep -q -- "--host-only"'
"$BIN" secrets key add me "$ME" >/dev/null
check "secrets key list: the key, for all Hosts" '"$BIN" secrets key list | grep "^me " | grep -q "all Hosts"'
out=$(cd "$REPO" && "$BIN" secrets init --host box 2>&1); rc=$?
check "secrets init writes the Application's rule, for the Host Key and the person's key (Q197, Q286)" \
  '[ $rc -eq 0 ] && grep -qF "path_regex: ^apps/shop/.*\.sops\." "$REPO/.sops.yaml" && grep -qF "age: $RCPT,$ME" "$REPO/.sops.yaml"'

# sops, as a person runs it, by that rule
printf 'SECRET=%s\n' "$SECRET" > "$REPO/apps/shop/.sops.env"
sopsme --encrypt --in-place apps/shop/.sops.env 2> "$T/sops.err"
gitc add -A && gitc commit -qm secret
out=$("$BIN" app sync $APP --wait --timeout 3m 2>&1); rc=$?
check "the prepared repository deploys the secret to its Host" '[ $rc -eq 0 ] && sleep 1 && docker logs $(ctr) 2>&1 | grep -q "secret=$SECRET"'
check "the person opens it" 'sopsme --decrypt apps/shop/.sops.env | grep -q "SECRET=$SECRET"'
NEW=$(openssl rand -hex 12)
sopsme set apps/shop/.sops.env '["SECRET"]' "\"$NEW\"" 2>> "$T/sops.err"
gitc commit -qam edited
"$BIN" app sync $APP --wait --timeout 3m >/dev/null 2>&1
check "and edits it: the new value deploys" 'sleep 1; docker logs $(ctr) 2>&1 | grep -q "secret=$NEW"'
check "no secret value in the Control Plane's or the Agent's log" '! grep -qE "$SECRET|$NEW" "$T/server.log" "$AGENT_LOG"'

# another person's key on the list: the file is encrypted for an outdated set (Q196, Q293)
ANA=$(age-keygen 2>/dev/null | grep -o 'age1[a-z0-9]*')
"$BIN" secrets key add ana "$ANA" --host box >/dev/null
out=$("$BIN" app diff $APP 2>&1)
check "a file encrypted for an outdated set of keys warns, naming it and the rekey" \
  'echo "$out" | grep -q "warning: .sops.env is encrypted for an outdated set of keys (not for ${ANA:0:8}…" && echo "$out" | grep -q "omegacd secrets rekey"'
out=$(cd "$REPO" && "$BIN" secrets rekey 2>&1); rc=$?
check "secrets rekey refreshes the rule and rekeys the file (Q288)" \
  '[ $rc -eq 0 ] && echo "$out" | grep -q "rekeyed     apps/shop/.sops.env" && grep -qF "age: $RCPT,$ANA,$ME" "$REPO/.sops.yaml"'
gitc commit -qam rekeyed
check "after the rekey: no warning" '! "$BIN" app diff $APP 2>&1 | grep -q "outdated set of keys"'

# the Host's keys are lost: it enrolls again with a new state dir, so a new Host Key (Q289); with Applications, by
# taking them out of OmegaCD first (Q127), its containers left running
OLD=$RCPT
kill $AGENT; wait $AGENT 2>/dev/null
mv "$T/box" "$T/box-old"; mv "$T/box.log" "$T/box-old.log"
"$BIN" app delete $APP --forget --yes >/dev/null && "$BIN" host delete box >/dev/null
enroll box
"$BIN" app create $APP --repo "$REPO" --path apps/shop --host box >> "$T/create.out" 2>&1
check "the Host enrolled again with a new Host Key" '[ -n "$RCPT" ] && [ "$RCPT" != "$OLD" ]'
out=$("$BIN" app sync $APP --wait --timeout 3m 2>&1); rc=$?
check "its secret files no longer decrypt there, the Sync saying why (M5-08)" '[ $rc -ne 0 ] && echo "$out" | grep -q "can.t be decrypted on this Host"'
out=$(cd "$REPO" && "$BIN" secrets rekey 2>&1); rc=$?
# ana's key covered only the deleted Host: it covers none now (Q291)
check "secrets rekey with the person's key encrypts them for the new Host Key" '[ $rc -eq 0 ] && grep -qF "age: $RCPT,$ME" "$REPO/.sops.yaml"'
gitc commit -qam "new host key"
out=$("$BIN" app sync $APP --wait --timeout 3m 2>&1); rc=$?
check "deployable again" '[ $rc -eq 0 ] && sleep 1 && docker logs $(ctr) 2>&1 | grep -q "secret=$NEW"'
check "and still editable with the person's key" 'sopsme --decrypt apps/shop/.sops.env | grep -q "SECRET=$NEW"'

# removing a key: the advice to rekey and change the values (Q193); a file only the old Host Key opens is listed (Q200)
out=$("$BIN" secrets key remove ana 2>&1)
check "secrets key remove says to rekey and change the values" 'echo "$out" | grep -q "omegacd secrets rekey" && echo "$out" | grep -q "change the values"'
printf 'X=1\n' > "$T/plain"
SOPS_AGE_RECIPIENTS=$OLD sops --encrypt --input-type dotenv --output-type dotenv "$T/plain" > "$REPO/apps/shop/old.sops.env"
out=$(cd "$REPO" && "$BIN" secrets rekey 2>&1); rc=$?
check "a file only the old Host Key opens: listed with the fix, exit 1 (Q200, Q297)" \
  '[ $rc -eq 1 ] && echo "$out" | grep -q "can.t open  apps/shop/old.sops.env" && echo "$out" | grep -q "write their values again"'

if [ $fail -ne 0 ]; then
  echo "--- server log"; tail -30 "$T/server.log"; echo "--- agent log"; tail -30 "$AGENT_LOG"; echo "--- sops"; cat "$T/sops.err"
  exit 1
fi
echo "ALL PASSED"
