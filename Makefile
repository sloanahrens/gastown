.PHONY: build install safe-install check-forward-only check-no-downgrade check-version-tag check-install-path clean test test-slow test-integration test-timing test-makefile test-e2e-container check-up-to-date lint lint-tools docs-lint bd-command-tree gate

# The gate (docs/testing.md, "The gate"). Three tiers, each one target, and
# every caller runs them verbatim: CI, gt done, the land path and a human at a
# shell.
#
#   make gate              the landing gate: lint, then `go build ./...`, then
#                          the fast tier: the budget runner over every package
#                          NOT in internal/testpolicy/slow.txt, failing any
#                          package that takes over $(FAST_TIER_MAX_WALL) of wall
#                          time (gt-z862q). Prints its wall time at the end.
#                          Never starts a container and never takes the
#                          container-gate slot: the recipe writes
#                          GT_TEST_DOCKER=0 itself, so an inherited value cannot
#                          turn containers on. Do not wrap it in `gt slot run`.
#   make test-slow         the slow tier, after landing: the packages in
#                          internal/testpolicy/slow.txt, then the shell tests
#                          (scripts/test-makefile.sh), with GT_TEST_DOCKER=0.
#                          No containers, no slot.
#   make test-integration  the integration tier: -tags integration over ./...,
#                          then every package in internal/testpolicy/docker.txt
#                          whole, with GT_TEST_DOCKER=1. It starts containers,
#                          so run it under `gt slot run`.
#   make test              all three, in that order, for a human. It starts
#                          containers, so run it under `gt slot run`.
#
# Exit codes, for all: 0 means green. Anything else means red: make exits 2
# when a recipe fails, and 130 when interrupted. Decide on the exit code only,
# never on the output. For a human reading the log: a lint failure ends with
# make's own error line for the lint target, and a later stage ends with a
# `gate: FAILED at <stage>` line on stderr.

BINARY := gt
BUILD_DIR := .
INSTALL_DIR := $(HOME)/.local/bin
E2E_IMAGE ?= gastown-test
E2E_BUILD_FLAGS ?=
E2E_RUN_FLAGS ?= --rm
E2E_BUILD_RETRIES ?= 1
E2E_RUN_RETRIES ?= 1

# Get version info for ldflags.
# Dirty detection is aligned with the rebuild-gt plugin's guard (excludes
# .beads/, whose config.yaml churns from bd's own writes and isn't part of
# what 'make build' produces) — otherwise every build stamps '-dirty' even
# when the tree the guard considers clean. See plugins/rebuild-gt/run.sh.
GIT_DESCRIBE := $(shell git describe --tags --always 2>/dev/null)
GIT_DIRTY := $(shell git status --porcelain --untracked-files=no -- . ':(exclude).beads' 2>/dev/null)
ifeq ($(GIT_DESCRIBE),)
VERSION := dev
else ifeq ($(strip $(GIT_DIRTY)),)
VERSION := $(GIT_DESCRIBE)
else
VERSION := $(GIT_DESCRIBE)-dirty
endif
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -s -w \
           -X github.com/steveyegge/gastown/internal/cmd.Version=$(VERSION) \
           -X github.com/steveyegge/gastown/internal/cmd.Commit=$(COMMIT) \
           -X github.com/steveyegge/gastown/internal/cmd.BuildTime=$(BUILD_TIME) \
           -X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1

# ICU4C detection for macOS (required by go-icu-regex transitive dependency).
# Homebrew installs icu4c as a keg-only package, so headers/libs aren't on the
# default search path. Auto-detect the prefix and export CGo flags.
ifeq ($(shell uname),Darwin)
  ICU_PREFIX := $(shell brew --prefix icu4c 2>/dev/null)
  ifneq ($(ICU_PREFIX),)
    export CGO_CPPFLAGS += -I$(ICU_PREFIX)/include
    export CGO_LDFLAGS  += -L$(ICU_PREFIX)/lib
  endif
endif

build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/gt

# golangci-lint must be built with a Go >= go.mod's version and understand .golangci.yml version 2;
# a stale ~/go/bin/golangci-lint fails every run with "can't load config" (2026-09-17). `make lint-tools`
# installs the pinned version with the current toolchain.
GOLANGCI_LINT_VERSION ?= v2.13.2

