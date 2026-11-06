#!/usr/bin/env bash
# End-to-end: the Control Plane renders through a standalone Source Server over gRPC with mutual TLS (M1-05).
set -u  # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2es.XXXX")
REPO=$T/repo
mkdir -p "$REPO/web" "$T/data" "$T/src-data" "$T/work"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }

cat > "$REPO/web/compose.yaml" <<'EOF'
services:
  web:
    image: busybox:1.36
    command: ["sh", "-c", "cat /conf.txt; sleep 3600"]
    volumes: ["./conf.txt:/conf.txt:ro"]
EOF
echo "from the source server" > "$REPO/web/conf.txt"
git -C "$REPO" init -q -b main && git -C "$REPO" add -A && git -C "$REPO" -c user.name=e2e -c user.email=e2e@local commit -qm init

"$BIN" source-certs --out "$T/tls" 2>/dev/null
check "certificates generated" '[ -f "$T/tls/ca.pem" ] && [ -f "$T/tls/server.key" ] && [ -f "$T/tls/client.key" ]'
OMEGACD_SOURCE_ADDR=127.0.0.1:19090 OMEGACD_DATA=$T/src-data OMEGACD_SOURCE_TLS_DIR=$T/tls \
  "$BIN" source-server > "$T/source.log" 2>&1 &
SRC=$!
age-keygen -o "$T/age.key" 2>/dev/null
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h \
  OMEGACD_SOURCE_SERVER=https://127.0.0.1:19090 OMEGACD_SOURCE_TLS_DIR=$T/tls "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() {
  "$BIN" app delete e2e-src --volumes --yes >/dev/null 2>&1
  kill $SRV $SRC 2>/dev/null; wait $SRV $SRC 2>/dev/null; rm -rf "$T"
}
trap cleanup EXIT
for _ in $(seq 50); do curl -sf http://127.0.0.1:18080/api/version >/dev/null && break; sleep 0.2; done

curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' http://127.0.0.1:18080/api/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' http://127.0.0.1:18080/api/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
check "got an API token" '[ -n "$OMEGACD_TOKEN" ]'
check "the Control Plane uses the standalone Source Server" 'grep -q "using the standalone source server" "$T/server.log"'

"$BIN" app create e2e-src --repo "$REPO" --path web >/dev/null
out=$("$BIN" app sync e2e-src --wait --timeout 2m 2>&1); rc=$?
check "app syncs through the Source Server" '[ $rc -eq 0 ]'
check "the file from Git is mounted" 'docker logs $(docker ps -q --filter label=dev.omegacd.application=e2e-src) 2>&1 | grep -q "from the source server"'
check "the Source Server cloned the repository, the Control Plane did not" '[ -d "$T/src-data/repos" ] && [ ! -d "$T/data/repos" ]'
check "diff works through it" '"$BIN" app diff e2e-src >/dev/null 2>&1'

# mutual TLS: without the client certificate the handshake fails; with it, gRPC answers
check "a client without the certificate is refused" '! curl -s --http2 --cacert "$T/tls/ca.pem" -o /dev/null https://127.0.0.1:19090/'
check "a client with the certificate gets in" 'curl -s --http2 --cacert "$T/tls/ca.pem" --cert "$T/tls/client.pem" --key "$T/tls/client.key" -o /dev/null -w "%{http_code}" https://127.0.0.1:19090/ | grep -q "^[0-9]"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -15 "$T/server.log"; echo "--- source server log"; tail -15 "$T/source.log"; }
exit $fail
