package doctor

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestHookSingletonFixReportsSurvivingDuplicates: when bd refuses to close
// one of the duplicate handoff beads and closes the rest, Fix fails and
// names the survivor. bd exits 0 for such a batch, and Fix used to report
// success while the duplicate stayed.
func TestHookSingletonFixReportsSurvivingDuplicates(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	var ids []string
	for i := 0; i < 3; i++ {
		is, err := bd.Create(beads.CreateOptions{Title: "mayor Handoff", Priority: -1})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, is.ID)
	}
	other := "gastown/crew/joe"
	if err := bd.Update(ids[2], beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}

	check := NewHookSingletonCheck()
	check.closer = func(string) beads.Client { return bd }
	check.duplicates = []duplicateHandoff{{title: "mayor Handoff", beadsDir: "/town/.beads", beadIDs: ids}}

	err := check.Fix(&CheckContext{})
	if err == nil || !strings.Contains(err.Error(), ids[2]) {
		t.Errorf("Fix = %v, want an error naming the surviving duplicate %s", err, ids[2])
	}
	if got, _ := bd.Show(ids[1]); got.Status != "closed" {
		t.Errorf("closable duplicate %s status %q, want closed", ids[1], got.Status)
	}
	if got, _ := bd.Show(ids[0]); got.Status != "open" {
		t.Errorf("kept handoff %s status %q, want open", ids[0], got.Status)
	}
}
