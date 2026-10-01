package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// reconcileTown is a town with a "gastown" rig routed via routes.jsonl and an
// in-process bd backing gt-gastown-polecat-garnet in both databases.
//
// `list --id` is answered STRICTLY from whichever database BEADS_DIR points
// at (never rerouted) — this is what GetAgentBeadInStoreOnly relies on, and
// matches real bd's list semantics. `show`/`delete`, by contrast, SIMULATE
// bd's real routes.jsonl fallback: once an ID is gone from the database
// BEADS_DIR points at, they silently answer from the OTHER database instead
// of reporting not-found (this is the gt-1361 hazard). Production code must
// never rely on `show`/`delete` behaving safely here — these tests exist to
// catch a regression that reintroduces that reliance.
type reconcileTown struct {
	townRoot, rigBeads, townBeads string
	rigJSON, townJSON             string

	mu            sync.Mutex
	log           []string // "beads_dir=<BEADS_DIR> args=<argv>"
	townDeleted   bool
	reroutedToRig bool // a delete reached the rig database
}

// setupReconcileTown builds the town. rigDesc/townDesc are appended to the
// standard agent preamble; an empty townDesc simulates "no legacy town row".
func setupReconcileTown(t *testing.T, rigDesc, townDesc string) *reconcileTown {
	t.Helper()
	townRoot := t.TempDir()
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
	w := &reconcileTown{townRoot: townRoot, rigBeads: rigBeadsDir, townBeads: townBeadsDir}
	w.rigJSON = agentBeadJSON(t, id, rigDesc, "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
	if townDesc != "" {
		w.townJSON = agentBeadJSON(t, id, townDesc, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	}
	return w
}

func (w *reconcileTown) run(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	beadsDir := envSlice(c.Env)["BEADS_DIR"]
	args := c.Args
	for len(args) > 0 && args[0] == "--allow-stale" {
		args = args[1:]
	}
	w.log = append(w.log, "beads_dir="+beadsDir+" args="+strings.Join(args, " "))
	words := nonFlagArgs(args)
	if len(words) == 0 {
		return nil, nil, nil
	}
	cmd, id := words[0], ""
	if len(words) > 1 {
		id = words[1]
	}
	townRow := w.townJSON != "" && !w.townDeleted
	switch cmd {
	case "delete":
		// SIMULATES bd's real routes.jsonl fallback for a single-ID mutation:
		// if this database's own copy of id is already gone, silently delete
		// from the OTHER database instead of failing not-found. Production
		// must never reach here for an ID absent from BEADS_DIR's own
		// database (DeleteLegacyAgentBead refuses first) — if it does, this
		// is the exact mechanism that deleted a live rig row for real
		// (gt-1361).
		switch {
		case beadsDir == w.townBeads && townRow:
			w.townDeleted = true
		case beadsDir == w.townBeads || beadsDir == w.rigBeads:
			w.reroutedToRig = true
		}
		return nil, nil, nil
	case "show":
		// SIMULATES bd's real routes.jsonl fallback for a single-ID read:
		// once gone from BEADS_DIR's own database, falls back to the OTHER
		// database instead of reporting not-found.
		switch {
		case id == "gt-wisp-0yhh":
			return nil, []byte("not found"), inprocBDExit(1)
		case beadsDir == w.townBeads && townRow:
			return []byte(w.townJSON), nil, nil
		case beadsDir == w.rigBeads || beadsDir == w.townBeads:
			return []byte(w.rigJSON), nil, nil
		}
		return nil, []byte("not found"), inprocBDExit(1)
	case "list":
		// Store-pinned, never rerouted: only ever answers from BEADS_DIR's
		// own database, matching real bd list semantics.
		switch {
		case beadsDir == w.rigBeads:
			return []byte(w.rigJSON), nil, nil
		case beadsDir == w.townBeads && townRow:
			return []byte(w.townJSON), nil, nil
		}
		return []byte("[]"), nil, nil
	}
	return nil, nil, nil
}

// logText is the call log, one line per bd call.
func (w *reconcileTown) logText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.log, "\n")
}

func (w *reconcileTown) reroutedDelete() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reroutedToRig
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
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n", // rig row
		"agent_state: done\nactive_mr: null\n")         // town row
	var out bytes.Buffer
	err := runReconcile(&out, w.run, w.townRoot, garnetID, false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "active_mr") || !strings.Contains(out.String(), "clear") {
		t.Fatalf("table must show active_mr with winner clear:\n%s", out.String())
	}
	log := w.logText()
	if strings.Contains(log, "args=update") || strings.Contains(log, "args=delete") {
		t.Fatalf("dry-run must not write; log:\n%s", log)
	}
}

func TestReconcile_ApplyUpdatesRigThenArchivesThenDeletesTown(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: done\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, w.run, w.townRoot, garnetID, true, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	log := w.logText()
	rigBeads, townBeads := w.rigBeads, w.townBeads
	upd := strings.Index(log, "beads_dir="+rigBeads+" args=update")
	del := strings.Index(log, "beads_dir="+townBeads+" args=delete")
	if upd == -1 || del == -1 || upd > del {
		t.Fatalf("expected rig update BEFORE town delete; log:\n%s", log)
	}
	archive, err := os.ReadFile(filepath.Join(w.townRoot, ".beads", "archive", "agent-bead-legacy.jsonl"))
	if err != nil || !strings.Contains(string(archive), `"gt-gastown-polecat-garnet"`) {
		t.Fatalf("archive must contain the town row before delete: %v / %s", err, archive)
	}
	if w.reroutedDelete() {
		t.Fatalf("reroute canary tripped: a delete reached the rig database")
	}
}

