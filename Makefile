.PHONY: build install gt-installer install-local uninstall check-forward-only check-no-downgrade check-version-tag check-install-path clean test test-slow test-integration test-integration-wall test-timing test-makefile test-e2e-container check-up-to-date lint lint-tools docs-lint bd-command-tree gate gate-lint gate-test tier-check presubmit exec-tax-preflight polecat-image polecat-seed-stage polecat-seed-pins polecat-caches openshell-providers

# The gate (docs/testing.md, "The gate"). Three tiers, each one target, and
# every caller runs them verbatim: CI, gt done, the land path and a human at a
# shell.
#
#   make gate              the landing gate: the exec-tax preflight (a warning
#                          at worst, gt-2ycne.1), then lint, then `go build
#                          ./...`, then the unit tier: plain `go test -timeout
#                          20m ./...` over every package, through go test's
#                          result cache (there is no slow tier: internal/cmd
#                          and internal/beads run here since gt-ik4a1.9;
#                          gt-s1vff). The gate runs no budget runner and
#                          measures no wall, CPU or idle time, so a landing is
#                          never refused for contention (gt-z7qtk). Prints its
#                          wall time at the end.
#   make tier-check        the unit tier through the budget runner, whose only
#                          caller it is: the runner judges each package's CPU
#                          budget and wall time, and a wall overrun fails
#                          (-strict-wall), so the drift the gate cannot see is
#                          still caught (gt-z862q). Run it on a quiet
#                          host from the post-landing job or a daily one, never
#                          as a landing gate. No lint or build stage.
#                          Never starts a container and never takes the
#                          container-gate slot: the recipe writes
#                          GT_TEST_DOCKER=0 itself, so an inherited value cannot
#                          turn containers on. Do not wrap it in `gt slot run`.
#   make presubmit         gt done's pre-submit check (gt-ssyxd): lint, `go build
#                          ./...`, then `go test` of only the packages the branch
#                          changed against origin/main and of the tree-wide guard
#                          tests its changed paths trigger (gt-ydzwb), then the
#                          error returns the branch adds with no unit test
#                          behind them (gt-b9j3o.5). The landing worker runs
#                          `make gate` on the merged tree, so this is the cheap
#                          first look, not the gate. No containers, no slot.
#   make test-slow         the post-landing check: the gate, then the shell
#                          tests (the shelltier Go runner, which `make
#                          test-makefile` also runs alone), with
#                          GT_TEST_DOCKER=0. No containers, no slot. The
#                          landing worker runs it as the rig's
#                          merge_queue.post_land_command; the name is kept
#                          for that setting (gt-ik4a1.9).
#   make test-integration  the integration tier: -tags integration over ./...,
#                          then every package in internal/testpolicy/docker.txt
#                          whole, with GT_TEST_DOCKER=1. It starts containers,
#                          so run it under `gt slot run`.
#   make test-integration-wall
#                          test-integration with a wall report (gt-ik4a1.4.5):
#                          the same two commands, each -json -count=1, through
#                          one tierwall run that prints a line per package,
#                          slowest first, then the tier's wall against the 90s
#                          the post-merge cadence waits for (gt-ik4a1.4). -count=1
#                          keeps it an uncached run, so the wall cannot come
#                          from the test cache (gt-gq4gl). Nothing fails on
#                          wall unless TIERWALL_FLAGS asks. It starts
#                          containers, so run it under `gt slot run`.
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
# Dirty detection is aligned with the rebuild_gt job's guard (excludes
# .beads/, whose config.yaml churns from bd's own writes and isn't part of
# what 'make build' produces) — otherwise every build stamps '-dirty' even
# when the tree the guard considers clean. See internal/daemon/rebuild_gt.go.
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
# Empty on a detached HEAD: symbolic-ref names no branch, where rev-parse --abbrev-ref prints "HEAD".
BRANCH := $(shell git symbolic-ref --short -q HEAD 2>/dev/null)
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -s -w \
           -X github.com/steveyegge/gastown/internal/cmd.Version=$(VERSION) \
           -X github.com/steveyegge/gastown/internal/cmd.Commit=$(COMMIT) \
           -X github.com/steveyegge/gastown/internal/cmd.Branch=$(BRANCH) \
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
	@# A plugin's binary is a build artifact that lives beside its plugin.md:
	@# the runner execs it there and gt plugin sync copies the directory, so an
	@# install that skips this leaves the runtime copy with no entry point.
	@mkdir -p plugins/tool-updater/bin
	go build -ldflags "$(LDFLAGS)" -o plugins/tool-updater/bin/tool-updater ./plugins/tool-updater
	@mkdir -p plugins/app-promote/bin
	go build -ldflags "$(LDFLAGS)" -o plugins/app-promote/bin/app-promote ./plugins/app-promote
	@mkdir -p plugins/polecat-image/bin
	go build -ldflags "$(LDFLAGS)" -o plugins/polecat-image/bin/polecat-image ./plugins/polecat-image
	@mkdir -p plugins/town-metrics/bin
	go build -ldflags "$(LDFLAGS)" -o plugins/town-metrics/bin/town-metrics ./plugins/town-metrics

