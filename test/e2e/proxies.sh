#!/usr/bin/env bash
# End-to-end: deploy/proxy's Caddy, Traefik and nginx overrides in front of deploy/compose.yaml's Control Plane (M6-06,
# Q277-Q280), the files as shipped, side by side: https with http redirected, the login cookie Secure, an Agent
# enrolled over wss:// trusting the proxy's certificate, the client's address taken from X-Forwarded-For only as the
# proxy set it (Q278), the Agent's connection kept through 75 s of quiet, past nginx's 60 s idle timeout (Q51), and
# a recreated Control Plane found again. Certificates: Caddy's own for localhost; Traefik and nginx a test CA's (the
# Traefik's Let's Encrypt resolver pointed nowhere). The binary is OMEGACD_BIN on Linux; elsewhere a Linux build.
set -u # no pipefail: grep -q closing the pipe early must not fail a check
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BIN=${OMEGACD_BIN:-$ROOT/bin/omegacd}
D=${TMPDIR:-/tmp} T=$(mktemp -d "${D%/}/omegacd-e2ep.XXXX")
IMAGE=omegacd-e2e:proxies PROXIES="caddy traefik nginx"
mkdir -p "$T/bc" "$T/tls"
fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
waitfor() { for _ in $(seq $((${2:-30} * 5))); do eval "$1" && return 0; sleep 0.2; done; return 1; } # CMD [SECONDS]
# per proxy, side by side: its own project, network, ports
declare -A NET=([caddy]=172.30.251 [traefik]=172.30.252 [nginx]=172.30.253) HTTPS=([caddy]=18441 [traefik]=18442 [nginx]=18443)
declare -A HTTP=([caddy]=18481 [traefik]=18482 [nginx]=18483) PORT=([caddy]=18091 [traefik]=18092 [nginx]=18093)
dc() { local p=$1; shift; docker compose -p e2e-px-$p --project-directory "$T/$p" -f "$T/$p/compose.yaml" -f "$T/$p/proxy/$p.yaml" -f "$T/$p/test.yaml" "$@"; }
cleanup() {
  for p in $PROXIES; do
    [ -z "${APID[$p]:-}" ] || { kill ${APID[$p]}; wait ${APID[$p]}; } 2>/dev/null
    dc $p down -v >/dev/null 2>&1
  done
  docker rmi $IMAGE >/dev/null 2>&1
  rm -rf "$T"
}
declare -A APID CA TOKEN N
trap cleanup EXIT

# the image: the Dockerfile's last stage around this checkout's Linux binary, as deploy-compose.sh's
LBIN=$BIN
if [ "$(uname)" != Linux ]; then
  LBIN=$T/omegacd-linux
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$(docker version -f '{{.Server.Arch}}') go build -o "$LBIN" ./cmd/omegacd) ||
    { echo "FAIL  build a Linux binary"; exit 1; }
fi
cp "$LBIN" "$T/bc/omegacd"
check "the image builds from the Dockerfile" 'docker buildx build -q --load --build-context go="$T/bc" -t $IMAGE "$ROOT" >/dev/null 2>&1'

# the test CA and localhost's certificate, named as certbot names them
(cd "$T/tls" && openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days 1 -subj /CN=e2e-ca &&
  openssl req -newkey rsa:2048 -nodes -keyout privkey.pem -out front.csr -subj /CN=localhost &&
  printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' > san.cnf &&
  openssl x509 -req -in front.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 1 -extfile san.cnf -out fullchain.pem &&
  chmod 0644 privkey.pem) >/dev/null 2>&1
printf 'tls:\n  stores:\n    default:\n      defaultCertificate: {certFile: /tls/fullchain.pem, keyFile: /tls/privkey.pem}\n' > "$T/tls/traefik.yaml"