lint-tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: docs-lint
	@golangci-lint version >/dev/null 2>&1 || { echo "golangci-lint missing: run 'make lint-tools'"; exit 1; }
	@echo "lint: golangci-lint run --timeout=5m (a contended lint exits in 5s naming the module lock; the gate and gt done wait it out and retry)"
	golangci-lint run --timeout=5m $(LINT_RUNNER_FLAGS) || { echo "lint failed; if the error is 'can't load config', run 'make lint-tools'"; exit 1; }
	bash scripts/repo-guards.sh
	@echo "lint: guardlint (fail-open guard check, gt-udrrw)"
	go test ./internal/guardlint/... -run TestNoNewFailOpenGuards -v

# Deterministic docs and comments checks (docs/writing-for-agents.md).
# Also the tier lists the weekly doc audit slices from.
docs-lint:
	bash scripts/docs-lint.sh

# Regenerate the bd command surface the command-tree lint (internal/cmdtree,
# gt-fcxe9.5) checks bd invocations against. BEADS_SRC is a beads fork
# checkout; BEADS_REF defaults to origin/main. Builds bd in a temp dir only.
bd-command-tree:
	scripts/refresh-bd-command-tree.sh

check-up-to-date:
	@# Deploy merged code only (gt-o848l): HEAD must be in origin/main with no
	@# tracked edits outside .beads/. ALLOW_UNMERGED=1 overrides loudly;
	@# SKIP_UPDATE_CHECK=1 only skips the fetch (gt-9jax). See the script.
	@bash $(CURDIR)/scripts/check-deploy-source.sh

# check-forward-only: Ensure HEAD is a descendant of the currently installed binary's commit.
# Prevents rebuilding to an older or diverged commit, which caused a crash loop where
# the replaced binary broke session startup hooks → witness respawned → loop every 1-2 min.
check-forward-only:
ifndef SKIP_FORWARD_CHECK
	@BINARY_COMMIT=$$($(INSTALL_DIR)/$(BINARY) version --verbose 2>/dev/null | grep -o '@[a-f0-9]*' | head -1 | tr -d '@'); \
	if [ -n "$$BINARY_COMMIT" ] && [ "$$BINARY_COMMIT" != "unknown" ]; then \
		HEAD_COMMIT=$$(git rev-parse HEAD 2>/dev/null); \
		if [ "$$BINARY_COMMIT" = "$$HEAD_COMMIT" ] || [ "$$(git rev-parse --short HEAD)" = "$$BINARY_COMMIT" ]; then \
			echo "Binary is already at HEAD, nothing to do"; \
			exit 1; \
		fi; \
	fi
endif
	@$(MAKE) --no-print-directory check-no-downgrade

# check-no-downgrade: refuse a build whose commit is not a descendant of the
# installed binary's. Shared by install and safe-install (install had no such
# guard before gt-o848l).
check-no-downgrade:
ifndef SKIP_FORWARD_CHECK
	@BINARY_COMMIT=$$($(INSTALL_DIR)/$(BINARY) version --verbose 2>/dev/null | grep -o '@[a-f0-9]*' | head -1 | tr -d '@'); \
	if [ -n "$$BINARY_COMMIT" ] && [ "$$BINARY_COMMIT" != "unknown" ]; then \
		if ! git merge-base --is-ancestor "$$BINARY_COMMIT" HEAD 2>/dev/null; then \
			echo "ERROR: HEAD ($$(git rev-parse --short HEAD)) is NOT a descendant of installed binary ($$BINARY_COMMIT)"; \
			echo "This would be a DOWNGRADE. Refusing to rebuild."; \
			echo "Use SKIP_FORWARD_CHECK=1 to override (dangerous)."; \
			exit 1; \
		fi; \
		echo "Forward-only check passed: $$BINARY_COMMIT → $$(git rev-parse --short HEAD)"; \
	else \
		echo "Warning: cannot determine installed binary commit, skipping forward check"; \
	fi
endif

