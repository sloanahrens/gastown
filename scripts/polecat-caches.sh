#!/usr/bin/env bash
# Creates the per-rig cache volumes an OpenShell polecat sandbox mounts at
# /sandbox/.cache, labelled so the gateway attaches them (claude-q3f.9).
# Run it through `make polecat-caches RIGS="gastown om"`.
#
# Idempotent: an existing volume is left alone, so a rerun changes nothing and
# a sandbox that already mounted it keeps its Go module cache.
set -euo pipefail

usage() {
  echo "usage: $0 <rig>..." >&2
  echo "  make polecat-caches RIGS=\"gastown om\"" >&2
  exit 2
}

[[ $# -gt 0 ]] || usage

for rig in "$@"; do
  if [[ ! "$rig" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
    echo "polecat-caches: '$rig' is not a rig name ([a-z0-9-])" >&2
    exit 2
  fi
done

for rig in "$@"; do
  volume="gt-cache-$rig"
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    echo "polecat-caches: $volume exists"
    continue
  fi
  docker volume create \
    --label openshell.ai/sandbox-attachable=true \
    --label openshell.ai/sandbox-attachable-workspace=default \
    "$volume"
done
