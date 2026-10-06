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
kubectl -n "$NS" rollout status "sts/$R" --timeout=300s >/dev/null || fail "members not ready"
wait_for 60 all_streaming || fail "standbys not streaming"
P=$(primary); [ "$P" = "$(holder)" ] || fail "labelled primary $P is not the lease holder $(holder)"
WANT_MAJOR=${PG_MAJOR:-$(sed -n 's/^appVersion: *"\{0,1\}\([0-9]*\).*/\1/p' "$CHART/Chart.yaml")}
got=$(sql "$P" "select current_setting('server_version_num')::int / 10000")
[ "$got" = "$WANT_MAJOR" ] || fail "the server runs PostgreSQL $got, the chart declares $WANT_MAJOR"
log "  PostgreSQL $got, as declared"
sync=$(sql "$P" "select string_agg(sync_state, ',' order by application_name) from pg_stat_replication")
[ "$sync" = "quorum,quorum" ] || fail "standbys are not quorum synchronous: $sync"

log "replica service: both standbys, never the primary"
PIP=$($K get pod "$(primary)" -o jsonpath='{.status.podIP}')
two_replicas() { [ "$(replica_endpoints | wc -w)" = "2" ]; }
wait_for 60 two_replicas || fail "the replica service does not have both standbys: $(replica_endpoints)"
case " $(replica_endpoints) " in *" $PIP "*) fail "the replica service routes to the primary" ;; esac

log "roles and databases: the app role writes through PgBouncer over TLS"
wait_for 60 app "drop table if exists e2e; create table e2e (id bigserial primary key, v text not null)" || fail "app cannot connect through pgbouncer"
app_write before-failover || fail "write through pgbouncer failed"
[ "$($K exec client -- psql "host=$R-primary.$NS.svc user=app dbname=app" -XAtq -c 'select ssl from pg_stat_ssl where pid = pg_backend_pid()')" = "t" ] || fail "client connection is not TLS"
if $K exec client -- env PGSSLMODE=disable psql "host=$R-primary.$NS.svc user=app dbname=app" -XAtq -c 'select 1' >/dev/null 2>&1; then
  fail "a connection without TLS was accepted"
fi
[ "$(sql "$P" "select count(*) from pg_stat_ssl s join pg_stat_replication r using (pid) where s.ssl")" = "2" ] || fail "replication is not over TLS"

log "pgbouncer: every app role logs in through it over TLS, and losing one pooler pod keeps service"
RPW=$($K get secret "$R-role-reporting" -o jsonpath='{.data.password}' | base64 -d)
role_via_pooler() { $K exec client -- env PGPASSWORD="$RPW" psql "host=$R-pgbouncer.$NS.svc user=reporting dbname=app" -XAtq -c "select current_user || ',' || (select ssl from pg_stat_ssl where pid = pg_backend_pid())"; }
wait_for 60 role_via_pooler || fail "the reporting role cannot log in through pgbouncer"
[ "$(role_via_pooler)" = "reporting,true" ] || fail "the reporting role's pooled connection is not TLS: $(role_via_pooler)"
if $K exec client -- env PGSSLMODE=disable psql "host=$R-pgbouncer.$NS.svc user=app dbname=app" -XAtq -c 'select 1' >/dev/null 2>&1; then
  fail "pgbouncer accepted a client without TLS"
fi
POOLER=$($K get pods -l app.kubernetes.io/name=postgres-ha-pgbouncer -o jsonpath='{.items[0].metadata.name}')
$K delete pod "$POOLER" --wait=false >/dev/null
for _ in $(seq 1 10); do app_write pooler-drain || fail "a write failed while one pgbouncer pod was replaced"; sleep 0.5; done
kubectl -n "$NS" rollout status deploy/"$R-pgbouncer" --timeout=120s >/dev/null || fail "pgbouncer did not come back to two pods"

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

