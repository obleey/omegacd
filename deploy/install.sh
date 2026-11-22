#!/bin/sh
# OmegaCD installer (M6-01). Each GitHub Release has its own, which installs that release:
#
#   curl -fsSL https://github.com/obleey/omegacd/releases/latest/download/install.sh | sh
#
# full:  the Control Plane with its Source Server, with Docker Compose or into Kubernetes (Q233).
# agent: a Host's Agent (Q232): the .deb's systemd unit where apt is there, as the docker group's omegacd-agent with
#        rootful Docker or as you with rootless Docker or Podman (user unit, lingering); or in a container.
# Run it as the user whose container engine the Agent uses; it calls sudo only for what needs root (Q264).
#
# It asks for what it needs; for unattended installs set it instead (Q263):
#   OMEGACD_MODE                  full | agent
#   full:   OMEGACD_TARGET        compose | kubernetes
#           compose:              OMEGACD_DIR (/opt/omegacd), OMEGACD_PORT (8080)
#           kubernetes:           OMEGACD_KUBE_CONTEXT (required when unattended), OMEGACD_NAMESPACE (omegacd)
#   agent:  OMEGACD_AGENT_CONTROL_PLANE, OMEGACD_AGENT_INVITE   (Add Host fills both in)
#           OMEGACD_AGENT_INSTALL systemd (the default with apt) | container
#           OMEGACD_AGENT_ENGINE  docker | rootless-docker | podman (default: the first found, in that order)
#           container:            OMEGACD_AGENT_DIR (/opt/omegacd-agent; ~/omegacd-agent on a rootless engine), and
#                                 any of deploy/agent.yaml's settings (OMEGACD_AGENT_WORKDIR, OMEGACD_DOCKER_GID, …)
#   OMEGACD_VERSION, OMEGACD_IMAGE, OMEGACD_DOWNLOAD, OMEGACD_RAW: another version, image or place to fetch from
# Running it again upgrades to its version and keeps the keys and settings (Q267).
set -eu

RELEASE=@VERSION@ # the release workflow fills it in (Q266)
case ${OMEGACD_VERSION:-$RELEASE} in
@*) printf 'error: %s\n' "this is the development copy: use the install.sh of a release (https://github.com/obleey/omegacd/releases), or set OMEGACD_VERSION" >&2
	exit 1 ;;
