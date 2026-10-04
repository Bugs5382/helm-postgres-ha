# helm-postgres-ha 🐘

> 🛡️ A highly available PostgreSQL Helm chart: streaming replication, Lease-based failover with fencing, WAL-G backup and point-in-time restore, and PgBouncer.

Each member runs the official PostgreSQL image with a small Go agent, `pgha`, as PID 1. The agent
supervises the server and keeps the member in its role. A Kubernetes Lease is the single source
of truth: **PostgreSQL runs read-write only while its pod holds the Lease**, and a primary that
cannot renew it stops itself before anyone else may take over.

## ✨ Highlights

- 🔒 **Fencing, not hope** — a primary that loses the API server stops PostgreSQL within the renew
  deadline, well before the Lease can expire for anyone else.
- 🗳️ **Quorum failover** — a standby takes over only when a majority of members answers, none of
  them still sees a primary, and it holds the most WAL.
- 🔁 **Self-healing members** — a former primary is rewound with `pg_rewind`, or moved aside and
  re-cloned, and follows the new primary on its own.
- 🤝 **Mutual TLS everywhere** — PostgreSQL, replication, PgBouncer and the members' peer API all
  use TLS from a per-release CA; passwords are SCRAM only.
- 💾 **WAL-G backups and PITR** — continuous archiving, scheduled base backups taken from a standby,
  and a `restore` bootstrap to any point in time.
- 🧱 **Secure defaults** — NetworkPolicies, a PodDisruptionBudget, one member per node,
  least-privilege RBAC and the restricted Pod Security Standard.

## 🚀 Install

Requirements: Kubernetes 1.29 or later, [cert-manager](https://cert-manager.io) (or your own TLS
Secret), and a StorageClass.

```bash
helm install pg ./deployments/postgres-ha \
  --namespace db --create-namespace \
  --set 'postgresql.allowedCIDRs={10.244.0.0/16}'
```

`postgresql.allowedCIDRs` is required: set it to your pod network. Then connect through
PgBouncer (`pg-pgbouncer`), or straight to `pg-primary` (read-write) or `pg-replica` (read-only),
with `sslmode=verify-full` and the CA from the `pg-tls` Secret.

The agent image is built from this repository's `Dockerfile` (`pgha` plus a static `wal-g`).
Until a release publishes it, build and push it yourself and set `agent.image`.

## 📚 Docs

- [Chart README and values](deployments/postgres-ha/README.md)
- [How failover works](docs/architecture.md)
- [Runbooks](docs/runbooks.md): switchover, failover, rebuilding a member, restore and PITR,
  certificates, major upgrades
- [Agent error codes](docs/errors.md)

## 🛠️ Develop

```bash
task test                     # Go unit tests
task lint                     # gofmt, golangci-lint, yamllint
bash test/chart/checks.sh     # helm lint, kubeconform, schema and unit tests
bash test/e2e/run.sh          # failover tests on a kind cluster (see test/e2e)
```

## ⚖️ License

MIT © 2026 Shane