# golangci-lint must be built with a Go >= go.mod's version and understand .golangci.yml version 2;
# a stale ~/go/bin/golangci-lint fails every run with "can't load config" (2026-09-17). `make lint-tools`
# installs the pinned version with the current toolchain.
GOLANGCI_LINT_VERSION ?= v2.13.2

lint-tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# The OpenShell polecat sandbox pieces an operator runs by hand (claude-q3f.9).
# deploy/polecat lands from the crew branch; no target below runs in the gate.

# What the sandbox profiles name: gastown-polecat:dev is the moving tag a
# profile points at, and the short sha is the rollback pin beside it.
POLE_IMAGE ?= gastown-polecat
POLE_TAG ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
# The commit the image's gt is built from. Prepare refuses a sandbox whose label
# differs from the installed gt's commit (gt-pmxo9), so the label is the full sha.
# The build compiles the working tree, so the label is only true of a tree with no
# changes: polecat-image refuses one that has any (gt-9fz0k).
POLE_COMMIT := $(shell git rev-parse HEAD 2>/dev/null)
# bd is built from a beads fork checkout, not this repo. That checkout is host
# state, so there is no default: the operator names it, BD_SRC=<checkout>.
BD_SRC ?=
# The commit of BD_SRC the image's bd is built from: empty builds the commit the
# installed bd reports (`bd version --json`, gt-7b7a5), so host and sandbox
# share one schema ceiling. A commit the checkout's object store does not hold
# is fetched for once there and looked for again; still missing, or a fetch that
# fails, refuses the build (gt-pme82).
BD_COMMIT ?=
# The image's Go floor is the highest `go` line of the rigs' go.mod files, read
# from each rig's canonical checkout under POLE_TOWN (deploy/polecat/go-floor.sh).
# POLE_RIG_GO_MODS names the files; override it to read other checkouts.
POLE_TOWN ?= $(HOME)/gt
POLE_RIG_GO_MODS ?= $(wildcard $(POLE_TOWN)/*/mayor/rig/go.mod)
# The rigs whose Go build and module caches the image is seeded with (gt-ieuvf).
# A pinned sha, not the rig's moving HEAD, keeps the seed layer's cache key (and
# so the image publish time) stable until the pins change: `make
# polecat-seed-pins` re-pins to each rig's current canonical checkout, weekly or
# when a go.sum changes.
# beads is seeded because internal/storage/uow's tests import
# github.com/DATA-DOG/go-sqlmock, and a beads polecat's presubmit has no
# network (hq-smfiv).
POLE_SEED_RIGS ?= gastown devops beads
POLE_SEED_PINS ?= deploy/polecat/seed-pins
POLE_SEED_DIR ?= $(CURDIR)/deploy/polecat/seed
# The rig this repo is: its seed is the commit the image is built at, never a
# pin, so a sandbox always has the modules of the gt it runs (gt-1rz2r).
POLE_SEED_BUILD_RIG ?= gastown
# Removing the old images from the sandbox nodes is opt-in, so a build changes
# only the image registry unless the operator asks for the cluster too: set
# POLE_NODE_PRUNE=1 to run polecat-node-prune after the build (gt-06ni7).
POLE_NODE_PRUNE ?= 0

# Cross-compile gt, bd and the plugin's cache-trim helper for the sandbox
# image, then build it. The Dockerfile copies deploy/polecat/bin/, so that
# directory is the build context's payload. The guard is the first recipe line,
# so a run without BD_SRC stops before anything is built.
polecat-image:
	@test -n "$(BD_SRC)" || { echo "polecat-image: BD_SRC is unset; run make polecat-image BD_SRC=<beads checkout>" >&2; exit 2; }
	@test -z "$$(git status --porcelain -- . ':(exclude).beads')" || { echo "polecat-image: the tree has uncommitted or untracked changes, so its gt is not the commit the label names ($(POLE_COMMIT)); commit or remove them first" >&2; exit 2; }
	@test -n "$(POLE_RIG_GO_MODS)" || { echo "polecat-image: no rig go.mod under $(POLE_TOWN)/*/mayor/rig; set POLE_TOWN=<town root>" >&2; exit 2; }
	@mkdir -p deploy/polecat/bin
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o deploy/polecat/bin/gt ./cmd/gt
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o deploy/polecat/bin/polecat-image ./plugins/polecat-image
	@bd_commit=$$(go run ./internal/polecat/bdbuild/cmd/build -src "$(BD_SRC)" -out $(CURDIR)/deploy/polecat/bin/bd -commit "$(BD_COMMIT)") || { echo "polecat-image: the bd build failed, so the image has no bd commit to label" >&2; exit 2; }; \
	test -n "$$bd_commit" || { echo "polecat-image: the bd build printed no commit, so the image's bd cannot be labelled" >&2; exit 2; }; \
	printf '%s\n' "$$bd_commit" > deploy/polecat/bin/bd.commit
	@$(MAKE) --no-print-directory polecat-seed-stage
	bd_commit=$$(cat deploy/polecat/bin/bd.commit) || { echo "polecat-image: no bd commit recorded, so the image's bd cannot be labelled; the bd build did not finish" >&2; exit 2; }; \
	test -n "$$bd_commit" || { echo "polecat-image: the bd commit file is empty, so the image's bd cannot be labelled" >&2; exit 2; }; \
	floor=$$(bash deploy/polecat/go-floor.sh rig $(POLE_RIG_GO_MODS)) || { echo "polecat-image: cannot read the rig Go floor from $(POLE_RIG_GO_MODS)" >&2; exit 2; }; \
	docker build --platform linux/arm64 --label org.gastown.gt-commit=$(POLE_COMMIT) --label org.gastown.bd-commit="$$bd_commit" --build-arg GO_FLOOR="$$floor" -t $(POLE_IMAGE):$(POLE_TAG) -t $(POLE_IMAGE):dev -f deploy/polecat/Dockerfile deploy/polecat
ifeq ($(POLE_NODE_PRUNE),1)
	@$(MAKE) --no-print-directory polecat-node-prune
endif

# polecat-seed-stage writes POLE_SEED_DIR: each pinned rig at its pin, and
# POLE_SEED_BUILD_RIG at POLE_COMMIT. The Dockerfile bind-mounts the directory for
# its seed RUN and copies none of it into the image. Before changing the recipe,
# read scripts/makefile-gate_test.sh, which runs this target against a fixture town.
polecat-seed-stage:
	@test -s $(POLE_SEED_PINS) || { echo "polecat-image: $(POLE_SEED_PINS) is missing or empty; run make polecat-seed-pins and commit it" >&2; exit 2; }
	@rm -rf "$(POLE_SEED_DIR)" && mkdir -p "$(POLE_SEED_DIR)"
	@go run ./internal/polecat/seed/cmd/stage \
		-pins "$(POLE_SEED_PINS)" \
		-town "$(POLE_TOWN)" \
		-dest "$(POLE_SEED_DIR)" \
		-build-rig "$(POLE_SEED_BUILD_RIG)" \
		-build-commit "$(POLE_COMMIT)"

# polecat-node-prune removes old polecat images from the sandbox nodes
# (gt-e14j6). Not fatal to the caller: a node that cannot be read keeps its
# images, and the failure is named here.
polecat-node-prune:
	@go run ./cmd/nodeprune -keep "$${POLECAT_IMAGE_KEEP:-3}" || echo "polecat-image: node prune failed; the old images stay on the sandbox nodes" >&2

# polecat-seed-pins rewrites deploy/polecat/seed-pins to each seeded rig's
# canonical checkout HEAD. Commit the result; the next polecat-image reseeds.
polecat-seed-pins:
	@tmp=$$(mktemp); printf '%s\n' "# <rig> <commit>: the sources the polecat image's Go cache is seeded from." "# Each commit must exist in that rig's canonical checkout under POLE_TOWN; make polecat-seed-pins rewrites this file." "# gastown is seeded from the commit the image is built at, not from a line here (gt-1rz2r)." > "$$tmp"; for rig in $(POLE_SEED_RIGS); do \
		sha=$$(git -C "$(POLE_TOWN)/$$rig/mayor/rig" rev-parse HEAD) || { rm -f "$$tmp"; echo "polecat-seed-pins: cannot read $(POLE_TOWN)/$$rig/mayor/rig" >&2; exit 2; }; \
		echo "$$rig $$sha" >> "$$tmp"; \
	done; mv "$$tmp" $(POLE_SEED_PINS); cat $(POLE_SEED_PINS)

# The shared cache's seed Job (gt-s9r1s): the claim every kubernetes sandbox
# mounts, the image its containers run, and the Forgejo host they clone from.
# An empty POLE_FORGEJO_HOST keeps each rig's configured host. A host:port here
# is the address the cluster reaches Forgejo at.
POLE_CACHE_CLAIM ?= gt-cache
POLE_CACHE_NAMESPACE ?=
POLE_CACHE_IMAGE ?= $(POLE_IMAGE):dev
POLE_FORGEJO_HOST ?=

# RIGS narrows the seed to the named pins: make polecat-caches RIGS="gastown
# om". Empty seeds every pin in POLE_SEED_PINS. The Job's marker skips the
# steps a rerun with the same pins would repeat.
polecat-caches:
	@go run ./cmd/polecatcaches -pins $(POLE_SEED_PINS) -town $(POLE_TOWN) -image $(POLE_CACHE_IMAGE) -claim $(POLE_CACHE_CLAIM) -namespace "$(POLE_CACHE_NAMESPACE)" -forgejo-host "$(POLE_FORGEJO_HOST)" $(RIGS)

# Profiles are imported and the two providers created from the key files under
# ~/.config/gt; a provider that already exists is left alone, so a rerun is a no-op.
openshell-providers:
	@go run ./cmd/openshell-providers

# How long `make lint` keeps re-running a lint that another worktree's
# golangci-lint holds the module lock against, in seconds (gt-uoppq). Each
# contended attempt still prints the lock line internal/lintlock reads, so a
# lint still contended at the end exits red with that evidence. 0 = one try.
LINT_LOCK_WAIT ?= 600

lint: docs-lint
	@golangci-lint version >/dev/null 2>&1 || { echo "golangci-lint missing: run 'make lint-tools'"; exit 1; }
	@echo "lint: golangci-lint run --timeout=5m (a contended lint is retried for up to $(LINT_LOCK_WAIT)s while another run holds the module lock)"
	bash scripts/lint-lock-wait.sh $(LINT_LOCK_WAIT) golangci-lint run --timeout=5m $(LINT_RUNNER_FLAGS) || { echo "lint failed; if the error is 'can't load config', run 'make lint-tools'"; exit 1; }
	@for m in $(TESTED_MODULES); do echo "lint: golangci-lint run in $$m"; (cd "$$m" && bash ../scripts/lint-lock-wait.sh $(LINT_LOCK_WAIT) golangci-lint run --timeout=5m --config ../.golangci.yml --build-tags integration $(LINT_RUNNER_FLAGS)) || { echo "lint failed in $$m"; exit 1; }; done
	go run ./cmd/repo-guards
	@echo "lint: guardlint (fail-open guard check, gt-udrrw)"
	go test ./internal/guardlint/... -run TestNoNewFailOpenGuards -v

# Deterministic docs and comments checks (docs/writing-for-agents.md).
# Also the tier lists the weekly doc audit slices from.
docs-lint:
	@go run ./cmd/docs-lint

# Regenerate the bd command surface the command-tree lint (internal/cmdtree,
# gt-fcxe9.5) checks bd invocations against. BEADS_SRC is a beads fork
# checkout; BEADS_REF defaults to origin/main. Builds bd in a temp dir only.
bd-command-tree:
	scripts/refresh-bd-command-tree.sh

check-up-to-date:
	@# Deploy merged code only (gt-o848l): HEAD must be in origin/main with no
	@# tracked edits outside .beads/. ALLOW_UNMERGED=1 overrides loudly;
	@# SKIP_UPDATE_CHECK=1 only skips the fetch (gt-9jax). See
	@# internal/deploysource/check.go for the rules.
	@go run ./cmd/check-deploy-source

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
# installed binary's (gt-o848l). Run by install-local through check-forward-only.
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

# install: THE way to install gt into a town (gt-z0l3s). Builds the Go
# installer (cmd/gt-installer) from this checkout and runs it; it installs
# origin/main (SHA=<ref> picks another merged commit) from
# <town>/gastown/mayor/rig whichever clone you run it in: lock, fast-forward,
# build, atomic swap, smoke check with rollback, formula and plugin sync, then
# daemon/restart-pending.json — the daemon restarts itself once idle. It never
# kickstarts the daemon. Exit codes 0 installed or already current, 1 failed
# (rolled back), 2 refused, 3 busy (retry later).
install: gt-installer
	@./gt-installer $(if $(SHA),--sha $(SHA))

# gt-installer: the Go installer `make install` builds and runs (gt-tpg0p.14).
# The binary is always ./gt-installer, the path the daemon's rebuild_gt job
# runs, so BUILD_DIR does not move it. Phony, so a binary left by an earlier
# checkout is never what installs. CGO_ENABLED=0: the installer links no cgo
# package. -buildvcs=false: nothing reads the VCS stamp, so the build needs no
# git metadata.
gt-installer:
	CGO_ENABLED=0 go build -buildvcs=false -o ./gt-installer ./cmd/gt-installer

# install-local: build THIS checkout and swap it into $(INSTALL_DIR). No daemon
# restart, no plugin sync, no lock. For a machine with no town yet (README,
# docs/INSTALLING.md); in a town it is the installer's build step, not a way
# to deploy — use make install.
install-local: check-up-to-date check-forward-only build
	@# Atomic replace (temp + rename). Do NOT go back to `rm -f` then `cp`:
	@# that leaves the live path missing or holding a partial binary, and
	@# exec'ing a partial Go binary is SIGKILLed on macOS (gt-0het).
	@go run ./cmd/install-binary $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR) $(BINARY)
	@# Nuke any stale go-install binaries that shadow the canonical location,
	@# but never the one just installed: INSTALL_DIR=$HOME/go/bin is supported,
	@# and that path is the first entry of this list (gt-tqxdd).
	@for bad in $(HOME)/go/bin/$(BINARY) $(HOME)/bin/$(BINARY); do \
		if [ -f "$$bad" ] && [ "$$bad" != "$(INSTALL_DIR)/$(BINARY)" ]; then \
			echo "Removing stale $$bad (use make install, not go install)"; \
			rm -f "$$bad"; \
		fi; \
	done
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY) (daemon NOT restarted)"
	@$(MAKE) --no-print-directory check-install-path

