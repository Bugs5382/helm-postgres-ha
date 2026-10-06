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
- `test/chart/checks.sh` - chart checks; `test/e2e/` - kind failover, backup and runbook suites.

The repo follows the `go/app` layout except that the binary lives in `cmd/pgha` (it is not a
server only) and the transport package is `internal/server` plus `internal/peer`.

## Build, test, lint

- Build: `go build ./...`; image: `docker build -t pgha:dev .`
- Image dependencies: WAL-G is built from source at a pinned release and commit. `WALG_BUMPS` in
  the `Dockerfile` raises its dependencies that have published fixes. CI runs govulncheck on
  both binaries in the image, and `scripts/ci/vuln-gate.sh` fails on any finding with a fixed
  version. Findings with no upstream fix are listed but do not fail: today, two aws-sdk-go v1
  advisories in its S3 client-side encryption package (GO-2022-0635 and GO-2022-0646), and
  GO-2026-5932 in golang.org/x/crypto.
- Test: `go test ./...` (run under `systemd-run --user --scope -p MemoryMax=6G` on shared boxes)
- Real PostgreSQL: `go test -tags integration ./internal/pg/` starts a primary and a streaming
  standby from the chart's pinned image in Docker and checks status, promotion and roles.
- Lint: `task lint` (gofmt, golangci-lint, yamllint)
- Chart: `bash test/chart/checks.sh` (needs helm, kubeconform, yq, promtool, helm-docs and the
  helm-unittest plugin); alert rule unit tests live in `deployments/postgres-ha/tests/rules`
- Values reference: `deployments/postgres-ha/VALUES.md` is generated from the `# --` comments in
  `values.yaml`. Every value gets one. After changing values, run
  `helm-docs --chart-search-root deployments/postgres-ha --template-files=VALUES.md.gotmpl --output-file=VALUES.md`;
  the chart checks fail when it is stale.
- End to end: create the kind cluster from `test/e2e/kind.yaml`, install cert-manager, load the
  image tagged `ghcr.io/bugs5382/helm-postgres-ha/pgha:e2e`, then `test/e2e/install.sh`,
  `test/e2e/run.sh`, `test/e2e/backup.sh` and `test/e2e/runbooks.sh`. The last one runs every
  `bash` block in `docs/runbooks.md` against the backup suite's release, so a runbook command
  that no longer works fails CI. CI does exactly this in `.github/workflows/checks.yaml`.
- The `E2E matrix` workflow (`.github/workflows/e2e-matrix.yaml`) runs the failover and backup
  suites, and `test/e2e/pgvector.sh` in pgvector cells, over PostgreSQL 15 to 18, plain and
  pgvector, each image pinned by digest. It runs nightly, on a published release and on dispatch,
  never per PR. Keep the README support table in step with its cells.
- CI (`.github/workflows/checks.yaml`) has two required jobs:
  - `🧪 Checks` runs on every PR in a few minutes: the chart checks above, the release pin test
    and the e2e scope test.
  - `🧪 E2E` runs the image build, the real-PostgreSQL tests and every kind suite. On a PR it
    runs only when `scripts/ci/e2e-scope.sh` says so: the `e2e` label, or changes under `cmd/`,
    `internal/`, `deployments/`, `test/e2e/`, the `Dockerfile`, `go.mod`/`go.sum` or the
    workflow itself. Otherwise it is skipped, which counts as passing.
  - It always runs on pushes to `main`, nightly, on a published release and on manual dispatch.
  - Add the `e2e` label to force the suites on any PR; it takes effect on the next push (label
    events do not start a run). Superseded runs are cancelled, and the Go module cache and the
    kind node image are cached between runs.
- License headers: `task license` (golic, Go sources).

## Releasing

Nothing publishes on a merge. When the maintainer publishes a GitHub Release `vX.Y.Z`,
`.github/workflows/job-release.yaml`:

1. builds and pushes the agent image `ghcr.io/bugs5382/helm-postgres-ha/pgha:X.Y.Z`;
2. runs `scripts/release/pin-chart.sh X.Y.Z <digest>`, which sets the chart version and pins the
   image digest in the packaged chart's values (`test/release/pin_test.sh` covers it);
3. attaches the chart to the release and pushes it to `oci://ghcr.io/bugs5382/charts`;
4. adds it to the Helm repository on GitHub Pages (`index.yaml` on `gh-pages`, through
   chart-releaser). The first release creates `gh-pages` and turns Pages on.

`main` keeps `agent.image.digest` empty; the release tag is the one source of the version.

Before the first release, once:

- add the release App's client ID and private key as the `APP_CLIENT_ID` and `APP_PRIVATE_KEY`
  repository secrets, with the App installed on this repository;
- if the release run warns that it could not turn on Pages, set Settings > Pages > Source to the
  `gh-pages` branch (root folder);
- after it runs, make the `helm-postgres-ha/pgha` and `charts/postgres-ha` packages public in the
  package settings, and link `charts/postgres-ha` to this repository.

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
