# Gas Town polecat image for OpenShell sandboxes.
#
# Build: `make polecat-image` (cross-compiles gt and bd into bin/, then runs
# docker build on this directory and tags gastown-polecat:<short sha> and :dev).
#
# The image carries the Claude Code CLI, the Go toolchain and git, plus the
# town's own `gt` and `bd` binaries from bin/. OpenShell ignores ENTRYPOINT and
# CMD: the sandbox runtime starts the main process given on
# `openshell sandbox create -- ...`.
#
# Every download is pinned to a version and checked against a digest recorded
# here. Update a digest only together with the version it belongs to.

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
# libicu-dev supplies the ICU headers dolthub/go-icu-regex's cgo build needs, so
# the seed's vet and test compile steps cache instead of failing (gt-gu1b7).
# Version read with `apt-cache policy libicu-dev` in ubuntu:24.04 on 2026-10-26;
# re-read it if the base image tag moves or noble supersedes the candidate, since
# the exact pin fails the build once superseded.
ARG LIBICU_VERSION=74.2-1ubuntu3.1

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl git gnupg jq openssh-client \
    "libicu-dev=${LIBICU_VERSION}" \
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

# Seeded Go caches (gt-ieuvf): a cold sandbox otherwise compiles every dependency
# of the rigs' modules before a presubmit can run. Both sit in /tmp, which
# deploy/polecat/policy.yaml grants read_write, so a sandbox can also fill them.
#
# Both live outside /sandbox, which the workspace-init container copies into every
# sandbox's volume: outside it, one image layer is shared and a write falls to the
# sandbox's own layer. /tmp gets no mount of its own (a live sandbox's
# /proc/mounts), so the seeded caches are there at runtime. A branch that moved
# its go.mod ahead of the image needs a module the seed lacks, and the writable
# path is what fetches it: the new files land in the sandbox's layer, the seeded
# ones stay as built and shared (gt-pvudz).
#
# The sources are the checkouts `make polecat-seed-stage` writes into
# deploy/polecat/seed: gastown at the commit the image is built at, every other
# rig at its deploy/polecat/seed-pins commit. The directory is bind-mounted for
# this one RUN so none of it lands in the image. Each module gets the commands
# the rigs' presubmits run, with their default flags (no -trimpath, default CGO)
# so the cache keys match. `go mod download` runs first because a linux build
# fetches only the modules linux imports. There is no darwin build here: the
# one presubmit test that cross-built for darwin/arm64 (devops'
# TestCloudqueryBinPinsAreARebuildOfTheCommit, gt-mukre) moved to the host tier
# in dv-0qre, so the sandbox's unit tier never needs the darwin cache. Macos
# builds stay off the Linux image.
#
# A failing linux `go build` or `go mod download` fails the image: the seed
# would otherwise publish a hole that the read-only cache cannot fill at
# runtime. A failing vet or test compile is logged and skipped: a module may
# need system headers the image lacks. The chmod for uid 10001 is in this same
# RUN, as one in a later layer would copy every file again.
#
# A rig's example modules are skipped and named in the install log: they belong
# to no rig's presubmit build, and a stale go.sum in one fails the whole image
# (gt-9yqcc). Only the examples directory directly under a rig is skipped; a
# module inside the rig's own tree is built like any other. This rewrites the
# walk's `-not -path */examples/*` filter that 813632d4 added (gt-vvu6d). A
# failure shows the first 40 lines of its log, where `go` writes the error that
# caused it. Past 40 lines it adds the last 10, where a failed download or
# toolchain error sits, so a log of 51 lines or more drops lines 41 up to its
# last 10; the heading gives the log's total length (gt-vvu6d).
ENV GOCACHE=/tmp/gocache \
    GOMODCACHE=/tmp/gomodcache
RUN --mount=type=bind,source=seed,target=/seed \
    export PATH="/usr/local/go/bin:$PATH" GOFLAGS="-p=4 -modcacherw" \
    && mkdir -p "$GOCACHE" "$GOMODCACHE" \
    && for gomod in $(find /seed -name go.mod -not -path '*/testdata/*' | sort); do \
         rel=${gomod#/seed/}; rig=${rel%%/*}; \
         case "$rel" in "$rig"/examples/*) \
           echo "seed: skip $gomod: a rig's example module is in no presubmit build"; continue ;; \
         esac; \
         dir=$(dirname "$gomod"); echo "seed: $dir"; \
         for step in "go mod download" "go build ./..." "go vet ./..." \
                     "go test -count=1 -run ^\$ ./..."; do \
           if ! (cd "$dir" && $step >/tmp/seed.log 2>&1); then \
             n=$(wc -l < /tmp/seed.log); \
             echo "seed: '$step' failed in $dir ($n lines):"; head -n 40 /tmp/seed.log; \
             if [ "$n" -gt 40 ]; then echo "seed: last 10 of $n lines:"; tail -n 10 /tmp/seed.log; fi; \
             case "$step" in "go build ./..."|"go mod download") exit 1 ;; esac; \
           fi; \
         done; \
       done \
    && rm -f /tmp/seed.log \
    && chmod -R a+rwX "$GOCACHE" "$GOMODCACHE"

# Town binaries, cross-compiled by `make polecat-image` (CGO_ENABLED=0 linux/arm64).
# After the seed RUN, not before it: bin/gt and bin/bd change every commit, and a
# layer above the seed would invalidate the seed's cache on every build (measured
# 2026-10-26). The seed does not use gt or bd.
COPY bin/gt /usr/local/bin/gt
COPY bin/bd /usr/local/bin/bd
# The trim helper a cache-volume container runs; plugin.md, "Per-rig cache
# volumes", says why the trim has to run in the image.
COPY bin/polecat-image /usr/local/bin/polecat-image
RUN chmod 0755 /usr/local/bin/gt /usr/local/bin/bd /usr/local/bin/polecat-image

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
