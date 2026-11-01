#!/usr/bin/env bash
# Records disk use on the runner and in a kind node every 5 seconds, so a run
# that fills the disk shows when it happened and where. Runs in the background
# for the whole matrix cell.
while :; do
  t=$(date -u +%H:%M:%S)
  df -BM --output=used,avail / 2>/dev/null | tail -1 | sed "s|^|$t runner |"
  timeout 3 docker exec pgha-e2e-worker df -BM --output=used,avail /var 2>/dev/null | tail -1 | sed "s|^|$t node |"
  sleep 5
done
