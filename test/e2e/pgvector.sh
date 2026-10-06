#!/usr/bin/env bash
# pgvector checks for the matrix's pgvector cells, against the release the
# failover suite leaves (pg in db): the extension creates, a table with a
# vector column and an hnsw index serves nearest-neighbour queries, and both
# survive a switchover and the old primary's rejoin. The backup suite covers
# point-in-time restore with PGVECTOR=1.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

nearest() { DB=app sql "$1" "set enable_seqscan = off; select id from vec order by e <-> '[0.9,0.1,0]' limit 1"; }

log "pgvector: extension, vector column and hnsw index"
wait_for 60 all_streaming || fail "cluster not settled"
P=$(primary)
DB=app sql "$P" "create extension if not exists vector; drop table if exists vec; create table vec (id int primary key, e vector(3)); insert into vec values (1, '[1,0,0]'), (2, '[0,1,0]'), (3, '[0,0,1]'); create index on vec using hnsw (e vector_l2_ops)" >/dev/null
[ "$(nearest "$P")" = "1" ] || fail "nearest-neighbour query on the primary returned '$(nearest "$P")'"

log "pgvector: survives a switchover and the old primary's rejoin"
$K exec "$P" -c postgres -- /pgha/bin/pgha switchover --to any >/dev/null
moved() { [ -n "$(holder)" ] && [ "$(holder)" != "$P" ] && [ "$(primary)" = "$(holder)" ]; }
wait_for 120 moved || fail "the switchover did not complete"
N=$(primary)
wait_for 180 all_streaming || fail "cluster did not settle after the switchover"
wait_for 60 member_streaming "$P" || fail "old primary $P did not rejoin as a streaming standby"
[ "$(nearest "$N")" = "1" ] || fail "nearest-neighbour query on the new primary $N returned '$(nearest "$N")'"
DB=app sql "$N" "insert into vec values (4, '[0.95,0.05,0]')" >/dev/null
caught() { [ "$(DB=app sql "$P" "select count(*) from vec")" = "4" ]; }
wait_for 60 caught || fail "the rejoined standby $P did not replay the new vector row"
[ "$(nearest "$P")" = "4" ] || fail "nearest-neighbour query on the rejoined standby returned '$(nearest "$P")'"

log "pgvector checks passed (primary $N, rejoined $P)"
