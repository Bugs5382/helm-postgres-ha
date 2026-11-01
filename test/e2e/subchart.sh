#!/usr/bin/env bash
# Installs the chart as a dependency of a small umbrella chart, with the kind
# of global block an umbrella passes down, and checks the cluster comes up
# under its subchart name with a primary that takes writes.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export NS=sub R=app-postgres-ha
# shellcheck source=lib.sh
source "$HERE/lib.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/charts"
cp -R "$HERE/../../deployments/postgres-ha" "$WORK/charts/postgres-ha"
printf 'apiVersion: v2\nname: umbrella\nversion: 0.1.0\ndependencies:\n  - name: postgres-ha\n    version: "*"\n    condition: postgres-ha.enabled\n' > "$WORK/Chart.yaml"
{
  printf 'global:\n  host: app.example.org\npostgres-ha:\n'
  sed 's/^/  /' "$HERE/values.yaml"
  printf '  replicas: 2\n'
} > "$WORK/values.yaml"

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
log "installing the umbrella"
helm upgrade --install app "$WORK" -n "$NS" --wait --timeout 6m

p=$(primary)
[ -n "$p" ] || fail "no primary under the subchart name"
[ "$(holder)" = "$p" ] || fail "the lease holder isn't the primary"
$K get service "$R-primary" >/dev/null || fail "no $R-primary Service"
$K exec "$p" -c postgres -- psql -h /var/run/postgresql -U postgres -XAtc \
  'create table subchart_check (n int); insert into subchart_check values (1); drop table subchart_check' >/dev/null \
  || fail "the primary didn't take a write"

log "turning it off through the condition"
helm upgrade app "$WORK" -n "$NS" --set postgres-ha.enabled=false --wait --timeout 3m
[ -z "$($K get statefulset -o name)" ] || fail "the StatefulSet survived postgres-ha.enabled=false"

helm uninstall app -n "$NS" --wait --timeout 3m
log "subchart test passed"
