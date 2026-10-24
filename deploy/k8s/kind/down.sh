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

status=0
# The town entry is handled before anything is torn down: a refusal or a failed
# removal stops the run with the cluster and both registrations intact. Once
# the removal has succeeded, a problem with the selection is reported and the
# teardown still finishes, so the run never stops between "entry gone" and
# "cluster gone".
if town_gateway_registered; then
  # A probe that fails is not "no active gateway": without the answer, the run
  # cannot tell whether it is about to remove the gateway the daemon uses.
  town_active_before="$(town_active_gateway)" ||
    die "cannot read the active gateway in $TOWN_CONFIG; run: openshell gateway list, then rerun down.sh"
  if [ "$town_active_before" = "$GATEWAY_NAME" ]; then
    die "$GATEWAY_NAME is the active gateway in $TOWN_CONFIG and removing it leaves none selected; run: openshell gateway select <other-gateway>, then rerun down.sh"
  fi
  town_openshell gateway remove "$GATEWAY_NAME" >/dev/null ||
    die "cannot remove $GATEWAY_NAME from $TOWN_CONFIG"
  # Removing a gateway that is not the active one leaves the selection alone.
  # If the CLI moves it anyway, put the previous selection back; an empty
  # previous selection is the config's own "none", so a new one is reported
  # rather than silently accepted.
  if ! town_active_after="$(town_active_gateway)"; then
    status=1
    warn "removed $GATEWAY_NAME from $TOWN_CONFIG but cannot read the active gateway now; run: openshell gateway list"
  elif [ "$town_active_after" != "$town_active_before" ]; then
    if [ -z "$town_active_before" ]; then
      status=1
      warn "removing $GATEWAY_NAME from $TOWN_CONFIG selected $town_active_after, but no gateway was selected before; run: openshell gateway select <gateway>"
    elif ! town_openshell gateway select "$town_active_before" >/dev/null 2>&1; then
      status=1
      warn "removed $GATEWAY_NAME from $TOWN_CONFIG but could not select $town_active_before again; run: openshell gateway select $town_active_before"
    fi
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
exit "$status"
