package beads

import (
	"strings"
	"testing"
)

// ListOptions.LabelsAny is bd list --label-any, once per label, next to
// --label; an empty label is skipped, and it is refused with Ephemeral because
// the wisp query has no OR (gt-5nq22).
func TestListOptionsLabelsAny(t *testing.T) {
	ResetBdAllowStaleCacheForTest()
	logPath := installMockBDRecorder(t)

	b := New(t.TempDir())
	if _, err := b.List(ListOptions{
		Label:     "rig:gastown",
		LabelsAny: []string{"gt:needs-overseer", "", "gt:needs-human"},
		Priority:  -1,
	}); err != nil {
		t.Fatalf("%s: List: %v", t.Name(), err)
	}
	log := readMockBDLog(t, logPath)
	for _, want := range []string{"--label=rig:gastown", "--label-any=gt:needs-overseer", "--label-any=gt:needs-human"} {
		if !strings.Contains(log, want) {
			t.Errorf("%s: bd log lacks %q:\n%s", t.Name(), want, log)
		}
	}
	if strings.Contains(log, "--label-any=\n") || strings.Contains(log, "--label-any= ") {
		t.Errorf("%s: an empty label reached bd:\n%s", t.Name(), log)
	}

	if _, err := b.List(ListOptions{LabelsAny: []string{"a"}, Ephemeral: true}); err == nil {
		t.Errorf("%s: LabelsAny with Ephemeral returned no error", t.Name())
	}
}
