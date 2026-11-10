#!/usr/bin/env bash
# End-to-end: relative bind mounts not in Git are refused, through a real server, Git repo and Docker.
set -u  # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2e.XXXX")
REPO=$T/repo; ABS=$T/abs-data
mkdir -p "$REPO/good" "$REPO/bad" "$T/data" "$T/work" "$ABS"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }

cat > "$REPO/good/compose.yaml" <<EOF
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "date >> /vol/log; date >> /abs/log; sleep 3600"]
    volumes: ["./conf.txt:/conf.txt:ro", "vol:/vol", "$ABS:/abs"]
volumes:
  vol: {}
EOF
echo hello > "$REPO/good/conf.txt"
cat > "$REPO/bad/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sleep", "3600"]
    volumes: ["./data:/data"]
EOF
git -C "$REPO" init -q -b main && git -C "$REPO" add -A && git -C "$REPO" -c user.name=e2e -c user.email=e2e@local commit -qm init

age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() {
  for a in e2e-good e2e-bad; do "$BIN" app delete $a --volumes --yes >/dev/null 2>&1; done
  kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; rm -rf "$T"
}
trap cleanup EXIT
for _ in $(seq 50); do curl -sf http://127.0.0.1:18080/api/version >/dev/null && break; sleep 0.2; done

J=$T/cookies
curl -sf -c "$J" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' http://127.0.0.1:18080/api/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$J" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' http://127.0.0.1:18080/api/settings/tokens | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["plaintext"])')
check "got an API token" '[ -n "$OMEGACD_TOKEN" ]'

# --- good app: file from Git, named volume, absolute path
"$BIN" app create e2e-good --repo "$REPO" --path good >/dev/null
out=$("$BIN" app sync e2e-good --wait --timeout 2m 2>&1); check "good app syncs" '[ $? -eq 0 ] || ! echo "$out" | grep -qi error'
check "good app Synced"  '"$BIN" app get e2e-good -o json | grep -q "\"Synced\""'
cid=$(docker ps -q --filter label=dev.omegacd.application=e2e-good)
check "good container running" '[ -n "$cid" ]'
sleep 1
"$BIN" app sync e2e-good --wait --timeout 2m >/dev/null 2>&1   # Sync again: data must survive
check "named volume kept its data across Syncs" 'docker exec $cid cat /vol/log | grep -q .'
check "absolute path kept its data across Syncs" '[ -s "$ABS/log" ]'

# --- bad app: ./data not in Git
"$BIN" app create e2e-bad --repo "$REPO" --path bad >/dev/null
diffout=$("$BIN" app diff e2e-bad 2>&1)
check "Diff explains the refusal" 'echo "$diffout" | grep -q "mounts ./data, which is not in Git"'
syncout=$("$BIN" app sync e2e-bad --wait --timeout 2m 2>&1)
check "Sync is refused with the reason" 'echo "$syncout$("$BIN" app get e2e-bad -o json)" | grep -q "mounts ./data, which is not in Git"'
check "no container created for the bad app" '[ -z "$(docker ps -aq --filter label=dev.omegacd.application=e2e-bad)" ]'

# --- ../ escapes the app folder
sed -i.bak 's#./data:/data#../shared:/shared#' "$REPO/bad/compose.yaml" && rm "$REPO/bad/compose.yaml.bak"
git -C "$REPO" -c user.name=e2e -c user.email=e2e@local commit -qam up
"$BIN" app refresh e2e-bad >/dev/null 2>&1
syncout=$("$BIN" app sync e2e-bad --wait --timeout 2m 2>&1)
check "../ mount is refused" 'echo "$syncout$("$BIN" app get e2e-bad -o json)" | grep -q "mounts ../shared, outside"'

# --- fixed in Git: named volume -> syncs
sed -i.bak 's#\["../shared:/shared"\]#["data:/data"]\nvolumes:\n  data: {}#' "$REPO/bad/compose.yaml" && rm "$REPO/bad/compose.yaml.bak"
git -C "$REPO" -c user.name=e2e -c user.email=e2e@local commit -qam fix
"$BIN" app refresh e2e-bad >/dev/null 2>&1
"$BIN" app sync e2e-bad --wait --timeout 2m >/dev/null 2>&1
check "after the fix in Git it syncs" '"$BIN" app get e2e-bad -o json | grep -q "\"Synced\""'
check "and its container runs" '[ -n "$(docker ps -q --filter label=dev.omegacd.application=e2e-bad)" ]'

check "deployed by the built-in Agent, as its log says (M4-04)" 'grep "component=agent" "$T/server.log" | grep -q "msg=deploy"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -20 "$T/server.log"; echo "--- last sync output"; echo "$syncout"; echo "$diffout"; }
exit $fail
