#!/usr/bin/env bash
# Renders the chart as a real dependency of a small umbrella chart: on, off,
# aliased, and with global settings. Run by test/chart/checks.sh.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CHART=$ROOT/deployments/postgres-ha
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
CIDR='postgres-ha.postgresql.allowedCIDRs={10.244.0.0/16}'

fail_test() { echo "FAIL: $*"; exit 1; }

umbrella() {
  local dir=$1 deps=$2
  mkdir -p "$dir/charts" "$dir/templates"
  cp -R "$CHART" "$dir/charts/postgres-ha"
  printf 'apiVersion: v2\nname: umbrella\nversion: 0.1.0\ndependencies:\n%b' "$deps" > "$dir/Chart.yaml"
  printf 'global:\n  host: app.example.org\n  arch: arm64\n' > "$dir/values.yaml"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n' > "$dir/templates/app.yaml"
}

U=$WORK/umbrella
umbrella "$U" '  - name: postgres-ha\n    version: "*"\n    condition: postgres-ha.enabled\n'

echo "-- on: global from the parent is accepted and names don't take the parent's"
out=$(helm template app "$U" --set "$CIDR")
helm lint --strict "$U" --set "$CIDR" >/dev/null || fail_test "the umbrella doesn't lint"
grep -q 'kind: StatefulSet' <<< "$out" || fail_test "no StatefulSet"
grep -q '^  name: app-postgres-ha$' <<< "$out" || fail_test "the cluster isn't named <release>-postgres-ha"
grep -q 'name: app-postgres-ha-primary$' <<< "$out" || fail_test "no <release>-postgres-ha-primary Service"
[ "$(grep -c '^  name: app$' <<< "$out")" = 1 ] || fail_test "a resource took the parent's name"

echo "-- off through the dependency condition: nothing from the chart renders"
out=$(helm template app "$U" --set postgres-ha.enabled=false)
if grep -q 'postgres-ha' <<< "$out"; then fail_test "the chart rendered while off"; fi

echo "-- off with no condition on the dependency: nothing renders"
N=$WORK/nocondition
umbrella "$N" '  - name: postgres-ha\n    version: "*"\n'
out=$(helm template app "$N" --set postgres-ha.enabled=false)
if grep -q 'postgres-ha' <<< "$out"; then fail_test "the chart rendered while off"; fi

echo "-- off in a plain install: nothing renders"
out=$(helm template pg "$CHART" --set enabled=false)
if grep -q 'kind:' <<< "$out"; then fail_test "a plain install rendered while off"; fi

echo "-- two aliases are two clusters"
A=$WORK/alias
umbrella "$A" '  - name: postgres-ha\n    alias: main\n    version: "*"\n  - name: postgres-ha\n    alias: audit\n    version: "*"\n'
out=$(helm template app "$A" --set 'main.postgresql.allowedCIDRs={10.244.0.0/16}' --set 'audit.postgresql.allowedCIDRs={10.244.0.0/16}')
grep -q '^  name: app-main$' <<< "$out" || fail_test "no app-main cluster"
grep -q '^  name: app-audit$' <<< "$out" || fail_test "no app-audit cluster"

echo "-- fullnameOverride still wins"
out=$(helm template app "$U" --set "$CIDR" --set postgres-ha.fullnameOverride=db)
grep -q 'name: db-primary$' <<< "$out" || fail_test "fullnameOverride was ignored"

echo "-- global image settings reach every image and pod"
{ echo 'postgres-ha:'; sed 's/^/  /' "$CHART/ci/full-values.yaml"; } > "$WORK/full.yaml"
out=$(helm template app "$U" -f "$WORK/full.yaml" --api-versions monitoring.coreos.com/v1 \
  --set global.imageRegistry=registry.example.org --set 'global.imagePullSecrets[0].name=regcred')
images=$(grep -E '^\s+(- )?image: ' <<< "$out" | sed 's/.*image: //' | tr -d '"' | sort -u)
[ -n "$images" ] || fail_test "no images"
if grep -v '^registry.example.org/' <<< "$images"; then fail_test "an image ignored global.imageRegistry"; fi
pods=$(grep -cE '^\s+containers:$' <<< "$out")
secrets=$(grep -c 'name: regcred' <<< "$out")
[ "$secrets" = "$pods" ] || fail_test "$secrets pull secrets for $pods pod specs"

echo "-- chart pull secrets are kept next to the global ones"
out=$(helm template app "$U" --set "$CIDR" --set 'global.imagePullSecrets[0].name=regcred' \
  --set 'postgres-ha.imagePullSecrets[0].name=own')
grep -q 'name: own' <<< "$out" || fail_test "the chart's pull secret was dropped"
grep -q 'name: regcred' <<< "$out" || fail_test "the global pull secret was dropped"

echo "subchart tests passed"
