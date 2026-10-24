#!/usr/bin/env bash
# The Go floor of the polecat image. The image's Go must be at least the highest
# `go` line of any rig's go.mod: a newer line makes go download a toolchain, and
# the sandbox policy does not let that download reach the module hosts. The
# floor is read from the rig files on the host and checked inside the image, so
# no version is kept by hand here.
#
#   go-floor.sh rig <go.mod>...  (host, make polecat-image) prints the highest
#                                `go` line of the files; exits 2 when none has one.
#   go-floor.sh check <floor>    (image, the Dockerfile's RUN) exits 0 when the Go
#                                in $GO_BIN (default /usr/local/go/bin/go) is at
#                                least <floor>, 1 when below, 2 when <floor> is empty.
set -euo pipefail

rig() {
  if [[ $# -eq 0 ]]; then
    echo "go-floor: rig needs at least one go.mod" >&2
    exit 2
  fi
  local floor
  floor=$(awk '$1 == "go" { print $2 }' "$@" | sort -V | tail -n 1)
  if [[ ! "$floor" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
    echo "go-floor: no go line in: $*" >&2
    exit 2
  fi
  echo "$floor"
}

check() {
  local floor="${1:-}"
  if [[ -z "$floor" ]]; then
    echo "go-floor: GO_FLOOR is empty; build with --build-arg GO_FLOOR=<rig floor>" >&2
    exit 2
  fi
  local now
  now=$(GOTOOLCHAIN=local "${GO_BIN:-/usr/local/go/bin/go}" env GOVERSION | sed 's/^go//')
  # The floor sorts first, or ties, only when the installed Go is at least the floor.
  if [[ "$(printf '%s\n%s\n' "$floor" "$now" | sort -V | head -n 1)" != "$floor" ]]; then
    echo "go-floor: image Go ${now} is below GO_FLOOR ${floor}" >&2
    exit 1
  fi
}

case "${1:-}" in
  rig) shift; rig "$@" ;;
  check) shift; check "$@" ;;
  *) echo "usage: go-floor.sh rig <go.mod>... | check <floor>" >&2; exit 2 ;;
esac
