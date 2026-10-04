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
- The agent image built from this repository's `Dockerfile` (`agent.image`).

## 🚀 Install

```bash
helm install pg ./deployments/postgres-ha -n db --create-namespace \
  --set 'postgresql.allowedCIDRs={10.244.0.0/16}'
```

| Endpoint | Use |
| --- | --- |
| `pg-pgbouncer:5432` | Pooled connections (transaction pooling by default) |
| `pg-primary:5432` | Read-write |
| `pg-replica:5432` | Read-only, standbys within the readiness lag limit |

Clients need TLS: `sslmode=verify-full` with `ca.crt` from the `pg-tls` Secret. The app role's
password is in `pg-role-<role>` (`username` and `password` keys).

## ⚙️ Values

Every value is validated by `values.schema.json`; `values.yaml` documents each one. The ones most
installs touch:

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
| `bootstrap.mode`, `bootstrap.restore.*` | `initdb` | Restore a new release from a backup, optionally to a point in time. |
| `pgbouncer.*` | on, 2 replicas | Pool mode, pool size, client limit. |
| `networkPolicy.allowedClients` | none | Pods and namespaces allowed to reach PostgreSQL and PgBouncer. |
| `networkPolicy.extraEgress` | none | Extra egress for the members, such as object storage. |
| `agent.leaseDuration`, `agent.renewDeadline` | `15s`, `10s` | Failover timing. The deadline must be shorter than the duration minus the retry period. |
| `agent.maxLagOnFailover` | 1 MiB | How far behind a standby may be and still be promoted. |
| `agent.logLevel` | `error` | `debug` in development clusters, `info` in staging. |

## 🔐 What is generated

- Secrets `pg-superuser`, `pg-replication`, `pg-rewind`, `pg-pgbouncer` and `pg-role-<role>`,
  created once by a hook Job and kept on uninstall, with the volumes they belong to.
- With cert-manager: a self-signed root, a CA Issuer and two certificates (members, PgBouncer).
- Leases `pg` and `pg-backup`, kept on uninstall so a reinstall keeps the cluster's identity.

## 🧪 Tests

`tests/` holds helm-unittest suites, and `test/chart/checks.sh` runs them with `helm lint` and
kubeconform.
