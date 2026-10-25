package cmd

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/spans"
)

// TestEngineSpanStartWritesUnderTheTownRuntime: the dispatch's span write lands
// in the log the daemon and gt done also read, so the attempt the polecat opens
// is the one its submission can close.
func TestEngineSpanStartWritesUnderTheTownRuntime(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var d slingDeps

	if err := d.engineSpanStart(townRoot, "gt-liseq", "gastown/nux"); err != nil {
		t.Fatalf("engineSpanStart: %v", err)
	}
	store := spans.New(spans.DefaultDir(townRoot), nil)
	sum, err := store.Summarize("gt-liseq")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !sum.HasOpen("gastown/nux") {
		t.Errorf("span log under %s holds no open attempt for gastown/nux: %+v",
			filepath.Join(spans.DefaultDir(townRoot)), sum.Attempts)
	}
}
