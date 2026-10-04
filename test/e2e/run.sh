#!/usr/bin/env bash
# End-to-end failover tests against an installed release. Each step asserts
# the behaviour an issue in the tracker specifies.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART=${CHART:-$HERE/../../deployments/postgres-ha}
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

start_client

log "bootstrap: every member ready and both standbys streaming"
$K rollout status "sts/$R" --timeout=300s >/dev/null || fail "members not ready"
wait_for 60 all_streaming || fail "standbys not streaming"
P=$(primary); [ "$P" = "$(holder)" ] || fail "labelled primary $P is not the lease holder $(holder)"
sync=$(sql "$P" "select string_agg(sync_state, ',' order by application_name) from pg_stat_replication")
[ "$sync" = "quorum,quorum" ] || fail "standbys are not quorum synchronous: $sync"

log "roles and databases: the app role writes through PgBouncer over TLS"
wait_for 60 app "drop table if exists e2e; create table e2e (id bigserial primary key, v text not null)" || fail "app cannot connect through pgbouncer"
app_write before-failover || fail "write through pgbouncer failed"
[ "$($K exec client -- psql "host=$R-primary.$NS.svc user=app dbname=app" -XAtq -c 'select ssl from pg_stat_ssl where pid = pg_backend_pid()')" = "t" ] || fail "client connection is not TLS"
if $K exec client -- env PGSSLMODE=disable psql "host=$R-primary.$NS.svc user=app dbname=app" -XAtq -c 'select 1' >/dev/null 2>&1; then
  fail "a connection without TLS was accepted"
fi
[ "$(sql "$P" "select count(*) from pg_stat_ssl s join pg_stat_replication r using (pid) where s.ssl")" = "2" ] || fail "replication is not over TLS"

log "network policy: only allowed clients reach the members and the pooler"
image=$($K get sts "$R" -o jsonpath='{.spec.template.spec.containers[0].image}')
sed -e "s|POSTGRES_IMAGE|$image|" -e "s|RELEASE|$R|g" -e 's|name: client|name: outsider|' -e 's|e2e-client: "true"|e2e-client: "false"|' "$HERE/client.yaml" | $K apply -f - >/dev/null
kubectl -n "$NS" wait --for=condition=Ready pod/outsider --timeout=120s >/dev/null || fail "the outsider pod did not start"
reach() { $K exec "$1" -- bash -c "timeout 4 bash -c '</dev/tcp/$2/$3'" >/dev/null 2>&1; }
P=$(primary)
for target in "$R-primary.$NS.svc 5432" "$R-pgbouncer.$NS.svc 5432" "$P.$R-headless.$NS.svc 8009" "$P.$R-headless.$NS.svc 8008"; do
  # shellcheck disable=SC2086 # host and port are two words on purpose
  if reach outsider $target; then fail "a pod outside the allowed clients reached $target"; fi
done
# shellcheck disable=SC2086
reach client $R-primary.$NS.svc 5432 || fail "the allowed client cannot reach the primary"
# shellcheck disable=SC2086
reach client $R-pgbouncer.$NS.svc 5432 || fail "the allowed client cannot reach pgbouncer"
if reach client "$P.$R-headless.$NS.svc" 8009; then fail "an application client reached the peer API"; fi
$K delete pod outsider --wait=false >/dev/null

log "unplanned failover: freeze the primary's node"
OLD=$P
NODE=$($K get pod "$OLD" -o jsonpath='{.spec.nodeName}')
start=$SECONDS
docker pause "$NODE" >/dev/null
trap 'docker unpause "$NODE" >/dev/null 2>&1 || true' EXIT
new_primary() { local p; p=$(primary); [ -n "$p" ] && [ "$p" != "$OLD" ] && ! in_recovery "$p"; }
# The frozen pod keeps its label until the new primary clears it.
only_new_primary() { new_primary && [ "$(primary | wc -w)" = "1" ]; }
wait_for 120 only_new_primary || fail "no new primary after freezing $NODE"
NEW=$(primary)
log "  $NEW promoted after $((SECONDS - start))s"
promoted=$SECONDS
# A pooler on a live node must serve writes soon after the promotion. The
# Service can still route some connections to the pooler on the frozen node
# until Kubernetes marks that node NotReady, so the Service is only checked
# for eventually recovering.
LIVE_POOLER=$($K get pods -l app.kubernetes.io/name=postgres-ha-pgbouncer -o jsonpath="{range .items[?(@.spec.nodeName!='$NODE')]}{.status.podIP}{' '}{end}" | awk '{print $1}')
[ -n "$LIVE_POOLER" ] || fail "no pgbouncer pod outside the frozen node"
app_at() { $K exec client -- psql "host=$1 port=6432 sslmode=require user=app dbname=app" -XAtq -c "insert into e2e (v) select '$2' where not exists (select 1 from e2e where v = '$2')"; }
wait_for 60 app_at "$LIVE_POOLER" after-failover || fail "the live pgbouncer did not serve writes"
log "  the live pgbouncer served writes $((SECONDS - promoted))s after the promotion"
(( SECONDS - promoted <= 30 )) || fail "the live pgbouncer took $((SECONDS - promoted))s to serve writes"
wait_for 120 app_write after-failover-service || fail "writes through the pgbouncer Service did not resume"
log "  writes through the Service resumed after $((SECONDS - start))s"
has_row "$NEW" before-failover || fail "a committed row was lost in the failover"
docker unpause "$NODE" >/dev/null
trap - EXIT

