+++
name = "dolt-archive"
description = "Offsite backup: JSONL snapshots to git"
version = 1

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:dolt-archive", "category:data-safety"]
digest = true

[execution]
type = "script"
timeout = "15m"
notify_on_failure = true
severity = "critical"
+++

# Dolt Archive

Gets production data off this machine. Two layers:

1. **JSONL export** — Human-readable snapshots (saved us in Clown Show #13)
2. **Git push** — JSONL files committed and pushed to GitHub

JSONL is the last-resort recovery layer. Always maintain it regardless of
whether the git push works. There is no Dolt push: Dolt remote sync was
removed (ADR 0002), and the Dolt data directory is backed up at the
filesystem level instead.

## Config

```bash
DOLT_DATA_DIR="$GT_TOWN_ROOT/.dolt-data"
PROD_DBS=("hq" "gt" "mo")
JSONL_EXPORT_DIR="$GT_TOWN_ROOT/.dolt-archive/jsonl"
DOLT_HOST="${GT_DOLT_HOST:-127.0.0.1}"
DOLT_PORT="${GT_DOLT_PORT:-3307}"
DOLT_USER="root"
```

## Step 1: JSONL export

Export all issues from each production database to JSONL files. These are
human-readable, diffable, and survive any storage backend failure.

```bash
echo "=== JSONL Export ==="
EXPORTED=0
EXPORT_FAILED=0

mkdir -p "$JSONL_EXPORT_DIR"

for DB in "${PROD_DBS[@]}"; do
  EXPORT_FILE="$JSONL_EXPORT_DIR/${DB}-$(date +%Y%m%d-%H%M).jsonl"
  LATEST_LINK="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"

  echo "Exporting $DB..."

  # Use bd export if available, otherwise query directly
  if bd export --db "$DB" --format jsonl > "$EXPORT_FILE" 2>/dev/null; then
    LINE_COUNT=$(wc -l < "$EXPORT_FILE" | tr -d ' ')
    FILE_SIZE=$(du -h "$EXPORT_FILE" | cut -f1)
    echo "  $DB: $LINE_COUNT issues exported ($FILE_SIZE)"

    # Update latest symlink
    ln -sf "$EXPORT_FILE" "$LATEST_LINK"
    EXPORTED=$((EXPORTED + 1))
  else
    # Fallback: query Dolt directly for issue data
    dolt sql -q "SELECT * FROM issues ORDER BY id" \
      --host "$DOLT_HOST" --port "$DOLT_PORT" -u "$DOLT_USER" \
      -d "$DB" --no-auto-commit --result-format json \
      > "$EXPORT_FILE" 2>/dev/null

    if [ $? -eq 0 ] && [ -s "$EXPORT_FILE" ]; then
      LINE_COUNT=$(wc -l < "$EXPORT_FILE" | tr -d ' ')
      echo "  $DB: exported via SQL ($LINE_COUNT lines)"
      ln -sf "$EXPORT_FILE" "$LATEST_LINK"
      EXPORTED=$((EXPORTED + 1))
    else
      echo "  WARN: $DB export failed"
      rm -f "$EXPORT_FILE"
      EXPORT_FAILED=$((EXPORT_FAILED + 1))
    fi
  fi
done

# Prune old exports (keep last 24 snapshots per DB)
for DB in "${PROD_DBS[@]}"; do
  SNAPSHOTS=$(ls -t "$JSONL_EXPORT_DIR/${DB}-2"*.jsonl 2>/dev/null | tail -n +25)
  if [ -n "$SNAPSHOTS" ]; then
    echo "$SNAPSHOTS" | xargs rm -f
    echo "Pruned old $DB snapshots"
  fi
done

echo "Exported: $EXPORTED, failed: $EXPORT_FAILED"
```

## Step 2: Git commit and push

Commit JSONL snapshots to a backup branch and push to GitHub.

```bash
echo "=== Git Push ==="
GIT_PUSHED=false

# Check if we have a git backup repo configured
BACKUP_REPO="$HOME/gt/.dolt-archive/git"

if [ ! -d "$BACKUP_REPO/.git" ]; then
  # Initialize on first run instead of silently no-op'ing forever (gt-kme).
  mkdir -p "$BACKUP_REPO"
  git init -b main "$BACKUP_REPO"
  echo "Initialized backup repo at $BACKUP_REPO"
fi

cd "$BACKUP_REPO"

# Copy latest JSONL files
for DB in "${PROD_DBS[@]}"; do
  LATEST="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"
  if [ -f "$LATEST" ]; then
    cp "$(readlink "$LATEST" || echo "$LATEST")" "$BACKUP_REPO/${DB}.jsonl"
  fi
done

# Stage BEFORE checking for changes — `git diff` (unstaged) never sees
# untracked files, so checking it first missed every brand-new *.jsonl file,
# including on the very first run when everything is untracked (gt-kme).
git add *.jsonl

if git diff --staged --quiet; then
  echo "No changes to commit"
else
  git commit -m "Archive snapshot $(date +%Y-%m-%d-%H%M)" \
    --author="Gas Town Archive <archive@gastown.local>" 2>/dev/null

  # Check if remote exists before pushing
  if git remote get-url origin > /dev/null 2>&1; then
    if git push origin main 2>/dev/null; then
      GIT_PUSHED=true
      echo "Pushed to GitHub"
    else
      echo "WARN: Git push to remote failed (check GitHub credentials/permissions)"
      # This is the offsite layer — a failed push here is a critical
      # precondition gap, not a benign skip. Escalate (see Record Result).
    fi
  else
    echo "WARN: No git remote configured for backup repo"
    echo "  To set up: cd $BACKUP_REPO && git remote add origin <github-url>"
    # Same: no remote means nothing is actually offsite. Escalate.
  fi
fi
```

## Step 3: Verify the git backup has data

Verify that the JSONL snapshots reached the git backup repo's remote and are
readable. This covers JSONL only: no Dolt data leaves this machine through
this plugin (ADR 0002).

```bash
echo "=== Verification ==="
VERIFY_PASSED=0
VERIFY_FAILED=0

# Verify JSONL in git backup
if [ -d "$BACKUP_REPO/.git" ]; then
  echo "Verifying git remote..."
  if cd "$BACKUP_REPO" && git ls-remote origin HEAD > /dev/null 2>&1; then
    # Try to clone into temp directory to verify
    TEMP_CLONE=$(mktemp -d)
    if git clone --depth 1 origin "$TEMP_CLONE" 2>/dev/null; then
      for DB in "${PROD_DBS[@]}"; do
        if [ -f "$TEMP_CLONE/${DB}.jsonl" ]; then
          REMOTE_COUNT=$(wc -l < "$TEMP_CLONE/${DB}.jsonl" | tr -d ' ')
          echo "  git: $DB verified ($REMOTE_COUNT lines in remote)"
          VERIFY_PASSED=$((VERIFY_PASSED + 1))
        else
          echo "  git: $DB MISSING from remote"
          VERIFY_FAILED=$((VERIFY_FAILED + 1))
        fi
      done
    else
      echo "  git: Clone verification failed"
      VERIFY_FAILED=$((VERIFY_FAILED + 1))
    fi
    rm -rf "$TEMP_CLONE"
  else
    echo "  git: Remote not accessible"
  fi
fi

echo "Verified: $VERIFY_PASSED, failed: $VERIFY_FAILED"
```

## Record Result

```bash
SUMMARY="Archive: jsonl=$EXPORTED/$((EXPORTED + EXPORT_FAILED)), git=${GIT_PUSHED}, verify=$VERIFY_PASSED/$((VERIFY_PASSED + VERIFY_FAILED))"
echo "=== $SUMMARY ==="

RESULT="success"
if [ "$EXPORT_FAILED" -gt 0 ] || [ "$VERIFY_FAILED" -gt 0 ]; then
  RESULT="warning"
fi

gt plugin record-run --plugin dolt-archive --result "$RESULT" \
  --title "$SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true

if [ "$EXPORT_FAILED" -gt 0 ]; then
  gt escalate "JSONL export failed for $EXPORT_FAILED databases" \
    --severity critical \
    --reason "JSONL is our last-resort recovery layer. $EXPORT_FAILED databases failed to export."
fi
```
