# helm-postgres-ha 🐘

> 🛡️ A highly available PostgreSQL Helm chart: streaming replication, Lease-based failover with fencing, WAL-G backup and point-in-time restore, and PgBouncer.

## 🤖 The agent

`pgha` runs as PID 1 in each member's PostgreSQL container and keeps the member in its role. A
Kubernetes Lease is the single source of truth: **PostgreSQL runs read-write only while its pod
holds the Lease**, and a primary that cannot renew it stops itself before anyone else may take
over. The chart that deploys it follows in this repository.

- 🔒 **Fencing** — a watchdog stops a primary whose renewals stop, within the renew deadline.
- 🗳️ **Quorum failover** — a standby takes over only when a majority of members answers, none of
  them still sees a primary, and it holds the most WAL.
- 🔁 **Rejoin** — a former primary is rewound with `pg_rewind`, or moved aside and re-cloned.
- 💾 **Backups** — WAL-G base backups on a schedule, taken from a standby, and point-in-time restore.

Build the image (the agent plus a static `wal-g`) with `docker build -t pgha:dev .`.

## 📚 Docs

- [How failover works](docs/architecture.md)
- [Agent error codes](docs/errors.md)

## 🛠️ Develop

```bash
task build    # go build ./...
task test     # go test ./...
task lint     # gofmt check + golangci-lint + yamllint
task license  # verify MIT headers (golic)
```

## ⚖️ License

MIT © 2026 Shane
