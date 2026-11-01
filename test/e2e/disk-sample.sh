#!/usr/bin/env bash
# Records disk use on the runner and in a kind node every 5 seconds, so a run
# that fills the disk shows when it happened and where. Runs in the background
# for the whole matrix cell.
#
# A runner that fills up dies with its logs, so below DISK_FLOOR_MB this stops
# the kind nodes and saves a breakdown to EVIDENCE while there is room to.
# The nodes stop first: measuring a disk that is still filling can lose the race.
# The floor sits far above a normal run's low point because the disk has gone
# from there to full between two 5 second samples.
DISK_FLOOR_MB=${DISK_FLOOR_MB:-40960}
EVIDENCE=${EVIDENCE:-${RUNNER_TEMP:-/tmp}/disk-evidence.log}

free_mb() { df -BM --output=avail / 2>/dev/null | tail -n 1 | tr -dc 0-9; }

save_evidence() {
  local nodes largest
  nodes=$(docker ps --quiet --filter label=io.x-k8s.kind.cluster=pgha-e2e)
  echo "$(date -u +%H:%M:%S) low disk: $1MB free, stopping the kind nodes"
  # shellcheck disable=SC2086 # one container id per word
  [ -z "$nodes" ] || docker stop --time 2 $nodes
  echo "--- largest directories on /"
  sudo du -xm --max-depth=3 / 2>/dev/null | sort -n | tail -n 30
  echo "--- inside the kind node volumes"
  sudo du -xm --max-depth=7 /var/lib/docker/volumes 2>/dev/null | sort -n | tail -n 40
  echo "--- container layers"
  sudo du -xm --max-depth=2 /var/lib/docker/overlay2 2>/dev/null | sort -n | tail -n 20
  echo "--- largest files"
  sudo find / -xdev -type f -size +200M -printf '%s %p\n' 2>/dev/null | sort -n | tail -n 30 | tee "$EVIDENCE.files"
  largest=$(tail -n 1 "$EVIDENCE.files" | cut -d' ' -f2-)
  rm -f "$EVIDENCE.files"
  echo "--- end of the largest file"
  [ -z "$largest" ] || sudo tail -c 2000 "$largest" | strings
}

check_disk() {
  local free
  [ ! -e "$EVIDENCE" ] || return 0
  free=$(free_mb)
  [ -n "$free" ] && [ "$free" -lt "$DISK_FLOOR_MB" ] || return 0
  save_evidence "$free" > "$EVIDENCE" 2>&1
}

watch_disk() {
  while :; do
    check_disk
    sleep 1
  done
}

sample() {
  local t
  watch_disk &
  while :; do
    t=$(date -u +%H:%M:%S)
    df -BM --output=used,avail / 2>/dev/null | tail -n 1 | sed "s|^|$t runner |"
    timeout 3 docker exec pgha-e2e-worker df -BM --output=used,avail /var 2>/dev/null | tail -n 1 | sed "s|^|$t node |"
    sleep 5
  done
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  sample
fi
