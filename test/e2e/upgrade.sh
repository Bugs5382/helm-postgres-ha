#!/usr/bin/env bash
# End-to-end major-version upgrade, as the runbook describes it: run the
# chart on the previous PostgreSQL major, then move every database into a new
# release on the current major with pg_dump and check the data arrived.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART=${CHART:-$HERE/../../deployments/postgres-ha}
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OLD_NS=up-old
NEW_NS=up-new
PREVIOUS_TAG=17.11-bookworm
PREVIOUS_DIGEST=sha256:639ab7ceb90e13123085b741fb31ef493fba25463002f6da665352e7b534b652

for ns in $OLD_NS $NEW_NS; do kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; done

log "old release on PostgreSQL ${PREVIOUS_TAG%%-*}"
helm upgrade --install old "$CHART" -n $OLD_NS -f "$HERE/values.yaml" \
  --set image.tag=$PREVIOUS_TAG --set image.digest=$PREVIOUS_DIGEST --wait --timeout 6m >/dev/null || { NS=$OLD_NS R=old dump; exit 1; }
# shellcheck disable=SC2034 # NS and R are read by the lib.sh helpers
NS=$OLD_NS K="timeout 30 kubectl -n $OLD_NS" R=old
wait_for 120 all_streaming || fail "the old release's standbys are not streaming"
OLDP=$(primary)
[ "$(sql "$OLDP" "show server_version_num" | cut -c1-2)" = "17" ] || fail "the old release does not run PostgreSQL 17"
DB=app sql "$OLDP" "create table moved (id int primary key, v text); insert into moved select g, 'row ' || g from generate_series(1, 1000) g" >/dev/null
OLD_SUM=$(DB=app sql "$OLDP" "select md5(string_agg(v, ',' order by id)) from moved")

log "new release on PostgreSQL 18; dump and restore every database"
helm upgrade --install new "$CHART" -n $NEW_NS -f "$HERE/values.yaml" --wait --timeout 6m >/dev/null || { NS=$NEW_NS R=new dump; exit 1; }
NS=$NEW_NS K="timeout 30 kubectl -n $NEW_NS" R=new
wait_for 120 all_streaming || fail "the new release's standbys are not streaming"
NEWP=$(primary)
[ "$(sql "$NEWP" "show server_version_num" | cut -c1-2)" = "18" ] || fail "the new release does not run PostgreSQL 18"
kubectl -n $OLD_NS exec "$OLDP" -c postgres -- pg_dump -h /var/run/postgresql -U postgres -d app \
  | kubectl -n $NEW_NS exec -i "$NEWP" -c postgres -- psql -h /var/run/postgresql -U postgres -d app -X -q -v ON_ERROR_STOP=1 >/dev/null \
  || fail "pg_dump into the new release failed"
[ "$(DB=app sql "$NEWP" "select md5(string_agg(v, ',' order by id)) from moved")" = "$OLD_SUM" ] || fail "the data differs after the move"
[ "$(DB=app sql "$NEWP" "select tableowner from pg_tables where tablename = 'moved'")" = "postgres" ] || fail "the table owner changed"
standby_has() { local s; for s in $(kubectl -n $NEW_NS get pods -l "app.kubernetes.io/instance=new,postgres-ha/role=replica" -o name); do [ "$(DB=app sql "${s#pod/}" "select count(*) from moved")" = "1000" ] || return 1; done; }
wait_for 60 standby_has || fail "the new release's standbys do not have the moved data"

log "major-version upgrade passed"