for p in $PROXIES; do
  mkdir -p "$T/$p" && cp "$ROOT/deploy/compose.yaml" "$T/$p/" && cp -R "$ROOT/deploy/proxy" "$T/$p/"
  printf 'OMEGACD_IMAGE=%s\nOMEGACD_PORT=%s\nOMEGACD_DOMAIN=localhost\nOMEGACD_PROXY_NET=%s\nOMEGACD_TLS_DIR=%s\nOMEGACD_ACME_EMAIL=e2e@example.com\n' \
    $IMAGE ${PORT[$p]} ${NET[$p]} "$T/tls" > "$T/$p/.env"
  { printf 'services:\n  omegacd:\n    environment: {OMEGACD_LOG_LEVEL: debug}\n'
    printf '  %s:\n    ports: !override ["%s:443", "%s:80"]\n' $p ${HTTPS[$p]} ${HTTP[$p]}
  } > "$T/$p/test.yaml"
  if [ $p = traefik ]; then # its shipped flags, and the test CA's certificate instead of Let's Encrypt's
    cmd=$(dc $p config --format json | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["services"]["traefik"]["command"] +
      ["--providers.file.filename=/tls/traefik.yaml", "--certificatesresolvers.le.acme.caserver=https://127.0.0.1:1/directory"]))')
    printf '    command: !override %s\n    volumes: ["%s:/tls:ro"]\n' "$cmd" "$T/tls" >> "$T/$p/test.yaml"
  fi
  dc $p up -d --quiet-pull > "$T/$p/up.log" 2>&1 &
done
wait
for p in $PROXIES; do
  CA[$p]=$T/tls/ca.pem
  if [ $p = caddy ]; then # Caddy's own CA for localhost, made at its start
    waitfor "dc caddy exec -T caddy cat /data/caddy/pki/authorities/local/root.crt > \"$T/caddy-ca.pem\" 2>/dev/null && [ -s \"$T/caddy-ca.pem\" ]"
    CA[$p]=$T/caddy-ca.pem
  fi
  check "$p: OmegaCD answers over https through it" \
    'waitfor "curl -sf --cacert ${CA[$p]} https://localhost:${HTTPS[$p]}/api/version >/dev/null" 60 || { cat "$T/$p/up.log"; dc $p logs --tail 20; false; }'
done

