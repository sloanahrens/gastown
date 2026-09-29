package doctor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// writeRigsFixture registers rigs in townRoot's mayor/rigs.json and creates
// their directories.
func writeRigsFixture(t *testing.T, townRoot string, rigs ...string) {
	t.Helper()
	entries := map[string]config.RigEntry{}
	for _, r := range rigs {
		entries[r] = config.RigEntry{}
		if err := os.MkdirAll(filepath.Join(townRoot, r), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(config.RigsConfig{Rigs: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWispGCCheckCountsAbandonedWisps: the check used to unmarshal `bd mol
// wisp list --json` as a bare array, but bd 1.x prints an object, so it
// counted zero abandoned wisps on every run (reproduced against bd 1.2.2's
// output through a PATH stub before the fix; beads.MolWispList now reads
// it, TestMolWispListReadsBdObject). The fake's wisps are stamped at its
// 2026-01-02 epoch, long past the 1h threshold.
func TestWispGCCheckCountsAbandonedWisps(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "rig1", "rig2")
	bd := newFakeBD()
	db := bd.db(filepath.Join(town, "rig1"))
	for _, title := range []string{"old a", "old b", "closed"} {
		w, err := db.Create(beads.CreateOptions{Title: title, Priority: -1, Ephemeral: true})
		if err != nil {
			t.Fatal(err)
		}
		if title == "closed" {
			if err := db.Close(w.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Create(beads.CreateOptions{Title: "durable", Priority: -1}); err != nil {
		t.Fatal(err)
	}

	c := NewWispGCCheck()
	ctx := bd.ctx(town)
	r := c.Run(ctx)
	if r.Status != StatusWarning || !strings.Contains(r.Message, "2 abandoned wisp(s)") {
		t.Fatalf("Run = %v %q %q, want 2 abandoned wisps in rig1", r.Status, r.Message, r.Details)
	}
	if err := c.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if n := db.GCCount(); n != 1 {
		t.Errorf("rig1 wisp gc ran %d times, want 1", n)
	}
	if n := bd.db(filepath.Join(town, "rig2")).GCCount(); n != 0 {
		t.Errorf("rig2 (nothing abandoned) wisp gc ran %d times", n)
	}
}

func TestWispGCCheckFixReportsGCFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "rig1")
	bd := newFakeBD()
	db := bd.db(filepath.Join(town, "rig1"))
	if _, err := db.Create(beads.CreateOptions{Title: "old", Priority: -1, Ephemeral: true}); err != nil {
		t.Fatal(err)
	}
	db.FailWith("mol wisp gc", errors.New("dolt unreachable"))
	c := NewWispGCCheck()
	ctx := bd.ctx(town)
	c.Run(ctx)
	if err := c.Fix(ctx); err == nil || !strings.Contains(err.Error(), "rig1") || !strings.Contains(err.Error(), "dolt unreachable") {
		t.Errorf("Fix = %v, want rig1's gc failure", err)
	}
}

func TestWispGCCheckListFailureCountsNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "rig1")
	bd := newFakeBD()
	bd.db(filepath.Join(town, "rig1")).FailWith("mol wisp list", errors.New("no wisps table"))
	if r := NewWispGCCheck().Run(bd.ctx(town)); r.Status != StatusOK {
		t.Errorf("Run = %v %q, want OK when the wisp list fails", r.Status, r.Message)
	}
}
