#!/usr/bin/env bash
# dolt-archive/run.sh — Deterministic JSONL backup + git push.
#
# Exports production databases to JSONL and commits them to the git backup
# repo. JSONL is the last-resort recovery layer. It no longer pushes Dolt
# remotes (ADR 0002); --skip-dolt-push is still accepted and does nothing.
#
# Usage: ./run.sh [--databases db1,db2,...] [--skip-git]

set -euo pipefail

# --- Configuration -----------------------------------------------------------

DOLT_HOST="${GT_DOLT_HOST:-${DOLT_HOST:-127.0.0.1}}"
DOLT_PORT="${GT_DOLT_PORT:-${DOLT_PORT:-3307}}"
DOLT_USER="${DOLT_USER:-root}"
DOLT_DATA_DIR="${DOLT_DATA_DIR:-$HOME/gt/.dolt-data}"
JSONL_EXPORT_DIR="$HOME/gt/.dolt-archive/jsonl"
BACKUP_REPO="$HOME/gt/.dolt-archive/git"
DEFAULT_DBS="auto"
SKIP_GIT=false

# --- Argument parsing --------------------------------------------------------

while [[ $# -gt 0 ]]; do
  case "$1" in
    --databases)    DEFAULT_DBS="$2"; shift 2 ;;
    --skip-git)     SKIP_GIT=true; shift ;;
    --skip-dolt-push) shift ;;  # no-op: Dolt remote push was removed (ADR 0002)
    --help|-h)
      echo "Usage: $0 [--databases db1,db2,...] [--skip-git]"
      exit 0
      ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# --- Helpers -----------------------------------------------------------------

log() {
  echo "[dolt-archive] $*"
}

LOGFILE=$(mktemp /tmp/dolt-archive-stderr.XXXXXX)
trap 'rm -f "$LOGFILE"' EXIT

dolt_query() {
  local db="$1"
  local query="$2"
  local args=(dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --no-tls -u "$DOLT_USER" -p "")
  if [[ -n "$db" ]]; then
    args+=(--use-db "$db")
  fi
  args+=(sql -q "$query" --result-format csv)
  "${args[@]}" 2>>"$LOGFILE" | tail -n +2 | tr -d '\r'
}

dolt_query_json() {
  local db="$1"
  local query="$2"
  dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --no-tls -u "$DOLT_USER" -p "" \
    --use-db "$db" sql -q "$query" --result-format json 2>>"$LOGFILE"
}

# --- Step 1: JSONL export ----------------------------------------------------