for p in $PROXIES; do
  U=https://localhost:${HTTPS[$p]} c="curl -s --cacert ${CA[$p]}"
  loc=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' http://localhost:${HTTP[$p]}/)
  check "$p: plain http is redirected to https" 'echo "$loc" | grep -Eq "^30[178] https://localhost"'
  pw=$(dc $p exec -T omegacd cat /data/initial-admin-password) # its first password (Q300)
  hdr=$($c -D - -o /dev/null -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$pw\"}" $U/api/auth/login)
  check "$p: the login cookie is Secure, from its X-Forwarded-Proto" 'echo "$hdr" | grep -i "^set-cookie:" | grep -qi "; *secure"'
  tok=$(echo "$hdr" | sed -n 's/^[Ss]et-[Cc]ookie: *\([^=]*=[^;]*\).*/\1/p' | head -1)
  code=$($c -o /dev/null -w '%{http_code}' -b "$tok" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' \
    -d "{\"currentPassword\":\"$pw\",\"newPassword\":\"$(openssl rand -hex 16)\"}" $U/api/auth/password)
  check "$p: the first login changes the password through it" '[ "$code" = 204 ]'
  # one failed login throttles the client's address for 2 s; a second carrying its own X-Forwarded-For is the same
  # client to OmegaCD, because the proxy sets that header, not the client (Q278)
  $c -o /dev/null -H 'Content-Type: application/json' -d '{"username":"admin","password":"wrong"}' $U/api/auth/login
  code=$($c -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 203.0.113.7' -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"wrong"}' $U/api/auth/login)
  check "$p: a client's own X-Forwarded-For doesn't make it another client" '[ "$code" = 429 ]'
  TOKEN[$p]=$($c -b "$tok" -H 'Content-Type: application/json' -H 'X-Requested-With: omegacd' -d '{"name":"e2e"}' \
    $U/api/settings/tokens | python3 -c 'import json,sys; print(json.load(sys.stdin)["plaintext"])')
  check "$p: an API token works through it (the CLI's, CI's)" \
    '[ "$($c -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${TOKEN[$p]}" $U/api/apps)" = 200 ]'
  # the CLI beside it, on loopback: it has no CA setting, and macOS's Go ignores SSL_CERT_FILE
  cli() { OMEGACD_SERVER=http://127.0.0.1:${PORT[$p]} OMEGACD_TOKEN=${TOKEN[$p]} "$BIN" "$@"; }
  invite=$(cli host add box | grep -o 'omegacd-invite-[0-9a-f]*' | head -1)
  env OMEGACD_AGENT_CONTROL_PLANE=$U OMEGACD_AGENT_CA_FILE=${CA[$p]} OMEGACD_AGENT_INVITE="$invite" \
    OMEGACD_AGENT_STATE_DIR="$T/$p/state" OMEGACD_AGENT_WORKDIR="$T/$p/work" OMEGACD_LOG_LEVEL=debug \
    "$BIN" agent > "$T/$p/agent.log" 2>&1 &
  APID[$p]=$!
  check "$p: an Agent enrolls through it over wss://" 'waitfor "grep -q \"waiting for an admin to approve\" \"$T/$p/agent.log\"" || { cat "$T/$p/agent.log"; false; }'
  cli host approve box "$(grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' "$T/$p/agent.log" | head -1 | cut -d= -f2)" >/dev/null
  check "$p: and is approved" 'waitfor "grep -q \"connected to the Control Plane, approved\" \"$T/$p/agent.log\""'
  seen=$(dc $p logs omegacd 2>&1 | sed -n 's/.*agent connected.* remote=\([^ ]*\).*/\1/p' | tail -1)
  check "$p: OmegaCD sees the client's address from X-Forwarded-For, not the proxy's (Q278)" '[ -n "$seen" ] && [ "$seen" != ${NET[$p]}.2 ]'
  cli host allow box "$seen" >/dev/null
  sleep 1
  check "$p: so host allow keeps the Agent at that address (Q28)" '! dc $p logs omegacd 2>&1 | grep -q "outside the new allowlist"'
  cli host allow box 203.0.113.0/24 >/dev/null
  check "$p: and drops it at another" 'waitfor "dc $p logs omegacd 2>&1 | grep -q \"agent outside the new allowlist: disconnected.*remote=$seen\""'
  cli host allow box >/dev/null # anywhere again: it reconnects
done

# quiet for longer than any proxy's idle timeout: the Agents' pings keep their connections (Q51)
for p in $PROXIES; do
  check "$p: the dropped Agent reconnects once allowed again" \
    'waitfor "[ \$(grep -c \"connected to the Control Plane, approved\" \"$T/$p/agent.log\") -ge 2 ]" 90'
  N[$p]=$(grep -c "connected to the Control Plane, approved" "$T/$p/agent.log")
done
sleep 75
for p in $PROXIES; do
  check "$p: the Agent's connection lasts 75 s of quiet" '[ "$(grep -c "connected to the Control Plane, approved" "$T/$p/agent.log")" = "${N[$p]}" ]'
done

ups=()
for p in $PROXIES; do dc $p up -d --force-recreate omegacd >/dev/null 2>&1 & ups+=($!); done
wait "${ups[@]}" # not the Agents, also this shell's children
for p in $PROXIES; do
  check "$p: a recreated Control Plane is found again, and the Agent reconnects through it" \
    'waitfor "[ \$(grep -c \"connected to the Control Plane, approved\" \"$T/$p/agent.log\") -gt ${N[$p]} ]" 90 || { tail -5 "$T/$p/agent.log"; dc $p logs --tail 10 $p; false; }'
done

if [ $fail -ne 0 ]; then
  for p in $PROXIES; do echo "--- $p"; dc $p logs --tail 15 2>&1; tail -15 "$T/$p/agent.log"; done
fi
[ $fail -eq 0 ] && echo "ALL PASSED"
exit $fail