# uninstall: THE way to remove the installed gt and bd binaries (gt-acdfp).
# Runs cmd/uninstall-gt (go run) on $(INSTALL_DIR) — an explicit path, never
# `command -v`, which resolves to the live install and is what deleted the
# town's binaries on 2026-09-30. It refuses when $(INSTALL_DIR) is a town's
# live install; FORCE=1 is the override for a real decommission. It removes
# the binaries only: deleting the workspace is a separate act, and one this
# target will not perform. Point INSTALL_DIR at a scratch directory to test it.
uninstall:
	@go run ./cmd/uninstall-gt --install-dir "$(INSTALL_DIR)" $(if $(filter 1,$(FORCE)),--force)

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
		echo "Run go run ./cmd/bump-version <version> before tagging, or re-tag HEAD correctly."; \
		echo "See gh#3459 for background."; \
		exit 1; \
	fi; \
	echo "check-version-tag: OK (tag $$TAG matches Version=$$CODE_VERSION)"

clean:
	rm -f $(BUILD_DIR)/$(BINARY)

# Modules nested in this repository (plugins/*/go.mod). The root module's
# `go build ./...` does not reach them, so the gate builds each in its own
# directory.
NESTED_MODULES := $(patsubst %/go.mod,%,$(shell find plugins tracker -name go.mod -not -path '*/testdata/*' 2>/dev/null | LC_ALL=C sort))