log "postmaster crash: kill -9 under a WAL backlog; the agent restarts it, the kubelet does not"
P=$(primary)
RESTARTS=$($K get pod "$P" -o jsonpath='{.status.containerStatuses[0].restartCount}')
app "create table if not exists crash (id bigserial primary key, pad text)" >/dev/null
# About 200 MB of WAL since the last checkpoint, so crash recovery has work.
# Uncapped: this insert can take longer than the 30s bound on other calls.
kubectl -n "$NS" exec "$P" -c postgres -- psql -h /var/run/postgresql -U postgres -d app -XAtq -c "checkpoint" -c "insert into crash (pad) select repeat('x', 1000) from generate_series(1, 150000)" >/dev/null
app_write before-crash || fail "write before the crash failed"
# shellcheck disable=SC2016 # expands inside the container
$K exec "$P" -c postgres -- sh -c 'kill -9 "$(head -1 "$PGDATA/postmaster.pid")"'
back() { local p; p=$(primary); [ -n "$p" ] && read_write "$p"; }
wait_for 180 back || fail "no primary after the postmaster was killed"
wait_for 60 app_write after-crash || fail "writes did not resume after the crash"
[ "$($K get pod "$P" -o jsonpath='{.status.containerStatuses[0].restartCount}')" = "$RESTARTS" ] || fail "the kubelet restarted $P's container"
wait_for 180 all_streaming || fail "the cluster did not settle after the crash"
has_row "$(primary)" before-crash || fail "a committed row was lost in the crash"
log "  $(primary) is primary; $P's container was not restarted"

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
wait_for 60 member_streaming "$OLD" || fail "$OLD is not streaming as a standby"
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
wait_for 60 member_streaming "$OLD" || fail "$OLD is not streaming as a standby after the switchover"

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
wait_for 60 member_streaming "$OLD" || fail "$OLD is not streaming as a standby after the partition healed"
wait_for 30 has_row "$OLD" during-partition || fail "$OLD is missing writes from the partition"

log "asymmetric partition: one standby loses the primary; nobody fails over"
P=$(primary)
S=$(for m in "$R-0" "$R-1" "$R-2"; do [ "$m" != "$P" ] && echo "$m" && break; done)
SNODE=$($K get pod "$S" -o jsonpath='{.spec.nodeName}')
SIP=$($K get pod "$S" -o jsonpath='{.status.podIP}')
PIP=$($K get pod "$P" -o jsonpath='{.status.podIP}')
TRANSITIONS=$($K get lease "$R" -o jsonpath='{.spec.leaseTransitions}')
cut_pair() { docker exec "$SNODE" iptables -t raw "$1" PREROUTING -s "$SIP" -d "$PIP" -j DROP; docker exec "$SNODE" iptables -t raw "$1" PREROUTING -s "$PIP" -d "$SIP" -j DROP; }
cut_pair -I
trap 'cut_pair -D 2>/dev/null || true' EXIT
# Well past the lease duration and the receiver timeout.
sleep 45
[ "$(holder)" = "$P" ] || fail "the lease moved from $P to $(holder) though only $S lost its path"
[ "$($K get lease "$R" -o jsonpath='{.spec.leaseTransitions}')" = "$TRANSITIONS" ] || fail "the lease changed hands"
[ "$(primary)" = "$P" ] || fail "another member was labelled primary"
in_recovery "$S" || fail "$S left recovery"
case " $(replica_endpoints) " in *" $SIP "*) fail "the replica service still routes to $S, which is not streaming" ;; esac
case " $(replica_endpoints) " in *" $PIP "*) fail "the replica service routes to the primary" ;; esac
app_write during-asymmetric-partition || fail "writes failed while only $S was cut off"
cut_pair -D
trap - EXIT
wait_for 120 all_streaming || fail "$S did not stream again after the path healed"
back_in_service() { case " $(replica_endpoints) " in *" $SIP "*) return 0 ;; esac; return 1; }
wait_for 60 back_in_service || fail "$S did not return to the replica service"
wait_for 30 has_row "$S" during-asymmetric-partition || fail "$S is missing writes from the partition"

