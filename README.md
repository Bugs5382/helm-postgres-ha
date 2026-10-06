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

From the Helm repository on GitHub Pages:

```bash
helm repo add postgres-ha https://bugs5382.github.io/helm-postgres-ha
helm install pg postgres-ha/postgres-ha \
  --namespace db --create-namespace \
  --set 'postgresql.allowedCIDRs={10.244.0.0/16}'
```

Or straight from the OCI registry:

```bash
helm install pg oci://ghcr.io/bugs5382/charts/postgres-ha --version <version> \
  --namespace db --create-namespace \
  --set 'postgresql.allowedCIDRs={10.244.0.0/16}'
```

`postgresql.allowedCIDRs` is required: set it to your pod network. Then connect through
PgBouncer (`pg-pgbouncer`), or straight to `pg-primary` (read-write) or `pg-replica` (read-only),
with `sslmode=verify-full` and the CA from the `pg-tls` Secret.

Each release publishes the agent image (`ghcr.io/bugs5382/helm-postgres-ha/pgha`, built from this
repository's `Dockerfile`: `pgha` plus a static `wal-g`) and pins its digest in the published
chart. Installing from a checkout (`./deployments/postgres-ha`) needs an agent image you build and
push yourself, set with `agent.image`.

## 🐘 Supported PostgreSQL

Each chart line ships one PostgreSQL major, pinned by digest: this line ships **18** (`appVersion`
in `Chart.yaml`). The `E2E matrix` workflow runs every cell below nightly and on every release;
a major is supported, through `image.*`, while its cells are green there:

| Major | Plain (`postgres:<major>-bookworm`) | pgvector (`pgvector/pgvector:0.8.7-pg<major>-bookworm`) |
| --- | --- | --- |
| 18 | failover, backup and PITR (also on every PR) | + vector extension, hnsw index, switchover and rejoin, PITR |
| 17 | failover, backup and PITR | + vector extension, hnsw index, switchover and rejoin, PITR |
| 16 | failover, backup and PITR | + vector extension, hnsw index, switchover and rejoin, PITR |
| 15 | failover, backup and PITR | + vector extension, hnsw index, switchover and rejoin, PITR |

A data directory made by another major refuses to start (code `1103`); see the major upgrade
runbook. The agent image is published for `linux/amd64` and `linux/arm64`, and every image the
chart pins has both variants.

## 📚 Docs

- [Chart README](deployments/postgres-ha/README.md) and the [values reference](deployments/postgres-ha/VALUES.md)
- [How failover works](docs/architecture.md)
- [Runbooks](docs/runbooks.md): switchover, failover, rebuilding a member, restore and PITR,
  certificates, major upgrades, pooling limits (CI runs every runbook command)
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
