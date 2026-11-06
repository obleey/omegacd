#!/usr/bin/env bash
# The engines the Agent's integration tests run against besides the local Docker (M2-06): rootless Docker and
# rootless Podman, each in a privileged container that sees ROOT at the same path, as an Agent's work dir must be.
#   test/engines.sh up ROOT    starts them (idempotent) and waits until they answer
#   test/engines.sh down       removes them
# They listen on 127.0.0.1:23750 (rootless Docker) and 127.0.0.1:23751 (rootless Podman), without TLS: tests only.
set -eu
case ${1:-} in
up)
  root=${2:?usage: test/engines.sh up ROOT}
  mkdir -p "$root"
  docker inspect omegacd-it-rootless >/dev/null 2>&1 ||
    docker run -d --name omegacd-it-rootless --privileged -p 127.0.0.1:23750:2375 -e DOCKER_TLS_CERTDIR= \
      -v "$root:$root" docker:29-dind-rootless >/dev/null
  docker inspect omegacd-it-podman >/dev/null 2>&1 ||
    docker run -d --name omegacd-it-podman --privileged --user podman -p 127.0.0.1:23751:8888 \
      -v "$root:$root" quay.io/podman/stable:v5 podman system service --time=0 tcp:0.0.0.0:8888 >/dev/null
  for port in 23750 23751; do
    for i in $(seq 120); do DOCKER_HOST=tcp://127.0.0.1:$port docker version >/dev/null 2>&1 && break; sleep 1; done
    DOCKER_HOST=tcp://127.0.0.1:$port docker version >/dev/null || { echo "engine on $port did not come up" >&2; exit 1; }
  done
  ;;
down) docker rm -f omegacd-it-rootless omegacd-it-podman >/dev/null 2>&1 || true ;;
*) echo "usage: test/engines.sh up ROOT | down" >&2; exit 2 ;;
esac
