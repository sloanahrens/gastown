package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEventsJournalCheck: a store whose config leaves the events journal off
// fails the check by name; bd is asked with gastown's own override removed
// and BEADS_DIR pinned to the store (gt-7iwy0.7).
func TestEventsJournalCheck(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "alpha", "beta")
	hqDir := filepath.Join(town, ".beads")
	alphaDir := filepath.Join(town, "alpha", "mayor", "rig", ".beads")
	betaDir := filepath.Join(town, "beta", ".beads")
	for _, d := range []string{hqDir, alphaDir, betaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	bd := newFakeBD()
	check := NewEventsJournalCheck()
	check.environ = func() []string { return []string{"BD_EVENTS_JOURNAL=1", "BEADS_DIR=/elsewhere"} }
	_ = bd.db(filepath.Dir(hqDir)).ConfigSet("events-journal", "true")
	_ = bd.db(filepath.Dir(alphaDir)).ConfigSet("events-journal", "false")
	// beta never set: bd's default is off.

	res := check.Run(bd.ctx(town))
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	joined := strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "beta") || strings.Contains(joined, "hq") {
		t.Errorf("details = %q, want alpha and beta, not hq", joined)
	}
	for _, o := range bd.opened() {
		env := strings.Join(o.env, " ")
		if strings.Contains(env, "BD_EVENTS_JOURNAL") || !strings.Contains(env, "BEADS_DIR="+filepath.Join(o.dir, ".beads")) {
			t.Errorf("bd in %s ran with env %v", o.dir, o.env)
		}
	}

	_ = bd.db(filepath.Dir(alphaDir)).ConfigSet("events-journal", "true")
	_ = bd.db(filepath.Dir(betaDir)).ConfigSet("events-journal", "1")
	if res := check.Run(bd.ctx(town)); res.Status != StatusOK {
		t.Errorf("all on: status = %v, %v", res.Status, res.Details)
	}
}
