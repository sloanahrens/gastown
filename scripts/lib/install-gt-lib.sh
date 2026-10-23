#!/usr/bin/env bash
# install-gt-lib.sh — helpers shared by install-gt.sh and uninstall-gt.sh.
# Sourced, never run. Callers set DAEMON_DIR and SOURCE before using igt_receipt.

# igt_receipt EVENT COMMIT PREV REASON MERGED_AT_EPOCH START_EPOCH
# Appends one line to $DAEMON_DIR/install-receipts.jsonl. A failed write is
# logged, never fatal: the receipt describes the install, it is not the install.
# Absent fields (commit, prev, source, merged_at, reason, duration_s) are
# written as JSON null; consumers must treat null and "" as absent.
igt_receipt() {
  mkdir -p "$DAEMON_DIR" 2>/dev/null || true
  python3 - "$DAEMON_DIR/install-receipts.jsonl" "$1" "${2:-}" "${3:-}" "${SOURCE:-}" "${5:-}" "${4:-}" "${6:-}" <<'PY' || echo "[install-gt] WARNING: could not append a receipt to $DAEMON_DIR/install-receipts.jsonl" >&2
import datetime, json, sys, time
path, event, commit, prev, source, merged_epoch, reason, start = sys.argv[1:9]
def utc(t):
    return datetime.datetime.fromtimestamp(t, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
now = time.time()
rec = {
    "ts": utc(now),
    "event": event,
    "commit": commit or None,
    "prev_commit": prev or None,
    "source": source or None,
    "merged_at": utc(int(merged_epoch)) if merged_epoch else None,
    "reason": reason or None,
    "duration_s": int(now - int(start)) if start else None,
}
# One short line per append: O_APPEND keeps concurrent writers (the daemon's
# daemon_restarted receipts) from interleaving inside a line.
with open(path, "a") as f:
    f.write(json.dumps(rec, sort_keys=True) + "\n")
PY
}

# igt_result EVENT COMMIT PREV REASON — the machine-readable last line.
igt_result() {
  echo "install-gt: RESULT ${1:--} ${2:--} ${3:--} ${4:--}"
}

# igt_binary_commit_raw GT DIR — the commit baked into the gt binary at GT, as
# it reports it (usually SHORT), or empty. 'gt stale --json' binary_commit
# first; 'gt version' as the fallback, which from a non-repo cwd prints
# "(dev: <short>)" with no '@' (gt-b5mpe), so both forms are parsed.
igt_binary_commit_raw() {
  local gt="$1" dir="$2" c=""
  [ -x "$gt" ] || { echo ""; return 0; }
  c=$( (cd "$dir" && "$gt" stale --json 2>/dev/null) | python3 -c '
import json, sys
try:
    print(json.load(sys.stdin).get("binary_commit") or "")
except Exception:
    print("")
' 2>/dev/null || true)
  if [ -z "$c" ]; then
    c=$( (cd "$dir" && "$gt" version 2>/dev/null) | python3 -c '
import re, sys
m = re.search(r"\([^:()]+: (?:[^@()]*@)?([0-9a-f]{7,40})\)", sys.stdin.read())
print(m.group(1) if m else "")
' 2>/dev/null || true)
  fi
  echo "$c"
}

# igt_json_commit FILE — the "commit" field of a JSON file, or empty when the
# file is missing, unreadable, not JSON, or has no commit in it. The daemon's
# state.json and the restart marker both name a build commit this way.
igt_json_commit() {
  python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as f:
        print(json.load(f).get("commit") or "")
except Exception:
    print("")
' "$1" 2>/dev/null || true
}

# igt_daemon_commit DAEMON_DIR — the build commit of the running daemon, from
# state.json's commit field (the daemon writes its own build commit there).
#
# The installed binary can be ahead of the daemon still running the old one —
# an install killed between install-local and the restart marker — so "is the
# daemon on this commit?" is a question about state.json, not about the binary
# at $GT.
igt_daemon_commit() { igt_json_commit "$1/state.json"; }

# igt_pending_commit DAEMON_DIR — the commit a pending restart marker names, or
# empty when there is no readable daemon/restart-pending.json. A marker already
# in place is what the daemon will restart into; callers compare before they
# overwrite it.
igt_pending_commit() { igt_json_commit "$1/restart-pending.json"; }

# igt_resolve DIR REF — full commit hash of REF inside DIR, or empty.
igt_resolve() {
  [ -n "${2:-}" ] || { echo ""; return 0; }
  git -C "$1" rev-parse --verify --quiet "$2^{commit}" 2>/dev/null || echo ""
}

# igt_town_root_above DIR — the outermost ancestor of DIR holding
# mayor/town.json, or empty when there is none. Outermost, as
# internal/workspace does it: a rig can carry its own mayor/.
igt_town_root_above() {
  local d="$1" found=""
  while [ "$d" != "/" ] && [ -n "$d" ]; do
    [ -f "$d/mayor/town.json" ] && found="$d"
    d=$(dirname "$d")
  done
  echo "$found"
}

# igt_gateway_agent MODE TEMPLATE HOME — install the OpenShell gateway's user
# launchd agent (com.gastown.openshell-gateway) from TEMPLATE, rendered with
# HOME. MODE is "install" or "dry-run" (print the plan, change nothing).
#
# The agent is written only once the operator's ~/.config/openshell/gateway.toml
# exists, since the gateway cannot start without it. It is bootstrapped when
# launchd has not loaded it and never kickstarted: a kickstart drops every live
# sandbox session the gateway is serving, so a restart is the operator's call
# (docs/openshell-gateway.md). The plan is also written to stdout as
# "gateway: ..." lines. Always returns 0; a failed step logs one WARNING.
# Env: INSTALL_GT_LAUNCHD_DIR (default HOME/Library/LaunchAgents).
igt_gateway_agent() {
  local mode="$1" tmpl="$2" home="$3"
  local label="com.gastown.openshell-gateway"
  local toml="$home/.config/openshell/gateway.toml"
  local dir="${INSTALL_GT_LAUNCHD_DIR:-$home/Library/LaunchAgents}"
  local plist="$dir/$label.plist"
  local domain="gui/$(id -u)" rendered
  if [ ! -f "$toml" ]; then
    echo "gateway: skipped, no $toml"
    return 0
  fi
  if [ ! -f "$tmpl" ]; then
    echo "[install-gt] WARNING: gateway: no template at $tmpl" >&2
    return 0
  fi
  rendered=$(python3 - "$tmpl" "$home" <<'PY'
import sys
from xml.sax.saxutils import escape
tmpl, home = sys.argv[1:3]
sys.stdout.write(open(tmpl).read().replace("__HOME__", escape(home)))
PY
  ) || { echo "[install-gt] WARNING: gateway: could not render $tmpl" >&2; return 0; }
  local changed=1
  [ -f "$plist" ] && [ "$(cat "$plist")" = "$rendered" ] && changed=0
  local loaded=0
  launchctl print "$domain/$label" >/dev/null 2>&1 && loaded=1
  if [ "$mode" = "dry-run" ]; then
    [ "$changed" = 1 ] && echo "gateway: would write $plist"
    [ "$loaded" = 0 ] && echo "gateway: would run: launchctl bootstrap $domain $plist"
    return 0
  fi
  if [ "$changed" = 1 ]; then
    mkdir -p "$dir" "$home/gt/.runtime/openshell" || { echo "[install-gt] WARNING: gateway: cannot create $dir" >&2; return 0; }
    printf '%s\n' "$rendered" > "$plist.tmp.$$" && mv -f "$plist.tmp.$$" "$plist" \
      || { rm -f "$plist.tmp.$$"; echo "[install-gt] WARNING: gateway: cannot write $plist" >&2; return 0; }
    echo "gateway: wrote $plist"
  fi
  if [ "$loaded" = 0 ]; then
    if launchctl bootstrap "$domain" "$plist" >/dev/null 2>&1; then
      echo "gateway: bootstrapped $label"
    else
      echo "[install-gt] WARNING: gateway: launchctl bootstrap $domain $plist failed" >&2
    fi
  elif [ "$changed" = 1 ]; then
    echo "gateway: $label is loaded and keeps its old unit until: launchctl kickstart -k $domain/$label"
  fi
  return 0
}
