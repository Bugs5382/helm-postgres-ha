#!/usr/bin/env bash
# set-chart-version.sh TAG [CHART_DIR]
#
# Keeps Chart.yaml at the version the release draft will publish. TAG is the
# draft's tag (a leading v is dropped); an empty TAG means there is no draft,
# so nothing changes. A chart already at that version is not rewritten, which
# leaves the workflow nothing to commit. Needs yq (v4).
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TAG=${1:-}
CHART=${2:-$ROOT/deployments/postgres-ha}

[ -n "$TAG" ] || { echo "set-chart-version: no release draft, nothing to do"; exit 0; }
VERSION=${TAG#v}
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'
[[ $VERSION =~ $semver ]] || { echo "set-chart-version: tag '$TAG' is not SemVer (MAJOR.MINOR.PATCH)" >&2; exit 2; }
[ -f "$CHART/Chart.yaml" ] || { echo "set-chart-version: no chart in $CHART" >&2; exit 2; }

current=$(yq -r .version "$CHART/Chart.yaml")
if [ "$current" = "$VERSION" ]; then
  echo "set-chart-version: Chart.yaml is already at $VERSION"
  exit 0
fi
V=$VERSION yq -i '.version = strenv(V)' "$CHART/Chart.yaml"
echo "set-chart-version: Chart.yaml $current -> $VERSION"
