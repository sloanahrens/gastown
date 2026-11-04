# Gas Town polecat image for OpenShell sandboxes.
#
# Build: `make polecat-image` (cross-compiles gt and bd into bin/, then runs
# docker build on this directory and tags gastown-polecat:<short sha> and :dev).
#
# The image carries the Claude Code CLI, the Go toolchain and git, plus the
# town's own binaries from bin/. OpenShell ignores ENTRYPOINT and
# CMD: the sandbox runtime starts the main process given on
# `openshell sandbox create -- ...`.
#
# Every download is pinned to a version and checked against a digest recorded
# here, and every external image stage is pinned by digest. Update a digest only
# together with the version it belongs to.

# The Docker client, for the tier-sweep runner's container-backed packages:
# internal/testutil decides Docker is available with `docker info`, and the
# runner reaches the town's engine over TCP, never a mounted socket. The client
# is copied from the engine image the sweep's Docker-in-Docker sidecar runs
# (internal/templates/images.go SweepDindImage), pinned by that image's index
# digest: a registry pull verifies every layer, where a tarball fetched in a
# RUN is verified only if a sha256 is written out beside it and kept in step.
# Only the client binary reaches the image below, so a sandbox gets a CLI and
# no daemon (gt-9lmam). TestPolecatImageInstallsDockerCLIPinned keeps this digest
# equal to SweepDindImage's.
ARG DOCKER_CLI_DIGEST=sha256:9e79b879b177fbd72c34376a3dd3d91747da5db49e1f560f76aea59561471977
FROM data.forgejo.org/oci/docker@${DOCKER_CLI_DIGEST} AS docker-cli

# GNU Make 4.4.1 built from source: Ubuntu's Make 4.3 fails inside OpenShell
# sandboxes because its recipe child calls setresuid/setresgid with unchanged
# ids, which the sandbox seccomp filter denies (NVIDIA/OpenShell#4102). 4.4.1
# does not make that call.
FROM ubuntu:24.04 AS make-build
ARG MAKE_VERSION=4.4.1
# sha256 of make-4.4.1.tar.gz as fetched from ftp.gnu.org on 2026-10-22.
ARG MAKE_SHA256=dd16fb1d67bfab79a72f5e8390735c49e3e8e70b4945a15ab1f81ddb78658fb3
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends build-essential ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && cd /tmp && curl -fsSL -o make.tar.gz "https://ftp.gnu.org/gnu/make/make-${MAKE_VERSION}.tar.gz" \
    && echo "${MAKE_SHA256}  make.tar.gz" | sha256sum -c - \
    && tar xzf make.tar.gz \
    && cd "make-${MAKE_VERSION}" && ./configure --prefix=/usr/local >/dev/null && make -j4 >/dev/null \
    && cp make /usr/local/bin/make

FROM ubuntu:24.04

