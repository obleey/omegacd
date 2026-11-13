#!/usr/bin/env bash
# End-to-end tests: each script starts a throwaway OmegaCD (on 127.0.0.1:18080) against the local Docker engine,
# drives it through the CLI and API, and removes everything it created. Run them one at a time (make e2e).
#
# Needs: docker with compose, git, curl, python3, age-keygen, sops; readme-walkthrough.sh fetches this repository
# from GitHub; webhook.sh needs a running Forgejo (OMEGACD_E2E_FORGEJO, default forgejo-forgejo-1; start one with
# docker compose -f test/e2e/forgejo.yaml up -d) and skips without.
# OMEGACD_BIN picks the binary (default bin/omegacd); name scripts to run only those: run.sh secrets adopt
set -u
cd "$(dirname "$0")"
# at debug, the most a log says: the scripts holding known secrets check that none of them is in it (Q62)
export OMEGACD_LOG_LEVEL=${OMEGACD_LOG_LEVEL:-debug}
scripts=("$@")
[ ${#scripts[@]} -gt 0 ] || scripts=(mounts compose-views secrets migrations adopt readme-walkthrough webhook source-server source-paths recovery payloads agent enrollment agent-sync agent-ops agent-self old-database)
failed=()
for s in "${scripts[@]}"; do
	out=$(bash "./${s%.sh}.sh" 2>&1)
	last=$(printf '%s\n' "$out" | tail -1)
	case $last in
	"ALL PASSED") printf '✓ %s\n' "$s" ;;
	SKIP*) printf '· %s: %s\n' "$s" "${last#SKIP  }" ;;
	*)
		printf '✗ %s\n' "$s"
		printf '%s\n' "$out" | grep -E '^(FAIL|PASS)  ' | sed 's/^/    /'
		failed+=("$s")
		;;
	esac
done
[ ${#failed[@]} -eq 0 ] || { echo "failed: ${failed[*]}"; exit 1; }
