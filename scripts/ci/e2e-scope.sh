#!/usr/bin/env bash
# e2e-scope.sh EVENT LABELLED < changed-files
#
# Decides whether the Checks workflow runs the kind e2e suites. Prints
# run=true|false and reason=... (the lines GITHUB_OUTPUT takes). Every event
# other than a pull request runs them. A pull request runs them when it
# carries the e2e label (LABELLED=true) or changes a path that can change
# cluster behaviour; the changed files arrive one per line on stdin.
set -euo pipefail
EVENT=${1:?event}
LABELLED=${2:-false}
PATHS='^(cmd/|internal/|deployments/|test/e2e/|Dockerfile$|go\.(mod|sum)$|\.github/workflows/checks\.yaml$)'

if [ "$EVENT" != "pull_request" ]; then
  echo "run=true"; echo "reason=${EVENT} event"; exit 0
fi
if [ "$LABELLED" = "true" ]; then
  echo "run=true"; echo "reason=the e2e label"; exit 0
fi
hits=$(grep -E "$PATHS" || true)
if [ -n "$hits" ]; then
  echo "run=true"; echo "reason=changes to $(printf '%s\n' "$hits" | head -5 | paste -sd ' ' -)"
else
  echo "run=false"; echo "reason=no agent, chart, e2e, image or module changes"
fi
