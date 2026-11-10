#!/usr/bin/env bash
# The README's "Your first Application", word for word, against GitHub, on a throwaway OmegaCD.
set -u
BIN=${OMEGACD_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/omegacd}
T=$(mktemp -d "${TMPDIR:-/tmp}/omegacd-e2eh.XXXX")
API=http://127.0.0.1:18080/api
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
# a page that answers within 10 s: "Healthy" without a healthcheck means running, not yet serving
serves() { for _ in $(seq 20); do curl -s --max-time 2 "$1" | grep -q "$2" && return 0; sleep 0.5; done; return 1; }

age-keygen -o "$T/age.key" 2>/dev/null
mkdir -p "$T/data" "$T/work"
OMEGACD_ADDR=127.0.0.1:18080 OMEGACD_DATA=$T/data OMEGACD_WORKDIR=$T/work OMEGACD_AGE_KEY_FILE=$T/age.key \
  OMEGACD_ADMIN_PASSWORD=e2e-password-123 OMEGACD_POLL_INTERVAL=1h "$BIN" > "$T/server.log" 2>&1 &
SRV=$!
cleanup() {
  for a in hello whoami examples; do "$BIN" app delete $a --volumes --yes >/dev/null 2>&1; done
  kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; rm -rf "$T"
}
trap cleanup EXIT
for _ in $(seq 50); do curl -sf $API/version >/dev/null && break; sleep 0.2; done
curl -sf -c "$T/j" -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-123"}' $API/auth/login >/dev/null
export OMEGACD_SERVER=http://127.0.0.1:18080
export OMEGACD_TOKEN=$(curl -sf -b "$T/j" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
  -d '{"name":"e2e"}' $API/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')

# --- the README's CLI commands, verbatim
"$BIN" app create hello --repo https://github.com/obleey/omegacd --path examples/hello --auto-sync >/dev/null
"$BIN" app wait hello --sync --health --timeout 3m >/dev/null 2>&1
check "hello Synced and Healthy" '"$BIN" app get hello -o json | python3 -c "import json,sys; a=json.load(sys.stdin); sys.exit(0 if a[\"syncStatus\"]==\"Synced\" and a[\"health\"]==\"Healthy\" else 1)"'
check "the page from Git is served on :8081" 'serves http://localhost:8081/ "Hello from OmegaCD"'
"$BIN" app delete hello --yes >/dev/null 2>&1
for _ in $(seq 30); do [ -z "$(docker ps -aq --filter label=dev.omegacd.application=hello)" ] && break; sleep 1; done
# OrbStack's macOS file sharing caches a path's file details for a moment after it is deleted; recreating the
# same app path within a second can mount a stale file (reproduced with plain docker run, Linux hosts unaffected)
sleep 5

# --- the declarative way: an App of Apps on examples/ creates hello from apps.yaml
"$BIN" app create examples --repo https://github.com/obleey/omegacd --path examples --kind apps >/dev/null
"$BIN" app sync examples --wait --timeout 3m >/dev/null 2>&1
check "App of Apps created hello from apps.yaml" '"$BIN" app get hello -o json | python3 -c "import json,sys; a=json.load(sys.stdin); sys.exit(0 if a[\"parent\"]==\"examples\" and a[\"autoSync\"] and a[\"selfHeal\"] else 1)"'
"$BIN" app wait hello --sync --health --timeout 3m >/dev/null 2>&1
check "and it serves the page again" 'serves http://localhost:8081/ "Hello from OmegaCD"'
if [ $fail -ne 0 ]; then
  echo "--- diagnostics"
  echo "page: $(curl -s --max-time 2 -o /dev/null -w '%{http_code}' http://localhost:8081/) $(curl -s --max-time 2 http://localhost:8081/ | head -c 120)"
  cid=$(docker ps -q --filter label=dev.omegacd.application=hello)
  echo "container: $cid $(docker inspect -f '{{.State.Status}} created={{.Created}}' $cid)"
  docker inspect -f '{{range .Mounts}}mount {{.Source}} -> {{.Destination}}{{"\n"}}{{end}}' $cid | sed "s#$T#\$T#"
  echo "work dir: $(ls -la $T/work/hello 2>&1 | tr '\n' ' ' | sed "s#$T#\$T#g")"
  docker exec $cid ls -la /usr/share/nginx/html/ 2>&1 | tail -3
  grep "hello" "$T/server.log" | grep -v "INFO http" | cut -c1-200
fi
check "whoami (Auto-Sync off) was created as a child" '"$BIN" app get whoami -o json | python3 -c "import json,sys; a=json.load(sys.stdin); sys.exit(0 if a[\"parent\"]==\"examples\" and not a[\"autoSync\"] else 1)"'
"$BIN" app wait whoami --sync --health --timeout 3m >/dev/null 2>&1
check "the App of Apps Sync also deployed whoami on :8082" 'serves http://localhost:8082/ Hostname'

check "deployed by the built-in Agent, as its log says (M4-04)" 'grep "component=agent" "$T/server.log" | grep -q "msg=deploy"'

[ $fail -eq 0 ] && echo "ALL PASSED" || { echo "--- server log"; tail -12 "$T/server.log"; }
exit $fail
