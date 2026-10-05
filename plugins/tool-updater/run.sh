#!/usr/bin/env bash
# tool-updater/run.sh — Report which Homebrew tools are outdated; never installs
# or upgrades. Why it is report-only, and how to run it: plugin.md beside this
# script (gt-th5it).

set -euo pipefail

# dolt is the only tool the town upgrades here. beads is reported for
# visibility only: the town runs its own bd fork build, so updating it would
# fix nothing (gt-th5it).
TOOLS=(dolt beads)
ACTIONABLE_TOOL=dolt

log() { echo "[tool-updater] $*"; }

# outdated_versions <tool> reads `brew outdated --json=v2` on stdin and prints
# "<installed> <available>", or exits non-zero when the tool is not outdated.
outdated_versions() {
  python3 -c '
import json, sys
entry = next((f for f in json.load(sys.stdin).get("formulae", [])
              if f.get("name") == sys.argv[1]), None)
if entry is None:
    sys.exit(1)
installed = (entry.get("installed_versions") or ["?"])[-1]
print(installed, entry.get("current_version") or "?")
' "$1"
}

# --- Refresh the Homebrew formula index (installed formulae unchanged) --------

if ! command -v brew >/dev/null 2>&1; then
  log "ERROR: brew not found on PATH; nothing to report"
  exit 1
fi

log "Running brew update..."
HOMEBREW_NO_AUTO_UPDATE=1 brew update 2>&1 | tail -3 || true

# --- Report outdated tools ----------------------------------------------------

log "Checking for updates..."
# One probe, one snapshot, and it fails closed: a broken brew must report an
# error, never the false all-clear "all tools current" (gt-th5it).
if ! OUTDATED_JSON="$(HOMEBREW_NO_AUTO_UPDATE=1 brew outdated --json=v2 2>/dev/null)"; then
  log "ERROR: brew outdated failed; cannot determine tool status"
  exit 1
fi
if ! printf '%s' "$OUTDATED_JSON" | python3 -c '
import json, sys
data = json.load(sys.stdin)
sys.exit(0 if isinstance(data.get("formulae"), list) else 1)
' 2>/dev/null; then
  log "ERROR: unreadable brew outdated output; cannot determine tool status"
  exit 1
fi

OUTDATED=0
for TOOL in "${TOOLS[@]}"; do
  if VERS="$(printf '%s' "$OUTDATED_JSON" | outdated_versions "$TOOL")"; then
    read -r INSTALLED AVAILABLE <<<"$VERS"
    if [[ "$TOOL" == "$ACTIONABLE_TOOL" ]]; then
      log "  $TOOL: update available (installed: $INSTALLED, available: $AVAILABLE)"
      OUTDATED=$((OUTDATED + 1))
    else
      log "  $TOOL: update available (installed: $INSTALLED, available: $AVAILABLE); no action (town runs its own bd fork)"
    fi
  else
    log "  $TOOL: up to date"
  fi
done

# --- Report -------------------------------------------------------------------

if [[ $OUTDATED -eq 0 ]]; then
  SUMMARY="tool-updater: all tools current"
else
  SUMMARY="tool-updater: outdated=$OUTDATED (report-only)"
fi
log ""
log "=== Done === $SUMMARY"
log "Upgrades are deliberate and manual (gt-th5it)."

gt plugin record-run --plugin tool-updater --result success \
  --title "$SUMMARY" >/dev/null 2>&1 || true

exit 0
