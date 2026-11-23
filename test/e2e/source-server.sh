#!/usr/bin/env bash
# End-to-end: the Control Plane renders through a standalone Source Server over gRPC with mutual TLS (M1-05), which
# also owns the Repository credentials (M1-06).
set -u  # no pipefail: grep -q closing the pipe early must not fail a check
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
. "$(dirname "$0")/admin.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2es.XXXX")
REPO=$T/repo
API=http://127.0.0.1:18080/api
mkdir -p "$REPO/web" "$T/data" "$T/src-data"
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
  OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_AGE_KEY_FILE=$T/age.key \
    OMEGACD_POLL_INTERVAL=1h \
    OMEGACD_SOURCE_SERVER=https://127.0.0.1:19090 OMEGACD_SOURCE_TLS_DIR=$T/tls "$BIN" >> "$T/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
  admin_login "$API" "$T/data"
  H=(-H "Authorization: Bearer $OMEGACD_TOKEN" -H 'X-Requested-With: omegacd' -H 'Content-Type: application/json')
}
stop() { kill "$@" 2>/dev/null; wait "$@" 2>/dev/null; }
FJ=${OMEGACD_E2E_FORGEJO:-forgejo-forgejo-1}   # a running Forgejo container, served at FURL, for a private repository
FURL=${OMEGACD_E2E_FORGEJO_URL:-http://git.localhost}
FU=omegacd-e2e-src
fj() { docker exec -u git "$FJ" forgejo "$@"; }
cleanup() {
  docker inspect "$FJ" >/dev/null 2>&1 && fj admin user delete --username $FU --purge >/dev/null 2>&1
  "$BIN" app delete e2e-src --volumes --yes >/dev/null 2>&1
  stop $SRV $SRC ${AGENT:-}; rm -rf "$T"
}
trap cleanup EXIT
start_source
start_control_plane
. "$(dirname "$0")/enroll.sh"; enroll box # the Control Plane's machine, a Host like any other (Q104)
check "got an API token" '[ -n "$OMEGACD_TOKEN" ]'
check "the Control Plane uses the standalone Source Server" 'grep -q "using the standalone source server" "$T/server.log"'

"$BIN" app create e2e-src --repo "$REPO" --path web --host box >/dev/null
out=$("$BIN" app sync e2e-src --wait --timeout 2m 2>&1); rc=$?
check "app syncs through the Source Server" '[ $rc -eq 0 ]'
check "the file from Git is mounted" 'docker logs $(docker ps -q --filter label=dev.omegacd.application=e2e-src) 2>&1 | grep -q "from the source server"'
check "the Source Server cloned the repository, the Control Plane did not" '[ -d "$T/src-data/repos" ] && [ ! -d "$T/data/repos" ]'
check "diff works through it" '"$BIN" app diff e2e-src >/dev/null 2>&1'
op=$(grep -o 'msg="sync started" .* op=[0-9]*' "$T/server.log" | head -1 | grep -o '[0-9]*$')
check "a Sync's op reads through the Control Plane's, the Source Server's and the Agent's logs (Q188, Q204)" \
  '[ -n "$op" ] && grep -E "op=$op( |\$)" "$T/source.log" | grep -q "app=e2e-src" && grep "op=$op " "$AGENT_LOG" | grep -q "msg=deploy"'
st=$(curl -sf "${H[@]}" $API/settings/source)
check "Settings shows it standalone, reachable, its version and cache (Q156, Q202)" 'echo "$st" | python3 -c "
import json,sys; s=json.load(sys.stdin)
assert s[\"mode\"]==\"standalone\" and s[\"url\"]==\"https://127.0.0.1:19090\" and s[\"reachable\"] and s[\"version\"] and s[\"repositories\"]>=1 and s[\"repoCacheBytes\"]>0, s"'
"$BIN" app create e2e-src-bad --repo "$REPO" --path nowhere --host box >/dev/null 2>&1
"$BIN" app sync e2e-src-bad --wait --timeout 1m >/dev/null 2>&1
check "the Source Server logs a render it can't make at Warn, with the reason (Q136)" \
  'grep "level=WARN" "$T/source.log" | grep "source server request failed" | grep nowhere | grep -q "not found"'
"$BIN" app delete e2e-src-bad --yes >/dev/null 2>&1
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
# rotated: the same credential with a new token; deleted: gone from the Source Server (Q205)
CID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$T/cred.json")
GITPW2=$(openssl rand -hex 12)
code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "${H[@]}" -d '{"urlPrefix":"https://git.example.com/","username":"me","password":"'"$GITPW2"'"}' \
  $API/settings/repo-credentials/$CID)
check "rotated: its token replaced through the Control Plane ($code)" '[ "$code" = 200 ] && listed'
check "and neither token is anywhere in the Control Plane's data" '! grep -rqF -e "$GITPW" -e "$GITPW2" "$T/data"'
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "${H[@]}" $API/settings/repo-credentials/$CID)
check "deleted: gone from the Source Server ($code)" '[ "$code" = 204 ] && ! listed'
code=$(curl -s -o "$T/cred.json" -w '%{http_code}' "${H[@]}" -d '{"urlPrefix":"https://git.example.com/","username":"me","password":"'"$GITPW"'"}' \
  $API/settings/repo-credentials) # again, for what follows

