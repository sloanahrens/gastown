#!/usr/bin/env bash
# install-gt-lib.sh — helpers shared by install-gt.sh and install-after-merge.sh.
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

# igt_resolve DIR REF — full commit hash of REF inside DIR, or empty.
igt_resolve() {
  [ -n "${2:-}" ] || { echo ""; return 0; }
  git -C "$1" rev-parse --verify --quiet "$2^{commit}" 2>/dev/null || echo ""
}
