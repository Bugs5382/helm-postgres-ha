#!/usr/bin/env bash
# Tests for scripts/ci/e2e-scope.sh, which decides whether the Checks
# workflow runs the kind e2e suites. Run by test/chart/checks.sh.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SCOPE=$ROOT/scripts/ci/e2e-scope.sh
fail() { echo "FAIL: $*"; exit 1; }

# expect WANT EVENT LABELLED FILES...: the files arrive one per line on stdin.
expect() {
  local want=$1 event=$2 labelled=$3; shift 3
  local got
  got=$(printf '%s\n' "$@" | bash "$SCOPE" "$event" "$labelled" | sed -n 's/^run=//p')
  [ "$got" = "$want" ] || fail "event=$event labelled=$labelled files=[$*]: run=$got, want $want"
}

echo "-- every non-PR event runs the suites"
for ev in push schedule release workflow_dispatch; do expect true "$ev" false; done

echo "-- the e2e label runs them whatever changed"
expect true pull_request true README.md

echo "-- code, chart, e2e, image, module and workflow changes run them"
for f in cmd/pgha/main.go internal/agent/agent.go deployments/postgres-ha/values.yaml \
  test/e2e/run.sh Dockerfile go.mod go.sum .github/workflows/checks.yaml; do
  expect true pull_request false docs/runbooks.md "$f"
done

echo "-- docs and other paths alone skip them"
expect false pull_request false README.md docs/runbooks.md AGENTS.md .github/workflows/job-release.yaml \
  test/chart/checks.sh scripts/release/pin-chart.sh deployments-notes.md xDockerfile
expect false pull_request false

echo "-- the reason is printed"
out=$(printf 'README.md\n' | bash "$SCOPE" pull_request false)
grep -q '^reason=' <<<"$out" || fail "no reason line: $out"

echo "e2e-scope tests passed"
