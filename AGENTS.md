# AGENTS.md - helm-postgres-ha

Guide for AI agents working in this repository. Pair with `CLAUDE.md` (the working agreement and
hook-enforced rules). Keep this file current when the build, layout, or public API changes.

## What this is

General-purpose highly available PostgreSQL Helm chart: streaming replication, lease-based failover with fencing, WAL-G backup and restore, and PgBouncer.

It ships two things that are versioned together:

- `pgha`, a Go agent that runs as PID 1 in each member's postgres container and supervises
  PostgreSQL (`cmd/pgha`, `internal/`), and the image that carries it with a static `wal-g`
  (`Dockerfile`);
- the `postgres-ha` Helm chart (`deployments/postgres-ha`).

Two things to understand before changing it:

1. The cluster Lease is the only source of truth. PostgreSQL may run read-write only while its pod
   holds the Lease, and the watchdog fences a primary whose renewals stop. Never add a path that
   promotes, or routes clients, without holding the Lease. See `docs/architecture.md`.
2. The chart never patches Services. Routing follows the `postgres-ha/role` pod label the agent
   sets, so Helm upgrades and GitOps resyncs cannot repoint clients.

## Using helm-postgres-ha

Consumers install the chart (`helm install ... ./deployments/postgres-ha`) with
`postgresql.allowedCIDRs` set and the agent image available. The values schema is the contract;
values the agent owns (`primary_conninfo`, `restore_command`, `synchronous_standby_names`, the TLS
and archive settings) are refused in `postgresql.parameters`.

## Layout

- `cmd/pgha/` - the binary; `internal/commands/` wires the cobra commands (`run`, `install`,
  `secrets ensure`, `switchover`, `backup`, `version`).
- `internal/agent/` - the control loop: bootstrap, follow, fence, elect, promote, rejoin, handover.
- `internal/election/` - the pure election rules (quorum, ranking, successor).
- `internal/lease/`, `internal/kube/` - the Lease and the role label.
- `internal/peer/`, `internal/server/` - the mutual-TLS status API, probes and metrics.
- `internal/pg/` - the postmaster supervisor, server tools, SQL, roles reconciler, SCRAM.
- `internal/pooler/` - resets PgBouncer's connections after a promotion (admin console over TLS).
- `internal/verify/` - the scheduled restore check (`pgha verify-restore`).
- `internal/backup/` - the WAL-G backup scheduler; `internal/secrets/` - the credential hook.
- `internal/errs/` - coded errors; `docs/errors.md` must list every code (a test checks it).
- `deployments/postgres-ha/` - the chart, its schema, `tests/` (helm-unittest) and `ci/` values.
- `test/chart/checks.sh` - chart checks; `test/e2e/` - kind failover and backup suites.

The repo follows the `go/app` layout except that the binary lives in `cmd/pgha` (it is not a
server only) and the transport package is `internal/server` plus `internal/peer`.

## Build, test, lint

- Build: `go build ./...`; image: `docker build -t pgha:dev .`
- Test: `go test ./...` (run under `systemd-run --user --scope -p MemoryMax=6G` on shared boxes)
- Real PostgreSQL: `go test -tags integration ./internal/pg/` starts a primary and a streaming
  standby from the chart's pinned image in Docker and checks status, promotion and roles.
- Lint: `task lint` (gofmt, golangci-lint, yamllint)
- Chart: `bash test/chart/checks.sh` (needs helm, kubeconform, yq and the helm-unittest plugin)
- End to end: create the kind cluster from `test/e2e/kind.yaml`, install cert-manager, load the
  image tagged `ghcr.io/bugs5382/helm-postgres-ha/pgha:e2e`, then `test/e2e/install.sh`,
  `test/e2e/run.sh` and `test/e2e/backup.sh`. CI does exactly this in `.github/workflows/checks.yaml`.
- License headers: `task license` (golic, Go sources).

## Logging

Follow the logging rules in `CLAUDE.md`. In short:

- Log generously: entry and exit of significant operations, decisions and branches, retries, state
  changes, external calls (target, duration, outcome), and every error with its context.
- Levels: `trace` for step-by-step detail, `debug` for flow, `info` for lifecycle, `warn` and
  `error` for problems. The environment filters the volume, so err on the side of too much.
- Environments: local dev `trace` with `LOG_FORMAT=console` (never JSON), dev cluster `debug`,
  qa/staging `info`, production `error`. Every cluster environment logs JSON. Set levels through
  `LOG_LEVEL` and `LOG_FORMAT`, never in code; local settings live in the run target or
  `.env.example`.
- Never log secrets, tokens, or personal data, not even at `trace`. Log an opaque or keyed ID.

## Conventions and gotchas

- See `CLAUDE.md` for the branch/commit/PR rules; they are enforced by the git hooks in
  `.claude/hooks` (run `bash .claude/hooks/install.sh` once per clone).
- Open every PR as a draft. CI skips drafts, so run the full checks locally, push once they pass,
  and mark the PR ready when the work is finished; see CLAUDE.md "CI and Actions minutes".
- Every SQL statement in the control loop must be bounded by a context timeout, and anything that
  can take long (role DDL, promotion, clone, rewind, backups) runs off the loop. A blocked loop
  stops renewals and the watchdog fences the primary.
- The e2e suites assert the behaviour each tracked issue specifies; when you change failover
  behaviour, change the matching step and keep it failing first.
