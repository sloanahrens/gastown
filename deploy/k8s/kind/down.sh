#!/usr/bin/env bash
# Remove the local Kubernetes sandbox environment. The Colima VM keeps running
# unless --stop-vm is given. Never touches the gt-kind profile or Docker Desktop.
set -euo pipefail
# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

stop_vm=0
[ "${1:-}" = "--stop-vm" ] && stop_vm=1
original_context="$(docker context show)"
# colima stop flips docker's global context before it can fail, so the restore
# is an EXIT trap, not the line after the stop.
trap 'restore_context "$original_context" || true' EXIT

vmkind delete cluster --name "$CLUSTER"
vmdocker rm -f "$REGISTRY_NAME" >/dev/null 2>&1 || true
openshell gateway remove "$GATEWAY_NAME" >/dev/null 2>&1 || true
if [ "$stop_vm" = 1 ]; then
  colima stop -p "$PROFILE"
fi
say "removed cluster $CLUSTER"