# Nested modules whose vet and tests the gate and presubmit run, and whose lint
# `make lint` runs: the root module's `go test ./...` and golangci-lint do not
# reach them. tracker/ is the town tracker (docs/plans/2026-10-25-town-tracker-design.md).
TESTED_MODULES := $(patsubst %/go.mod,%,$(wildcard tracker/go.mod))

# The shell tests, run by test-slow after the gate: the shelltier Go runner runs
# every manifest entry as `bash <path>` and exits 0 green, 1 red, 2 on a usage
# or log-dir failure. A make variable only so scripts/makefile-gate_test.sh can
# drive the failure paths with a stub command.
SHELL_TESTS ?= go run ./internal/shelltier/cmd/shelltier --all

# The exec-tax preflight the gate runs first (gt-2ycne.1): it measures what an
# exec of a fresh program costs in this process tree and warns, naming itself
# on a "gate: WARNING" line, when the tree pays a macOS scan per new
# executable. A warning fails nothing; the landing worker copies it into the
# landing record. A make variable only so scripts/makefile-gate_test.sh can
# drive the warning path with a stub.
EXEC_TAX_PREFLIGHT ?= go run ./internal/exectax/cmd/preflight

# The gate's lint waits its turn on golangci-lint's module lock instead of
# exiting in 5s: the gate is judged by its exit code alone, so a contended
# lint must not read as red. Plain `make lint` retries a contended lint for
# LINT_LOCK_WAIT instead, keeping the lock line internal/lintlock reads (gt-kqwu).
# A target-specific variable, so it reaches the lint prerequisite.
gate: LINT_RUNNER_FLAGS := --allow-serial-runners
# The gate's start, read when make parses the Makefile, so the wall it prints
# includes lint.
gate: GATE_START := $(shell date +%s)
gate: gate-lint gate-test

