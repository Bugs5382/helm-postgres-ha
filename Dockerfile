# syntax=docker/dockerfile:1.7
# The agent image: pgha and a static wal-g on a distroless base. It is never
# run as the database itself; the chart's init container copies both binaries
# into the official PostgreSQL image's pod.
ARG GO_IMAGE=docker.io/library/golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Both Go stages run on the build machine and cross-compile for the target
# (both binaries are static), so a multi-platform build needs no emulation.
# The final stage picks the target's variant of the distroless base.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS walg
ARG TARGETOS TARGETARCH
ARG WALG_VERSION=v3.0.9
ARG WALG_COMMIT=3e493188db28335fb93a45bd680c6ff8fc3028c1
RUN git clone --depth 1 --branch "${WALG_VERSION}" https://github.com/wal-g/wal-g /src \
 && test "$(git -C /src rev-parse HEAD)" = "${WALG_COMMIT}"
WORKDIR /src
# The PostgreSQL build without the cgo-only compressors (brotli, lzo,
# libsodium), so the binary is static and runs in any image.
ENV CGO_ENABLED=0 GOEXPERIMENT=jsonv2 GOFLAGS=-mod=mod
# Dependencies with published fixes newer than the WAL-G release pins.
# scripts/ci/vuln-gate.sh fails CI when the built binary has a fixable
# finding, so drop an entry here once a WAL-G release includes it.
ARG WALG_BUMPS="google.golang.org/grpc@v1.83.2 golang.org/x/crypto@v0.56.0 go.opentelemetry.io/otel/sdk@v1.45.0"
# hadolint ignore=SC2086
RUN go get ${WALG_BUMPS} && go mod tidy
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/wal-g/wal-g/cmd/pg.walgVersion=${WALG_VERSION} -X github.com/wal-g/wal-g/cmd/pg.gitRevision=${WALG_COMMIT}" \
      -o /out/wal-g ./main/pg

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS agent
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/Bugs5382/helm-postgres-ha/internal/commands.Version=${VERSION}" \
      -o /out/pgha ./cmd/pgha

FROM ${BASE_IMAGE}
COPY --from=walg /out/wal-g /usr/local/bin/wal-g
COPY --from=agent /out/pgha /usr/local/bin/pgha
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pgha"]
