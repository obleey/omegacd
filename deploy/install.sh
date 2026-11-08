#!/bin/sh
# OmegaCD installer: Docker Compose on this server, or Kubernetes with kubectl.
#
#   curl -fsSL https://raw.githubusercontent.com/obleey/omegacd/main/deploy/install.sh | sh
#
# It asks for its settings; for unattended installs set them instead:
#   OMEGACD_TARGET    compose | kubernetes
#   OMEGACD_VERSION   image tag, e.g. 0.1.0 (default latest)
#   compose:          OMEGACD_DIR (/opt/omegacd), OMEGACD_PORT (8080), OMEGACD_WORKDIR (/run/omegacd)
#   kubernetes:       OMEGACD_KUBE_CONTEXT (required when unattended), OMEGACD_NAMESPACE (omegacd)
# Running it again is safe: an existing Master Key (age.key, or the omegacd-keys Secret) is never replaced.
set -eu

RAW=${OMEGACD_RAW:-https://raw.githubusercontent.com/obleey/omegacd/main/deploy}
VERSION=${OMEGACD_VERSION:-latest}
IMAGE=${OMEGACD_IMAGE:-ghcr.io/obleey/omegacd:$VERSION}

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ask VAR "question" default: keeps VAR when set, else asks on the terminal (also when piped from curl)
ask() {
	eval "cur=\${$1:-}"
	if [ -n "$cur" ]; then return; fi
	# shellcheck disable=SC2034 # ans is read by the eval below
	if [ -r /dev/tty ] && [ -w /dev/tty ]; then
		printf '%s [%s]: ' "$2" "$3" >/dev/tty
		read -r ans </dev/tty || ans=
	else
		ans=
	fi
	eval "$1=\${ans:-\$3}"
}

fetch() { # url file
	if have curl; then curl -fsSL "$1" -o "$2"; elif have wget; then wget -qO "$2" "$1"; else die "need curl or wget"; fi
}

# keygen file: a new age key, with whatever is at hand; the OmegaCD image needs nothing installed
keygen() {
	if have omegacd; then
		omegacd keygen --out "$1" 2>/dev/null
	elif have age-keygen; then
		age-keygen -o "$1" 2>/dev/null
	elif have docker; then
		# images before omegacd keygen existed: age-keygen in a throwaway Alpine container
		(umask 077 && { docker run --rm "$IMAGE" keygen 2>/dev/null >"$1" ||
			docker run --rm alpine:3.24 sh -c 'apk add -q --no-cache age >/dev/null && age-keygen' 2>/dev/null >"$1"; })
		grep -q '^AGE-SECRET-KEY-1' "$1" || die "could not create the age key with docker"
	else
		die "cannot create the age key: install docker, age-keygen or the omegacd binary"
	fi
	chmod 600 "$1"
}

pubkey() { sed -n 's/^# public key: //p' "$1"; }

install_compose() {
	have docker || die "Docker is not installed (https://docs.docker.com/engine/install/)"
	docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is missing"
	docker info >/dev/null 2>&1 || die "cannot reach the Docker engine (running? permission for this user?)"

	ask OMEGACD_DIR "Install folder (compose.yaml, .env, age.key)" /opt/omegacd
	ask OMEGACD_PORT "Port for the web UI" 8080
	say "Work dir: where OmegaCD writes each Application's files. /run/omegacd is RAM (decrypted secret"
	say "files never touch disk, restored after a reboot); a disk path like /opt/services keeps them."
	ask OMEGACD_WORKDIR "Work dir" /run/omegacd

	mkdir -p "$OMEGACD_DIR" "$OMEGACD_WORKDIR"
	cd "$OMEGACD_DIR"
	fetch "$RAW/compose.yaml" compose.yaml
	if [ -s age.key ]; then
		say "Keeping the existing Master Key $OMEGACD_DIR/age.key"
	else
		keygen age.key
	fi
	[ -e known_hosts ] || touch known_hosts
	cat >.env <<EOF
OMEGACD_IMAGE=$IMAGE
OMEGACD_PORT=$OMEGACD_PORT
OMEGACD_WORKDIR=$OMEGACD_WORKDIR
EOF
	docker compose up -d --pull missing

	say "Waiting for OmegaCD..."
	i=0
	until docker compose logs omegacd 2>&1 | grep -q "omegacd listening"; do
		i=$((i + 1))
		[ $i -lt 30 ] || die "OmegaCD did not start; see: cd $OMEGACD_DIR && docker compose logs omegacd"
		sleep 2
	done
	pw=$(docker compose logs omegacd 2>&1 | sed -n 's/.*initial admin password.*initial=\([^ ]*\).*/\1/p' | tail -1)

	say ""
	say "OmegaCD is running: http://$(hostname -f 2>/dev/null || hostname):$OMEGACD_PORT"
	if [ -n "$pw" ]; then say "Initial admin password: $pw   (log in as admin; valid until you set a new one on first login)"; fi
	say "Master Key public key (for .sops.yaml): $(pubkey age.key)"
	say "Back up $OMEGACD_DIR/age.key: without it, stored credentials and encrypted secrets cannot be read."
	say "Put OmegaCD behind a TLS reverse proxy before exposing it."
}

install_kubernetes() {
	have kubectl || die "kubectl is not installed"
	ctx=${OMEGACD_KUBE_CONTEXT:-}
	if [ -z "$ctx" ]; then
		[ -r /dev/tty ] || die "set OMEGACD_KUBE_CONTEXT for an unattended install (never guessed)"
		cur=$(kubectl config current-context 2>/dev/null || true)
		[ -n "$cur" ] || die "no current kubectl context"
		ask ctx "Install into Kubernetes context" "$cur"
		ok=
		printf 'Install OmegaCD into context "%s"? [y/N]: ' "$ctx" >/dev/tty
		read -r ok </dev/tty || ok=
		case $ok in y | Y | yes) ;; *) die "aborted" ;; esac
	fi
	ask OMEGACD_NAMESPACE "Namespace" omegacd
	k() { kubectl --context "$ctx" -n "$OMEGACD_NAMESPACE" "$@"; }

	kubectl --context "$ctx" get namespace "$OMEGACD_NAMESPACE" >/dev/null 2>&1 || kubectl --context "$ctx" create namespace "$OMEGACD_NAMESPACE"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	if k get secret omegacd-keys >/dev/null 2>&1; then
		say "Keeping the existing Secret omegacd-keys (Master Key)"
		pub="(unchanged)"
	else
		keygen "$tmp/age.key"
		touch "$tmp/known_hosts"
		k create secret generic omegacd-keys --from-file=age.key="$tmp/age.key" --from-file=known_hosts="$tmp/known_hosts"
		pub=$(pubkey "$tmp/age.key")
		backup="$PWD/omegacd-age.key"
		(umask 077 && cp "$tmp/age.key" "$backup")
		say "A copy of the new Master Key is in $backup: store it somewhere safe, then delete it here."
	fi
	fetch "$RAW/kubernetes.yaml" "$tmp/kubernetes.yaml"
	sed "s#image: ghcr.io/obleey/omegacd:.*#image: $IMAGE#" "$tmp/kubernetes.yaml" | k apply -f -
	k rollout status deploy/omegacd --timeout=180s

	pw=$(k logs deploy/omegacd 2>&1 | sed -n 's/.*initial admin password.*initial=\([^ ]*\).*/\1/p' | tail -1)
	say ""
	say "OmegaCD is running. Reach it with: kubectl --context $ctx -n $OMEGACD_NAMESPACE port-forward svc/omegacd 8080"
	say "(or add the Ingress at the end of kubernetes.yaml)"
	if [ -n "$pw" ]; then say "Initial admin password: $pw   (log in as admin; valid until you set a new one on first login)"; fi
	say "Master Key public key (for .sops.yaml): $pub"
	say "Add your Docker servers under Settings -> Hosts: in Kubernetes there is no local Docker engine."
}

if [ -z "${OMEGACD_TARGET:-}" ]; then
	def=compose
	have docker || { have kubectl && def=kubernetes; }
	ask OMEGACD_TARGET "Install with (compose / kubernetes)" "$def"
fi
case $OMEGACD_TARGET in
compose) install_compose ;;
kubernetes | k8s) install_kubernetes ;;
*) die "OMEGACD_TARGET must be compose or kubernetes" ;;
esac
