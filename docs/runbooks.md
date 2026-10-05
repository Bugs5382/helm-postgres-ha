# Runbooks

Every command assumes release `pg` in namespace `db`. Replace both with yours.

## Who is primary

```bash
kubectl -n db get lease pg -o jsonpath='{.spec.holderIdentity}{"\n"}'
kubectl -n db get pods -l app.kubernetes.io/instance=pg -L postgres-ha/role
```

The Lease holder and the pod labelled `primary` agree once a promotion has finished. For a
member's own view, read its peer status from inside any member:

```bash
kubectl -n db exec pg-0 -c postgres -- psql -h /var/run/postgresql -U postgres -Atc \
  "select pg_is_in_recovery(), application_name, state, sync_state from pg_stat_replication"
```

## Planned switchover

```bash
kubectl -n db exec pg-0 -c postgres -- /pgha/bin/pgha switchover --to any
# or a named standby:
kubectl -n db exec pg-0 -c postgres -- /pgha/bin/pgha switchover --to pg-2
```

The primary stops PostgreSQL cleanly, which sends its last WAL to the standbys, and hands the
Lease to the successor. Writes pause for a few seconds. A request naming a standby that is not
streaming is ignored and logged.

Rolling updates use the same path: a terminating primary hands over before its pod stops.

## Unplanned failover

Nothing to do: when the primary's node or pod fails, it stops renewing the Lease. After
`agent.leaseDuration` (15s by default) the most complete standby takes over, if a quorum of
members answers. Check the members' logs for `"message":"waiting"` lines when no failover
happens; the `reason` field says what is missing (`need 2 to fail over`, `still streams from`,
`bytes behind the last primary position`).

To accept more data loss than `agent.maxLagOnFailover` allows, raise it with `helm upgrade`. The
lag check only applies while every candidate is behind the last recorded position.

## Rebuilding a member

A member whose data cannot be rewound is moved aside and cloned automatically. So is a standby
that fell further behind than its slot may hold (`max_slot_wal_keep_size`): the primary recreates
the lost slot, and the standby, still unable to stream after a rewind that found nothing to
change, re-clones. To force a rebuild
(a corrupted or foreign volume):

```bash
kubectl -n db delete pvc data-pg-2 --wait=false
kubectl -n db delete pod pg-2
```

The pod comes back with an empty volume and clones from the primary. Moved-aside directories
(`pgdata.aside-*` on the volume) are never deleted by the agent; remove them once you no longer
need them.

A member that logs code `1102` holds data from another cluster and refuses to start; rebuild it.

## Restore and point-in-time recovery

Restore into a **new** release, never over a running one:

```bash
helm install pgr ./deployments/postgres-ha -n restore --create-namespace \
  --set 'postgresql.allowedCIDRs={10.244.0.0/16}' \
  --set backup.enabled=true --set backup.s3.prefix=s3://bucket/pgr \
  --set backup.s3.existingSecret=s3-creds \
  --set bootstrap.mode=restore \
  --set bootstrap.restore.prefix=s3://bucket/pg \
  --set-string 'bootstrap.restore.targetTime=2026-10-04 17:05:46+00'
```

- `bootstrap.restore.backup` picks a base backup (default `LATEST`).
- Set at most one of `targetTime`, `targetLSN` or `targetName`; none restores to the end of the
  archived WAL. `targetExclusive` stops just before the target.
- The restored primary promotes at the target onto a new timeline, archives to its own prefix
  and takes a fresh base backup at once. The standbys clone from it.

### The scheduled restore check

With backups on, the `pg-backup-verify` CronJob (`backup.verify`, daily at 05:00 UTC by default)
proves the latest backup restores. It:

1. fetches the latest base backup into scratch space;
2. replays the archived WAL in a throwaway server that only listens on a local socket and never
   archives;
3. runs `backup.verify.query`;
4. records the outcome on the backup Lease (`postgres-ha/last-verify`,
   `postgres-ha/last-verify-result`).

Every member exports the outcome as `pgha_backup_last_verify_timestamp_seconds` and
`pgha_backup_last_verify_ok`. To run the check now:

```bash
kubectl -n db create job verify-now --from=cronjob/pg-backup-verify
kubectl -n db logs -f job/verify-now -c verify
```

Point `backup.verify.query` at a table your application always writes to, so the check proves
recent data is there and not only that the server starts.

## Taking a backup now

```bash
kubectl -n db exec pg-0 -c postgres -- /pgha/bin/pgha backup
kubectl -n db get lease pg-backup -o jsonpath='{.metadata.annotations}{"\n"}'
```

`postgres-ha/last-success` and `postgres-ha/last-backup` on the backup Lease record the last
backup and the member that took it. `pgha_backup_last_success_timestamp_seconds` exports the same
time from every member.

## Certificates

With `tls.certManager.enabled` (the default) the chart creates a CA for the release and issues the
members' and PgBouncer's certificates from it. cert-manager renews them; the agent reloads
PostgreSQL and its peer listener when the files change.

To bring your own, create a Secret with `tls.crt`, `tls.key` and `ca.crt` and set
`tls.existingSecret`. The certificate needs:

- DNS names `*.pg-headless.db.svc`, `*.pg-headless.db.svc.cluster.local`, and `pg-primary` and
  `pg-replica` with their `.db`, `.db.svc` and `.db.svc.cluster.local` forms;
- both `serverAuth` and `clientAuth` usages;
- `ca.crt` holding the CA that signed it (members trust only that CA).

## Major version upgrade

The chart runs one PostgreSQL major per chart line. A member whose data directory was made by
another major logs code `1103` and refuses to start. The supported path is a dump and restore
into a new release:

1. Install a new release with the new chart version next to the old one.
2. Stop writes to the old release.
3. `pg_dumpall` from the old primary into `psql` on the new primary (both over TLS).
4. Move clients to the new release, then uninstall the old one.

## Connection pooling limits

PgBouncer runs in `transaction` mode by default. In that mode session state does not survive
between transactions: session-level prepared statements across transactions, advisory locks,
`LISTEN`/`NOTIFY`, `SET` without `LOCAL` and temporary tables do not work. Clients that need them
connect to `pg-primary` directly or use `pgbouncer.poolMode: session`.
