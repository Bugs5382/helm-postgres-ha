#!/usr/bin/env bash
# Installs the chart for the failover tests into a namespace that enforces the
# restricted Pod Security Standard, so every pod must be admitted under it.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART=${CHART:-$HERE/../../deployments/postgres-ha}
NS=${NS:-db}
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# cert-manager's Deployment reports ready before its webhook serves. Wait
# until the webhook admits an Issuer (a server-side dry run creates nothing).
for _ in $(seq 1 60); do
  if printf 'apiVersion: cert-manager.io/v1\nkind: Issuer\nmetadata:\n  name: webhook-probe\nspec:\n  selfSigned: {}\n' |
    kubectl -n "$NS" apply --dry-run=server -f - >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
kubectl label namespace "$NS" --overwrite pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/enforce-version=latest >/dev/null
helm upgrade --install pg "$CHART" -n "$NS" -f "$HERE/values.yaml" --wait --timeout 6m
