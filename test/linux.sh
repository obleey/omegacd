#!/usr/bin/env bash
# Runs the whole test suite (make test-all) on real Linux, in OrbStack machines: arm64 natively, amd64 through Rosetta.
# Hosts are Linux, and macOS file sharing ignores owners and modes, so permission bugs only show up there.
#   test/linux.sh [arm64|amd64|all]   (default all, both in parallel; machines omegacd-test-<arch> created on first use)
# The repository is shared into the machines at the same path; temp files and the engines live on Linux's own disk.
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
go_version=$(sed -n 's/^go //p' "$repo/go.mod")
sops_version=v3.13.3 # the one in go.mod

provision() { # in the machine, as root
  cat <<EOF
set -eu
export DEBIAN_FRONTEND=noninteractive
for tool in docker make gcc git curl python3 setfacl age-keygen openssl; do
  command -v \$tool >/dev/null || { apt-get update -q && apt-get install -y -q docker.io docker-compose-v2 make gcc git curl python3 acl age openssl >/dev/null; break; }
done
usermod -aG docker "\$1"
systemctl enable --now docker >/dev/null 2>&1 || true
arch=\$(dpkg --print-architecture)
if [ "\$(/usr/local/go/bin/go version 2>/dev/null | cut -d' ' -f3)" != go$go_version ]; then
  rm -rf /usr/local/go && curl -fsSL https://go.dev/dl/go$go_version.linux-\$arch.tar.gz | tar -C /usr/local -xz
fi
command -v sops >/dev/null || { curl -fsSLo /usr/local/bin/sops https://github.com/getsops/sops/releases/download/$sops_version/sops-$sops_version.linux.\$arch && chmod +x /usr/local/bin/sops; }
EOF
}

run() {
  local arch=$1 m=omegacd-test-$1
  orb list -q 2>/dev/null | grep -qx "$m" || orb create --arch "$arch" ubuntu:24.04 "$m"
  orb -m "$m" -u root bash -c "$(provision)" provision "$(orb -m "$m" whoami)"
  if [ "$arch" = amd64 ] && [ "$(uname -m)" = arm64 ]; then
    # amd64 through Rosetta: systemd can't create containers' cgroup scopes there ("is a kernel thread: Inappropriate
    # ioctl"), so Docker uses cgroupfs; a restart applies it (systemd can't stop dockerd cleanly under Rosetta either)
    orb -m "$m" -u root bash -c 'echo "{\"exec-opts\": [\"native.cgroupdriver=cgroupfs\"]}" > /etc/docker/daemon.json'
    if [ "$(orb -m "$m" docker info --format '{{.CgroupDriver}}' 2>/dev/null)" != cgroupfs ]; then
      orb restart "$m" >/dev/null
      orb -m "$m" bash -c 'for i in $(seq 60); do docker info >/dev/null 2>&1 && exit; sleep 1; done; exit 1'
    fi
  fi
  local engines="rootful rootless podman"
  # rootless Podman's networking (pasta) can't run under Rosetta; Podman on real amd64 is CI's amd64 runner
  [ "$arch" = amd64 ] && [ "$(uname -m)" = arm64 ] && engines="rootful rootless"
  echo "=== $arch: make test-all (engines: $engines)"
  # a login shell for the docker group; TMPDIR on Linux's own disk, not the shared repository
  orb -m "$m" bash -lc "export PATH=/usr/local/go/bin:\$PATH TMPDIR=/tmp; cd '$repo' && make test-all ENGINES='$engines'"
}

case ${1:-all} in
arm64 | amd64) run "$1" ;;
all) # both at once: separate machines, nothing shared but the repository; each one's output in its own log
  logs=${TMPDIR:-/tmp}
  run arm64 > "$logs/omegacd-test-arm64.log" 2>&1 & a=$!
  run amd64 > "$logs/omegacd-test-amd64.log" 2>&1 & b=$!
  rc=0
  for arch_pid in arm64:$a amd64:$b; do
    if wait "${arch_pid#*:}"; then result=passed; else result=FAILED; rc=1; fi
    echo "=== ${arch_pid%%:*}: $result (log: $logs/omegacd-test-${arch_pid%%:*}.log)"
  done
  exit $rc
  ;;
*) echo "usage: test/linux.sh [arm64|amd64|all]" >&2; exit 2 ;;
esac
