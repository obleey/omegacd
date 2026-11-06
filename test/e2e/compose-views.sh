#!/usr/bin/env bash
# End-to-end: Live / Desired / Compose diff through a real server, Git repo, SOPS and Docker.
set -u  # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ec.XXXX")
REPO=$T/repo; API=http://127.0.0.1:18080/api
mkdir -p "$REPO/sec" "$T/data" "$T/work"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$REPO" -c user.name=e2e -c user.email=e2e@local "$@"; }

age-keygen -o "$T/age.key" 2>/dev/null
RCPT=$(age-keygen -y "$T/age.key")
cat > "$REPO/sec/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "echo $$PW_LEN; sleep 3600"]
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
EOF
printf 'DB_PASSWORD=sup3r-s3cret-value\n' > "$T/plain.env"
SOPS_AGE_RECIPIENTS=$RCPT sops --encrypt --input-type dotenv --output-type dotenv "$T/plain.env" > "$REPO/sec/.sops.env"
check "secret is encrypted in Git" '! grep -q sup3r "$REPO/sec/.sops.env"'
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
diffjson() { curl -sf -H "Authorization: Bearer $OMEGACD_TOKEN" $API/apps/e2e-sec/diff; }
field() { python3 -c "import json,sys; print(json.load(sys.stdin)['$1'])"; }

"$BIN" app create e2e-sec --repo "$REPO" --path sec >/dev/null
D=$(diffjson)
check "before the first Sync: Desired shown, Live empty" '[ -n "$(echo "$D" | field desired)" ] && [ -z "$(echo "$D" | field live)" ]'
check "Desired is the compose file exactly as in Git" '[ "$(echo "$D" | field desired)" = "$(cat "$REPO/sec/compose.yaml")" ]'
check "the secret appears nowhere in the response" '! echo "$D" | grep -q sup3r'

"$BIN" app sync e2e-sec --wait --timeout 2m >/dev/null 2>&1
check "container got the real secret" '[ "$(docker exec $(docker ps -q --filter label=dev.omegacd.application=e2e-sec) printenv DB_PASSWORD)" = sup3r-s3cret-value ]'
D=$(diffjson)
check "after Sync: Live equals Desired" '[ "$(echo "$D" | field live)" = "$(echo "$D" | field desired)" ] && [ -n "$(echo "$D" | field live)" ]'
check "Live is the deployed file as written, no secret" '! echo "$D" | grep -q sup3r && [ "$(echo "$D" | field live)" = "$(cat "$REPO/sec/compose.yaml")" ]'

sed -i '' 's/busybox:1.36/busybox:1.37/' "$REPO/sec/compose.yaml"; gitc commit -qam bump
"$BIN" app refresh e2e-sec >/dev/null 2>&1
D=$(diffjson)
check "after a push: Live still the deployed image" 'echo "$D" | field live | grep -q "image: busybox:1.36"'
check "Desired has the new image" 'echo "$D" | field desired | grep -q "image: busybox:1.37"'
check "and nothing else differs" '[ "$(diff <(echo "$D" | field live) <(echo "$D" | field desired) | grep -c "^[<>]")" = 2 ]'

check "no warnings while secrets come from \${VAR}" '[ -z "$(echo "$D" | python3 -c "import json,sys; print(json.load(sys.stdin).get(\"warnings\") or \"\")")" ]'
printf '      API_TOKEN: abc123\n' >> "$REPO/sec/compose.yaml"; gitc commit -qam "plain token"
"$BIN" app refresh e2e-sec >/dev/null 2>&1
D=$(diffjson)
check "a literal API_TOKEN is warned about" 'echo "$D" | python3 -c "import json,sys; w=json.load(sys.stdin).get(\"warnings\") or []; sys.exit(0 if len(w)==1 and w[0].startswith(\"web: API_TOKEN is in plain text in compose.yaml\") else 1)"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -15 "$T/server.log"; echo "$D" | head -c 2000; }
exit $fail
