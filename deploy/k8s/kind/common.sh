#!/usr/bin/env bash
# shellcheck disable=SC2034
# Shared settings and helpers for up.sh and down.sh. Sourced, not run.
#
# The kind environment lives in its own Colima profile so it cannot disturb the
# town's Docker Desktop or the gt-kind profile. Everything here is per command:
# the kind VM's docker is reached with DOCKER_HOST, never by switching the
# global docker context (colima start does that on its own; restore_context
# puts it back).
#
# Shell options stay with the caller: up.sh and down.sh set -euo pipefail
# before sourcing this file, so a future caller with different options is not
# tightened behind its back.
# The settings below are read by the scripts that source this file.

PROFILE="${GT_K8S_COLIMA_PROFILE:-gt-sbx}"
CLUSTER="${GT_K8S_CLUSTER:-gt-sbx}"
OUT="${GT_K8S_DIR:-$HOME/gt/.runtime/sbx-k8s}"
GATEWAY_PORT="${GT_K8S_GATEWAY_PORT:-28443}"
REGISTRY_PORT="${GT_K8S_REGISTRY_PORT:-5001}"
REGISTRY_NAME="gt-registry"
AGENT_SANDBOX_VERSION="v1.0.7"
# Empty records the first fetch's digest next to the manifest; a release tag
# can be re-pointed, so set this to pin the bytes before the first fetch.
AGENT_SANDBOX_SHA256="${GT_K8S_AGENT_SANDBOX_SHA256:-}"
CHART_VERSION="0.1.4" # 0.1.5-pre.4 publishes a server leaf not signed by its CA
GATEWAY_NAME="k8s"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VALUES="$HERE/../values/kind.yaml"

export KUBECONFIG="$OUT/kubeconfig"
# Where the daemon's openshell reads its gateways. Captured before the export
# below moves this script's own calls to the private config, so --register-town
# (up.sh) writes to the same directory the daemon reads (gt-0knzj).
TOWN_XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
export XDG_CONFIG_HOME="$OUT/xdg" # `up.sh --register-town` is the only path into ~/.config/openshell
export PATH="$OUT/bin:$PATH"

say() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# sha256_of FILE prints the hex digest the manifest pin file stores.
sha256_of() { # sha256_of FILE
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# vmdocker runs docker against the kind VM only.
vmdocker() { DOCKER_HOST="unix://$HOME/.colima/$PROFILE/docker.sock" docker "$@"; }

# vmkind runs kind against the kind VM's docker. Every kind call must use it:
# a bare `kind get clusters` asks the town's Docker Desktop and sees no cluster.
vmkind() { DOCKER_HOST="unix://$HOME/.colima/$PROFILE/docker.sock" kind "$@"; }

# restore_context puts docker's global context back to what it was before a
# colima start, which switches it to colima-<profile> as a side effect and
# sends every plain `docker` call in the town (Forgejo, the CI runner, the
# polecat-image plugin) to the wrong VM.
restore_context() { # restore_context ORIGINAL
  local now
  now="$(docker context show)"
  if [ "$now" != "$1" ]; then
    say "colima switched the docker context to $now; restoring $1"
    docker context use "$1" >/dev/null
  fi
}