# The gate's two stages, which the landing worker runs one at a time, each
# under its own timeout, and om review only after both pass (gt-b5ugw). make
# gate is the two in order, unchanged in effect.
gate-lint: LINT_RUNNER_FLAGS := --allow-serial-runners
gate-lint: exec-tax-preflight lint

# gate-test is build plus the unit tier. Its printed wall is its own: lint
# reports separately (a target-specific value here wins over the one gate
# would pass down).
gate-test: GATE_START := $(shell date +%s)
gate-test:
	@echo "gate: build (go build ./... and the nested modules: $(NESTED_MODULES))" >&2
	@go build ./... || { echo "gate: FAILED at build" >&2; exit 1; }
	@# -o into a temp dir: `go build ./...` over a module with one main
	@# package writes that binary into the module's directory.
	@out=$$(mktemp -d); for m in $(NESTED_MODULES); do (cd "$$m" && go build -o "$$out/" ./...) || { rm -rf "$$out"; echo "gate: FAILED at build ($$m)" >&2; exit 1; }; done; rm -rf "$$out"
	@# The unit tier: every package, through go test's result cache, so a
	@# landing reruns only the packages its change can affect (gt-s1vff).
	@# The budget runner's CPU-measuring exec wrapper bypassed the cache and
	@# reran every test on every landing; per-package CPU and wall budgets are
	@# enforced by make tier-check (the hourly sweep), not the landing path.
	@# -timeout 20m is the per-package hang detector (gt-ik4a1.1).
	@echo "gate: unit tier (every package)" >&2
	@# The suite runs in the background so the trap fires at once on INT or
	@# TERM (bash defers traps until a foreground child exits); the trap finds
	@# it as this shell's child (pgrep -P) and stops it before its children.
	@trap 'for p in $$(pgrep -P $$$$); do k=$$(pgrep -P $$p); kill $$p 2>/dev/null; [ -n "$$k" ] && kill $$k 2>/dev/null; done; exit 130' INT TERM; \
	GT_TEST_DOCKER=0 go test -timeout 20m ./... & gt=$$!; \
	wait $$gt; go_rc=$$?; \
	if [ $$go_rc -eq 0 ]; then for m in $(TESTED_MODULES); do echo "gate: vet and unit tier in $$m" >&2; (cd "$$m" && go vet ./... && GT_TEST_DOCKER=0 go test ./...) || { go_rc=$$?; break; }; done; fi; \
	wall=$$(( $$(date +%s) - $(GATE_START) )); \
	if [ $$go_rc -ne 0 ]; then echo "gate: FAILED at unit tier (Go suite, exit $$go_rc) after $${wall}s wall" >&2; exit 1; fi; \
	echo "gate: PASSED in $${wall}s wall" >&2

