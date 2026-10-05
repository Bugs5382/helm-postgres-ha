#!/usr/bin/env bash
# Runs every bash block in docs/runbooks.md, in order, against the release
# the backup suite leaves behind (pg in namespace bk, with backups on). The
# commands run as written; only facts about the test environment are
# substituted (namespace, chart path and test values, bucket, restore target),
# and each block must exit 0 and leave the cluster settled.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART=${CHART:-$HERE/../../deployments/postgres-ha}
DOC=${DOC:-$HERE/../../docs/runbooks.md}
NS=bk
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

BLOCKS=$(mktemp -d)
trap 'rm -rf "$BLOCKS"' EXIT
awk -v dir="$BLOCKS" '
  /^## / { title = substr($0, 4) }
  /^```bash$/ { n++; f = sprintf("%s/%02d", dir, n); print title > (f ".title"); inblock = 1; next }
  /^```$/ && inblock { inblock = 0; close(f); next }
  inblock { print > f }
' "$DOC"
count=$(find "$BLOCKS" -name '*.title' | wc -l)
[ "$count" -gt 0 ] || fail "no bash blocks found in $DOC"

TARGET=""
RUN=$(date +%s)
substitute() {
  sed -e "s|-n db |-n $NS |g" \
      -e "s|cronjob/pg-backup-verify|cronjob/$R-backup-verify|g" \
      -e "s|\./deployments/postgres-ha|$CHART -f $HERE/values.yaml -f $HERE/backup-values.yaml|g" \
      -e "s|s3://bucket/pgr|s3://pgha/runbook-restore-$RUN|g" \
      -e "s|s3://bucket/pg|s3://pgha/$NS|g" \
      -e "s|targetTime=[^']*|targetTime=$TARGET|g" "$1"
}

settled() {
  local h; h=$(holder)
  [ -n "$h" ] && [ "$(primary)" = "$h" ] && all_streaming
}

prepare() {
  case "$1" in
  *"delete pvc data-pg-2"*)
    # The runbook rebuilds standbys only; move the primary off pg-2 first, as it says.
    if [ "$(holder)" = "$R-2" ]; then
      # The agent ignores a switchover while no standby can take over (one
      # still re-attaching after the previous handover), so ask again until
      # the primary moves.
      moved() { [ "$(holder)" != "$R-2" ] && settled; }
      ok=
      for _ in 1 2 3 4 5 6; do
        $K exec "$R-0" -c postgres -- /pgha/bin/pgha switchover --to any >/dev/null
        if wait_for 20 moved; then ok=1; break; fi
      done
      [ -n "$ok" ] || fail "could not move the primary off $R-2 before the rebuild"
    fi
    ;;
  *"create job verify-now"*) $K delete job verify-now --ignore-not-found >/dev/null ;;
  *"helm install pgr"*)
    kubectl create namespace restore --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    kubectl -n restore create secret generic s3-creds --from-literal=AWS_ACCESS_KEY_ID=e2e-access \
      --from-literal=AWS_SECRET_ACCESS_KEY=e2e-secret-key --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    # A time target is only reached once a commit after it is archived, so
    # write after taking it and wait for that WAL to reach the archive.
    P=$(primary)
    TARGET=$(sql "$P" "select now()::text")
    sleep 1
    sql "$P" "create table if not exists runbook_marker (at timestamptz default now()); insert into runbook_marker default values" >/dev/null
    sql "$P" "select pg_switch_wal()" >/dev/null
    archived_past() { [ "$(sql "$P" "select last_archived_wal >= pg_walfile_name(pg_current_wal_lsn() - 1) from pg_stat_archiver")" = "t" ]; }
    wait_for 60 archived_past || fail "the WAL after the restore target was not archived"
    ;;
  esac
}

after() {
  case "$1" in
  *"helm install pgr"*)
    NS=restore R=pgr K="timeout 30 kubectl -n restore" wait_for 600 all_streaming \
      || { NS=restore R=pgr dump; fail "the runbook restore did not come up"; }
    helm uninstall pgr -n restore --wait >/dev/null
    ;;
  *"delete pvc data-pg-2"*)
    wait_for 300 member_streaming "$R-2" || fail "the rebuilt member is not streaming"
    ;;
  esac
}

for t in "$BLOCKS"/*.title; do
  b=${t%.title}
  prepare "$(cat "$b")"
  run=$(substitute "$b")
  log "runbook: $(cat "$t") (block $(basename "$b"))"
  diff <(cat "$b") <(printf '%s\n' "$run") | sed -n 's/^> /  substituted: /p' || true
  bash -euo pipefail -c "$run" >"$BLOCKS/out" 2>&1 || { cat "$BLOCKS/out"; fail "runbook block failed: $(cat "$t")"; }
  after "$(cat "$b")"
  sleep 5
  wait_for 180 settled || fail "the cluster did not settle after: $(cat "$t")"
done

log "all $count runbook blocks ran"
