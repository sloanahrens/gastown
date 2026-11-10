# The CI base image extension the landing gate needs (gt-zzo1g2): golangci-lint
# at the version the gastown Makefile pins, and jq 1.7.1, which the image's
# Debian jq 1.6 is too old for (scripts/deep-review_test.sh in CI).
#
# Build: `make ci-base-image CI_BASE_IMAGE=<runner image>`. The base is the
# runner image the operator builds from the host file
# ~/forgejo/runner-image/Dockerfile, which is outside this repo; this layer is
# the part gastown adds to it, and every check below fails the image build
# rather than one landing.
#
# The base has no default: an empty CI_BASE_IMAGE fails the build before any
# stage runs, so an image cannot be tagged as the gate's without extending a
# named runner image.

ARG CI_BASE_IMAGE
FROM ${CI_BASE_IMAGE}

# The runner image's last USER is an unprivileged user, so the install steps
# below run as root, and the image ends as BASE_USER again. The default is ci,
# the user the runner image is built with; a base whose user is not ci passes
# BASE_USER to name it.
ARG BASE_USER=ci
USER root

# The pin comes from `make -s golangci-lint-version`, so the Makefile is its one
# home and an image is only ever built with the version `make lint` accepts. The
# test is the build's own check: a missing binary or another version fails here.
#
# The install writes its module and build caches under GOPATH and GOCACHE, which
# the runner image points at /home/ci, not /root. go clean empties those caches
# in this RUN, before the layer closes, so the layer does not carry them.
ARG GOLANGCI_LINT_VERSION
RUN test -n "${GOLANGCI_LINT_VERSION}" \
 && GOBIN=/usr/local/bin GOFLAGS= go install \
      "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" \
 && test "$(golangci-lint version --short)" = "${GOLANGCI_LINT_VERSION#v}" \
 && go clean -cache -modcache \
 && rm -rf /root/go /root/.cache

# jq 1.7.1, the version scripts/deep-review_test.sh needs; Debian's 1.6 fails
# it. Pinned by digest and checked after download, and the test fails a rebuild
# that drops it. arm64: the runner host is Apple silicon.
ARG JQ_VERSION=1.7.1
ARG JQ_SHA256=4dd2d8a0661df0b22f1bb9a1f9830f06b6f3b8f7d91211a1ef5d7c4f06a8b4a5
RUN curl -fsSL -o /usr/local/bin/jq \
      "https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-linux-arm64" \
 && echo "${JQ_SHA256}  /usr/local/bin/jq" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/jq \
 && test "$(jq --version)" = "jq-${JQ_VERSION}"

# Back to the base image's user, so the image runs as the user it was built on.
USER ${BASE_USER}