# The preflight the gate runs before lint. It prints at most a warning, so a
# preflight that cannot measure (no `go run`, a temp dir it cannot write)
# leaves the gate's verdict to the tree's tests.
exec-tax-preflight:
	@$(EXEC_TAX_PREFLIGHT) || true

# tier-check is the unit tier with the wall check failing. It is the drift
# guard that the gate is not: it reruns every package, so it belongs
# after a landing or in a daily job, where a loaded host costs a retry and not
# a refused landing.
tier-check:
	@echo "tier-check: unit tier, failing any package over the wall limit" >&2
	@GT_TEST_DOCKER=0 go run ./internal/testpolicy/cmd/budget -fast-tier -strict-wall -- -timeout 20m ./...

# presubmit is gt done's pre-submit check (gt-ssyxd): the landing worker gates
# every branch on the merged tree with `make gate`, so a polecat running the
# whole gate first only doubles the host load. This target lints, builds, and
# tests just the packages the branch changed (internal/land/cmd/changedpkgs,
# unit-tested in internal/land/changed_test.go). It skips the budget runner, the
# drift guard and the shell tests. Judged by exit code alone, like the gate. A
# package the branch deleted is not tested: there is nothing to run.
#
# It then runs the tree-wide guard tests the branch's changed paths trigger,
# under -run so the whole suite of the package hosting one stays out of it:
# internal/cmd and internal/polecat take minutes each, the guard test inside
# them a second. Otherwise a branch that breaks a guard learns it from the
# landing gate minutes later, which is what gt-ydzwb exists to stop.
#
# The profile the changed packages' test run writes is judged too
# (internal/presubmit, gt-b9j3o.5): an error return the branch adds that no
# unit test executes fails here rather than as an om finding at landing. That
# run passes -coverprofile, which go test cannot cache, so the changed
# packages' tests rerun even when nothing else about them changed.
#
# A commit that changes a go.mod or go.sum file must explain it in a
# "Rationale:" line (internal/presubmit/cmd/gomodrationale, gt-b9j3o.6).
# PRESUBMIT_BASE is the ref the branch is compared against.
PRESUBMIT_BASE ?= origin/main
presubmit: LINT_RUNNER_FLAGS := --allow-serial-runners
presubmit: GATE_START := $(shell date +%s)
presubmit: lint
	@echo "presubmit: build (go build ./... and the nested modules: $(NESTED_MODULES))" >&2
	@go build ./... || { echo "presubmit: FAILED at build" >&2; exit 1; }
	@out=$$(mktemp -d); for m in $(NESTED_MODULES); do (cd "$$m" && go build -o "$$out/" ./...) || { rm -rf "$$out"; echo "presubmit: FAILED at build ($$m)" >&2; exit 1; }; done; rm -rf "$$out"
	@for m in $(TESTED_MODULES); do if ! git diff --quiet $(PRESUBMIT_BASE)...HEAD -- "$$m"; then echo "presubmit: vet and tests in $$m" >&2; (cd "$$m" && go vet ./... && GT_TEST_DOCKER=0 go test -timeout 10m ./...) || { echo "presubmit: FAILED at tests ($$m)" >&2; exit 1; }; fi; done
	@pkgs=$$(go run ./internal/land/cmd/changedpkgs -base $(PRESUBMIT_BASE)) || { echo "presubmit: FAILED computing the changed packages against $(PRESUBMIT_BASE)" >&2; exit 1; }; \
	guards=$$(go run ./internal/land/cmd/changedpkgs -base $(PRESUBMIT_BASE) -guards) || { echo "presubmit: FAILED computing the tree-wide guard tests against $(PRESUBMIT_BASE)" >&2; exit 1; }; \
	cover=; \
	if [ -z "$$pkgs" ] && [ -z "$$guards" ]; then echo "presubmit: neither a Go package nor a tree-wide guard input changed against $(PRESUBMIT_BASE); no tests to run" >&2; \
	else \
		if [ -n "$$pkgs" ]; then \
			cover=$$(mktemp "$${TMPDIR:-/tmp}/gt-presubmit-cover.XXXXXX") || { echo "presubmit: FAILED making room for the cover profile" >&2; exit 1; }; \
			trap 'rm -f "$$cover"' EXIT; \
			echo "presubmit: go test of the changed packages:" $$pkgs >&2; \
			GT_TEST_DOCKER=0 go test -timeout 10m -coverprofile="$$cover" $$pkgs || { echo "presubmit: FAILED at tests" >&2; exit 1; }; \
		fi; \
		if [ -n "$$guards" ]; then \
			echo "presubmit: go test of the tree-wide guard tests:" $$guards >&2; \
			GT_TEST_DOCKER=0 go test -timeout 10m $$guards || { echo "presubmit: FAILED at the tree-wide guard tests" >&2; exit 1; }; \
		fi; \
		if [ -n "$$cover" ]; then \
			go run ./internal/presubmit/cmd/errbranch -base $(PRESUBMIT_BASE) -cover "$$cover" || { echo "presubmit: FAILED at the error-branch check" >&2; exit 1; }; \
		fi; \
	fi; \
	echo "presubmit: go.mod rationale check (go run ./internal/presubmit/cmd/gomodrationale)" >&2; \
	go run ./internal/presubmit/cmd/gomodrationale -base $(PRESUBMIT_BASE) || { echo "presubmit: FAILED at the go.mod rationale check (the message above names the commit and the cause)" >&2; exit 1; }; \
	gtdir=$$(mktemp -d) || { echo "presubmit: FAILED making room for the machine-stdout check" >&2; exit 1; }; \
	$(MAKE) --no-print-directory build BUILD_DIR="$$gtdir" >/dev/null || { rm -rf "$$gtdir"; echo "presubmit: FAILED building gt for the machine-stdout check" >&2; exit 1; }; \
	echo "presubmit: machine-readable stdout check (go run ./cmd/machinecheck)" >&2; \
	go run ./cmd/machinecheck -gt "$$gtdir/$(BINARY)" || { rm -rf "$$gtdir"; echo "presubmit: FAILED at the machine-stdout check" >&2; exit 1; }; \
	rm -rf "$$gtdir"; \
	echo "presubmit: PASSED in $$(( $$(date +%s) - $(GATE_START) ))s wall" >&2