# the Source Server uses a credential the Control Plane never sees: a private Forgejo repository (Q205)
if docker inspect "$FJ" >/dev/null 2>&1; then
  fj admin user delete --username $FU --purge >/dev/null 2>&1 # a run killed before its cleanup
  fj admin user create --username $FU --password "$(openssl rand -hex 12)" --email src@example.com --must-change-password=false >/dev/null 2>&1
  FTOK=$(fj admin user generate-access-token --username $FU --token-name e2e-$RANDOM --scopes write:repository,write:user --raw 2>/dev/null | tail -1)
  curl -sf -H "Authorization: token $FTOK" -H 'Content-Type: application/json' \
    -d '{"name":"private-src","auto_init":true,"default_branch":"main","private":true}' $FURL/api/v1/user/repos >/dev/null
  PRIV=$FURL/$FU/private-src.git
  check "Forgejo made a private repository for it" '[ -n "$FTOK" ] && curl -sf -H "Authorization: token $FTOK" $FURL/api/v1/repos/$FU/private-src | grep -q "\"private\":true"'
  check "without a credential, the private repository is denied through it" \
    'curl -sf "${H[@]}" -d "{\"repoURL\":\"$PRIV\"}" $API/repos/check | grep -q "\"access\":\"denied\""'
  curl -sf -o /dev/null "${H[@]}" -d '{"urlPrefix":"'"$FURL/$FU/"'","username":"'"$FU"'","password":"'"$FTOK"'"}' $API/settings/repo-credentials
  check "with one, the standalone Source Server reads it" \
    'curl -sf "${H[@]}" -d "{\"repoURL\":\"$PRIV\"}" $API/repos/check | grep -q "\"access\":\"credential\",\"credentialPrefix\":\"$FURL/$FU/\""'
  check "and the Forgejo token is nowhere in the Control Plane's data" '! grep -rqF "$FTOK" "$T/data"'
else
  echo "SKIP  a private Forgejo repository through it: no Forgejo container $FJ (set OMEGACD_E2E_FORGEJO)"
fi

stop $SRC; start_source
check "it survives a Source Server restart" 'listed'

"$BIN" app delete e2e-src --volumes --yes >/dev/null 2>&1
stop $SRV; rm -rf "$T/data"; mkdir -p "$T/data"; start_control_plane
check "losing the Control Plane's data loses no credential" 'listed'

# mutual TLS: without the client certificate the handshake fails; with it, gRPC answers
check "a client without the certificate is refused" '! curl -s --http2 --cacert "$T/tls/ca.pem" -o /dev/null https://127.0.0.1:19090/'
check "a client with the certificate gets in" 'curl -s --http2 --cacert "$T/tls/ca.pem" --cert "$T/tls/client.pem" --key "$T/tls/client.key" -o /dev/null -w "%{http_code}" https://127.0.0.1:19090/ | grep -q "^[0-9]"'

check "the logs are at debug and hold no secret (Q62)" 'grep -q level=DEBUG "$T/source.log" && ! grep -qF -e "$GITPW" -e "$GITPW2" -e "${FTOK:-$GITPW}" -e "$ADMIN_FIRST_PW" -e "$ADMIN_PW" "$T/server.log" "$T/source.log" "$AGENT_LOG"'

check "deployed by the Agent of the Control Plane's machine, as its log says (Q104)" 'grep -q "msg=deploy" "$AGENT_LOG"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- agent log"; tail -15 "$AGENT_LOG"; echo "--- server log"; tail -15 "$T/server.log"; echo "--- source server log"; tail -15 "$T/source.log"; }
exit $fail
