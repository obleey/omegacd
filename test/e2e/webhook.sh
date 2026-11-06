#!/usr/bin/env bash
# Test 4: a push to a Forgejo repo deploys through the webhook, not polling (poll set to 1h).
# Uses a throwaway OmegaCD and a throwaway Forgejo user + repo, both removed at the end.
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
FJ=${OMEGACD_E2E_FORGEJO:-forgejo-forgejo-1}   # a running Forgejo container, served at FURL
FURL=${OMEGACD_E2E_FORGEJO_URL:-http://git.localhost}
docker inspect "$FJ" >/dev/null 2>&1 || { echo "SKIP  no Forgejo container $FJ (set OMEGACD_E2E_FORGEJO)"; exit 0; }
U=omegacd-e2e
SECRET=e2e-webhook-secret-$RANDOM$RANDOM
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ew.XXXX")
API=http://127.0.0.1:18080/api
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
fj() { docker exec -u git "$FJ" forgejo "$@"; }

# --- Forgejo: test user, token, repo with a compose app
PW=$(openssl rand -hex 12)
fj admin user create --username $U --password "$PW" --email e2e@example.com --must-change-password=false >/dev/null 2>&1
FTOK=$(fj admin user generate-access-token --username $U --token-name e2e-$RANDOM --scopes write:repository,write:user --raw 2>/dev/null | tail -1)
[ -n "$FTOK" ] || { echo "no Forgejo token"; exit 1; }
fapi() { curl -sf -H "Authorization: token $FTOK" -H 'Content-Type: application/json' "$@"; }
fapi -X POST -d '{"name":"hook-test","auto_init":true,"default_branch":"main","private":true}' $FURL/api/v1/user/repos >/dev/null
git clone -q "http://$U:$FTOK@git.localhost/$U/hook-test.git" "$T/wt"
gitc() { git -C "$T/wt" -c user.name=e2e -c user.email=e2e@example.com "$@"; }
cat > "$T/wt/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sleep", "3600"]
EOF
gitc add -A && gitc commit -qm "hook-test app" && gitc push -q origin main

# --- throwaway OmegaCD (only the webhook can make it notice a push: poll 1h)
age-keygen -o "$T/age.key" 2>/dev/null
mkdir -p "$T/data" "$T/work"
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h OMEGACD_WEBHOOK_SECRET=$SECRET "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() {
  "$BIN" app delete hook-test --volumes --yes >/dev/null 2>&1
  kill $SRV 2>/dev/null; wait $SRV 2>/dev/null
  fj admin user delete --username $U --purge >/dev/null 2>&1
  rm -rf "$T"
}
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
# the private repo needs a credential in OmegaCD
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $OMEGACD_TOKEN" -H 'X-Requested-With: omegacd' -H 'Content-Type: application/json' \
  -d "{\"urlPrefix\":\"http://git.localhost/\",\"username\":\"$U\",\"password\":\"$FTOK\"}" $API/settings/repo-credentials)
check "repository credential saved ($code)" '[ "$code" = 201 ] || [ "$code" = 200 ]'
"$BIN" app create hook-test --repo http://git.localhost/$U/hook-test.git --auto-sync >/dev/null || tail -3 "$T/server.log"
# Auto-Sync deploys it right after creation: wait for that instead of starting a second Sync
"$BIN" app wait hook-test --sync --health --timeout 2m >/dev/null 2>&1
check "first deploy (by Auto-Sync)" '"$BIN" app get hook-test -o json | grep -q "\"Synced\""'
first=$(git -C "$T/wt" rev-parse HEAD)

# --- the Forgejo webhook
check "OmegaCD reachable from the Forgejo container" 'docker exec $FJ wget -qO- http://host.docker.internal:18080/healthz | grep -q ok'
HOOK=$(fapi -X POST -d "{\"type\":\"forgejo\",\"active\":true,\"events\":[\"push\"],\"config\":{\"url\":\"http://host.docker.internal:18080/api/webhooks/gitea\",\"content_type\":\"json\",\"secret\":\"$SECRET\"}}" \
  $FURL/api/v1/repos/$U/hook-test/hooks | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
check "webhook created in Forgejo" '[ -n "$HOOK" ]'

# --- push a change and time the deploy
sed -i '' 's/busybox:1.36/busybox:1.37/' "$T/wt/compose.yaml"; gitc commit -qam "bump busybox"
start=$(date +%s); gitc push -q origin main; second=$(git -C "$T/wt" rev-parse HEAD)
deployed=""
for _ in $(seq 60); do
  if "$BIN" app get hook-test -o json | python3 -c "import json,sys; a=json.load(sys.stdin); op=a.get('lastOperation') or {}; sys.exit(0 if op.get('revision')=='$second' and op.get('phase')=='Succeeded' else 1)" 2>/dev/null; then deployed=$(( $(date +%s) - start )); break; fi
  sleep 1
done
check "push deployed by the webhook (no polling) in ${deployed:-?}s" '[ -n "$deployed" ] && [ "$deployed" -le 30 ]'
check "the new image runs" '[ "$(docker inspect -f "{{.Config.Image}}" $(docker ps -q --filter label=dev.omegacd.application=hook-test))" = busybox:1.37 ]'
check "Forgejo recorded a delivered webhook" 'fapi $FURL/api/v1/repos/$U/hook-test/hooks/$HOOK | grep -q "\"active\":true"'

# --- signatures are checked
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forgejo-Event: push' -H 'X-Forgejo-Signature: 00ff' -d '{"repository":{"clone_url":"x"}}' $API/webhooks/gitea)
check "a forged webhook is refused (401)" '[ "$code" = 401 ]'
grep -i "webhook\|sync finished" "$T/server.log" | cut -c1-200 | tail -4

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -15 "$T/server.log"; }
exit $fail
