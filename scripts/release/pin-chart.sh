#!/usr/bin/env bash
# pin-chart.sh VERSION DIGEST [CHART_DIR]
#
# Writes a release into the chart before it is packaged: the chart version
# (a leading v is dropped) and the digest of the agent image published for
# it. The agent tag already defaults to the chart version, so the packaged
# chart runs ghcr.io/bugs5382/helm-postgres-ha/pgha:VERSION@DIGEST. Needs yq
# (v4). Nothing is written unless both inputs are valid.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
VERSION=${1:-}
DIGEST=${2:-}
CHART=${3:-$ROOT/deployments/postgres-ha}
VERSION=${VERSION#v}

semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'
[[ $VERSION =~ $semver ]] || { echo "pin-chart: version '$VERSION' is not SemVer (MAJOR.MINOR.PATCH)" >&2; exit 2; }
[[ $DIGEST =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "pin-chart: digest '$DIGEST' is not sha256:<64 hex>" >&2; exit 2; }
[ -f "$CHART/Chart.yaml" ] && [ -f "$CHART/values.yaml" ] || { echo "pin-chart: no chart in $CHART" >&2; exit 2; }

V=$VERSION yq -i '.version = strenv(V)' "$CHART/Chart.yaml"
D=$DIGEST yq -i '.agent.image.digest = strenv(D)' "$CHART/values.yaml"
echo "pinned $(yq -r .name "$CHART/Chart.yaml") $VERSION to agent image digest $DIGEST"
