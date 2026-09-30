package doltserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// PurgeClosedEphemerals runs "bd purge" for a specific rig database to remove
// closed ephemeral beads (wisps, convoys). gt maintain calls it in its reap
// phase. Returns the number of beads purged and any error encountered.
// Errors are non-fatal — the caller should log them and continue.
// Must be called while the Dolt server is still running (bd purge needs SQL access).
func (h *host) PurgeClosedEphemerals(townRoot, dbName string, dryRun bool) (int, error) {
	// Resolve the beads directory for this rig (read-only — never create dirs during purge)
	beadsDir := FindRigBeadsDir(townRoot, dbName)

	// Check that the beads directory actually exists on disk.
	// FindRigBeadsDir returns a path even for non-existent directories,
	// so we must verify existence explicitly.
	if _, err := os.Stat(beadsDir); err != nil {
		if os.IsNotExist(err) {
			return 0, nil // no beads dir — nothing to purge
		}
		return 0, fmt.Errorf("checking beads dir for %s: %w", dbName, err)
	}

	// Skip databases with uninitialized beads dirs (no metadata.json).
	// An empty .beads/ directory causes bd to attempt a fresh bootstrap,
	// which hangs waiting on dolt init or lock acquisition.
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if info, err := os.Stat(metadataPath); err != nil {
		if os.IsNotExist(err) {
			return 0, nil // not initialized — nothing to purge
		}
		return 0, fmt.Errorf("checking metadata for %s: %w", dbName, err)
	} else if info.IsDir() {
		return 0, fmt.Errorf("metadata.json for %s is a directory", dbName)
	}

	// Build bd purge command with safety-net timeout.
	// bd purge v2 uses batched SQL (completes in seconds), but we keep a
	// generous timeout as a circuit breaker against future regressions.
	env := beads.BuildMutationPinnedBDEnv(h.environ(), beadsDir)
	// Probe --allow-stale support with the same hardened target env used by purge.
	args := h.allowStaleArgs(env, []string{"purge", "--json"})
	if dryRun {
		args = append(args, "--dry-run")
	} else {
		// bd purge previews by default; --force is required to actually delete.
		args = append(args, "--force")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := beads.CommandContextWithEnv(ctx, filepath.Dir(beadsDir), env, args...) // run from parent of .beads
	setProcessGroup(cmd.Cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := h.runBD(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		return 0, fmt.Errorf("bd purge for %s: timed out after 60s", dbName)
	}
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = strings.TrimSpace(stdout.String())
		}
		return 0, fmt.Errorf("bd purge for %s: %w (%s)", dbName, err, errMsg)
	}

	// Parse JSON output (from stdout only) to get purged count.
	// bd may emit non-JSON warning lines before the JSON object,
	// so extract the first JSON object from stdout.
	jsonBytes := extractJSON(stdout.Bytes())
	var result struct {
		PurgedCount *int `json:"purged_count"`
	}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return 0, fmt.Errorf("bd purge for %s: unexpected output format: %s", dbName, strings.TrimSpace(stdout.String()))
	}

	// Warn if purged_count field was missing from the JSON response — may indicate
	// a schema mismatch (e.g., field renamed). An explicit 0 is a valid success case.
	if result.PurgedCount == nil {
		fmt.Fprintf(os.Stderr, "Warning: bd purge for %s: purged_count field missing (raw: %s)\n", dbName, strings.TrimSpace(stdout.String()))
		return 0, nil
	}

	return *result.PurgedCount, nil
}

// PurgeClosedEphemerals is (*host).PurgeClosedEphemerals on the real machine.
func PurgeClosedEphemerals(townRoot, dbName string, dryRun bool) (int, error) {
	return std.PurgeClosedEphemerals(townRoot, dbName, dryRun)
}

// extractJSON finds the first JSON object in raw output that may contain
// non-JSON preamble (warnings, debug lines). Returns data from the first '{' onward,
// letting json.Unmarshal handle end-detection (it stops at the end of the first valid
// JSON value and tolerates trailing content).
func extractJSON(data []byte) []byte {
	start := bytes.IndexByte(data, '{')
	if start < 0 {
		return data
	}
	return data[start:]
}