esac
VERSION=${OMEGACD_VERSION:-$RELEASE}
DOWNLOAD=${OMEGACD_DOWNLOAD:-https://github.com/obleey/omegacd/releases/download/$VERSION}
RAW=${OMEGACD_RAW:-https://raw.githubusercontent.com/obleey/omegacd/$VERSION/deploy}
IMAGE=${OMEGACD_IMAGE:-ghcr.io/obleey/omegacd:${VERSION#v}}

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
asroot() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi; }

# ask VAR "question" default: keeps VAR when set, else asks on the terminal (also when piped from curl)
ask() {
	eval "cur=\${$1:-}"
	if [ -n "$cur" ]; then return; fi
	# shellcheck disable=SC2034 # ans is read by the eval below
	if (: >/dev/tty) 2>/dev/null; then # a terminal to ask on, also when piped from curl
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
		(umask 077 && docker run --rm "$IMAGE" keygen 2>/dev/null >"$1")
		grep -q '^AGE-SECRET-KEY-1' "$1" || die "could not create the age key with docker"
	else
		die "cannot create the age key: install docker, age-keygen or the omegacd binary"
	fi
	chmod 600 "$1"
}

install_compose() {
	have docker || die "Docker is not installed (https://docs.docker.com/engine/install/)"
	docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is missing"
	docker info >/dev/null 2>&1 || die "cannot reach the Docker engine (running? permission for this user?)"

	ask OMEGACD_DIR "Install folder (compose.yaml, .env)" /opt/omegacd
	ask OMEGACD_PORT "Port for the web UI" 8080
	mkdir -p "$OMEGACD_DIR"
	cd "$OMEGACD_DIR"
	fetch "$RAW/compose.yaml" compose.yaml
	# the Control Plane makes its Master Key at its first start, in the omegacd-keys volume, and keeps it (Q115)
	cat >.env <<EOF
OMEGACD_IMAGE=$IMAGE
OMEGACD_PORT=$OMEGACD_PORT
EOF
	docker compose up -d --pull missing

	say "Waiting for OmegaCD..."
	i=0
	until docker compose logs omegacd 2>&1 | grep -q "omegacd listening"; do
		i=$((i + 1))
		[ $i -lt 60 ] || die "OmegaCD did not start; see: cd $OMEGACD_DIR && docker compose logs omegacd"
		sleep 2
	done
	pw=$(docker compose logs omegacd 2>&1 | sed -n 's/.*initial admin password.*initial=\([^ ]*\).*/\1/p' | tail -1)

	say ""
	say "OmegaCD $VERSION is running: http://$(hostname -f 2>/dev/null || hostname):$OMEGACD_PORT"
	if [ -n "$pw" ]; then say "Initial admin password: $pw   (log in as admin; valid until you set a new one on first login)"; fi
	say "Back up its Master Key (age.key in the omegacd-keys volume) apart from your backups."
	say "Put OmegaCD behind a TLS reverse proxy before exposing it."
	say "Next: open the UI and add a Host (Hosts -> Add Host), this machine too if Applications should run on it."
}

install_kubernetes() {
	have kubectl || die "kubectl is not installed"
	ctx=${OMEGACD_KUBE_CONTEXT:-}
	if [ -z "$ctx" ]; then
		(: >/dev/tty) 2>/dev/null || die "set OMEGACD_KUBE_CONTEXT for an unattended install (never guessed)"
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
	# kubernetes.yaml mounts the keys Secret read-only, so the Master Key is made here (M6-04 changes both, Q262)
	if k get secret omegacd-keys >/dev/null 2>&1; then
		say "Keeping the existing Secret omegacd-keys (Master Key)"
	else
		keygen "$tmp/age.key"
		touch "$tmp/known_hosts"
		k create secret generic omegacd-keys --from-file=age.key="$tmp/age.key" --from-file=known_hosts="$tmp/known_hosts"
		backup="$PWD/omegacd-age.key"
		(umask 077 && cp "$tmp/age.key" "$backup")
		say "A copy of the new Master Key is in $backup: store it somewhere safe, then delete it here."
	fi
	fetch "$RAW/kubernetes.yaml" "$tmp/kubernetes.yaml"
	sed "s#image: ghcr.io/obleey/omegacd:.*#image: $IMAGE#" "$tmp/kubernetes.yaml" | k apply -f -
	k rollout status deploy/omegacd --timeout=180s

	pw=$(k logs deploy/omegacd 2>&1 | sed -n 's/.*initial admin password.*initial=\([^ ]*\).*/\1/p' | tail -1)
	say ""
	say "OmegaCD $VERSION is running. Reach it with: kubectl --context $ctx -n $OMEGACD_NAMESPACE port-forward svc/omegacd 8080"
	say "(or add the Ingress at the end of kubernetes.yaml)"
	if [ -n "$pw" ]; then say "Initial admin password: $pw   (log in as admin; valid until you set a new one on first login)"; fi
	say "Next: open the UI and add a Host (Hosts -> Add Host): each machine that runs Applications gets an Agent."
}

# --- agent mode

RT=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
# engines: the ones found, in the Agent's own order (Q103); root has no rootless engine
engines() {
	[ -S /var/run/docker.sock ] && printf 'docker '
	[ "$(id -u)" = 0 ] && return
	[ -S "$RT/docker.sock" ] && printf 'rootless-docker '
	{ [ -S "$RT/podman/podman.sock" ] || have podman; } && printf 'podman '
	return 0
}
socket() { # ENGINE: its socket
	case $1 in
	docker) echo /var/run/docker.sock ;;
	rootless-docker) echo "$RT/docker.sock" ;;
	podman) echo "$RT/podman/podman.sock" ;;
	esac
}

