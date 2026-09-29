package doctor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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

// wispRig gives rig a fake database on clk with wisps: two open ones and a
// closed one, a durable issue, and an in-progress wisp.
func wispRig(t *testing.T, bd *fakeBD, town, rig string, clk clockwork.Clock) *beadsfake.Fake {
	t.Helper()
	db := beadsfake.New(beadsfake.WithClock(clk))
	bd.put(filepath.Join(town, rig), db)
	for _, title := range []string{"old a", "old b", "closed", "working"} {
		w, err := db.Create(beads.CreateOptions{Title: title, Priority: -1, Ephemeral: true})
		if err != nil {
			t.Fatal(err)
		}
		switch title {
		case "closed":
			err = db.Close(w.ID)
		case "working":
			err = db.Update(w.ID, beads.UpdateOptions{Status: ptrTo("in_progress")})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Create(beads.CreateOptions{Title: "durable", Priority: -1}); err != nil {
		t.Fatal(err)
	}
	return db
}

func ptrTo[T any](v T) *T { return &v }

// TestWispGCCheckCountsGCCandidates: the count is bd gc's own dry-run
// candidates, so the warning names exactly what gc would delete: the two
// idle open wisps, not the closed or in-progress ones.
func TestWispGCCheckCountsGCCandidates(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "rig1", "rig2")
	clk := clockwork.NewFakeClockAt(beadsfake.Epoch)
	bd := newFakeBD()
	wispRig(t, bd, town, "rig1", clk)
	clk.Advance(2 * time.Hour)

	r := NewWispGCCheck().Run(bd.ctx(town))
	if r.Status != StatusWarning || !strings.Contains(r.Message, "2 abandoned wisp(s)") ||
		len(r.Details) != 1 || !strings.HasPrefix(r.Details[0], "rig1: 2 ") {
		t.Fatalf("Run = %v %q %q, want 2 abandoned wisps in rig1", r.Status, r.Message, r.Details)
	}
}

func TestWispGCCheckFreshWispsAreOK(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "rig1")
	clk := clockwork.NewFakeClockAt(beadsfake.Epoch)
	bd := newFakeBD()
	wispRig(t, bd, town, "rig1", clk)
	clk.Advance(30 * time.Minute)
	if r := NewWispGCCheck().Run(bd.ctx(town)); r.Status != StatusOK {
		t.Errorf("Run = %v %q, want OK for wisps idle 30m", r.Status, r.Message)
	}
}

// TestWispGCCheckFixDeletesNothing is the C1 guard: gt doctor --fix (which
// the mol-session-gc formula runs) must not collect wisps, because age gc
// deletes open merge-request wisps too. The check is not fixable, a doctor
// fix leaves every wisp in place, and doctor's bd surface (bdCLI) has no
// method that runs gc at all.
func TestWispGCCheckFixDeletesNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "rig1")
	clk := clockwork.NewFakeClockAt(beadsfake.Epoch)
	bd := newFakeBD()
	db := wispRig(t, bd, town, "rig1", clk)
	clk.Advance(2 * time.Hour)
	before, err := db.MolWispList()
	if err != nil {
		t.Fatal(err)
	}

	check := NewWispGCCheck()
	if check.CanFix() {
		t.Fatal("wisp-gc is fixable; doctor --fix would run it")
	}
	d := NewDoctor()
	d.Register(check)
	report := d.Fix(bd.ctx(town))
	for _, r := range report.Checks {
		if r.Fixed {
			t.Errorf("%s reported fixed", r.Name)
		}
	}
	after, err := db.MolWispList()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || len(after) != 3 {
		t.Errorf("wisps after doctor --fix = %d, before %d; want all 3 open or in-progress wisps kept", len(after), len(before))
	}
	if cands, _ := db.WispGCCandidates(time.Hour); len(cands) != 2 {
		t.Errorf("gc candidates after doctor --fix = %v, want both still there", cands)
	}
}

func TestWispGCCheckListFailureCountsNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "rig1")
	bd := newFakeBD()
	bd.db(filepath.Join(town, "rig1")).FailWith("mol wisp gc", errors.New("no wisps table"))
	if r := NewWispGCCheck().Run(bd.ctx(town)); r.Status != StatusOK {
		t.Errorf("Run = %v %q, want OK when the dry run fails", r.Status, r.Message)
	}
}