check-install-path:
	@resolved=$$(command -v $(BINARY) 2>/dev/null || true); \
	if [ "$$resolved" != "$(INSTALL_DIR)/$(BINARY)" ]; then \
		echo "Warning: $(BINARY) resolves to $${resolved:-nothing in PATH}, not $(INSTALL_DIR)/$(BINARY)"; \
		echo "  Add this before other PATH entries in your shell profile:"; \
		echo '  export PATH="$(INSTALL_DIR):$$PATH"'; \
	fi

install: check-up-to-date check-no-downgrade build
	@# Atomic replace (temp + rename). Do NOT go back to `rm -f` then `cp`:
	# that leaves the live path missing or holding a partial binary, and
	# exec'ing a partial Go binary is SIGKILLed on macOS (gt-0het).
	@bash $(CURDIR)/scripts/install-binary.sh $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR) $(BINARY)
	@# Nuke any stale go-install binaries that shadow the canonical location
	@for bad in $(HOME)/go/bin/$(BINARY) $(HOME)/bin/$(BINARY); do \
		if [ -f "$$bad" ]; then \
			echo "Removing stale $$bad (use make install, not go install)"; \
			rm -f "$$bad"; \
		fi; \
	done
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY)"
	@$(MAKE) --no-print-directory check-install-path
	@# Restart only a running daemon: `gt daemon status` exits non-zero when
	@# it is not running (gt-o848l), so a deliberately stopped town stays
	@# stopped. It used to exit 0 either way, and this step started a stopped
	@# daemon mid-shutdown.
	@# Restart daemon so it picks up the new binary: a stale daemon is a
	@# recurring source of bugs (wrong session prefixes, etc.). Through the
	@# supervisor ('gt daemon restart' = launchctl kickstart -k), never
	@# stop-then-start: gt daemon stop unloads the launchd job, so the daemon
	@# is unsupervised until the start lands and stays down if that start
	@# fails (gt-sq9e).
	@if $(INSTALL_DIR)/$(BINARY) daemon status >/dev/null 2>&1; then \
		echo "Restarting daemon to pick up new binary..."; \
		$(INSTALL_DIR)/$(BINARY) daemon restart && \
			echo "Daemon restarted." || \
			echo "Warning: daemon restart failed (start manually with: gt daemon restart)"; \
	fi
	@# Sync plugins from build repo to town runtime directories.
	@# Prevents drift when plugin fixes merge but runtime dirs are stale.
	@# Fail-open by design: a stale runtime copy must not fail an install.
	@# But NOT silent — a failed sync is reported, so the drift is visible
	@# instead of hidden behind a success line. `plugin sync` resolves the
	@# town root from the CWD, so it fails outright when this checkout lives
	@# outside the town root (LocalRepo override).
	@$(INSTALL_DIR)/$(BINARY) plugin sync --source $(CURDIR)/plugins || \
		echo "Warning: plugin sync failed — plugins under <town_root>/plugins may be stale (see plugins/README.md)"

# safe-install: Replace binary WITHOUT restarting daemon or killing sessions.
# Use this for automated rebuilds (e.g., rebuild-gt plugin). Sessions pick up
# the new binary on their next natural cycle/handoff.
safe-install: check-up-to-date check-forward-only build
	@# Atomic replace, shared with `install`. The temp file is created with a
	@# unique name in the destination directory, so concurrent installs cannot
	@# interleave into one another's copy (gt-0het).
	@bash $(CURDIR)/scripts/install-binary.sh $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR) $(BINARY)
	@# Nuke any stale go-install binaries that shadow the canonical location
	@for bad in $(HOME)/go/bin/$(BINARY) $(HOME)/bin/$(BINARY); do \
		if [ -f "$$bad" ]; then \
			echo "Removing stale $$bad (use make install, not go install)"; \
			rm -f "$$bad"; \
		fi; \
	done
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY) (daemon NOT restarted)"
	@$(MAKE) --no-print-directory check-install-path
	@echo "Sessions will pick up new binary on next cycle."

