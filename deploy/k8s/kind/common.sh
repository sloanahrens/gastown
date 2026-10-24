#!/usr/bin/env bash
# Shared settings and helpers for up.sh and down.sh. Sourced, not run.
#
# The kind environment lives in its own Colima profile so it cannot disturb the
# town's Docker Desktop or the gt-kind profile. Everything here is per command:
# the kind VM's docker is reached with DOCKER_HOST, never by switching the
# global docker context (colima start does that on its own; restore_context
# puts it back).
# Variables here are used by the scripts that source this file.
# shellcheck disable=SC2034
set -euo pipefail

PROFILE="${GT_K8S_COLIMA_PROFILE:-gt-sbx}"
CLUSTER="${GT_K8S_CLUSTER:-gt-sbx}"
OUT="${GT_K8S_DIR:-$HOME/gt/.runtime/sbx-k8s}"
GATEWAY_PORT="${GT_K8S_GATEWAY_PORT:-28443}"
REGISTRY_PORT="${GT_K8S_REGISTRY_PORT:-5001}"
REGISTRY_NAME="gt-registry"
AGENT_SANDBOX_VERSION="v1.0.7"
CHART_VERSION="0.1.4" # 0.1.5-pre.4 publishes a server leaf not signed by its CA
GATEWAY_NAME="k8s"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VALUES="$HERE/../values/kind.yaml"

export KUBECONFIG="$OUT/kubeconfig"
export XDG_CONFIG_HOME="$OUT/xdg" # isolates `openshell gateway add` from ~/.config/openshell
export PATH="$OUT/bin:$PATH"

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

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
