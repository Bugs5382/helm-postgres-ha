#!/usr/bin/env bash
# Tests for the helpers in test/e2e/lib.sh that need no cluster. Run by
# test/chart/checks.sh.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=../e2e/lib.sh
source "$ROOT/test/e2e/lib.sh"
SEQ=$(mktemp)
trap 'rm -f "$SEQ"' EXIT

fail_test() { echo "FAIL: $*"; exit 1; }

# holder answers from $SEQ, one line per read, so a test can change the Lease
# between two reads the way a running handover does.
holder() { local h; h=$(head -n 1 "$SEQ"); sed -i.bak 1d "$SEQ"; rm -f "$SEQ.bak"; printf '%s' "$h"; }
primary() { printf '%s' "$PRIMARY"; }

echo "-- a lease emptied between reads is not a move"
printf 'pg-2\n\n\n\n' > "$SEQ"; PRIMARY=""
if lease_moved_from pg-2; then fail_test "the old holder then an empty lease counted as moved"; fi

echo "-- an empty lease is not a move"
printf '\n\n\n\n' > "$SEQ"; PRIMARY=""
if lease_moved_from pg-2; then fail_test "an empty lease counted as moved"; fi

echo "-- a new holder that is not labelled yet is not a move"
printf 'pg-1\npg-1\npg-1\npg-1\n' > "$SEQ"; PRIMARY=""
if lease_moved_from pg-2; then fail_test "a holder without the primary label counted as moved"; fi

echo "-- a new holder labelled primary is a move"
printf 'pg-1\npg-1\npg-1\npg-1\n' > "$SEQ"; PRIMARY=pg-1
lease_moved_from pg-2 || fail_test "the finished move was not seen"

echo "e2e lib tests passed"
