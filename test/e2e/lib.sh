#!/usr/bin/env bash
# Shared helpers for the end-to-end tests. Needs kubectl on a cluster with the
# chart installed as release $R (default pg) in namespace $NS (default db).
set -euo pipefail

NS=${NS:-db}
R=${R:-pg}
# Every kubectl call is bounded, so a call into a frozen node cannot stall
# the run.
K="timeout 30 kubectl -n $NS"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "FAIL: $*"; dump; exit 1; }

dump() {
  $K get pods -o wide -L postgres-ha/role || true
  $K get lease "$R" -o yaml || true
  for p in "$R-0" "$R-1" "$R-2"; do
    echo "--- $p"
    $K logs "$p" -c postgres --tail=40 2>/dev/null || true
  done
}

# primary prints the member labelled primary (empty when none).
primary() {
  $K get pods -l "app.kubernetes.io/name=postgres-ha,app.kubernetes.io/instance=$R,postgres-ha/role=primary" -o jsonpath='{.items[*].metadata.name}'
}

holder() { $K get lease "$R" -o jsonpath='{.spec.holderIdentity}'; }

# sql runs SQL on a member as postgres over its local socket. DB picks the
# database (default postgres).
sql() {
  local pod=$1; shift
  $K exec "$pod" -c postgres -- psql -h /var/run/postgresql -U postgres -d "${DB:-postgres}" -XAtq -c "$*"
}

# app runs SQL as the app role through PgBouncer, over TLS.
app() {
  $K exec client -- psql "host=$R-pgbouncer.$NS.svc port=5432 user=app dbname=app" -XAtq -c "$*"
}

# wait_for retries a command until it succeeds or the timeout (seconds) ends.
wait_for() {
  local timeout=$1; shift
  local start=$SECONDS
  until "$@" >/dev/null 2>&1; do
    (( SECONDS - start > timeout )) && return 1
    sleep 1
  done
}

# streaming_count prints how many standbys stream from pod.
streaming_count() {
  sql "$1" "select count(*) from pg_stat_replication where state = 'streaming'"
}

all_streaming() {
  local p; p=$(primary)
  [ -n "$p" ] && [ "$(streaming_count "$p")" = "2" ]
}

# replica_endpoints prints the ready addresses behind the replica Service.
replica_endpoints() {
  $K get endpointslices -l "kubernetes.io/service-name=$R-replica" \
    -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{" "}{end}'
}

in_recovery() { [ "$(sql "$1" 'select pg_is_in_recovery()')" = "t" ]; }

# read_write succeeds only when the member answers and is not in recovery;
# a member that does not answer is neither.
read_write() { [ "$(sql "$1" 'select pg_is_in_recovery()')" = "f" ]; }

# member_streaming checks the member's own WAL receiver, not the primary's
# view, which can still list a connection that is going away.
member_streaming() { [ "$(sql "$1" "select status from pg_stat_wal_receiver")" = "streaming" ]; }

has_row() { [ "$(DB=app sql "$1" "select count(*) from e2e where v = '$2'" 2>/dev/null)" = "1" ]; }

# app_write is idempotent: an attempt that timed out on the client may still
# have committed, and the retry must not insert the row twice.
app_write() { app "insert into e2e (v) select '$1' where not exists (select 1 from e2e where v = '$1')"; }

# start_client runs the psql client pod for release $R.
start_client() {
  local image
  image=$($K get sts "$R" -o jsonpath='{.spec.template.spec.containers[0].image}')
  sed -e "s|POSTGRES_IMAGE|$image|" -e "s|RELEASE|$R|g" "$HERE/client.yaml" | $K apply -f - >/dev/null
  $K wait --for=condition=Ready pod/client --timeout=120s >/dev/null
}
