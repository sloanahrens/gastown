.PHONY: build desktop-build desktop-run install safe-install check-forward-only check-no-downgrade check-version-tag check-install-path clean test test-changed test-makefile test-e2e-container check-up-to-date lint lint-tools docs-lint

BINARY := gt
BINARY_DESKTOP := gt-desktop
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
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-proxy-server ./cmd/gt-proxy-server
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-proxy-client ./cmd/gt-proxy-client
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
	golangci-lint run --timeout=5m || { echo "lint failed; if the error is 'can't load config', run 'make lint-tools'"; exit 1; }
	@echo "lint: guardlint (fail-open guard check, gt-udrrw)"
	go test ./internal/guardlint/... -run TestNoNewFailOpenGuards -v

# Deterministic docs and comments checks (docs/writing-for-agents.md).
# Also the tier lists the weekly doc audit slices from.
docs-lint:
	bash scripts/docs-lint.sh

desktop-build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_DESKTOP) ./cmd/gt-desktop

desktop-run:
	go run ./cmd/gt-desktop

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

test: test-makefile
	# -timeout 20m: the 10m default is a per-package budget and internal/cmd
	# and internal/refinery legitimately run 500-600s under contention, so
	# every gate against them flapped on the budget rather than a hung test
	# (gt-g8kr). gt-fo3h shrank those packages instead of leaning on the
	# budget: both ran their tests serially, so their wall clock was the sum
	# of their tests' runtimes; they now parallelize (651s -> 264s and
	# 429s -> 126s, back to back at matched load). The budget stays where
	# gt-g8kr put it — it still has to absorb a loaded host, and a budget
	# tightened against an idle host is not a hang detector.
	# GT_TEST_DOCKER=1: container-backed tests are opt-in (internal/testutil
	# DockerTestsEnv); the gate is where they run, under the refinery's slot.
	# Defaulted rather than hardcoded, so an inherited GT_TEST_DOCKER=0 wins:
	# gt done's default gate runs this same recipe with the opt-in off
	# and therefore needs no container-gate slot (gt-wx53), while the refinery
	# gate and the daemon's main-branch patrol pass no value and still get the
	# container suite. A hardcoded =1 here is invisible to every caller that
	# tries to turn containers off (a recipe assignment beats the child env),
	# so the gate stayed welded to the town-wide slot.
	GT_TEST_DOCKER=$${GT_TEST_DOCKER:-1} go test -timeout 20m ./...

# test-changed runs the same hermetic suite as `test` over a caller-supplied
# package list, for `gt done`'s pre-verify gate (merge_queue.test_verify_command
# with the {packages} token). The refinery's gate still runs `test` over the
# whole module, so nothing reaches main without a full run; this exists so four
# polecats do not each run the entire suite beside that gate. Measured
# 2026-09-22: one suite alone is 244s, four concurrently are 683s each — the
# contention, not the suite, is what made gates slow.
#
# PKGS defaults to the whole module so a bare `make test-changed` is never
# narrower than `make test` by accident.
PKGS ?= ./...
test-changed: test-makefile
	GT_TEST_DOCKER=$${GT_TEST_DOCKER:-1} go test -timeout 20m $(PKGS)

test-makefile:
	bash scripts/check-install-path_test.sh
	bash scripts/install-binary_test.sh
	bash scripts/check-deploy-source_test.sh
	bash -n scripts/install-gt.sh
	bash -n scripts/lib/install-gt-lib.sh
	bash scripts/install-gt_test.sh
	bash -n scripts/install-after-merge.sh
	bash scripts/install-after-merge_test.sh
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
