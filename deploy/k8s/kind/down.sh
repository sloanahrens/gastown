#!/usr/bin/env bash
# Remove the local Kubernetes sandbox environment. The Colima VM keeps running
# unless --stop-vm is given. Never touches the gt-kind profile or Docker Desktop.
set -euo pipefail
# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

stop_vm=0
[ "${1:-}" = "--stop-vm" ] && stop_vm=1
original_context="$(docker context show)"

vmkind delete cluster --name "$CLUSTER"
vmdocker rm -f "$REGISTRY_NAME" >/dev/null 2>&1 || true
openshell gateway remove "$GATEWAY_NAME" >/dev/null 2>&1 || true
if [ "$stop_vm" = 1 ]; then
  colima stop -p "$PROFILE"
  restore_context "$original_context"
fi
say "removed cluster $CLUSTER"