log "rejoin: the old primary comes back as a standby of the new one"
wait_for 240 all_streaming || fail "old primary did not rejoin"
in_recovery "$OLD" || fail "$OLD came back read-write"
OLD_LOG=$($K logs "$OLD" -c postgres)
grep -q '"code":1111' <<<"$OLD_LOG" || fail "$OLD did not fence itself when it woke up"
wait_for 30 has_row "$OLD" after-failover || fail "$OLD does not have the row written after the failover"

log "planned switchover"
OLD=$(primary)
$K exec "$OLD" -c postgres -- /pgha/bin/pgha switchover --to any >/dev/null
wait_for 60 new_primary || fail "switchover did not complete"
NEW=$(primary)
log "  $OLD handed over to $NEW"
wait_for 60 app_write after-switchover || fail "writes did not resume after the switchover"
wait_for 120 all_streaming || fail "cluster did not settle after the switchover"
in_recovery "$OLD" || fail "$OLD is not a standby after the switchover"

log "partition: cut the primary off; it fences itself and the others fail over"
OLD=$(primary)
NODE=$($K get pod "$OLD" -o jsonpath='{.spec.nodeName}')
IP=$($K get pod "$OLD" -o jsonpath='{.status.podIP}')
# Drop the pod's traffic before connection tracking, so established
# connections (its API watch, its replication streams) are cut as well.
partition() { docker exec "$NODE" iptables -t raw "$1" PREROUTING -s "$IP" -j DROP; docker exec "$NODE" iptables -t raw "$1" PREROUTING -d "$IP" -j DROP; }
partition -I
trap 'partition -D 2>/dev/null || true' EXIT
wait_for 120 new_primary || fail "no failover while $OLD was partitioned"
NEW=$(primary)
log "  $NEW took over from the partitioned $OLD"
# kubectl exec goes through the kubelet, which still reaches the pod.
running_rw() { [ "$($K exec "$OLD" -c postgres -- sh -c 'psql -h /var/run/postgresql -U postgres -XAtq -c "select pg_is_in_recovery()" 2>/dev/null || echo down')" = "f" ]; }
if running_rw; then fail "partitioned $OLD still runs read-write"; fi
start=$SECONDS
wait_for 60 app_write during-partition || fail "writes failed on the new primary"
log "  writes through pgbouncer resumed $((SECONDS - start))s after the takeover"
partition -D
trap - EXIT
wait_for 180 all_streaming || fail "partitioned member did not rejoin"
in_recovery "$OLD" || fail "$OLD came back read-write after the partition healed"
wait_for 30 has_row "$OLD" during-partition || fail "$OLD is missing writes from the partition"

log "config rollout: a values change reaches the running cluster without a restart"
P=$(primary)
UIDS=$($K get pods -l "app.kubernetes.io/instance=$R,app.kubernetes.io/name=postgres-ha" -o jsonpath='{.items[*].metadata.uid}')
helm upgrade "$R" "$CHART" -n "$NS" -f "$HERE/values.yaml" --set 'postgresql.allowedCIDRs={10.244.0.0/16,10.99.0.0/16}' --wait --timeout 5m >/dev/null || fail "helm upgrade failed"
hba_updated() { [ "$(sql "$P" "select count(*) from pg_hba_file_rules where address = '10.99.0.0'")" -ge 1 ]; }
wait_for 150 hba_updated || fail "pg_hba.conf change did not reach $P"
[ "$($K get pods -l "app.kubernetes.io/instance=$R,app.kubernetes.io/name=postgres-ha" -o jsonpath='{.items[*].metadata.uid}')" = "$UIDS" ] || fail "a reloadable change restarted the members"

log "rebuild: pod 0 loses its volume while another member is primary"
if [ "$(primary)" = "$R-0" ]; then
  $K exec "$R-0" -c postgres -- /pgha/bin/pgha switchover --to "$R-1" >/dev/null
  not_zero() { local p; p=$(primary); [ -n "$p" ] && [ "$p" != "$R-0" ] && ! in_recovery "$p"; }
  wait_for 60 not_zero || fail "could not move the primary off $R-0"
  wait_for 120 all_streaming || fail "cluster did not settle before the rebuild"
fi
P=$(primary)
SYSID=$($K get lease "$R" -o jsonpath='{.metadata.annotations.postgres-ha/system-identifier}')
app_write before-rebuild || fail "write before the rebuild failed"
$K delete pvc "data-$R-0" --wait=false >/dev/null
$K delete pod "$R-0" >/dev/null
wait_for 240 all_streaming || fail "$R-0 did not come back streaming"
in_recovery "$R-0" || fail "$R-0 came back read-write on an empty volume"
[ "$(sql "$R-0" 'select system_identifier from pg_control_system()')" = "$SYSID" ] || fail "$R-0 has another system identifier"
$K logs "$R-0" -c postgres | grep -q 'bootstrapping the cluster' && fail "$R-0 ran initdb over an existing cluster"
wait_for 30 has_row "$R-0" before-rebuild || fail "$R-0 is missing data from $P"
[ "$(primary)" = "$P" ] || fail "the rebuild moved the primary"

log "all failover tests passed"
