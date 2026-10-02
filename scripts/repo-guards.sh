#!/usr/bin/env bash
# Repository guards. Run from make lint. Usage: repo-guards.sh [repo-root]
# (default: this repo).
#   1. go.mod has no replace directives: they break
#      `go install github.com/steveyegge/gastown/cmd/gt@latest` (upstream #2230).
#   2. .beads/issues.jsonl is not tracked: the Dolt server is the only beads
#      backend. Only tracked files count; bd may export the file locally.
#   3. every tracked .go file is gofmt -s clean, test files included (gt-kc9ck).
# 1 and 2 came from the deleted upstream CI (gt-638go.6).
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

# make lint's golangci-lint carries gofmt as a formatter, but .golangci.yml sets
# run.tests: false, so golangci-lint never loads a *_test.go file and test drift
# reaches lint only here (gt-kc9ck). -s matches the formatter's simplify
# setting. testdata/ is skipped: the Go toolchain does not treat it as source,
# so a fixture there may hold code no formatter has touched.
files=()
while IFS= read -r -d '' f; do
	case "$f" in
	testdata/* | */testdata/*) continue ;;
	esac
	files+=("$f")
done < <(git -C "$root" ls-files -z -- '*.go')

if ! command -v gofmt >/dev/null 2>&1; then
	echo "repo-guards: gofmt is not on PATH; make lint needs the Go toolchain" >&2
	rc=1
elif ((${#files[@]})); then
	if ! unformatted=$(cd "$root" && gofmt -s -l "${files[@]}"); then
		echo "repo-guards: gofmt -s failed" >&2
		rc=1
	elif [[ -n "$unformatted" ]]; then
		echo "repo-guards: these files are not gofmt -s clean; run 'gofmt -s -w' on them:" >&2
		printf '%s\n' "$unformatted" >&2
		rc=1
	fi
fi

exit "$rc"
