#!/usr/bin/env bash
# Tests for scripts/release/pin-chart.sh, the step of the release workflow
# that writes the release version and the published agent image digest into
# the chart before it is packaged. Run by test/chart/checks.sh.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
PIN=$ROOT/scripts/release/pin-chart.sh
DIGEST=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*"; exit 1; }
fresh() { rm -rf "$WORK/chart"; cp -r "$ROOT/deployments/postgres-ha" "$WORK/chart"; }
render() {
  helm template pg "$WORK/chart" -f "$WORK/chart/ci/full-values.yaml" --api-versions monitoring.coreos.com/v1 "$@"
}
images() { render "$@" | yq -N -r '.. | select(tag == "!!map" and has("image")) | .image' | grep -v '^null$' | sort -u; }

echo "-- pins the version and the agent digest"
fresh
bash "$PIN" v1.2.3 "$DIGEST" "$WORK/chart" >/dev/null
[ "$(yq -r .version "$WORK/chart/Chart.yaml")" = "1.2.3" ] || fail "Chart.yaml version is not 1.2.3"
render | grep -q "image: ghcr.io/bugs5382/helm-postgres-ha/pgha:1.2.3@$DIGEST" \
  || fail "the agent image is not pinned to the release tag and digest"
grep -q '# -- Defaults to the chart version.' "$WORK/chart/values.yaml" || fail "values.yaml lost its comments"

echo "-- every rendered image is pinned by digest once the agent is"
all=$(images)
[ "$(printf '%s\n' "$all" | wc -l)" -ge 4 ] || fail "too few images rendered to check: $all"
unpinned=$(printf '%s\n' "$all" | grep -v '@sha256:[0-9a-f]\{64\}$' || true)
[ -z "$unpinned" ] || fail "images without a digest: $unpinned"
unpinned=$(images --set backup.enabled=true --set backup.s3.prefix=s3://b/p --set backup.s3.existingSecret=s3 \
  | grep -v '@sha256:[0-9a-f]\{64\}$' || true)
[ -z "$unpinned" ] || fail "images without a digest with backups on: $unpinned"

echo "-- the PostgreSQL image is the major the chart declares"
app=$(yq -r .appVersion "$ROOT/deployments/postgres-ha/Chart.yaml")
tag=$(yq -r .image.tag "$ROOT/deployments/postgres-ha/values.yaml")
case "$tag" in "$app" | "$app"-*) ;; *) fail "image.tag $tag is not appVersion $app" ;; esac

echo "-- refuses bad input and leaves the chart alone"
for args in "1.2 $DIGEST" "latest $DIGEST" "1.2.3 sha256:nope" "1.2.3 " ; do
  fresh
  # shellcheck disable=SC2086 # split on purpose: the cases are argument lists
  if bash "$PIN" $args "$WORK/chart" >/dev/null 2>&1; then fail "accepted: $args"; fi
  diff -r "$ROOT/deployments/postgres-ha" "$WORK/chart" >/dev/null || fail "changed the chart on bad input: $args"
done

echo "pin-chart tests passed"
