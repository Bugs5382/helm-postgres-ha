#!/usr/bin/env bash
# vuln-gate.sh < govulncheck-json
#
# Reads `govulncheck -format json` output on stdin. Fails, naming each one,
# when any finding has a fixed version: those are fixable by a dependency
# bump. Findings with no fixed version are listed and pass, since nothing
# upstream fixes them yet. Needs jq.
set -euo pipefail
findings=$(jq -r 'select(.finding) | .finding | "\(.osv) \(.fixed_version // "none")"' | sort -u)
fixable=$(awk '$2 != "none"' <<<"$findings")
unfixable=$(awk '$2 == "none" {print $1}' <<<"$findings")
if [ -n "$unfixable" ]; then
  echo "Known findings with no fixed version: $(paste -sd ' ' - <<<"$unfixable")"
fi
if [ -n "$fixable" ]; then
  echo "Fixable vulnerabilities (advisory, fixed in):" >&2
  sed 's/^/  /' <<<"$fixable" >&2
  exit 1
fi
echo "No fixable vulnerabilities."
