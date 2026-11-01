#!/usr/bin/env bash
# Tests for scripts/release/set-chart-version.sh, the step of the version bump
# workflow that keeps Chart.yaml at the version the release draft will publish.
# Run by test/chart/checks.sh.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SET=$ROOT/scripts/release/set-chart-version.sh
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*"; exit 1; }
fresh() { rm -rf "$WORK/chart"; cp -r "$ROOT/deployments/postgres-ha" "$WORK/chart"; }

echo "-- sets the version from the draft tag and nothing else"
fresh
yq -i '.version = "1.0.0"' "$WORK/chart/Chart.yaml"
cp "$WORK/chart/Chart.yaml" "$WORK/before.yaml"
bash "$SET" v1.2.3 "$WORK/chart" >/dev/null
[ "$(yq -r .version "$WORK/chart/Chart.yaml")" = "1.2.3" ] || fail "Chart.yaml version is not 1.2.3"
[ "$(diff "$WORK/before.yaml" "$WORK/chart/Chart.yaml" | grep -c '^[<>]')" = 2 ] \
  || fail "changed more than the version line"
grep -q '# The PostgreSQL release the chart runs' "$WORK/chart/Chart.yaml" || fail "Chart.yaml lost its comments"

echo "-- leaves the file alone when it is already at that version"
cp "$WORK/chart/Chart.yaml" "$WORK/same.yaml"
touch -t 200001010000 "$WORK/chart/Chart.yaml"
bash "$SET" 1.2.3 "$WORK/chart" >/dev/null
cmp -s "$WORK/same.yaml" "$WORK/chart/Chart.yaml" || fail "rewrote an up to date chart"
[ "$(find "$WORK/chart/Chart.yaml" -newermt 2001-01-01)" = "" ] || fail "touched an up to date chart"

echo "-- does nothing without a draft"
fresh
bash "$SET" "" "$WORK/chart" >/dev/null
diff -r "$ROOT/deployments/postgres-ha" "$WORK/chart" >/dev/null || fail "changed the chart with no draft"

echo "-- refuses a tag that is not a version and leaves the chart alone"
for tag in 1.2 latest untagged-abc; do
  fresh
  if bash "$SET" "$tag" "$WORK/chart" >/dev/null 2>&1; then fail "accepted: $tag"; fi
  diff -r "$ROOT/deployments/postgres-ha" "$WORK/chart" >/dev/null || fail "changed the chart on bad input: $tag"
done

echo "chart version tests passed"