func TestReconcile_RefusesWhenNoTownRow(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t, "agent_state: done\n", "") // empty => town row absent
	err := runReconcile(io.Discard, w.run, w.townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
	// gt-1361: with the town row already absent, a `show`/`delete`-based
	// implementation would reroute via routes.jsonl and silently touch the
	// rig row. Confirm the fixed (list-based) implementation never does.
	if w.reroutedDelete() {
		t.Fatalf("reroute canary tripped: a delete reached the rig database")
	}
}

func TestReconcile_DeleteOnlySkipsMergeButStillArchivesAndDeletes(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: stuck\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, w.run, w.townRoot, garnetID, true, true); err != nil {
		t.Fatalf("apply --delete-only: %v", err)
	}
	if !strings.Contains(out.String(), "delete-only") {
		t.Fatalf("expected delete-only notice in output:\n%s", out.String())
	}
	log := w.logText()
	if strings.Contains(log, "args=update") {
		t.Fatalf("--delete-only must not update the rig row; log:\n%s", log)
	}
	if !strings.Contains(log, "args=delete") {
		t.Fatalf("--delete-only must still delete the town row; log:\n%s", log)
	}
	archive, err := os.ReadFile(filepath.Join(w.townRoot, ".beads", "archive", "agent-bead-legacy.jsonl"))
	if err != nil || !strings.Contains(string(archive), `"gt-gastown-polecat-garnet"`) {
		t.Fatalf("archive must contain the town row before delete: %v / %s", err, archive)
	}
}

func TestReconcile_NukedTownRowAutoForcesDeleteOnly(t *testing.T) {
	t.Parallel()
	// Town row is a dead nuked incarnation with a stale agent_state that,
	// under the normal recency/severity rules, would otherwise overwrite
	// the live rig row's fields (gt-1361).
	w := setupReconcileTown(t,
		"agent_state: done\n",
		"agent_state: nuked\n")
	var out bytes.Buffer
	if err := runReconcile(&out, w.run, w.townRoot, garnetID, true, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out.String(), "nuked incarnation") {
		t.Fatalf("expected nuked-incarnation notice in output:\n%s", out.String())
	}
	log := w.logText()
	if strings.Contains(log, "args=update") {
		t.Fatalf("nuked town row must not update the rig row; log:\n%s", log)
	}
	if !strings.Contains(log, "args=delete") {
		t.Fatalf("nuked town row must still be archived and deleted; log:\n%s", log)
	}
}

// TestReconcile_TownRowAlreadyGoneNeverDeletesRig reproduces the exact live
// incident (gt-1361): an earlier partial run already deleted the town row
// (townDeleted pre-set), leaving only the rig row. A `show`/`delete`-based
// re-run would reroute via routes.jsonl, read the rig row AS the "town" row,
// see identical content, and delete the rig row. The fixed implementation
// must refuse (list-based reads correctly see the town row as absent) and
// must never touch the rig database.
func TestReconcile_TownRowAlreadyGoneNeverDeletesRig(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\n",
		"agent_state: done\n") // identical content to the rig row, as a real dual-written copy would be
	rigBeads, townBeads := w.rigBeads, w.townBeads
	// Simulate "already deleted by an earlier run".
	w.townDeleted = true

	err := runReconcile(io.Discard, w.run, w.townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
	log := w.logText()
	if strings.Contains(log, "beads_dir="+rigBeads+" args=delete") || strings.Contains(log, "beads_dir="+townBeads+" args=delete") {
		t.Fatalf("must not attempt any delete once the town row is already absent; log:\n%s", log)
	}
	if w.reroutedDelete() {
		t.Fatalf("reroute canary tripped: a delete reached the rig database")
	}
}

// TestReconcile_RefusesWhenReadsResolveToSameRow is the belt-and-suspenders
// check: if the "town" and "rig" reads ever return identical created_at AND
// updated_at (which two genuinely distinct rows never do), refuse outright
// rather than merge or delete anything.
func TestReconcile_RefusesWhenReadsResolveToSameRow(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t, "agent_state: done\n", "agent_state: done\n")
	// The rig fixture's updated_at (2026-01-02) is the only field that
	// differs from town's; collapse it to match so both reads carry
	// identical created_at AND updated_at, exercising the identity guard.
	w.rigJSON = strings.ReplaceAll(w.rigJSON, "2026-01-02T00:00:00Z", "2026-01-01T00:00:00Z")

	err := runReconcile(io.Discard, w.run, w.townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "identical created_at/updated_at") {
		t.Fatalf("expected identity-collision refusal, got %v", err)
	}
	log := w.logText()
	if strings.Contains(log, "args=delete") || strings.Contains(log, "args=update") {
		t.Fatalf("must not write anything on an identity-collision refusal; log:\n%s", log)
	}
}

func TestResolveReconcileID(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	cases := []struct {
		name    string
		rawID   string
		arg     string
		want    string
		wantErr string
	}{
		{name: "polecat default", arg: "gastown/garnet", want: "gt-gastown-polecat-garnet"},
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