# test-slow is the post-landing check: the gate, then the shell tests. There
# is no slow tier any more (gt-ik4a1.9); the name stays because the rig's
# merge_queue.post_land_command names it.
test-slow: gate
	@start=$$(date +%s); \
	GT_TEST_DOCKER=0 $(SHELL_TESTS) || { echo "test-slow: FAILED at shell tests" >&2; exit 1; }; \
	echo "test-slow: PASSED (shell tests in $$(( $$(date +%s) - start ))s wall, after the gate)" >&2

# test runs every tier for a human: the gate and the shell tests (test-slow),
# then test-integration. It starts containers, so run it under `gt slot run`.
test: test-slow test-integration

# The Docker-backed packages (internal/testpolicy/docker.txt, kept exact by
# TestDockerTier). Their container tests skip in the gate and run here.
DOCKER_PKGS := $(addprefix ./,$(shell sed -e 's/\#.*//' internal/testpolicy/docker.txt))

# test-integration runs the //go:build integration tier (real tmux, bd, Dolt,
# the gt binary) and then the Docker-backed packages whole. It passes no -run
# filter, so every test in an integration-tagged file runs whatever its name —
# a filter is what left ~120 of them running nowhere (gt-ik4a1.2). The tag also
# compiles the unit tests in beside them, and those run too.
#
# Each command pins GT_TEST_DOLT_INIT_CONCURRENCY to the scheduler town slot
# count, internal/cmd's schedulerTownSlots. Package beads reads that override
# at init() and otherwise caps test-Dolt inits at its default of 4, so town
# setup queues as soon as the slot cap rises above that (gt-ik4a1.4.9,
# gt-ik4a1.4.12). It is a literal, like GT_TEST_DOCKER, so an inherited value
# cannot decide it; TestMakefileHandsTheContainerOptInToTheSuite and
# scripts/makefile-gate_test.sh read schedulerTownSlots and fail on drift.
#
# Before changing this target, read the gate section of docs/testing.md: it
# holds when each tier runs, and INTEGRATION_GO_TEST, the runner swap CI uses
# to collect JUnit output from this one definition of the tier.
INTEGRATION_GO_TEST ?= go test
test-integration:
	@test -n "$(strip $(DOCKER_PKGS))" || { echo "test-integration: internal/testpolicy/docker.txt lists no package; refusing to run go test over nothing" >&2; exit 1; }
	GT_TEST_DOCKER=1 GT_TEST_DOLT_INIT_CONCURRENCY=8 $(INTEGRATION_GO_TEST) -tags integration -timeout 20m ./...
	GT_TEST_DOCKER=1 GT_TEST_DOLT_INIT_CONCURRENCY=8 $(INTEGRATION_GO_TEST) -timeout 20m $(DOCKER_PKGS)
	@for m in $(TESTED_MODULES); do echo "test-integration: vet and integration tier in $$m" >&2; (cd "$$m" && go vet -tags integration ./... && go test -tags integration -timeout 20m ./...) || { echo "test-integration: FAILED in $$m" >&2; exit 1; }; done