# deb: the release's package for this machine, checked against the release's SHA256SUMS, installed or upgraded
deb() {
	arch=$(dpkg --print-architecture)
	name=omegacd_${VERSION}_linux_$arch.deb
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	fetch "$DOWNLOAD/$name" "$tmp/$name"
	fetch "$DOWNLOAD/SHA256SUMS" "$tmp/SHA256SUMS"
	(cd "$tmp" && grep " $name\$" SHA256SUMS | sha256sum -c - >/dev/null) || die "$name doesn't match the release's SHA256SUMS"
	chmod 0755 "$tmp" && chmod 0644 "$tmp/$name" # apt reads it as its own user
	say "Installing $name"
	asroot env DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$tmp/$name" >/dev/null || die "apt could not install $name"
}

# settings FILE: the Agent's settings given, as an env file; nothing when none were given (a rerun keeps the file)
settings() {
	[ -n "${OMEGACD_AGENT_CONTROL_PLANE:-}" ] || return 0
	printf 'OMEGACD_AGENT_CONTROL_PLANE=%s\nOMEGACD_AGENT_INVITE=%s\n' "$OMEGACD_AGENT_CONTROL_PLANE" "${OMEGACD_AGENT_INVITE:-}"
}

# fingerprint CMD...: the Agent's key fingerprint from the log CMD prints, waited for
fingerprint() {
	i=0
	until fp=$("$@" 2>/dev/null | grep -o 'fingerprint=SHA256:[A-Za-z0-9+/]*' | tail -1 | cut -d= -f2) && [ -n "$fp" ]; do
		i=$((i + 1))
		[ $i -lt 30 ] || die "the Agent logged no fingerprint; see its log"
		sleep 1
	done
	say ""
	say "The Agent runs. Approve it in OmegaCD's UI (Hosts) if its fingerprint is: $fp"
}

agent_systemd() {
	have apt-get || die "no apt here: install the Agent in a container (OMEGACD_AGENT_INSTALL=container)"
	deb
	if [ "$ENGINE" = docker ]; then
		if [ -n "${OMEGACD_AGENT_CONTROL_PLANE:-}" ]; then
			settings | asroot install -m 0640 -g omegacd-agent /dev/stdin /etc/omegacd/agent.env
		fi
		asroot test -f /etc/omegacd/agent.env || die "no /etc/omegacd/agent.env: set OMEGACD_AGENT_CONTROL_PLANE and OMEGACD_AGENT_INVITE"
		asroot systemctl start omegacd-agent
		fingerprint asroot journalctl -u omegacd-agent -o cat -n 200
		return
	fi
	# a rootless engine: the package's user units, as this user, from boot without a login
	mkdir -p ~/.config/omegacd
	if [ -n "${OMEGACD_AGENT_CONTROL_PLANE:-}" ]; then
		(umask 077 && { settings; echo "DOCKER_HOST=unix://$(socket "$ENGINE")"; } >~/.config/omegacd/agent.env)
	fi
	[ -f ~/.config/omegacd/agent.env ] || die "no ~/.config/omegacd/agent.env: set OMEGACD_AGENT_CONTROL_PLANE and OMEGACD_AGENT_INVITE"
	loginctl enable-linger "$(id -un)" 2>/dev/null || sudo loginctl enable-linger "$(id -un)"
	if [ "$ENGINE" = podman ]; then
		# Self-Heal reads events from a file, and logs go to capped files (Q246); containers with restart: always come
		# back at boot, after the restore unit put their files back (Q59)
		mkdir -p ~/.config/containers/containers.conf.d
		cp /usr/share/omegacd/containers.conf.d/omegacd-agent.conf ~/.config/containers/containers.conf.d/
		systemctl --user stop podman.service 2>/dev/null || true
		systemctl --user enable --now podman.socket >/dev/null 2>&1
		systemctl --user enable podman-restart.service >/dev/null 2>&1
	fi
	systemctl --user daemon-reload
	systemctl --user enable omegacd-agent-restore.service >/dev/null 2>&1
	systemctl --user enable omegacd-agent.service >/dev/null 2>&1
	systemctl --user restart omegacd-agent.service
	fingerprint journalctl --user -u omegacd-agent -o cat -n 200
}