log "fell behind: a standby misses more WAL than its slot may hold; it is rebuilt and streams again"
P=$(primary)
S=$(for m in "$R-0" "$R-1" "$R-2"; do [ "$m" != "$P" ] && echo "$m" && break; done)
helm upgrade "$R" "$CHART" -n "$NS" --reuse-values --set-string postgresql.parameters.max_slot_wal_keep_size=32MB --set-string postgresql.parameters.wal_keep_size=0 --wait --timeout 5m >/dev/null || fail "helm upgrade failed"
slot_limit() { [ "$(sql "$P" "show max_slot_wal_keep_size")" = "32MB" ]; }
wait_for 150 slot_limit || fail "the lower slot limit did not reach $P"
SNODE=$($K get pod "$S" -o jsonpath='{.spec.nodeName}')
SIP=$($K get pod "$S" -o jsonpath='{.status.podIP}')
PIP=$($K get pod "$P" -o jsonpath='{.status.podIP}')
# Only the path between the standby and the primary: the kubelet and the
# API server still reach the standby, so it is not restarted meanwhile.
cut_standby() { docker exec "$SNODE" iptables -t raw "$1" PREROUTING -s "$SIP" -d "$PIP" -j DROP; docker exec "$SNODE" iptables -t raw "$1" PREROUTING -s "$PIP" -d "$SIP" -j DROP; }
cut_standby -I
trap 'cut_standby -D 2>/dev/null || true' EXIT
app "create table if not exists behind (id bigserial primary key, pad text)" >/dev/null
# Write WAL until the slot loses it. These statements can take longer than
# the 30s bound on other calls, so they use kubectl directly.
psql_long() { kubectl -n "$NS" exec "$P" -c postgres -- psql -h /var/run/postgresql -U postgres -d app -XAtq -c "$1" >/dev/null; }
# The primary recreates a lost slot within a minute, so either the slot is
# lost now or the primary has logged replacing it.
lost() {
  [ "$(sql "$P" "select wal_status from pg_replication_slots where slot_name = replace('$S', '-', '_')")" = "lost" ] ||
    $K logs "$P" -c postgres --since=10m | grep -q 'dropped a replication slot whose WAL was removed'
}
for _ in $(seq 1 20); do
  lost && break
  psql_long "insert into behind (pad) select repeat('x', 1000) from generate_series(1, 20000)"
  psql_long "select pg_switch_wal()"
  psql_long "checkpoint"
done
lost || fail "the slot for $S did not lose its WAL"
app_write while-behind || fail "write while $S was behind failed"
cut_standby -D
trap - EXIT
# Recovery takes up to two rejoin timeouts: one rewind that finds nothing
# to change, then a re-clone. A receiver that starts and fails on removed
# WAL reports streaming for a moment, so wait on the data.
wait_for 360 has_row "$S" while-behind || fail "$S is missing the rows written while it was behind"
wait_for 60 member_streaming "$S" || fail "$S is not streaming after falling behind"
helm upgrade "$R" "$CHART" -n "$NS" --reuse-values --set-string postgresql.parameters.max_slot_wal_keep_size=512MB --set-string postgresql.parameters.wal_keep_size=64MB --wait --timeout 5m >/dev/null || fail "helm upgrade failed"

log "config rollout: a values change reaches the running cluster without a restart"
P=$(primary)
UIDS=$($K get pods -l "app.kubernetes.io/instance=$R,app.kubernetes.io/name=postgres-ha" -o jsonpath='{.items[*].metadata.uid}')
helm upgrade "$R" "$CHART" -n "$NS" --reuse-values --set 'postgresql.allowedCIDRs={10.244.0.0/16,10.99.0.0/16}' --wait --timeout 5m >/dev/null || fail "helm upgrade failed"
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
wait_for 60 app_write before-rebuild || fail "write before the rebuild failed"
$K delete pvc "data-$R-0" --wait=false >/dev/null
$K delete pod "$R-0" >/dev/null
wait_for 240 member_streaming "$R-0" || fail "$R-0 did not come back streaming"
wait_for 60 all_streaming || fail "the cluster did not settle after the rebuild"
if read_write "$R-0"; then fail "$R-0 came back read-write on an empty volume"; fi
[ "$(sql "$R-0" 'select system_identifier from pg_control_system()')" = "$SYSID" ] || fail "$R-0 has another system identifier"
$K logs "$R-0" -c postgres | grep -q 'bootstrapping the cluster' && fail "$R-0 ran initdb over an existing cluster"
wait_for 30 has_row "$R-0" before-rebuild || fail "$R-0 is missing data from $P"
[ "$(primary)" = "$P" ] || fail "the rebuild moved the primary"
log "rolling upgrade under write load: one planned handover, few failed writes"
all_ready() { [ "$($K get sts "$R" -o jsonpath='{.status.readyReplicas}')" = "3" ]; }
wait_for 180 all_streaming || fail "cluster not settled before the upgrade"
TRANSITIONS=$($K get lease "$R" -o jsonpath='{.spec.leaseTransitions}')
app "create table if not exists load (id bigserial primary key, at timestamptz default now())" >/dev/null
# One write every 200ms through PgBouncer, each its own connection, until
# the stop file appears; the loop records ok or fail per attempt.
$K exec -i client -- sh -c 'cat > /tmp/load.sh' <<LOAD
#!/bin/bash
rm -f /tmp/stop /tmp/load.log
until [ -f /tmp/stop ]; do
  t=\$(date +%H:%M:%S.%N | cut -c1-12)
  if out=\$(psql "host=$R-pgbouncer.$NS.svc user=app dbname=app" -XAtq -c "insert into load default values" 2>&1); then echo "\$t ok"; else echo "\$t fail \$(echo \$out | cut -c1-120)"; fi >> /tmp/load.log
  sleep 0.2