# check-version-tag: Verify that if HEAD is tagged vX.Y.Z, the Version constant
# in internal/cmd/version.go equals X.Y.Z. No-op when HEAD is untagged, so it is
# safe to run on every build but only fails release tag checkouts.
# Prevents recurrence of gh#3459 (v0.13.0 shipped reporting 0.12.1).
check-version-tag:
	@TAG=$$(git describe --tags --exact-match HEAD 2>/dev/null || true); \
	if [ -z "$$TAG" ]; then \
		echo "check-version-tag: HEAD is not a release tag, skipping"; \
		exit 0; \
	fi; \
	case "$$TAG" in \
		v[0-9]*) TAG_VERSION=$${TAG#v} ;; \
		*) echo "check-version-tag: tag '$$TAG' is not a vX.Y.Z release tag, skipping"; exit 0 ;; \
	esac; \
	CODE_VERSION=$$(grep -E '^[[:space:]]*Version[[:space:]]*=[[:space:]]*"' internal/cmd/version.go | head -1 | sed 's/.*"\([^"]*\)".*/\1/'); \
	if [ -z "$$CODE_VERSION" ]; then \
		echo "ERROR: could not parse Version from internal/cmd/version.go"; \
		exit 1; \
	fi; \
	if [ "$$TAG_VERSION" != "$$CODE_VERSION" ]; then \
		echo "ERROR: version mismatch between git tag and Version constant"; \
		echo "  git tag at HEAD:          $$TAG (expects Version=$$TAG_VERSION)"; \
		echo "  internal/cmd/version.go:  Version=$$CODE_VERSION"; \
		echo ""; \
		echo "Run scripts/bump-version.sh before tagging, or re-tag HEAD correctly."; \
		echo "See gh#3459 for background."; \
		exit 1; \
	fi; \
	echo "check-version-tag: OK (tag $$TAG matches Version=$$CODE_VERSION)"

clean:
	rm -f $(BUILD_DIR)/$(BINARY)

# Modules nested in this repository (plugins/*/go.mod). The root module's
# `go build ./...` does not reach them, so the gate builds each in its own
# directory.
NESTED_MODULES := $(patsubst %/go.mod,%,$(shell find plugins -name go.mod -not -path '*/testdata/*' 2>/dev/null | LC_ALL=C sort))

# The shell tests, run by the slow tier. A make variable only so
# scripts/makefile-gate_test.sh can drive the failure paths with a stub.
SHELL_TESTS ?= scripts/test-makefile.sh

# The tier boundary (gt-z862q). slow.txt names the packages the gate skips and
# test-slow runs; the gate fails any other package that runs longer than
# FAST_TIER_MAX_WALL (keep it equal to testpolicy.FastTierMaxWall).
SLOW_LIST := internal/testpolicy/slow.txt
SLOW_PKGS := $(addprefix ./,$(shell sed -e 's/\#.*//' $(SLOW_LIST) | awk 'NF{print $$1}'))
FAST_TIER_MAX_WALL := 15s

# The gate's lint waits its turn on golangci-lint's module lock instead of
# exiting in 5s: the gate is judged by its exit code alone, so a contended
# lint must not read as red. Plain `make lint` keeps the fast exit that gt
# done's and the refinery's retry policy reads (internal/lintlock, gt-kqwu).
# A target-specific variable, so it reaches the lint prerequisite.
gate: LINT_RUNNER_FLAGS := --allow-serial-runners
# The gate's start, read when make parses the Makefile, so the wall it prints
# includes lint.
gate: GATE_START := $(shell date +%s)
gate: lint
	@echo "gate: build (go build ./... and the nested modules: $(NESTED_MODULES))" >&2
	@go build ./... || { echo "gate: FAILED at build" >&2; exit 1; }
	@# -o into a temp dir: `go build ./...` over a module with one main
	@# package writes that binary into the module's directory.
	@out=$$(mktemp -d); for m in $(NESTED_MODULES); do (cd "$$m" && go build -o "$$out/" ./...) || { rm -rf "$$out"; echo "gate: FAILED at build ($$m)" >&2; exit 1; }; done; rm -rf "$$out"
	@# The fast tier: every package not in $(SLOW_LIST). -max-wall fails a
	@# package that ran longer than the fast tier allows, naming it, so the
	@# boundary cannot drift (gt-z862q). The budget runner measures converted
	@# packages through its CPU-measuring -exec wrapper, which bypasses the
	@# test result cache, and runs the packages in unconverted.txt afterwards
	@# with the cache (gt-22hdp.53). -timeout 20m is the per-package hang
	@# detector, kept from the one gate definition (gt-ik4a1.1).
	@echo "gate: unit tier (fast tier: every package not in $(SLOW_LIST); make test-slow runs those)" >&2
	@# The suite runs in the background so the trap fires at once on INT or
	@# TERM (bash defers traps until a foreground child exits); the trap finds
	@# it as this shell's child (pgrep -P) and stops it before its children.
	@trap 'for p in $$(pgrep -P $$$$); do k=$$(pgrep -P $$p); kill $$p 2>/dev/null; [ -n "$$k" ] && kill $$k 2>/dev/null; done; exit 130' INT TERM; \
	GT_TEST_DOCKER=0 go run ./internal/testpolicy/cmd/budget -skip $(SLOW_LIST) -max-wall $(FAST_TIER_MAX_WALL) -- -timeout 20m ./... & gt=$$!; \
	wait $$gt; go_rc=$$?; \
	wall=$$(( $$(date +%s) - $(GATE_START) )); \
	if [ $$go_rc -ne 0 ]; then echo "gate: FAILED at unit tier (Go suite, exit $$go_rc) after $${wall}s wall" >&2; exit 1; fi; \
	echo "gate: PASSED in $${wall}s wall" >&2

