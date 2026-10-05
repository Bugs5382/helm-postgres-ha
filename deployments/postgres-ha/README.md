# postgres-ha 🐘

> 🛡️ Highly available PostgreSQL with Lease-based failover and fencing, WAL-G backup and restore, and PgBouncer.

PostgreSQL 18 (one major per chart line), three members by default, no operator and no CRDs of
its own. See [How failover works](../../docs/architecture.md) for the design and
[Runbooks](../../docs/runbooks.md) for day-two operations.

## 📋 Requirements

- Kubernetes 1.29 or later.
- [cert-manager](https://cert-manager.io), or your own TLS Secret (`tls.existingSecret`).
- A StorageClass, and one node per member for the default hard anti-affinity
  (`affinity.podAntiAffinity: soft` for small clusters).
- The agent image (`agent.image`). A published chart pins the image released with it; from a
  checkout, build it from this repository's `Dockerfile` and push it yourself.

## 🚀 Install

```bash
helm repo add postgres-ha https://bugs5382.github.io/helm-postgres-ha
helm install pg postgres-ha/postgres-ha -n db --create-namespace \
  --set 'postgresql.allowedCIDRs={10.244.0.0/16}'

# or from the OCI registry
helm install pg oci://ghcr.io/bugs5382/charts/postgres-ha --version <version> -n db --create-namespace \
  --set 'postgresql.allowedCIDRs={10.244.0.0/16}'
```

A published chart runs the agent image released with it, pinned by digest.

| Endpoint | Use |
| --- | --- |
| `pg-pgbouncer:5432` | Pooled connections (transaction pooling by default) |
| `pg-primary:5432` | Read-write |
| `pg-replica:5432` | Read-only, standbys within the readiness lag limit |

Clients need TLS: `sslmode=verify-full` with `ca.crt` from the `pg-tls` Secret. The app role's
password is in `pg-role-<role>` (`username` and `password` keys).

## ⚙️ Values

Every value is validated by `values.schema.json` and listed in the generated
[values reference](VALUES.md). The ones most installs touch:

| Value | Default | What it does |
| --- | --- | --- |
| `replicas` | `3` | Members. Three or more gives a quorum for failover. |
| `postgresql.allowedCIDRs` | required | Address ranges `pg_hba.conf` accepts, TLS and SCRAM only. |
| `postgresql.synchronous.mode` | `quorum` | `quorum`, `strict` or `off` (see the architecture doc). |
| `postgresql.parameters` | sizing defaults | Extra `postgresql.conf` settings. Restart-only ones roll the pods; the rest are reloaded in place. |
| `roles`, `databases` | `app` / `app` | Application roles (each with a generated or existing password Secret) and databases. |
| `credentials.*.existingSecret` | generated | Bring your own superuser, replication, rewind or PgBouncer password. |
| `tls.certManager.issuerRef` | per-release CA | Sign with your own CA issuer instead. |
| `backup.enabled`, `backup.s3.*` | off | WAL archiving and scheduled base backups with WAL-G. |
| `backup.storage`, `backup.file.*` | `s3` | `file` keeps backups and WAL on a mounted volume instead of S3, for single-node installs or a local export path. |
| `backup.verify.*` | on with backups | A daily restore check into a throwaway server, recorded on the backup Lease. |
| `bootstrap.mode`, `bootstrap.restore.*` | `initdb` | Restore a new release from a backup (S3 or a file store on a volume), optionally to a point in time or a named restore point. |
| `pgbouncer.*` | on, 2 replicas | Pool mode, pool size, client limit. After a promotion the new primary drops every pooler's connections, so clients move over in seconds. |
| `pgbouncer.sessionDatabases` | none | Databases pooled in session mode, for clients that need session state. |
| `networkPolicy.allowedClients` | none | Pods and namespaces allowed to reach PostgreSQL and PgBouncer. |
| `networkPolicy.extraEgress` | none | Extra egress for the members, such as object storage. |
| `agent.leaseDuration`, `agent.renewDeadline` | `15s`, `10s` | Failover timing. The deadline must be shorter than the duration minus the retry period. |
| `agent.maxLagOnFailover` | 1 MiB | How far behind a standby may be and still be promoted. |
| `metrics.exporters.enabled` | off | postgres_exporter beside each member (local socket, `pgha_monitor` with `pg_monitor`) and pgbouncer_exporter beside each pooler. |
| `metrics.prometheusRule.enabled` | off | Alerts: no primary, split brain, member not ready, replication lag, fencing, settings pending a restart, data volume filling, and with backups on, archiving failures, backup age and the restore check. |
| `agent.logLevel` | `error` | `debug` in development clusters, `info` in staging. |

## 📏 Sizing

Set `resources` for the postgres container, which runs PostgreSQL and the agent. Unless you set
them in `postgresql.parameters`, `shared_buffers` is a quarter and `effective_cache_size` three
quarters of `resources.limits.memory` (or the request when there is no limit). A memory change
that moves `shared_buffers` rolls the pods; other settings reload in place. The container has no
CPU limit by default, so a busy server never throttles the agent's Lease renewals. Size PgBouncer
with `pgbouncer.resources`, and the tools init container and hook Job with `agent.initResources`.

## 🔐 What is generated

- Secrets `pg-superuser`, `pg-replication`, `pg-rewind`, `pg-pgbouncer` and `pg-role-<role>`,
  created once by a hook Job and kept on uninstall, with the volumes they belong to.
- With cert-manager: a self-signed root, a CA Issuer and two certificates (members, PgBouncer).
- Leases `pg` and `pg-backup`, kept on uninstall so a reinstall keeps the cluster's identity.

## 🧪 Tests

`tests/` holds helm-unittest suites, and `test/chart/checks.sh` runs them with `helm lint` and
kubeconform. `test/e2e/` drives failover, fencing, switchover and a point-in-time restore on kind.
