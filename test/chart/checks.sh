#!/usr/bin/env bash
# Chart checks: lint, kubeconform against the Kubernetes and CRD schemas,
# the helm-unittest suites, and the restart checksum rule. CI runs this; run
# it locally with helm, kubeconform and the helm-unittest plugin installed.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CHART=$ROOT/deployments/postgres-ha
KUBE_VERSION=${KUBE_VERSION:-1.34.0}
CRDS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

for v in default full; do
  echo "== helm lint ($v)"
  helm lint --strict "$CHART" -f "$CHART/ci/$v-values.yaml"
  echo "== kubeconform ($v)"
  helm template pg "$CHART" -n db -f "$CHART/ci/$v-values.yaml" --api-versions monitoring.coreos.com/v1 \
    | kubeconform -strict -summary -kubernetes-version "$KUBE_VERSION" \
        -schema-location default -schema-location "$CRDS"
done

echo "== helm unittest"
helm unittest "$CHART"

echo "== restart checksum: reloadable settings keep it, restart-only settings change it"
sum() { helm template pg "$CHART" -f "$CHART/ci/default-values.yaml" "$@" | sed -n 's/.*checksum\/restart: //p'; }
base=$(sum)
[ "$(sum --set postgresql.parameters.work_mem=64MB)" = "$base" ] || { echo "a reloadable setting rolled the pods"; exit 1; }
[ "$(sum --set postgresql.parameters.shared_buffers=512MB)" != "$base" ] || { echo "a restart-only setting did not roll the pods"; exit 1; }
[ "$(sum --set resources.limits.memory=8Gi)" != "$base" ] || { echo "a memory change moved shared_buffers but did not roll the pods"; exit 1; }
[ "$(sum --set replicas=5)" != "$base" ] || { echo "max_wal_senders follows replicas but did not roll the pods"; exit 1; }
echo "== pgbouncer checksum: the pods roll whenever the rendered pgbouncer.ini changes"
render=$(helm template pg "$CHART" -f "$CHART/ci/default-values.yaml" -s templates/pgbouncer.yaml)
ini=$(printf '%s' "$render" | yq -r 'select(.kind == "ConfigMap") | .data["pgbouncer.ini"]')
ini_sum=$(printf '%s' "$ini" | sha256sum | cut -d' ' -f1)
ann_sum=$(printf '%s' "$render" | yq -r 'select(.kind == "Deployment") | .spec.template.metadata.annotations["checksum/config"]')
[ "$ini_sum" = "$ann_sum" ] || { echo "checksum/config ($ann_sum) is not the hash of pgbouncer.ini ($ini_sum)"; exit 1; }
echo "== alert rules: promtool check and unit tests"
RULES_DIR=$(mktemp -d)
helm template pg "$CHART" -n db -f "$CHART/ci/default-values.yaml" --set metrics.prometheusRule.enabled=true -s templates/prometheusrule.yaml \
  | yq '.spec' > "$RULES_DIR/rules.yaml"
cp "$CHART/tests/rules/rules.test.yaml" "$RULES_DIR/"
promtool check rules "$RULES_DIR/rules.yaml"
promtool test rules "$RULES_DIR/rules.test.yaml"
echo "chart checks passed"
