#!/usr/bin/env bash
# End-to-end: *.sops.* copies leave the work dir, decrypted files stay masked, a changed secret reaches the container.
set -u  # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2es.XXXX")
REPO=$T/repo; API=http://127.0.0.1:18080/api; W=$T/work/e2e-sec
mkdir -p "$REPO/sec" "$T/data" "$T/work"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }

age-keygen -o "$T/age.key" 2>/dev/null
RCPT=$(age-keygen -y "$T/age.key")
enc() { printf '%s\n' "$1" > "$T/plain"; SOPS_AGE_RECIPIENTS=$RCPT sops --encrypt --input-type dotenv --output-type dotenv "$T/plain" > "$2"; }
cat > "$REPO/sec/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sleep", "3600"]
    env_file: app.env
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
EOF
enc 'DB_PASSWORD=first-db-pw' "$REPO/sec/.sops.env"
enc 'API_TOKEN=first-token' "$REPO/sec/app.sops.env"
gitc init -q -b main && gitc add -A && gitc commit -qm init

OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() { "$BIN" app delete e2e-sec --volumes --yes >/dev/null 2>&1; kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; rm -rf "$T"; }
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
api() { curl -sf -H "Authorization: Bearer $OMEGACD_TOKEN" "$API/apps/e2e-sec/$1"; }
ctr() { docker ps -q --filter label=dev.omegacd.application=e2e-sec; }

"$BIN" app create e2e-sec --repo "$REPO" --path sec >/dev/null
"$BIN" app sync e2e-sec --wait --timeout 2m >/dev/null 2>&1
check "work dir has the decrypted files" '[ -f "$W/.env" ] && [ -f "$W/app.env" ]'
check "and no *.sops.* copies" '[ -z "$(ls -A "$W" | grep "\.sops\.")" ]'
check "decrypted list in the work dir" '[ "$(LC_ALL=C sort "$T/work/.decrypted/e2e-sec" | tr "\n" " ")" = ".env app.env " ]'
check "container got both secrets" '[ "$(docker exec $(ctr) printenv DB_PASSWORD)" = first-db-pw ] && [ "$(docker exec $(ctr) printenv API_TOKEN)" = first-token ]'
R=$(api resources)
check "Resources marks app.env as secret" 'echo "$R" | python3 -c "import json,sys; r=[x for x in json.load(sys.stdin)[\"resources\"] if x[\"name\"]==\"app.env\"]; sys.exit(0 if r and r[0].get(\"secret\") else 1)"'
D=$(api "resource?kind=env_file&name=app.env")
check "its content is masked" 'echo "$D" | grep -q "API_TOKEN=••••" && ! echo "$D" | grep -q first-token'
check "no secret anywhere in the API" '! echo "$R$D$(api diff)" | grep -q "first-"'

first=$(ctr)
enc 'API_TOKEN=second-token' "$REPO/sec/app.sops.env"; gitc commit -qam "rotate token"
"$BIN" app refresh e2e-sec >/dev/null 2>&1
check "changed secret makes the app OutOfSync" '"$BIN" app get e2e-sec -o json | grep -q "\"OutOfSync\""'
"$BIN" app sync e2e-sec --wait --timeout 2m >/dev/null 2>&1
check "after Sync the container has the new secret" '[ "$(docker exec $(ctr) printenv API_TOKEN)" = second-token ]'
check "it was recreated for it" '[ "$(ctr)" != "$first" ]'
check "still no *.sops.* copies" '[ -z "$(ls -A "$W" | grep "\.sops\.")" ]'
check "app is Synced again" '"$BIN" app get e2e-sec -o json | grep -q "\"Synced\""'

"$BIN" app delete e2e-sec --yes >/dev/null 2>&1
check "delete removes the list" '[ ! -e "$T/work/.decrypted/e2e-sec" ] && [ ! -e "$W" ]'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -15 "$T/server.log"; }
exit $fail
