#!/usr/bin/env bash
# Tests for scripts/ci/vuln-gate.sh, which fails the build when govulncheck
# reports a finding that has a fixed version. The fixtures are govulncheck
# JSON findings (trimmed to the advisory and fixed version) from the WAL-G
# binary before and after the dependency bump. Run by test/chart/checks.sh.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
GATE=$ROOT/scripts/ci/vuln-gate.sh
DATA=$ROOT/test/ci/testdata
fail() { echo "FAIL: $*"; exit 1; }

echo "-- fixable findings fail, and each is named"
if out=$(bash "$GATE" <"$DATA/govulncheck-fixable.json" 2>&1); then fail "fixable findings passed: $out"; fi
for id in GO-2026-6443 GO-2026-6441 GO-2026-6355 GO-2026-6354 GO-2026-6348; do
  grep -q "$id" <<<"$out" || fail "$id not reported: $out"
done

echo "-- findings without a fix pass, and are listed"
out=$(bash "$GATE" <"$DATA/govulncheck-unfixable.json") || fail "unfixable findings failed: $out"
grep -q 'GO-2022-0646' <<<"$out" || fail "unfixable findings not listed: $out"

echo "-- no findings pass"
bash "$GATE" </dev/null >/dev/null || fail "an empty scan failed"

echo "vuln-gate tests passed"