done
LOAD
$K exec client -- sh -c 'nohup bash /tmp/load.sh >/dev/null 2>&1 &'
# Keep each member's log through the roll: the pods are replaced, and their
# old logs go with them.
UPLOG=${UPGRADE_LOG_DIR:-$(mktemp -d)}
LOGS=()
for m in "$R-0" "$R-1" "$R-2"; do kubectl -n "$NS" logs -f "$m" -c postgres --since=1s > "$UPLOG/$m.log" 2>&1 & LOGS+=($!); done
log "  member logs for the roll in $UPLOG"
sleep 3
helm upgrade "$R" "$CHART" -n "$NS" --reuse-values --set-string "podAnnotations.e2e-roll=$(date +%s)" --wait --timeout 10m >/dev/null || fail "helm upgrade failed"
kubectl -n "$NS" rollout status "sts/$R" --timeout=600s >/dev/null || fail "the rollout did not finish"
wait_for 180 all_streaming || fail "cluster did not settle after the upgrade"
sleep 3
$K exec client -- touch /tmp/stop
sleep 1
OK=$($K exec client -- grep -c ' ok$' /tmp/load.log || true)
FAILED=$($K exec client -- grep -c ' fail' /tmp/load.log || true)
kill "${LOGS[@]}" 2>/dev/null || true
if (( FAILED > 0 )); then
  log "  failed writes, first and last of each run of errors:"
  $K exec client -- grep ' fail' /tmp/load.log | awk '{e=substr($0, index($0,$3)); if (e!=last) {print "    " $0; last=e}}' | head -20
fi
log "  writes during the upgrade: $OK ok, $FAILED failed"
AFTER=$($K get lease "$R" -o jsonpath='{.spec.leaseTransitions}')
(( AFTER - TRANSITIONS <= 1 )) || fail "the upgrade moved the lease $((AFTER - TRANSITIONS)) times, want at most one planned handover"
(( FAILED <= 40 )) || fail "$FAILED writes failed during the upgrade"
(( OK >= 50 )) || fail "only $OK writes succeeded during the upgrade"
[ "$(app 'select count(*) from load')" -ge "$OK" ] || fail "committed writes are missing after the upgrade"

log "disruption budget: a second member cannot be evicted while one is out"
wait_for 180 all_ready || fail "members not ready before the eviction test"
evict() { printf '{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"%s","namespace":"%s"}}' "$1" "$NS" | kubectl create --raw "/api/v1/namespaces/$NS/pods/$1/eviction" -f - >/dev/null 2>&1; }
S1=""; S2=""
for m in "$R-0" "$R-1" "$R-2"; do [ "$m" = "$(primary)" ] && continue; if [ -z "$S1" ]; then S1=$m; else S2=$m; fi; done
evict "$S1" || fail "the first eviction was refused with every member ready"
if evict "$S2"; then fail "a second member was evicted while $S1 was out"; fi
wait_for 240 all_ready || fail "$S1 did not come back after the eviction"

log "all failover tests passed"
