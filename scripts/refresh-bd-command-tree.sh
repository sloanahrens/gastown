#!/usr/bin/env bash
# refresh-bd-command-tree.sh — regenerate internal/cmdtree/bd-command-tree.json,
# the bd command surface the command-tree lint checks formulas, templates,
# plugins, hooks and Go exec literals against (gt-fcxe9.5).
#
# Usage:
#   BEADS_SRC=<beads fork checkout> [BEADS_REF=origin/main] [BEADS_SOURCE=<name>] scripts/refresh-bd-command-tree.sh
#   make bd-command-tree BEADS_SRC=<checkout> [BEADS_REF=<ref>]
#
# Exports BEADS_REF with git archive (the checkout is not modified), builds bd
# into a temporary directory (never on PATH), and runs gen-bd-tree against it:
# `bd capabilities --json` plus `bd <parent> --help` for each parent, in an
# empty directory (neither opens a store). Refresh when the fork's commands
# change or its contract_version is bumped; on a bump, add the new version to
# KnownBDContractVersions (internal/deps/bd_handshake.go) in the same commit,
# or TestBdCommandTreeSnapshotMatchesContract fails.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="${BEADS_SRC:?set BEADS_SRC to a beads fork checkout}"
REF="${BEADS_REF:-origin/main}"
# BEADS_SOURCE names the bd fork in the snapshot's "source" field.
NAME="${BEADS_SOURCE:-sloanahrens/beads}"
OUT="$ROOT/internal/cmdtree/bd-command-tree.json"

commit="$(git -C "$SRC" rev-parse --verify "$REF^{commit}")"
work="$(mktemp -d "${TMPDIR:-/tmp}/bd-command-tree.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/src"

git -C "$SRC" archive "$commit" | tar -x -C "$work/src"
(cd "$work/src" && go build -ldflags "-X main.Commit=$commit" -o "$work/bd" ./cmd/bd)
(cd "$ROOT" && go run ./internal/cmdtree/gen-bd-tree -bd "$work/bd" -source "$NAME $REF") >"$work/tree.json"
mv "$work/tree.json" "$OUT"
echo "wrote $OUT from $REF ($commit)"
