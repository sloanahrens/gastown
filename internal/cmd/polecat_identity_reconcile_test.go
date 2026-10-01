package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// reconcileTown is a town with a "gastown" rig routed via routes.jsonl, and
// gt-gastown-polecat-garnet in both its databases. Each database is a
// reconcileDB; every write to either is logged in order. The reroute hazard
// of bd's per-ID show and delete (gt-1361) is the beads methods' to refuse,
// pinned by TestGetAgentBeadInStoreOnly_IgnoresReroutedShowResult and
// TestDeleteLegacyAgentBead_RefusesRatherThanRerouteDelete.
type reconcileTown struct {
	townRoot, rigBeads, townBeads string
	rig, town                     *reconcileDB

	mu  sync.Mutex
	log []string // "<rig|town> update" or "<rig|town> delete"
}

// reconcileDB is one database: the agent row, or nil when it has none.
type reconcileDB struct {
	w    *reconcileTown
	name string
	row  *beads.Issue
}

func (d *reconcileDB) GetAgentBeadInStoreOnly(id string) (*beads.Issue, *beads.AgentFields, error) {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	if d.row == nil || d.row.ID != id {
		return nil, nil, nil
	}
	row := *d.row
	return &row, beads.ParseAgentFields(row.Description), nil
}

func (d *reconcileDB) UpdateAgentDescriptionFields(id string, _ beads.AgentFieldUpdates) error {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	d.w.log = append(d.w.log, d.name+" update")
	return nil
}

func (d *reconcileDB) DeleteLegacyAgentBead(id string) error {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	d.w.log = append(d.w.log, d.name+" delete")
	if d.row == nil || d.row.ID != id {
		return errors.New("refusing to delete " + id + ": not present in this database")
	}
	d.row = nil
	return nil
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

	// Distinct, non-empty timestamps on rig vs town: production's identity
	// guard refuses to reconcile when both reads return matching
	// created_at/updated_at, so fixtures must never leave both blank. The
	// rig row is the NEWER of the two (a live row kept current by a real
	// polecat), matching how the recency rule is meant to be exercised by
	// these fixtures.
	w := &reconcileTown{townRoot: townRoot, rigBeads: rigBeadsDir, townBeads: townBeadsDir}
	w.rig = &reconcileDB{w: w, name: "rig", row: agentBeadRow(garnetID, rigDesc, "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")}
	w.town = &reconcileDB{w: w, name: "town"}
	if townDesc != "" {
		w.town.row = agentBeadRow(garnetID, townDesc, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	}
	return w
}

// stores opens the rig database for the rig directory, the town's for any
// other; only gt-wisp-0yhh is not a bead.
func (w *reconcileTown) stores() reconcileStores {
	return reconcileStores{
		local: func(dir string) reconcileStore {
			if dir == filepath.Dir(w.rigBeads) {
				return w.rig
			}
			return w.town
		},
		exists: func(_, ref string) bool { return ref != "gt-wisp-0yhh" },
	}
}

// logText is the write log, one line per write.
func (w *reconcileTown) logText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.log, "\n")
}

// agentBeadRow is one agent bead as bd list returns it.
func agentBeadRow(id, desc, createdAt, updatedAt string) *beads.Issue {
	return &beads.Issue{
		ID: id, Title: "polecat garnet", Type: "task", Status: "open", Labels: []string{"gt:agent"},
		CreatedAt: createdAt, UpdatedAt: updatedAt,
		Description: "polecat garnet\n\nrole_type: polecat\nrig: gastown\n" + desc,
	}
}

const garnetID = "gt-gastown-polecat-garnet"

func TestReconcile_DryRunPrintsTableAndWritesNothing(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n", // rig row
		"agent_state: done\nactive_mr: null\n")         // town row
	var out bytes.Buffer
	err := runReconcile(&out, w.stores(), w.townRoot, garnetID, false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "active_mr") || !strings.Contains(out.String(), "clear") {
		t.Fatalf("table must show active_mr with winner clear:\n%s", out.String())
	}
	if log := w.logText(); log != "" {
		t.Fatalf("dry-run must not write; log:\n%s", log)
	}
}

func TestReconcile_ApplyUpdatesRigThenArchivesThenDeletesTown(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: done\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, w.stores(), w.townRoot, garnetID, true, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if log := w.logText(); log != "rig update\ntown delete" {
		t.Fatalf("expected rig update BEFORE town delete; log:\n%s", log)
	}
	archive, err := os.ReadFile(filepath.Join(w.townRoot, ".beads", "archive", "agent-bead-legacy.jsonl"))
	if err != nil || !strings.Contains(string(archive), `"gt-gastown-polecat-garnet"`) {
		t.Fatalf("archive must contain the town row before delete: %v / %s", err, archive)
	}
}

func TestReconcile_RefusesWhenNoTownRow(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t, "agent_state: done\n", "") // empty => town row absent
	err := runReconcile(io.Discard, w.stores(), w.townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if log := w.logText(); log != "" {
		t.Fatalf("a refusal must not write; log:\n%s", log)
	}
}

func TestReconcile_DeleteOnlySkipsMergeButStillArchivesAndDeletes(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: stuck\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, w.stores(), w.townRoot, garnetID, true, true); err != nil {
		t.Fatalf("apply --delete-only: %v", err)
	}
	if !strings.Contains(out.String(), "delete-only") {
		t.Fatalf("expected delete-only notice in output:\n%s", out.String())
	}
	if log := w.logText(); log != "town delete" {
		t.Fatalf("--delete-only must delete the town row and not update the rig row; log:\n%s", log)
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
	if err := runReconcile(&out, w.stores(), w.townRoot, garnetID, true, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out.String(), "nuked incarnation") {
		t.Fatalf("expected nuked-incarnation notice in output:\n%s", out.String())
	}
	if log := w.logText(); log != "town delete" {
		t.Fatalf("nuked town row must be deleted without updating the rig row; log:\n%s", log)
	}
}

// TestReconcile_TownRowAlreadyGoneNeverDeletesRig reproduces the exact live
// incident (gt-1361): an earlier partial run already deleted the town row,
// leaving only the rig row. Reconcile must refuse and write nothing.
func TestReconcile_TownRowAlreadyGoneNeverDeletesRig(t *testing.T) {
	t.Parallel()
	w := setupReconcileTown(t,
		"agent_state: done\n",
		"agent_state: done\n") // identical content to the rig row, as a real dual-written copy would be
	// Simulate "already deleted by an earlier run".
	w.town.row = nil

	err := runReconcile(io.Discard, w.stores(), w.townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if log := w.logText(); log != "" {
		t.Fatalf("must not attempt any write once the town row is already absent; log:\n%s", log)
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
	w.rig.row.UpdatedAt = w.town.row.UpdatedAt

	err := runReconcile(io.Discard, w.stores(), w.townRoot, garnetID, true, false)
	if err == nil || !strings.Contains(err.Error(), "identical created_at/updated_at") {
		t.Fatalf("expected identity-collision refusal, got %v", err)
	}
	if log := w.logText(); log != "" {
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
