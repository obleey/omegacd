#!/usr/bin/env bash
# The gate before a push, in one command (make gate): the whole suite with coverage on (make cover-profiles) on every
# supported Linux Host at once (test/linux.sh), then the coverage of all of them merged and checked against main
# (test/coverage: changed lines covered, the total not below test/coverage-floor). One short summary at the end, also in
# cover/gate-summary.txt; the exit code says whether it all passed.
set -u
repo=$(cd "$(dirname "$0")/.." && pwd)
infra=${OMEGACD_TESTINFRA:-$(dirname "$repo")/omegacd-testinfra}
base=${COVER_BASE:-origin/main}
logs=${TMPDIR:-/tmp}
out=$repo/cover/gate-summary.txt
start=$(date +%s)
cd "$repo"
mkdir -p cover
git fetch -q origin 2>/dev/null # the base the changed lines are diffed against

# GATE_CMD replaces the suite, to try this script itself in seconds
test/linux.sh --only "${GATE_CMD:-make cover-profiles ENGINES='rootful rootless podman host-podman'}" "$@" > "$repo/cover/gate.log" 2>&1
suite=$?

rm -rf cover/gate && mkdir -p cover/gate
{
  echo "gate $(git rev-parse --short HEAD) ($(git branch --show-current))  $(( ($(date +%s) - start) / 60 ))m"
  for t in ${*:-ubuntu2404-amd64 ubuntu2404-arm64 debian13-amd64 debian13-arm64}; do
    log=$logs/omegacd-test-$t.log
    if grep -q "=== $t: passed" cover/gate.log; then
      printf '  %-18s passed\n' "$t"
      mkdir -p "cover/gate/$t"
      scp -q -F "$infra/ssh_config" "omegacd-test-$t:omegacd/cover/*.out" "cover/gate/$t/" 2>/dev/null
    else
      why=$(grep -aE -m3 -- '^--- FAIL|_test.go:[0-9]+:|^✗ |^    FAIL  |^make: \*\*\*' "$log" | tr '\n' ' ' | cut -c1-240)
      [ -n "$why" ] || why=$(grep -a "=== $t: FAILED" cover/gate.log | sed 's/.*log): //') # the runner's own reason
      printf '  %-18s FAILED  %s\n' "$t" "$why"
    fi
  done
  if [ $suite -eq 0 ]; then
    go run ./test/coverage -base "$base" -floor test/coverage-floor cover/gate/*/*.out > cover/gate-coverage.txt 2>&1
    cov=$?
    total=$(grep -o '[0-9.]*%  total' cover/gate-coverage.txt | cut -d' ' -f1)
    if [ $cov -eq 0 ]; then
      echo "coverage $total (floor $(cat test/coverage-floor)%) · changed lines: all covered"
    else
      echo "coverage $total (floor $(cat test/coverage-floor)%) · FAILED:"
      sed -n '/^FAIL/,$p' cover/gate-coverage.txt | sed 's/^/  /'
    fi
  else
    cov=1
    echo "coverage: not checked, the suite failed (full logs: $logs/omegacd-test-*.log)"
  fi
} > "$out"
cat "$out"
[ $suite -eq 0 ] && [ $cov -eq 0 ]
