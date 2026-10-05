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
change, re-clones. To force a rebuild of a standby (a corrupted or foreign volume), delete its volume
and pod. Rebuild only standbys: if the member is the primary, run a planned switchover first.

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
- A time target is reached only once a commit after it has been archived. A target past the end
  of the archive (in the future, or newer than the last archived commit) cannot be reached: the
  restoring member logs code `1113` and stays down rather than retrying. Uninstall the release,
  delete its volumes, and restore again with an earlier target or none.
- The restored primary promotes at the target onto a new timeline, archives to its own prefix
  and takes a fresh base backup at once. The standbys clone from it.

### Restoring from a file backend

To restore from a WAL-G file store (`backup.storage: file` on the old cluster), set
`bootstrap.restore.source: file`:

```yaml
bootstrap:
  mode: restore
  restore:
    source: file
    backup: LATEST                 # or a backup name from `wal-g backup-list`
    targetName: before-upgrade     # or targetTime / targetLSN, or none
    file:
      existingClaim: old-backups   # the volume holding the store, mounted read-only
      directory: pg                # the store's directory on it (the old cluster's name)
```

Without `existingClaim`, the directory is read from this release's own backup volume
(`backup.storage: file`). The source never mixes with the new cluster's own storage: base backups
and WAL are read only from the source, and the restored cluster archives to its own prefix, which
must be empty.

Before fetching anything, the agent lists the source's base backups. It stops with code `1109`
and a clear message when the source directory is not reachable, holds no base backup, or does not
hold the named backup; the message lists the backups it does hold. A target the archive cannot
reach logs `1113`.

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
kubectl -n db wait --for=condition=complete job/verify-now --timeout=1h
kubectl -n db logs job/verify-now -c verify
```

Point `backup.verify.query` at a table your application always writes to, so the check proves
recent data is there and not only that the server starts.

## Backups on a local volume

With `backup.storage: file`, WAL-G archives WAL and writes base backups to a volume mounted on every
member at `backup.file.mountPath` (`/backup`), under a directory named after the cluster
(`backup.file.prefix`). The chart creates a `ReadWriteMany` claim, `<name>-backup`, and keeps it
when the release is uninstalled. Set `backup.file.existingClaim` to use your own.

- Every member archives and may take the base backup, so with more than one member the volume
  must be shared: a `ReadWriteMany` class, or a single-node cluster where `ReadWriteOnce` works.
- The agent creates the prefix directory as the postgres user before the server starts. The
  members' `fsGroup` (999) must apply to the volume; some NFS provisioners ignore it, and then the
  directory needs to be writable by uid 999.
- Retention, the backup-age alert and the restore check work the same as with S3.
  `PostgresHABackupVolumeFilling` warns when the volume passes
  `metrics.prometheusRule.diskUsedPercent`.
- To export, copy the prefix directory off the volume. It is a complete WAL-G store, and
  `WALG_FILE_PREFIX=<dir> wal-g backup-list` reads it anywhere.

## Backup retention

After each successful base backup, the member that took it runs
`wal-g delete retain FULL <backup.retainFull> --after <now - backup.retainDays> --confirm`. That
keeps every full backup newer than `retainDays` days, and never fewer than `retainFull` full backups
even if no backup succeeded for a while. WAL older than the oldest kept backup goes with it.

Your point-in-time restore window is therefore at least `retainDays` days back from now, or back to
the `retainFull`-th most recent full backup if that is older. With the defaults (daily backups,
`retainDays: 14`, `retainFull: 7`) you can restore to any moment of the last two weeks.

Retention only runs after a successful backup, so failing backups never delete older ones.
`PostgresHABackupTooOld` pages when the newest backup is older than
`metrics.prometheusRule.backupMaxAgeHours`, including when the schedule stops running
altogether.

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
another major logs code `1103` and refuses to start, so an in-place image bump never starts the
wrong server on your data. Move to a new major with a dump and restore into a new release,
which `test/e2e/upgrade.sh` runs in CI from 17 to 18:

1. Install a new release from the new chart version next to the old one, with the same `roles`
   and `databases` values, so the roles and empty databases exist.
2. Stop writes to the old release (scale your applications down, or point them at a maintenance
   page).
3. Copy each database from the old primary into the new one:

   ```bash
   OLD=$(kubectl -n old get pods -l postgres-ha/role=primary -o jsonpath='{.items[0].metadata.name}')
   NEW=$(kubectl -n new get pods -l postgres-ha/role=primary -o jsonpath='{.items[0].metadata.name}')
   kubectl -n old exec "$OLD" -c postgres -- pg_dump -h /var/run/postgresql -U postgres -d app \
     | kubectl -n new exec -i "$NEW" -c postgres -- psql -h /var/run/postgresql -U postgres -d app -v ON_ERROR_STOP=1
   ```

   Use `pg_dump` per database rather than `pg_dumpall`: the new release manages its own roles
   and passwords, and a full dump would overwrite them.
4. Move clients to the new release's Services, then uninstall the old release.

## Connection pooling limits

PgBouncer runs in `transaction` mode by default. In that mode session state does not survive
between transactions: session-level prepared statements across transactions, advisory locks,
`LISTEN`/`NOTIFY`, `SET` without `LOCAL` and temporary tables do not work. Clients that need them
connect to `pg-primary` directly or use `pgbouncer.poolMode: session`.
