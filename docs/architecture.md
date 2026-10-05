# How failover works

## The pieces

Each member is one pod of a StatefulSet. An init container copies two static binaries, `pgha` and
`wal-g`, from the agent image into a shared volume. The `postgres` container runs the official
PostgreSQL image with `pgha run` as PID 1. The agent starts PostgreSQL as its child, so it can stop,
promote, rewind and restart it without sharing a process namespace or a second container.

| Object | Role |
| --- | --- |
| Lease `<name>` | The holder is the primary. Its annotations hold the cluster's system identifier and the primary's last timeline and WAL position. |
| Lease `<name>-backup` | Held by the member taking a base backup. |
| Label `postgres-ha/role` | `primary`, `replica` or `none`, set by each agent on its own pod. |
| Service `<name>-primary` | Selects `postgres-ha/role=primary`. |
| Service `<name>-replica` | Selects `postgres-ha/role=replica`; readiness keeps lagging standbys out. |
| Service `<name>-headless` | Stable per-member names, published before members are ready. |
| Port 8009 (mutual TLS) | The read-only peer status API members use during an election. |
| Port 8008 | `/livez`, `/readyz` and `/metrics`. |

The chart owns every Service and never has one patched, so a Helm upgrade or a GitOps resync
cannot repoint clients.

## The rules

1. **PostgreSQL runs read-write only while this member holds the Lease.** Any other member starts
   with `standby.signal`.
2. **The holder renews every retry period** (2s). The time a renew was *sent* is what counts.
3. **Fencing:** a watchdog that never waits on the control loop stops PostgreSQL immediately once
   the last successful renew is older than the renew deadline (10s). Observers only treat the
   Lease as expired after a full lease duration (15s) without seeing it change on their own clock,
   so the old primary is always stopped before anyone can take over.
4. **A member that runs read-write without the Lease fences itself** the moment it sees another
   holder, for example a former primary that comes back on its old volume.
5. **Election.** When the Lease has no live holder, a standby takes it only if:
   - a quorum of members (a majority; one for a two-member cluster) answered the peer API;
   - no member still runs read-write or still streams from a primary. A server that runs but does
     not answer queries yet counts as read-write unless it was started with `standby.signal` or
     `recovery.signal`: those stay in recovery until promoted, so a standby that is starting, or
     failing to start, never blocks a failover;
   - it is the most complete eligible standby: highest timeline, then most WAL received, then a
     pod on a newer StatefulSet revision, then the lowest ordinal;
   - it is within `agent.maxLagOnFailover` of the last position the primary recorded.

   The Lease is taken with an optimistic-concurrency update, so two candidates can never both win.
6. **Promotion** runs `pg_promote()` over the local socket while the loop keeps renewing. If it does
   not finish in time the member gives the Lease back and stays a standby; the Service is never
   pointed at a server that cannot take writes. Once the new primary is up it logs in to every
   PgBouncer pod's admin console and runs `KILL` then `RESUME` for each database, so no pooled
   connection stays on the old primary. A server that hangs rather than dies keeps acknowledging
   TCP, so timeouts alone would leave clients queued behind it.
7. **Following.** A standby connects to the holder's headless name with `verify-full` TLS, its pod
   name as `application_name` and its own replication slot, and reloads when the holder changes.
8. **Rejoin.** Data that last ran read-write (or carries the `pgha.rejoin` marker) is rewound with
   `pg_rewind` before it follows anyone. If the rewind fails, the data directory is moved aside,
   never deleted, and the member is cloned again. A standby that cannot stream from a healthy
   primary for `agent.rejoinTimeout` goes through the same path.
9. **Bootstrap.** With no data anywhere and no system identifier on the Lease, the lowest-ordinal
   member among a quorum that all report empty data directories runs `initdb` (or restores a
   backup). Every other empty member clones from the holder. A data directory whose system
   identifier differs from the Lease's is refused.
10. **Shutdown and switchover.** A terminating primary stops PostgreSQL cleanly, which sends all WAL
    to the attached standbys, then releases the Lease naming its successor. It keeps renewing the
    Lease, and keeps the watchdog's fencing rule, until the server is down, so a slow shutdown never
    lets the Lease lapse under a server that still takes writes. A switchover request
    (`pgha switchover`) does the same without the pod stopping.

## Synchronous replication

The primary keeps `synchronous_standby_names` in step with the standbys that are streaming:

- `quorum` (default): `ANY n (...)` over the streaming standbys, and none while no standby streams,
  so a lone primary keeps taking writes.
- `strict`: every other member is listed, so writes stop when no standby can confirm them.
- `off`: asynchronous.

The agent's own sessions use `synchronous_commit = local`, so it can always create the roles a
standby needs to attach.

## Roles and credentials

A pre-install and pre-upgrade hook Job creates each generated credential Secret once and never
rewrites it. The primary then creates or updates, idempotently:

- `postgres` (superuser, password from its Secret);
- `replicator` (replication);
- `pgha_rewind` (the functions `pg_rewind` needs, nothing more);
- `pgbouncer_auth` and the `pgbouncer.get_auth` lookup function, when PgBouncer is on;
- the application roles and databases from values.

Passwords are sent as SCRAM verifiers, never as plain text, and only rewritten when they change.

## Metrics and alerts

Every member's agent exports `pgha_*` metrics on port 8008: role, Lease holder, timeline, WAL
position and lag, promotions and fencings, archiver counters, last base backup and restore check,
and data volume use. With `metrics.exporters.enabled`, postgres_exporter (port 9187) and
pgbouncer_exporter (9127) run beside the members and poolers. postgres_exporter logs in over the
local socket as `pgha_monitor`, a role with `pg_monitor` and no password, which only the postgres
OS user can use. `metrics.prometheusRule.enabled` ships the alerts; `tests/rules` holds their
promtool unit tests.

## What it does not do

- It is not an operator and adds no CRDs.
- It does not promote across a partition it cannot see through: without a quorum it waits.
- In `quorum` mode a failover can lose commits made while no standby was streaming; use `strict`
  when that is not acceptable.