ARG GO_VERSION=1.27.1
# GO_FLOOR is the highest `go` line of the rigs' go.mod files. make polecat-image
# reads it from those files (deploy/polecat/go-floor.sh rig) and passes it in.
# There is no default: an image built without it fails the check below.
ARG GO_FLOOR
# sha256 of go1.27.1.linux-arm64.tar.gz and go1.27.1.linux-amd64.tar.gz, as listed
# by https://go.dev/dl/?mode=json&include=all and matching the tarballs' own bytes,
# checked on 2026-10-23. Update a digest only together with GO_VERSION.
ARG GO_SHA256_ARM64=3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec
ARG GO_SHA256_AMD64=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
ARG NODE_MAJOR=22
ARG NODE_VERSION=22.23.4-1nodesource1
# sha256 of https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key on 2026-10-22.
ARG NODESOURCE_KEYFILE_DIGEST=b42e0321dabdc24e892115da705cf061167eac12a317f23d329862d0aa0a271d
ARG PG_MAJOR=17
ARG PG_VERSION=17.11-1.pgdg24.04+2
# sha256 of https://www.postgresql.org/media/keys/ACCC4CF8.asc as served, computed on
# the host on 2026-10-25 (primary key fingerprint B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8).
# Update a digest only together with PG_VERSION.
ARG PGDG_KEYFILE_DIGEST=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
ARG CLAUDE_CODE_VERSION=2.1.309
ARG GOLANGCI_LINT_VERSION=v2.13.2
# libicu-dev supplies the ICU headers dolthub/go-icu-regex's cgo build needs; a
# sandbox that builds that package fails without them (gt-gu1b7).
# Version read with `apt-cache policy libicu-dev` in ubuntu:24.04 on 2026-10-26;
# re-read it if the base image tag moves or noble supersedes the candidate, since
# the exact pin fails the build once superseded.
ARG LIBICU_VERSION=74.2-1ubuntu3.1
# lsof lets the sweep runner's doltserver integration test name the process that
# holds a port (internal/doltserver TestIntegrationProcessIdentity); the image
# has neither lsof nor ss otherwise. Version read the same way as LIBICU_VERSION,
# with `apt-cache policy lsof`, on 2026-11-01.
ARG LSOF_VERSION=4.95.0-1build3

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl git gnupg jq openssh-client \
    "libicu-dev=${LIBICU_VERSION}" "lsof=${LSOF_VERSION}" \
    ripgrep shellcheck tmux unzip xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Node.js from NodeSource (signing key checked by digest, package version
# pinned) and the Claude Code CLI at a pinned version (npm verifies the
# registry's integrity hash for a pinned version).
RUN mkdir -p /etc/apt/keyrings \
    && curl -fsSL -o /tmp/nodesource.key https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
    && echo "${NODESOURCE_KEYFILE_DIGEST}  /tmp/nodesource.key" | sha256sum -c - \
    && gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg < /tmp/nodesource.key && rm /tmp/nodesource.key \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
       > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update && apt-get install -y --no-install-recommends "nodejs=${NODE_VERSION}" \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && npm cache clean --force

# PostgreSQL 17 server binaries from PGDG for the tracker's integration tests
# (signing key checked by digest, package version pinned). The package would create
# a default cluster at install, so cluster creation is switched off first: no
# service runs and no data directory is made here. Tests make their own cluster in a temp dir.
# PG_VERSION was taken from the noble-pgdg Packages index on 2026-10-25, on the host.
RUN mkdir -p /etc/apt/keyrings /etc/postgresql-common \
    && printf 'create_main_cluster = false\n' > /etc/postgresql-common/createcluster.conf \
    && curl -fsSL -o /tmp/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc \
    && echo "${PGDG_KEYFILE_DIGEST}  /tmp/pgdg.asc" | sha256sum -c - \
    && gpg --dearmor -o /etc/apt/keyrings/pgdg.gpg < /tmp/pgdg.asc && rm /tmp/pgdg.asc \
    && echo "deb [signed-by=/etc/apt/keyrings/pgdg.gpg] https://apt.postgresql.org/pub/repos/apt noble-pgdg main" \
       > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update && apt-get install -y --no-install-recommends "postgresql-${PG_MAJOR}=${PG_VERSION}" \
    && rm -rf /var/lib/apt/lists/*

# Go from the official tarball, checked against the published digest.
RUN ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in arm64) SHA="$GO_SHA256_ARM64" ;; amd64) SHA="$GO_SHA256_AMD64" ;; *) echo "unsupported arch $ARCH" >&2; exit 1 ;; esac \
    && curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" \
    && echo "${SHA}  /tmp/go.tgz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz

# The Dolt client, at the version the town's server runs (internal/templates/
# images.go DoltVersion, which TestPolecatImageInstallsDoltPinned keeps equal to
# DOLT_VERSION): the sweep runner's internal/doltserver, internal/deps
# and internal/cmd tests start or check a server and need it on PATH (gt-q7wkn).
ARG DOLT_VERSION=2.3.2
# sha256 of dolt-linux-arm64.tar.gz and dolt-linux-amd64.tar.gz at
# https://github.com/dolthub/dolt/releases/download/v2.3.2/, read from the
# release page and re-computed from each tarball on the host on 2026-10-31.
# Update a digest only together with DOLT_VERSION.
ARG DOLT_SHA256_ARM64=b2231e84e06adf95ea81c6e889409ee7a72de5a96cb03bbf1bf3433ac763cf9c
ARG DOLT_SHA256_AMD64=7a2949fa2b2b3799ee1e57e6d64519a8d65d675fd832f6469d4e07e5a1c72b14
RUN ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in arm64) SHA="$DOLT_SHA256_ARM64" ;; amd64) SHA="$DOLT_SHA256_AMD64" ;; *) echo "unsupported arch $ARCH" >&2; exit 1 ;; esac \
    && curl -fsSL -o /tmp/dolt.tgz "https://github.com/dolthub/dolt/releases/download/v${DOLT_VERSION}/dolt-linux-${ARCH}.tar.gz" \
    && echo "${SHA}  /tmp/dolt.tgz" | sha256sum -c - \
    && tar -C /tmp -xzf /tmp/dolt.tgz \
    && install -m 0755 "/tmp/dolt-linux-${ARCH}/bin/dolt" /usr/local/bin/dolt \
    && rm -rf /tmp/dolt.tgz "/tmp/dolt-linux-${ARCH}"

# The Docker client, from the image above: the CLI alone. The engine's dockerd
# and containerd stay out of this image, so a seat that runs it gets no daemon
# and no new capability (gt-9lmam).
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker

# Refuse the image when its Go is below GO_FLOOR (see go-floor.sh for the rule).
COPY go-floor.sh /usr/local/lib/gastown/go-floor.sh
RUN bash /usr/local/lib/gastown/go-floor.sh check "$GO_FLOOR"

# GNU Make 4.4.1 ahead of /usr/bin/make on PATH (see the make-build stage).
COPY --from=make-build /usr/local/bin/make /usr/local/bin/make
# The same binary as /usr/bin/make too, so a restricted PATH never finds Ubuntu's
# 4.3. Why: TestIntegrationPolecatImageReplacesTheDistroMake.
RUN ln -sf /usr/local/bin/make /usr/bin/make \
    && /usr/bin/make --version | head -1 | grep -q 'GNU Make 4.4.1' \
    && /bin/make --version | head -1 | grep -q 'GNU Make 4.4.1'

# golangci-lint at the version the gastown Makefile pins (GOLANGCI_LINT_VERSION);
# make lint refuses to run without it, and gt done runs make presubmit. The Go
# module proxy verifies the module against the checksum database.
RUN GOBIN=/usr/local/bin GOFLAGS= /usr/local/go/bin/go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" \
    && rm -rf /root/go /root/.cache

# Non-root sandbox user; OpenShell runs the workload as the image USER and
# uses /sandbox as the workspace.
RUN userdel -r ubuntu 2>/dev/null || true \
    && groupadd -g 1000 sandbox \
    && useradd -m -u 1000 -g 1000 -s /bin/bash sandbox \
    && mkdir -p /sandbox /sandbox/.cache/go-build /sandbox/go \
    && chown -R sandbox:sandbox /sandbox

# The OpenShell Kubernetes driver runs the workload as uid/gid 10001, not as the
# image USER, so that uid needs a name too: without it `id -un`, `ls -l`, chown
# and git or ssh lookups of the current user fail ("unknown userid 10001").
# Home is the shared /sandbox, which already exists and is world-writable.
RUN groupadd -g 10001 polecat \
    && useradd -M -u 10001 -g 10001 -d /sandbox -s /bin/bash polecat

# Town binaries, cross-compiled by `make polecat-image` (CGO_ENABLED=0 linux/arm64).
COPY bin/gt /usr/local/bin/gt
COPY bin/bd /usr/local/bin/bd
# The trim helper a cache-volume container runs; plugin.md, "Per-rig cache
# volumes", says why the trim has to run in the image.
COPY bin/polecat-image /usr/local/bin/polecat-image
# The entrypoint a tier-sweep runner container runs (cmd/sweep-run, gt-q7wkn); its
# path is sweeprunner.RunnerCommand, which TestPolecatImageCopiesTheSweepRunner
# keeps equal to this COPY's target.
COPY bin/sweep-run /usr/local/bin/sweep-run
RUN chmod 0755 /usr/local/bin/gt /usr/local/bin/bd /usr/local/bin/polecat-image /usr/local/bin/sweep-run

ENV PATH="/usr/local/go/bin:/sandbox/go/bin:${PATH}" \
    GOPATH=/sandbox/go \
    GOFLAGS=-p=4 \
    BD_DISABLE_METRICS=1 \
    DO_NOT_TRACK=1 \
    DISABLE_AUTOUPDATER=1 \
    HOME=/sandbox \
    TERM=xterm-256color

USER sandbox
WORKDIR /sandbox