# test-slow is the slow tier: the packages the gate skips, then the shell
# tests. Both run even when the first fails; either failing fails the target.
test-slow:
	@test -n "$(strip $(SLOW_PKGS))" || { echo "test-slow: $(SLOW_LIST) lists no package; refusing to run go test over nothing" >&2; exit 1; }
	@start=$$(date +%s); rc=0; \
	GT_TEST_DOCKER=0 go run ./internal/testpolicy/cmd/budget -- -timeout 20m $(SLOW_PKGS) || { rc=1; echo "test-slow: FAILED at Go suite" >&2; }; \
	GT_TEST_DOCKER=0 bash $(SHELL_TESTS) || { rc=1; echo "test-slow: FAILED at shell tests" >&2; }; \
	echo "test-slow: $$([ $$rc -eq 0 ] && echo PASSED || echo FAILED) in $$(( $$(date +%s) - start ))s wall" >&2; \
	exit $$rc

# test runs every tier for a human: gate, then test-slow, then
# test-integration. It starts containers, so run it under `gt slot run`.
test: gate test-slow test-integration

# The Docker-backed packages (internal/testpolicy/docker.txt, kept exact by
# TestDockerTier). Their container tests skip in the gate and run here.
DOCKER_PKGS := $(addprefix ./,$(shell sed -e 's/\#.*//' internal/testpolicy/docker.txt))

# test-integration runs the //go:build integration tier (real tmux, bd, Dolt,
# the gt binary; tests named TestIntegration*) and then the Docker-backed
# packages whole. The daemon's main_branch_test patrol runs it on the gastown
# rig once a day. INTEGRATION_GO_TEST swaps the runner so CI can collect JUnit
# output from this one definition of the tier, e.g.
#   make test-integration INTEGRATION_GO_TEST="gotestsum --junitfile j.xml --"
INTEGRATION_GO_TEST ?= go test
test-integration:
	@test -n "$(strip $(DOCKER_PKGS))" || { echo "test-integration: internal/testpolicy/docker.txt lists no package; refusing to run go test over nothing" >&2; exit 1; }
	GT_TEST_DOCKER=1 $(INTEGRATION_GO_TEST) -tags integration -run '^TestIntegration' -timeout 20m ./...
	GT_TEST_DOCKER=1 $(INTEGRATION_GO_TEST) -timeout 20m $(DOCKER_PKGS)

# test-timing measures the unit tier in a tmux server started by launchd, which
# macOS does not exempt from its first-run scan of new executables. It is the
# acceptance measurement for docs/plans/2026-09-27-test-rewrite-design.md.
# PKGS narrows the run, e.g. make test-timing PKGS=./internal/tmux/...
test-timing:
	bash scripts/test-timing.sh $(PKGS)

