#!/usr/bin/env bash
# Adopting running, hand-started Compose stacks: refused without adopt; an images-only stack keeps its containers,
# a stack mounting a file from its folder is recreated once and keeps serving; named volumes stay.
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2ea.XXXX")
API=http://127.0.0.1:18080/api
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
gitc() { git -C "$T/repo" -c user.name=e2e -c user.email=e2e@local "$@"; }

# two stacks, as someone would have them on a server: a) images + a named volume, b) plus a config file mount
mkdir -p "$T/stacks/adopt-a" "$T/stacks/adopt-b" "$T/repo/adopt-a" "$T/repo/adopt-b"
cat > "$T/stacks/adopt-a/compose.yaml" <<'EOF'
services:
  app:
    image: busybox:1.36
    command: ["sleep", "3600"]
    volumes: [data:/data]
    restart: unless-stopped
volumes:
  data: {}
EOF
cat > "$T/stacks/adopt-b/compose.yaml" <<'EOF'
services:
  web:
    image: nginx:1.29-alpine
    volumes: ["./site.conf:/etc/nginx/conf.d/default.conf:ro"]
    restart: unless-stopped
EOF
# 127.0.0.1, not localhost: inside the container localhost is IPv6 first and this server listens on IPv4
printf 'server { listen 80; location / { return 200 "adopted\\n"; } }\n' > "$T/stacks/adopt-b/site.conf"
for s in adopt-a adopt-b; do (cd "$T/stacks/$s" && docker compose -p $s up -d --quiet-pull >/dev/null 2>&1); cp "$T/stacks/$s"/* "$T/repo/$s/"; done
gitc init -q -b main && gitc add -A && gitc commit -qm stacks

ids() { docker ps -q --no-trunc --filter label=com.docker.compose.project=$1 | sort | tr '\n' ' '; }
serves() { for _ in $(seq 15); do [ "$(docker exec $(docker ps -q --filter label=com.docker.compose.project=adopt-b) wget -qO- http://127.0.0.1/ 2>&1)" = adopted ] && return 0; sleep 1; done; return 1; }
before_a=$(ids adopt-a); before_b=$(ids adopt-b)
check "adopt-b serves before the takeover" serves

age-keygen -o "$T/age.key" 2>/dev/null
mkdir -p "$T/data" "$T/work"
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() {
  for s in adopt-a adopt-b; do "$BIN" app delete $s --volumes --yes >/dev/null 2>&1; docker compose -p $s down -v >/dev/null 2>&1; done
  kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; rm -rf "$T"
}
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')

for s in adopt-a adopt-b; do
  out=$("$BIN" app create $s --repo "$T/repo" --path $s --host local 2>&1)
  check "$s: refused without adopt" 'echo "$out" | grep -q "not managed by omegacd"'
  "$BIN" app create $s --repo "$T/repo" --path $s --host local --adopt >/dev/null 2>&1
  "$BIN" app sync $s --wait --timeout 3m >/dev/null 2>&1
  check "$s: Synced and Healthy after the takeover" '"$BIN" app get $s -o json | python3 -c "import json,sys; a=json.load(sys.stdin); sys.exit(0 if a[\"syncStatus\"]==\"Synced\" and a[\"health\"]==\"Healthy\" else 1)"'
done
after_a=$(ids adopt-a); after_b=$(ids adopt-b)
check "adopt-a (images + named volume) keeps its containers" '[ -n "$before_a" ] && [ "$before_a" = "$after_a" ]'
check "adopt-b (mounts a file from its folder) is recreated once" '[ -n "$after_b" ] && [ "$before_b" != "$after_b" ]'
check "adopt-b serves the same config after the takeover" serves
check "adopt-a's named volume is kept" '[ "$(docker volume ls -q --filter name=adopt-a_data | wc -l | tr -d " ")" = 1 ]'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -12 "$T/server.log"; }
exit $fail
