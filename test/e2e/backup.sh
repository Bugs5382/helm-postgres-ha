#!/usr/bin/env bash
# End-to-end backup and point-in-time restore: archive WAL and take a base
# backup from one release, then restore a second release to a moment between
# two writes and check only the first write is there.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART=${CHART:-$HERE/../../deployments/postgres-ha}
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

SRC_NS=bk
DST_NS=rs

log "object storage"
kubectl create namespace s3 --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n s3 apply -f "$HERE/s3.yaml" >/dev/null
kubectl -n s3 rollout status deploy/seaweedfs --timeout=180s >/dev/null
wait_for 60 kubectl -n s3 exec deploy/seaweedfs -- sh -c 'echo "s3.bucket.create -name pgha" | weed shell -master=localhost:9333' || fail "cannot create the bucket"

for ns in $SRC_NS $DST_NS; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$ns" create secret generic s3-creds --from-literal=AWS_ACCESS_KEY_ID=e2e-access \
    --from-literal=AWS_SECRET_ACCESS_KEY=e2e-secret-key --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done

log "source cluster with WAL archiving and scheduled backups"
helm upgrade --install pg "$CHART" -n $SRC_NS -f "$HERE/values.yaml" -f "$HERE/backup-values.yaml" ${EXTRA_VALUES:+-f "$EXTRA_VALUES"} \
  --set backup.s3.prefix=s3://pgha/$SRC_NS --wait --timeout 5m >/dev/null || { NS=$SRC_NS dump; exit 1; }
NS=$SRC_NS K="timeout 30 kubectl -n $SRC_NS" R=pg
start_client
wait_for 60 all_streaming || fail "source standbys not streaming"
P=$(primary)

log "archiving runs where wal-g and its credentials are"
sql "$P" "select pg_switch_wal()" >/dev/null
archived() { [ "$(sql "$P" "select archived_count > 0 and failed_count = 0 from pg_stat_archiver")" = "t" ]; }
wait_for 60 archived || fail "WAL is not archived: $(sql "$P" 'select * from pg_stat_archiver')"

log "base backup on request, taken by a standby"
sleep 1
REQ=$(date -u +%Y-%m-%dT%H:%M:%SZ)
$K exec "$P" -c postgres -- /pgha/bin/pgha backup >/dev/null
backed_up() {
  local last; last=$($K get lease pg-backup -o jsonpath='{.metadata.annotations.postgres-ha/last-success}')
  [ -n "$last" ] && [[ "$last" > "$REQ" || "$last" == "$REQ" ]]
}
wait_for 240 backed_up || fail "the requested base backup was not recorded: $($K get lease pg-backup -o yaml)"
BY=$($K get lease pg-backup -o jsonpath='{.metadata.annotations.postgres-ha/last-backup}')
[ "$BY" != "$P" ] || fail "the backup ran on the primary, not a standby"
log "  backup by standby $BY"

log "the primary archives the standby backup's last WAL segment at once"
# The segment holding the backup's end must be fetchable from the archive;
# that is what a restore needs. Without the primary's WAL switch it waits for
# archive_timeout (300s).
FIN=$($K exec "$P" -c postgres -- /pgha/bin/wal-g backup-list --detail --json 2>/dev/null | jq -r 'max_by(.start_time).finish_lsn')
# The backup needs WAL up to, not including, finish_lsn: the segment holding
# the byte before it. A backup ending on a boundary needs no newer segment.
SEG=$(sql "$P" "select pg_walfile_name('0/0'::pg_lsn + ($FIN - 1))")
log "  backup ends at $(sql "$P" "select '0/0'::pg_lsn + $FIN"), in segment $SEG"
seg_archived() { $K exec "$P" -c postgres -- /pgha/bin/wal-g wal-fetch "$SEG" /tmp/seg-check >/dev/null 2>&1; }
wait_for 60 seg_archived || fail "segment $SEG with the backup's end was not archived (archive_timeout is 300s)"

log "restore check: the scheduled job restores the latest backup and records the result"
$K delete job verify-now --ignore-not-found >/dev/null
$K create job verify-now --from=cronjob/pg-backup-verify >/dev/null
kubectl -n "$NS" wait --for=condition=complete job/verify-now --timeout=300s >/dev/null || { $K logs job/verify-now --all-containers | tail -30; fail "the restore check did not complete"; }
[ "$($K get lease pg-backup -o jsonpath='{.metadata.annotations.postgres-ha/last-verify-result}')" = "ok" ] || fail "the restore check did not record ok"
$K logs job/verify-now -c verify | grep -q '"message":"restore verified"' || fail "the restore check did not log its result"

log "two writes either side of the restore target"
wait_for 60 app "drop table if exists pitr; create table pitr (v text)" || fail "app cannot write"
app "insert into pitr values ('kept')"
if [ "${PGVECTOR:-0}" = 1 ]; then
  DB=app sql "$P" "create extension if not exists vector; create table vec (id int primary key, e vector(3)); insert into vec values (1, '[1,0,0]'), (2, '[0,1,0]'); create index on vec using hnsw (e vector_l2_ops)" >/dev/null
fi
sleep 2
TARGET=$(sql "$P" "select now()::text")
sleep 2
app "insert into pitr values ('discarded')"
SWITCHED=$(sql "$P" "select pg_walfile_name(pg_switch_wal())")
caught_up() { [ "$(sql "$P" "select coalesce(last_archived_wal >= '$SWITCHED', false) from pg_stat_archiver")" = "t" ]; }
wait_for 60 caught_up || fail "the WAL with the second write was not archived"
log "  target $TARGET"

log "restore a new release to the target"
helm upgrade --install pgr "$CHART" -n $DST_NS -f "$HERE/values.yaml" -f "$HERE/backup-values.yaml" ${EXTRA_VALUES:+-f "$EXTRA_VALUES"} \
  --set backup.s3.prefix=s3://pgha/$DST_NS --set bootstrap.mode=restore \
  --set bootstrap.restore.prefix=s3://pgha/$SRC_NS --set-string "bootstrap.restore.targetTime=$TARGET" \
  --wait --timeout 8m >/dev/null || { NS=$DST_NS R=pgr dump; exit 1; }
# shellcheck disable=SC2034 # NS and R are read by the lib.sh helpers
NS=$DST_NS K="timeout 30 kubectl -n $DST_NS" R=pgr
wait_for 120 all_streaming || fail "restored cluster's standbys not streaming"
P=$(primary)
got=$(DB=app sql "$P" "select string_agg(v, ',' order by v) from pitr")
[ "$got" = "kept" ] || fail "restored rows are '$got', want only 'kept'"
if [ "${PGVECTOR:-0}" = 1 ]; then
  near=$(DB=app sql "$P" "set enable_seqscan = off; select id from vec order by e <-> '[0.9,0.1,0]' limit 1")
  [ "$near" = "1" ] || fail "pgvector after the restore: nearest is '$near', want 1"
  log "  pgvector table and hnsw index restored"
fi
in_recovery "$P" && fail "restored primary is still in recovery"

log "backup and point-in-time restore passed"
