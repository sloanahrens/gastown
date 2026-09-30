#!/usr/bin/env bash
# Repository guards carried over from the deleted upstream CI (gt-638go.6).
# Run from make lint. Usage: repo-guards.sh [repo-root] (default: this repo).
#   1. go.mod has no replace directives: they break
#      `go install github.com/steveyegge/gastown/cmd/gt@latest` (upstream #2230).
#   2. .beads/issues.jsonl is not tracked: the Dolt server is the only beads
#      backend. Only tracked files count; bd may export the file locally.
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
rc=0

if grep -nE '^replace([[:space:]]|\()' "$root/go.mod"; then
	echo "repo-guards: go.mod contains replace directives; they break go install ...@latest (upstream #2230)" >&2
	rc=1
fi

if git -C "$root" ls-files --error-unmatch .beads/issues.jsonl >/dev/null 2>&1; then
	echo "repo-guards: .beads/issues.jsonl is tracked; the Dolt server is the only beads backend (git rm --cached it)" >&2
	rc=1
fi

exit "$rc"