# test-integration-wall is test-integration with the tier's wall measured
# (gt-ik4a1.4.5): the same two commands, each -json, both through one
# internal/testpolicy/cmd/tierwall run, which prints a line per package,
# slowest first, then the tier's own wall against the 90s the post-merge
# cadence waits for (gt-ik4a1.4). The sweep (cmd/tier-sweep) runs packages one
# at a time, so this target, not the sweep, measures the tier's wall under
# make test-integration, where packages run together.
#
# Both commands also carry -count=1 (gt-gq4gl): the number decides the cadence
# switch (gt-ik4a1.4), and without it a second run on unchanged source is
# served from the Go test cache, so a wall measured from the cache can pass
# the 90s bar a cold run of the same tree would blow. The flag sits after
# $(INTEGRATION_GO_TEST) so a CI runner swap keeps it.
#
# Nothing here fails on wall: wall time varies with host load, and a red
# post-land run reverts landings (gt-z7qtk). TIERWALL_FLAGS=-max-wall=90s asks
# for that failure. Either way the target sets GT_TEST_DOCKER=1 and the same
# GT_TEST_DOLT_INIT_CONCURRENCY as test-integration, so it starts containers
# and belongs under `gt slot run`.
#
# The recipe runs under bash for `set -o pipefail`, which keeps go test's own
# failure red: without it the pipeline's exit status is tierwall's alone, and
# a go test that died before emitting a FAIL event would read green.
# TIERWALL_FLAGS is a variable only so a caller can add -max-wall.
TIERWALL_FLAGS ?=
test-integration-wall: SHELL := /bin/bash
test-integration-wall:
	@test -n "$(strip $(DOCKER_PKGS))" || { echo "test-integration-wall: internal/testpolicy/docker.txt lists no package; refusing to run go test over nothing" >&2; exit 1; }
	@set -o pipefail; { \
		GT_TEST_DOCKER=1 GT_TEST_DOLT_INIT_CONCURRENCY=8 $(INTEGRATION_GO_TEST) -json -count=1 -tags integration -timeout 20m ./...; \
		GT_TEST_DOCKER=1 GT_TEST_DOLT_INIT_CONCURRENCY=8 $(INTEGRATION_GO_TEST) -json -count=1 -timeout 20m $(DOCKER_PKGS); \
	} | go run ./internal/testpolicy/cmd/tierwall $(TIERWALL_FLAGS)

# test-timing measures the unit tier in a tmux server started by launchd, which
# macOS does not exempt from its first-run scan of new executables. It is the
# acceptance measurement for docs/plans/2026-09-27-test-rewrite-design.md.
# PKGS narrows the run, e.g. make test-timing PKGS=./internal/tmux/...
test-timing:
	go run ./internal/testpolicy/cmd/testtiming -- $(PKGS)

# test-makefile runs the shell tests alone: the same runner (SHELL_TESTS) that
# test-slow runs too, so a second copy cannot drift from it and make
# `make test-slow` run less than this target advertised (gt-tqxdd).
test-makefile:
	GT_TEST_DOCKER=0 $(SHELL_TESTS)

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
