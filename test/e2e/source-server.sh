#!/usr/bin/env bash
# End-to-end: the Control Plane renders through a standalone Source Server over gRPC with mutual TLS (M1-05), which
# also owns the Repository credentials (M1-06).
set -u  # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2es.XXXX")
REPO=$T/repo
API=http://127.0.0.1:18080/api
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
age-keygen -o "$T/age.key" 2>/dev/null
SRC= SRV=
start_source() {
  OMEGACD_SOURCE_ADDR=127.0.0.1:19090 OMEGACD_DATA=$T/src-data OMEGACD_SOURCE_TLS_DIR=$T/tls \
    "$BIN" source-server >> "$T/source.log" 2>&1 &
  SRC=$!
  for _ in $(seq 50); do curl -s --cacert "$T/tls/ca.pem" https://127.0.0.1:19090/ -o /dev/null; [ $? -ne 7 ] && break; sleep 0.2; done
}
start_control_plane() {
  OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
    OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h \
    OMEGACD_SOURCE_SERVER=https://127.0.0.1:19090 OMEGACD_SOURCE_TLS_DIR=$T/tls "$BIN" >> "$T/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
  curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
  export OMEGACD_SERVER=http://127.0.0.1:18080
  export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
    -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
  H=(-H "Authorization: Bearer $OMEGACD_TOKEN" -H 'X-Requested-With: omegacd' -H 'Content-Type: application/json')
}
stop() { kill "$@" 2>/dev/null; wait "$@" 2>/dev/null; }
cleanup() {
  "$BIN" app delete e2e-src --volumes --yes >/dev/null 2>&1
  stop $SRV $SRC; rm -rf "$T"
}
trap cleanup EXIT
start_source
start_control_plane
check "got an API token" '[ -n "$OMEGACD_TOKEN" ]'
check "the Control Plane uses the standalone Source Server" 'grep -q "using the standalone source server" "$T/server.log"'

"$BIN" app create e2e-src --repo "$REPO" --path web >/dev/null
out=$("$BIN" app sync e2e-src --wait --timeout 2m 2>&1); rc=$?
check "app syncs through the Source Server" '[ $rc -eq 0 ]'
check "the file from Git is mounted" 'docker logs $(docker ps -q --filter label=dev.omegacd.application=e2e-src) 2>&1 | grep -q "from the source server"'
check "the Source Server cloned the repository, the Control Plane did not" '[ -d "$T/src-data/repos" ] && [ ! -d "$T/data/repos" ]'
check "diff works through it" '"$BIN" app diff e2e-src >/dev/null 2>&1'
access=$(curl -sf "${H[@]}" -d "{\"repoURL\":\"$REPO\"}" $API/repos/check)
check "the repository access check goes through it" 'echo "$access" | grep -q "\"access\":\"public\""'

# Repository credentials go through the Control Plane to the Source Server, which keeps them (M1-06)
GITPW=$(openssl rand -hex 12) # a throwaway value, made up per run
code=$(curl -s -o "$T/cred.json" -w '%{http_code}' "${H[@]}" -d '{"urlPrefix":"https://git.example.com/","username":"me","password":"'"$GITPW"'"}' \
  $API/settings/repo-credentials)
check "a credential is saved through the Control Plane ($code)" '[ "$code" = 201 ]'
listed() { curl -sf "${H[@]}" $API/settings/repo-credentials | grep -q '"urlPrefix":"https://git.example.com/".*"hasPassword":true'; }
check "it is listed, without its secret" 'listed && ! curl -sf "${H[@]}" $API/settings/repo-credentials | grep -q "$GITPW"'
check "the Source Server keeps it, encrypted" '[ -s "$T/src-data/credentials.age" ] && ! grep -q "$GITPW" "$T/src-data/credentials.age"'
check "the Control Plane keeps no credential" '[ ! -e "$T/data/credentials.age" ] && ! grep -rq "git.example.com" "$T/data"'

stop $SRC; start_source
check "it survives a Source Server restart" 'listed'

"$BIN" app delete e2e-src --volumes --yes >/dev/null 2>&1
stop $SRV; rm -rf "$T/data"; mkdir -p "$T/data"; start_control_plane
check "losing the Control Plane's data loses no credential" 'listed'

# mutual TLS: without the client certificate the handshake fails; with it, gRPC answers
check "a client without the certificate is refused" '! curl -s --http2 --cacert "$T/tls/ca.pem" -o /dev/null https://127.0.0.1:19090/'
check "a client with the certificate gets in" 'curl -s --http2 --cacert "$T/tls/ca.pem" --cert "$T/tls/client.pem" --key "$T/tls/client.key" -o /dev/null -w "%{http_code}" https://127.0.0.1:19090/ | grep -q "^[0-9]"'

check "the log is at debug and holds no secret (Q62)" 'grep -q level=DEBUG "$T/source.log" && ! grep -qF -e "$GITPW" -e e2e-password-123 "$T/server.log" "$T/source.log"'

check "deployed by the built-in Agent, as its log says (M4-04)" 'grep "component=agent" "$T/server.log" | grep -q "msg=deploy"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -15 "$T/server.log"; echo "--- source server log"; tail -15 "$T/source.log"; }
exit $fail