# Auto-discover production databases or use the explicit list.
if [[ "$DEFAULT_DBS" == "auto" ]]; then
  PROD_DBS=()
  while IFS= read -r line; do
    PROD_DBS+=("$line")
  done < <(
    dolt_query "" "SHOW DATABASES" \
      | grep -v -E '^(information_schema|mysql|dolt_cluster)$' \
      | grep -v -E '^(testdb_|beads_t|beads_pt|doctest_)'
  )
  if [[ ${#PROD_DBS[@]} -eq 0 ]]; then
    log "ERROR: No production databases found via auto-discovery"
    exit 1
  fi
else
  IFS=',' read -ra PROD_DBS <<< "$DEFAULT_DBS"
fi

log "Starting archive cycle (databases: ${PROD_DBS[*]})"
mkdir -p "$JSONL_EXPORT_DIR"

EXPORTED=0
EXPORT_FAILED=0
EXPORT_ERRORS=""

for DB in "${PROD_DBS[@]}"; do
  EXPORT_FILE="$JSONL_EXPORT_DIR/${DB}-$(date +%Y%m%d-%H%M).jsonl"
  LATEST_LINK="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"

  log "Exporting $DB..."

  # Skip databases without an issues table (e.g. gastown config DB)
  if ! dolt_query "$DB" "SHOW TABLES LIKE 'issues'" 2>/dev/null | grep -q 'issues'; then
    log "  $DB: skipped (no issues table)"
    continue
  fi

  # Export via Dolt SQL (reliable for all databases with an issues table)
  if dolt_query_json "$DB" "SELECT * FROM issues ORDER BY id" > "$EXPORT_FILE" 2>/dev/null && [[ -s "$EXPORT_FILE" ]]; then
    LINE_COUNT=$(wc -l < "$EXPORT_FILE" | tr -d ' ')
    log "  $DB: exported via SQL ($LINE_COUNT lines)"
    ln -sf "$(basename "$EXPORT_FILE")" "$LATEST_LINK"
    EXPORTED=$((EXPORTED + 1))
  else
    log "  WARN: $DB export failed"
    rm -f "$EXPORT_FILE"
    EXPORT_FAILED=$((EXPORT_FAILED + 1))
    EXPORT_ERRORS="${EXPORT_ERRORS}${DB} "
  fi
done

# Prune old exports (keep last 24 snapshots per DB)
for DB in "${PROD_DBS[@]}"; do
  SNAPSHOTS=$(ls -t "$JSONL_EXPORT_DIR/${DB}-2"*.jsonl 2>/dev/null | tail -n +25)
  if [[ -n "$SNAPSHOTS" ]]; then
    echo "$SNAPSHOTS" | xargs rm -f
    log "Pruned old $DB snapshots"
  fi
done

log "JSONL export: $EXPORTED succeeded, $EXPORT_FAILED failed"

# --- Step 2: Git commit and push ---------------------------------------------

GIT_PUSHED=false
GIT_PRECONDITION_FAILED=""

if ! $SKIP_GIT; then
  log ""
  log "=== Git Push ==="

  # Initialize the backup repo on first run instead of silently no-op'ing
  # forever when it doesn't exist yet (gt-kme).
  if [[ ! -d "$BACKUP_REPO/.git" ]]; then
    mkdir -p "$BACKUP_REPO"
    if git init -b main "$BACKUP_REPO" >/dev/null 2>>"$LOGFILE"; then
      log "Initialized backup repo at $BACKUP_REPO"
    else
      log "ERROR: could not initialize backup repo at $BACKUP_REPO"
      GIT_PRECONDITION_FAILED="init failed for $BACKUP_REPO"
    fi
  fi

  if [[ -z "$GIT_PRECONDITION_FAILED" ]]; then
    # Copy latest JSONL files to git repo
    for DB in "${PROD_DBS[@]}"; do
      LATEST="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"
      if [[ -L "$LATEST" ]]; then
        REAL_FILE="$JSONL_EXPORT_DIR/$(readlink "$LATEST")"
        if [[ -f "$REAL_FILE" ]]; then
          cp "$REAL_FILE" "$BACKUP_REPO/${DB}.jsonl"
        fi
      elif [[ -f "$LATEST" ]]; then
        cp "$LATEST" "$BACKUP_REPO/${DB}.jsonl"
      fi
    done

    cd "$BACKUP_REPO"

    # Stage BEFORE checking for changes — `git diff` (unstaged) never sees
    # untracked files, so on every prior run (including the very first,
    # where every *.jsonl file is untracked) this reported "No changes to
    # commit" and silently threw away real exported data (gt-kme).
    git add *.jsonl 2>/dev/null || true

    if git diff --staged --quiet; then
      log "No changes to commit"
    else
      git commit -m "Archive snapshot $(date +%Y-%m-%d-%H%M)" \
        --author="Gas Town Archive <archive@gastown.local>" 2>/dev/null || true

      if git remote get-url origin > /dev/null 2>&1; then
        if git push origin main 2>/dev/null; then
          GIT_PUSHED=true
          log "Pushed to GitHub"
        else
          log "WARN: Git push to remote failed"
          GIT_PRECONDITION_FAILED="git push to origin failed"
        fi
      else
        log "WARN: No git remote configured for backup repo"
        GIT_PRECONDITION_FAILED="no git remote configured for $BACKUP_REPO"
      fi
    fi
  fi
fi

# --- Step 3: Report results --------------------------------------------------

log ""
log "=== Archive Cycle Complete ==="

SUMMARY="Archive: jsonl=$EXPORTED/$((EXPORTED + EXPORT_FAILED)), git=${GIT_PUSHED}"
log "$SUMMARY"

RESULT="success"
if [[ "$EXPORT_FAILED" -gt 0 ]] || [[ -n "$GIT_PRECONDITION_FAILED" ]]; then
  RESULT="warning"
fi

gt plugin record-run --plugin dolt-archive --result "$RESULT" \
  --title "$SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true

if [[ "$EXPORT_FAILED" -gt 0 ]]; then
  gt escalate "dolt-archive: JSONL export failed for $EXPORT_FAILED databases ($EXPORT_ERRORS)" \
    -s critical \
    --reason "JSONL is our last-resort recovery layer. Failed databases: $EXPORT_ERRORS" 2>/dev/null || true
fi

# The offsite git layer is severity:critical (plugin.md) — a missing
# precondition here must escalate, not silently no-op, or this patrol goes
# back to "0 successes, all silent" the moment the remote or repo drifts
# (gt-kme).
if [[ -n "$GIT_PRECONDITION_FAILED" ]]; then
  gt escalate "dolt-archive: offsite git backup not landing ($GIT_PRECONDITION_FAILED)" \
    -s critical \
    --reason "Git push is the offsite recovery layer. $GIT_PRECONDITION_FAILED" 2>/dev/null || true
fi

log "Done."
