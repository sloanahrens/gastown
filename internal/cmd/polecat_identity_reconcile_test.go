package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// setupReconcileTown builds a town with a "gastown" rig routed via
// routes.jsonl and a fake `bd` on PATH backing gt-gastown-polecat-garnet.
//
// `list --id` is answered STRICTLY from whichever database BEADS_DIR points
// at (never rerouted) — this is what GetAgentBeadInStoreOnly relies on, and
// matches real bd's list semantics. `show`/`delete`, by contrast, SIMULATE
// bd's real routes.jsonl fallback: once an ID is gone from the database
// BEADS_DIR points at, they silently answer from the OTHER database instead
// of reporting not-found (this is the gt-1361 hazard). Production code must
// never rely on `show`/`delete` behaving safely here — these tests exist to
// catch a regression that reintroduces that reliance.
// rigDesc/townDesc are appended to the standard agent preamble; an empty
// townDesc simulates "no legacy town row".
func setupReconcileTown(t *testing.T, rigDesc, townDesc string) (townRoot, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}

	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	rigBeadsDir := filepath.Join(rigDir, ".beads")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), townBeadsDir, rigBeadsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{{Prefix: "gt-", Path: "gastown/mayor/rig"}}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	for _, dir := range []string{townBeadsDir, rigBeadsDir} {
		if err := os.WriteFile(filepath.Join(dir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
			t.Fatalf("write types sentinel: %v", err)
		}
	}

	const id = "gt-gastown-polecat-garnet"
	// Distinct, non-empty timestamps on rig vs town: production's identity
	// guard refuses to reconcile when both reads return matching
	// created_at/updated_at, so fixtures must never leave both blank. The
	// rig row is the NEWER of the two (a live row kept current by a real
	// polecat), matching how the recency rule is meant to be exercised by
	// these fixtures.
	rigJSON := agentBeadJSON(t, id, rigDesc, "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
	var townJSON string
	if townDesc != "" {
		townJSON = agentBeadJSON(t, id, townDesc, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	}

	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "bd.log")
	deletedMarker := filepath.Join(binDir, "town-deleted")
	script := fmt.Sprintf(`#!/bin/sh
LOG=%q
RIG_DIR=%q
TOWN_DIR=%q
DELETED_MARKER=%q
RIG_JSON='%s'
TOWN_JSON='%s'
printf 'beads_dir=%%s args=%%s\n' "${BEADS_DIR:-<unset>}" "$*" >> "$LOG"
cmd=""
id=""
for arg in "$@"; do
  case "$arg" in
    --*) continue ;;
  esac
  if [ -z "$cmd" ]; then
    cmd="$arg"
  elif [ -z "$id" ]; then
    id="$arg"
    break
  fi
done
case "$cmd" in
  version|update|reopen)
    exit 0
    ;;
  delete)
    # SIMULATES bd's real routes.jsonl fallback for a single-ID mutation:
    # if this database's own copy of $id is already gone, silently delete
    # from the OTHER database instead of failing not-found. Production must
    # never reach here for an ID absent from BEADS_DIR's own database
    # (DeleteLegacyAgentBead refuses first) — if it does, this is the exact
    # mechanism that deleted a live rig row for real (gt-1361).
    if [ "${BEADS_DIR:-}" = "$TOWN_DIR" ]; then
      if [ -n "$TOWN_JSON" ] && [ ! -f "$DELETED_MARKER" ]; then
        : > "$DELETED_MARKER"
      else
        : > "$RIG_DIR/.rerouted-delete-hit-rig"
      fi
    elif [ "${BEADS_DIR:-}" = "$RIG_DIR" ]; then
      : > "$RIG_DIR/.rerouted-delete-hit-rig"
    fi
    exit 0
    ;;
  show)
    # SIMULATES bd's real routes.jsonl fallback for a single-ID read: once
    # gone from BEADS_DIR's own database, falls back to the OTHER database
    # instead of reporting not-found. Production must not rely on this
    # being safe (GetAgentBeadInStoreOnly uses 'list', never 'show', for
    # exactly this reason).
    if [ "$id" = "gt-wisp-0yhh" ]; then
      echo 'not found' >&2
      exit 1
    fi
    if [ "${BEADS_DIR:-}" = "$TOWN_DIR" ] && [ -n "$TOWN_JSON" ] && [ ! -f "$DELETED_MARKER" ]; then
      printf '%%s\n' "$TOWN_JSON"
      exit 0
    fi
    if [ "${BEADS_DIR:-}" = "$RIG_DIR" ] || [ "${BEADS_DIR:-}" = "$TOWN_DIR" ]; then
      printf '%%s\n' "$RIG_JSON"
      exit 0
    fi
    echo 'not found' >&2
    exit 1
    ;;
  list)
    # Store-pinned, never rerouted: only ever answers from BEADS_DIR's own
    # database, matching real bd list semantics.
    if [ "${BEADS_DIR:-}" = "$RIG_DIR" ]; then
      printf '%%s\n' "$RIG_JSON"
      exit 0
    fi
    if [ "${BEADS_DIR:-}" = "$TOWN_DIR" ]; then
      if [ -n "$TOWN_JSON" ] && [ ! -f "$DELETED_MARKER" ]; then
        printf '%%s\n' "$TOWN_JSON"
        exit 0
      fi
    fi
    printf '%%s\n' '[]'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, logPath, rigBeadsDir, townBeadsDir, deletedMarker, rigJSON, townJSON)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return townRoot, logPath
}

// agentBeadJSON builds a single-line `bd list --json` array response for one
// agent bead, as a string safe to embed inside a single-quoted shell literal
// (bd's JSON output never contains a literal single quote).
func agentBeadJSON(t *testing.T, id, desc, createdAt, updatedAt string) string {
	t.Helper()
	issue := map[string]any{
		"id":          id,
		"title":       "polecat garnet",
		"issue_type":  "task",
		"status":      "open",
		"labels":      []string{"gt:agent"},
		"created_at":  createdAt,
		"updated_at":  updatedAt,
		"description": "polecat garnet\n\nrole_type: polecat\nrig: gastown\n" + desc,
	}
	b, err := json.Marshal([]any{issue})
	if err != nil {
		t.Fatalf("marshal fixture issue: %v", err)
	}
	if strings.Contains(string(b), "'") {
		t.Fatalf("fixture JSON must not contain a single quote: %s", b)
	}
	return string(b)
}

const garnetID = "gt-gastown-polecat-garnet"

func TestReconcile_DryRunPrintsTableAndWritesNothing(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n", // rig row
		"agent_state: done\nactive_mr: null\n")         // town row
	var out bytes.Buffer
	err := runReconcile(&out, townRoot, garnetID, false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "active_mr") || !strings.Contains(out.String(), "clear") {
		t.Fatalf("table must show active_mr with winner clear:\n%s", out.String())
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "args=update") || strings.Contains(string(log), "args=delete") {
		t.Fatalf("dry-run must not write; log:\n%s", log)
	}
}

func TestReconcile_ApplyUpdatesRigThenArchivesThenDeletesTown(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: done\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, townRoot, garnetID, true, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	townBeads := filepath.Join(townRoot, ".beads")
	upd := strings.Index(string(log), "beads_dir="+rigBeads+" args=update")
	del := strings.Index(string(log), "beads_dir="+townBeads+" args=delete")
	if upd == -1 || del == -1 || upd > del {
		t.Fatalf("expected rig update BEFORE town delete; log:\n%s", log)
	}
	archive, err := os.ReadFile(filepath.Join(townRoot, ".beads", "archive", "agent-bead-legacy.jsonl"))
	if err != nil || !strings.Contains(string(archive), `"gt-gastown-polecat-garnet"`) {
		t.Fatalf("archive must contain the town row before delete: %v / %s", err, archive)
	}
	if _, err := os.Stat(filepath.Join(rigBeads, ".rerouted-delete-hit-rig")); err == nil {
		t.Fatalf("reroute canary tripped: a delete reached the rig database")
	}
}

func TestReconcile_RefusesWhenNoTownRow(t *testing.T) {
	townRoot, _ := setupReconcileTown(t, "agent_state: done\n", "") // empty => town row absent
	err := runReconcile(io.Discard, townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
	// gt-1361: with the town row already absent, a `show`/`delete`-based
	// implementation would reroute via routes.jsonl and silently touch the
	// rig row. Confirm the fixed (list-based) implementation never does.
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	if _, err := os.Stat(filepath.Join(rigBeads, ".rerouted-delete-hit-rig")); err == nil {
		t.Fatalf("reroute canary tripped: a delete reached the rig database")
	}
}

func TestReconcile_DeleteOnlySkipsMergeButStillArchivesAndDeletes(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: stuck\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, townRoot, garnetID, true, true); err != nil {
		t.Fatalf("apply --delete-only: %v", err)
	}
	if !strings.Contains(out.String(), "delete-only") {
		t.Fatalf("expected delete-only notice in output:\n%s", out.String())
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "args=update") {
		t.Fatalf("--delete-only must not update the rig row; log:\n%s", log)
	}
	if !strings.Contains(string(log), "args=delete") {
		t.Fatalf("--delete-only must still delete the town row; log:\n%s", log)
	}
	archive, err := os.ReadFile(filepath.Join(townRoot, ".beads", "archive", "agent-bead-legacy.jsonl"))
	if err != nil || !strings.Contains(string(archive), `"gt-gastown-polecat-garnet"`) {
		t.Fatalf("archive must contain the town row before delete: %v / %s", err, archive)
	}
}

func TestReconcile_NukedTownRowAutoForcesDeleteOnly(t *testing.T) {
	// Town row is a dead nuked incarnation with a stale agent_state that,
	// under the normal recency/severity rules, would otherwise overwrite
	// the live rig row's fields (gt-1361).
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\n",
		"agent_state: nuked\n")
	var out bytes.Buffer
	if err := runReconcile(&out, townRoot, garnetID, true, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out.String(), "nuked incarnation") {
		t.Fatalf("expected nuked-incarnation notice in output:\n%s", out.String())
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "args=update") {
		t.Fatalf("nuked town row must not update the rig row; log:\n%s", log)
	}
	if !strings.Contains(string(log), "args=delete") {
		t.Fatalf("nuked town row must still be archived and deleted; log:\n%s", log)
	}
}

// TestReconcile_TownRowAlreadyGoneNeverDeletesRig reproduces the exact live
// incident (gt-1361): an earlier partial run already deleted the town row
// (DELETED_MARKER pre-set), leaving only the rig row. A `show`/`delete`-based
// re-run would reroute via routes.jsonl, read the rig row AS the "town" row,
// see identical content, and delete the rig row. The fixed implementation
// must refuse (list-based reads correctly see the town row as absent) and
// must never touch the rig database.
func TestReconcile_TownRowAlreadyGoneNeverDeletesRig(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\n",
		"agent_state: done\n") // identical content to the rig row, as a real dual-written copy would be
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	townBeads := filepath.Join(townRoot, ".beads")
	// Simulate "already deleted by an earlier run": pre-create the marker
	// the mock's own `delete` handler would have written.
	deletedMarker := filepath.Join(filepath.Dir(logPath), "town-deleted")
	if err := os.WriteFile(deletedMarker, nil, 0644); err != nil {
		t.Fatalf("pre-seed deleted marker: %v", err)
	}

	err := runReconcile(io.Discard, townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "beads_dir="+rigBeads+" args=delete") || strings.Contains(string(log), "beads_dir="+townBeads+" args=delete") {
		t.Fatalf("must not attempt any delete once the town row is already absent; log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(rigBeads, ".rerouted-delete-hit-rig")); err == nil {
		t.Fatalf("reroute canary tripped: a delete reached the rig database")
	}
}

// TestReconcile_RefusesWhenReadsResolveToSameRow is the belt-and-suspenders
// check: if the "town" and "rig" reads ever return identical created_at AND
// updated_at (which two genuinely distinct rows never do), refuse outright
// rather than merge or delete anything.
func TestReconcile_RefusesWhenReadsResolveToSameRow(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t, "agent_state: done\n", "agent_state: done\n")
	// Overwrite the fixture with a mock that returns the SAME timestamps for
	// both rig and town list reads, simulating a resolution collision.
	binDir := filepath.Dir(logPath)
	script, err := os.ReadFile(filepath.Join(binDir, "bd"))
	if err != nil {
		t.Fatalf("read mock: %v", err)
	}
	// The rig fixture's updated_at (2026-01-02) is the only field that
	// differs from town's; collapse it to match so both reads carry
	// identical created_at AND updated_at, exercising the identity guard.
	patched := strings.ReplaceAll(string(script), "2026-01-02T00:00:00Z", "2026-01-01T00:00:00Z")
	if patched == string(script) {
		t.Fatalf("patch did not change the mock script")
	}
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(patched), 0755); err != nil {
		t.Fatalf("rewrite mock: %v", err)
	}

	err = runReconcile(io.Discard, townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "identical created_at/updated_at") {
		t.Fatalf("expected identity-collision refusal, got %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "args=delete") || strings.Contains(string(log), "args=update") {
		t.Fatalf("must not write anything on an identity-collision refusal; log:\n%s", log)
	}
}

func TestResolveReconcileID(t *testing.T) {
	townRoot := t.TempDir()
	cases := []struct {
		name    string
		rawID   string
		arg     string
		want    string
		wantErr string
	}{
		{name: "polecat default", arg: "gastown/garnet", want: "gt-gastown-polecat-garnet"},
		{name: "witness", arg: "gastown/witness", want: "gt-gastown-witness"},
		{name: "refinery", arg: "gastown/refinery", want: "gt-gastown-refinery"},
		{name: "crew", arg: "gastown/crew/sloan", want: "gt-gastown-crew-sloan"},
		{name: "raw id override", rawID: "gt-gastown-witness", want: "gt-gastown-witness"},
		{name: "id and arg mutually exclusive", rawID: "gt-gastown-witness", arg: "gastown/witness", wantErr: "mutually exclusive"},
		{name: "missing both", wantErr: "expected"},
		{name: "crew without name", arg: "gastown/crew/", wantErr: "expected"},
		{name: "too many segments", arg: "gastown/crew/sloan/extra", wantErr: "expected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveReconcileID(townRoot, tc.rawID, tc.arg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