agent_container() {
	have docker || die "the docker command is missing"
	sock=$(socket "$ENGINE")
	export DOCKER_HOST="unix://$sock"
	docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is missing"
	if [ "$ENGINE" = docker ]; then
		ask OMEGACD_AGENT_DIR "Install folder (agent.yaml, .env)" /opt/omegacd-agent
		gid=${OMEGACD_DOCKER_GID:-$(stat -L -c %g "$sock")}
	else
		# Q252: rootless Docker's host network has a loopback of its own
		case $ENGINE:${OMEGACD_AGENT_CONTROL_PLANE:-} in
		rootless-docker:http://127.* | rootless-docker:http://localhost*)
			die "with rootless Docker the Agent's container can't reach $OMEGACD_AGENT_CONTROL_PLANE: give OmegaCD's https:// address, or install the Agent with systemd (OMEGACD_AGENT_INSTALL=systemd)" ;;
		esac
		ask OMEGACD_AGENT_DIR "Install folder (agent.yaml, .env)" "$HOME/omegacd-agent"
	fi
	mkdir -p "$OMEGACD_AGENT_DIR"
	cd "$OMEGACD_AGENT_DIR"
	fetch "$RAW/agent.yaml" agent.yaml
	if [ -n "${OMEGACD_AGENT_CONTROL_PLANE:-}" ] || [ ! -f .env ]; then
		{
			settings
			echo "OMEGACD_IMAGE=$IMAGE"
			if [ "$ENGINE" = docker ]; then
				echo "OMEGACD_DOCKER_GID=$gid"
			else # the engine's root in a container is this user on the Host (Q240)
				echo "OMEGACD_AGENT_USER=${OMEGACD_AGENT_USER:-0:0}"
				echo "OMEGACD_DOCKER_GID=${OMEGACD_DOCKER_GID:-0}"
				echo "OMEGACD_DOCKER_SOCK=$sock"
				echo "OMEGACD_AGENT_WORKDIR=${OMEGACD_AGENT_WORKDIR:-$RT/omegacd}"
			fi
			[ "$ENGINE" != docker ] || [ -z "${OMEGACD_AGENT_WORKDIR:-}" ] || echo "OMEGACD_AGENT_WORKDIR=$OMEGACD_AGENT_WORKDIR"
		} >.env.new
		(umask 077 && mv .env.new .env)
	else # a rerun: the new version's image, the rest kept
		sed "s#^OMEGACD_IMAGE=.*#OMEGACD_IMAGE=$IMAGE#" .env >.env.new && mv .env.new .env
	fi
	grep -q '^OMEGACD_AGENT_CONTROL_PLANE=' .env || die "set OMEGACD_AGENT_CONTROL_PLANE and OMEGACD_AGENT_INVITE"
	docker compose up -d --pull missing
	fingerprint docker compose logs agent
}

install_agent() {
	ask OMEGACD_AGENT_CONTROL_PLANE "OmegaCD's address, as this machine reaches it (Add Host shows it)" ""
	[ -z "$OMEGACD_AGENT_CONTROL_PLANE" ] || ask OMEGACD_AGENT_INVITE "Invite (from Add Host)" ""
	found=$(engines)
	[ -n "$found" ] || die "no container engine found: install Docker or Podman first (on Debian with its recommended packages, as apt does by default)"
	if [ -z "${OMEGACD_AGENT_ENGINE:-}" ]; then
		first=${found%% *}
		if [ "$found" != "$first " ]; then ask OMEGACD_AGENT_ENGINE "Engine for the Agent (${found% })" "$first"; fi
		ENGINE=${OMEGACD_AGENT_ENGINE:-$first}
	else
		ENGINE=$OMEGACD_AGENT_ENGINE
	fi
	case " $found" in *" $ENGINE "*) ;; *) die "engine $ENGINE not found here (found: ${found% })" ;; esac
	def=container
	have apt-get && def=systemd
	ask OMEGACD_AGENT_INSTALL "Install the Agent with (systemd / container)" "$def"
	say "Installing the Agent $VERSION for $ENGINE ($OMEGACD_AGENT_INSTALL)"
	case $OMEGACD_AGENT_INSTALL in
	systemd) agent_systemd ;;
	container) agent_container ;;
	*) die "OMEGACD_AGENT_INSTALL must be systemd or container" ;;
	esac
}

ask OMEGACD_MODE "Install (full: OmegaCD itself / agent: a Host's Agent)" full
case $OMEGACD_MODE in
full)
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
	;;
agent) install_agent ;;
*) die "OMEGACD_MODE must be full or agent" ;;
esac
