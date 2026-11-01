#!/usr/bin/env bash
# Tests for the low-disk watchdog in test/e2e/disk-sample.sh. Run by
# test/chart/checks.sh.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=../e2e/disk-sample.sh
source "$ROOT/test/e2e/disk-sample.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
CALLS="$WORK/calls"
EVIDENCE="$WORK/evidence"

fail_test() { echo "FAIL: $*"; exit 1; }

# Stand-ins for the commands the watchdog runs, so a test can read their order.
FREE_MB=9000
df() { printf '  Avail\n %sM\n' "$FREE_MB"; }
docker() { echo "docker $*" >> "$CALLS"; [ "$1" != ps ] || echo node1; }
sudo() { echo "sudo $*" >> "$CALLS"; }

echo "-- plenty of free space saves nothing"
: > "$CALLS"; rm -f "$EVIDENCE"; FREE_MB=9000
check_disk
[ ! -s "$CALLS" ] || fail_test "the watchdog acted with space free"
[ ! -e "$EVIDENCE" ] || fail_test "evidence written with space free"

echo "-- an unreadable df saves nothing"
: > "$CALLS"; rm -f "$EVIDENCE"; FREE_MB=""
check_disk
[ ! -e "$EVIDENCE" ] || fail_test "evidence written without a reading"

echo "-- low space stops the nodes before measuring"
: > "$CALLS"; rm -f "$EVIDENCE"; FREE_MB=5000
check_disk
[ -s "$EVIDENCE" ] || fail_test "no evidence written"
grep -q 'docker stop --time 2 node1' "$CALLS" || fail_test "the nodes were not stopped: $(cat "$CALLS")"
grep -q '5000MB free' "$EVIDENCE" || fail_test "evidence does not say how much was free"
first=$(head -n 1 "$CALLS")
case "$first" in
  "docker ps"*) ;;
  *) fail_test "the first call was not finding the nodes: $first" ;;
esac
stop=$(grep -n 'docker stop' "$CALLS" | head -n 1 | cut -d: -f1)
du=$(grep -n 'sudo du' "$CALLS" | head -n 1 | cut -d: -f1)
[ "$stop" -lt "$du" ] || fail_test "measured before the nodes stopped"
grep -q '/var/lib/docker/volumes' "$CALLS" || fail_test "the kind node volumes were not measured"

echo "-- the evidence is saved once"
: > "$CALLS"
check_disk
[ ! -s "$CALLS" ] || fail_test "the watchdog ran twice"

echo "disk sample tests passed"