test-makefile:
	bash scripts/check-install-path_test.sh
	bash scripts/install-binary_test.sh
	bash scripts/check-deploy-source_test.sh
	bash -n scripts/install-gt.sh
	bash -n scripts/lib/install-gt-lib.sh
	bash scripts/install-gt_test.sh
	bash -n scripts/install-after-merge.sh
	bash scripts/install-after-merge_test.sh
	bash -n plugins/dolt-log-rotate/run.sh
	bash -n plugins/dolt-log-rotate/run_test.sh
	bash plugins/dolt-log-rotate/run_test.sh
	bash -n plugins/stuck-agent-dog/run.sh
	bash -n plugins/stuck-agent-dog/run_test.sh
	bash plugins/stuck-agent-dog/run_test.sh
	bash -n plugins/compactor-dog/run.sh
	bash -n plugins/compactor-dog/run_test.sh
	bash plugins/compactor-dog/run_test.sh
	bash -n plugins/stuck-work-dog/run.sh
	bash -n plugins/stuck-work-dog/run_test.sh
	bash plugins/stuck-work-dog/run_test.sh
	bash -n plugins/rebuild-gt/run.sh
	bash -n plugins/rebuild-gt/run_test.sh
	bash plugins/rebuild-gt/run_test.sh
	bash -n plugins/gitignore-reconcile/run.sh
	bash -n plugins/git-hygiene/run.sh
	bash -n plugins/submodule-commit/run.sh
	bash -n plugins/submodule-commit/run_test.sh
	bash plugins/submodule-commit/run_test.sh
	bash -n plugins/rig-list-consumers/run_test.sh
	bash plugins/rig-list-consumers/run_test.sh
	bash -n plugins/quality-review/run.sh
	bash -n plugins/quality-review/run_test.sh
	bash plugins/quality-review/run_test.sh
	bash -n plugins/seat-refill/run.sh
	bash -n plugins/seat-refill/run_test.sh
	bash plugins/seat-refill/run_test.sh
	bash -n scripts/docs-lint.sh
	bash scripts/docs-lint_test.sh
	bash -n scripts/repo-guards.sh
	bash scripts/repo-guards_test.sh

# Run e2e tests in isolated container (the only supported way to run them)
test-e2e-container:
ifeq ($(OS),Windows_NT)
	@powershell -NoProfile -Command "$$max=$(E2E_BUILD_RETRIES); for($$i=1; $$i -le $$max; $$i++){ docker build $(E2E_BUILD_FLAGS) -f Dockerfile.e2e -t $(E2E_IMAGE) .; if($$LASTEXITCODE -eq 0){ break }; if($$i -eq $$max){ exit 1 }; Write-Host ('docker build failed (attempt ' + $$i + '), retrying...'); Start-Sleep -Seconds 2 }"
	@powershell -NoProfile -Command "$$max=$(E2E_RUN_RETRIES); for($$i=1; $$i -le $$max; $$i++){ docker run $(E2E_RUN_FLAGS) $(E2E_IMAGE); if($$LASTEXITCODE -eq 0){ break }; if($$i -eq $$max){ exit 1 }; Write-Host ('docker run failed (attempt ' + $$i + '), retrying...'); Start-Sleep -Seconds 2 }"
else
	@attempt=1; \
	while [ $$attempt -le $(E2E_BUILD_RETRIES) ]; do \
		docker build $(E2E_BUILD_FLAGS) -f Dockerfile.e2e -t $(E2E_IMAGE) . && break; \
		if [ $$attempt -eq $(E2E_BUILD_RETRIES) ]; then exit 1; fi; \
		echo "docker build failed (attempt $$attempt), retrying..."; \
		attempt=$$((attempt+1)); \
		sleep 2; \
	done
	@attempt=1; \
	while [ $$attempt -le $(E2E_RUN_RETRIES) ]; do \
		docker run $(E2E_RUN_FLAGS) $(E2E_IMAGE) && break; \
		if [ $$attempt -eq $(E2E_RUN_RETRIES) ]; then exit 1; fi; \
		echo "docker run failed (attempt $$attempt), retrying..."; \
		attempt=$$((attempt+1)); \
		sleep 2; \
	done
endif
