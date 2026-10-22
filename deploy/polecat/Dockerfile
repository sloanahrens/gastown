# Gas Town polecat image for OpenShell sandboxes (claude-q3f.1, phase 0).
#
# Build (from the repo root, after `make polecat-bins`):
#   docker build -t gastown-polecat:dev -f deploy/polecat/Dockerfile deploy/polecat
#
# The image carries the Claude Code CLI, the Go toolchain and git, plus the
# town's own `gt` and `bd` binaries cross-compiled for linux/arm64 into
# deploy/polecat/bin/. OpenShell ignores ENTRYPOINT/CMD: the sandbox runtime
# starts the main process given on `openshell sandbox create -- ...`.
# GNU Make 4.4.1 built from source: Ubuntu's Make 4.3 fails inside OpenShell
# sandboxes because its recipe child calls setresuid/setresgid with unchanged
# ids, which the sandbox seccomp filter denies (NVIDIA/OpenShell#4102). 4.4.1
# does not make that call.
FROM ubuntu:24.04 AS make-build
ARG MAKE_VERSION=4.4.1
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends build-essential ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && cd /tmp && curl -fsSL "https://ftp.gnu.org/gnu/make/make-${MAKE_VERSION}.tar.gz" | tar xz \
    && cd "make-${MAKE_VERSION}" && ./configure --prefix=/usr/local >/dev/null && make -j4 >/dev/null \
    && cp make /usr/local/bin/make

FROM ubuntu:24.04

ARG GO_VERSION=1.26.2
ARG NODE_MAJOR=22
ARG CLAUDE_CODE_VERSION=latest

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl git gnupg jq make openssh-client \
    ripgrep unzip xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Node.js (NodeSource) and the Claude Code CLI as a global npm package.
RUN mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
       | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
       > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
    && npm cache clean --force

# Go from the official tarball (apt golang-go is too old for the repo's go.mod).
RUN ARCH=$(dpkg --print-architecture) \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz

# GNU Make 4.4.1 ahead of /usr/bin/make on PATH (see the make-build stage).
COPY --from=make-build /usr/local/bin/make /usr/local/bin/make

# golangci-lint at the version the gastown Makefile pins (GOLANGCI_LINT_VERSION);
# make lint refuses to run without it, and gt done runs make presubmit.
ARG GOLANGCI_LINT_VERSION=v2.13.2
RUN GOBIN=/usr/local/bin GOFLAGS= /usr/local/go/bin/go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" \
    && rm -rf /root/go /root/.cache

# Town binaries, cross-compiled on the host (CGO_ENABLED=0 linux/arm64).
COPY bin/gt /usr/local/bin/gt
COPY bin/bd /usr/local/bin/bd
RUN chmod 0755 /usr/local/bin/gt /usr/local/bin/bd

# Non-root sandbox user; OpenShell runs the workload as the image USER and
# uses /sandbox as the workspace.
RUN userdel -r ubuntu 2>/dev/null || true \
    && groupadd -g 1000 sandbox \
    && useradd -m -u 1000 -g 1000 -s /bin/bash sandbox \
    && mkdir -p /sandbox /sandbox/.cache/go-build /sandbox/go \
    && chown -R sandbox:sandbox /sandbox

ENV PATH="/usr/local/go/bin:/sandbox/go/bin:${PATH}" \
    GOPATH=/sandbox/go \
    GOCACHE=/sandbox/.cache/go-build \
    GOFLAGS=-p=4 \
    BD_DISABLE_METRICS=1 \
    DO_NOT_TRACK=1 \
    DISABLE_AUTOUPDATER=1 \
    HOME=/sandbox \
    TERM=xterm-256color

USER sandbox
WORKDIR /sandbox
