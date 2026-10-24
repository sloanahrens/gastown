#!/usr/bin/env bash
# Remove the local Kubernetes sandbox environment. The Colima VM keeps running
# unless --stop-vm is given. Never touches the gt-kind profile or Docker Desktop.
# The k8s entry in the town's OpenShell config goes too (up.sh --register-town
# added it), so no gateway is left pointing at a dead cluster (gt-0mhgw).
set -euo pipefail
# shellcheck source=common.sh
. "$(dirname "$0")/common.sh"

stop_vm=0
[ "${1:-}" = "--stop-vm" ] && stop_vm=1
original_context="$(docker context show)"
# colima stop flips docker's global context before it can fail, so the restore
# is an EXIT trap, not the line after the stop.
trap 'restore_context "$original_context" || true' EXIT

# The town entry is handled before anything is torn down: a refusal or a failed
# removal stops the run with the cluster and both registrations intact.
if town_gateway_registered; then
  town_active_before="$(town_active_gateway)"
  if [ "$town_active_before" = "$GATEWAY_NAME" ]; then
    die "$GATEWAY_NAME is the active gateway in $TOWN_CONFIG and removing it leaves none selected; run: openshell gateway select <other-gateway>, then rerun down.sh"
  fi
  town_openshell gateway remove "$GATEWAY_NAME" >/dev/null ||
    die "cannot remove $GATEWAY_NAME from $TOWN_CONFIG"
  # Removing a gateway that is not the active one leaves the selection alone;
  # if the CLI dropped it anyway, select the previously active gateway back.
  if [ -n "$town_active_before" ] && [ "$(town_active_gateway)" != "$town_active_before" ]; then
    town_openshell gateway select "$town_active_before" >/dev/null 2>&1 ||
      die "removed $GATEWAY_NAME from $TOWN_CONFIG but could not select $town_active_before again; run: openshell gateway select $town_active_before"
  fi
  say "removed $GATEWAY_NAME from $TOWN_CONFIG"
fi

vmkind delete cluster --name "$CLUSTER"
vmdocker rm -f "$REGISTRY_NAME" >/dev/null 2>&1 || true
openshell gateway remove "$GATEWAY_NAME" >/dev/null 2>&1 || true
if [ "$stop_vm" = 1 ]; then
  colima stop -p "$PROFILE"
fi
say "removed cluster $CLUSTER"
