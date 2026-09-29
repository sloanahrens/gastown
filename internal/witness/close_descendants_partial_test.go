package witness

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestCloseDescendantsViaCLICountsOnlyClosedSteps: bd 1.2 skips a refused
// step in a multi-issue close and exits 0 (here the dead polecat's step 2 is
// refused). closeDescendantsViaCLI must count only the steps that closed and
// report the one left open, not count every step it named.
func TestCloseDescendantsViaCLICountsOnlyClosedSteps(t *testing.T) {
	t.Parallel()
	status := map[string]string{"gt-s1": "open", "gt-s2": "open", "gt-s3": "open"}
	refused := map[string]bool{"gt-s2": true}
	bd := &BdCli{
		Exec: func(_ string, args ...string) (string, error) {
			switch {
			case args[0] == "list" && args[1] == "--parent=gt-mol":
				return `[{"id":"gt-s1","status":"open"},{"id":"gt-s2","status":"open"},{"id":"gt-s3","status":"open"}]`, nil
			case args[0] == "list":
				return "[]", nil
			case args[0] == "show":
				var out []string
				for _, a := range args[1:] {
					if st, ok := status[a]; ok {
						out = append(out, `{"id":"`+a+`","status":"`+st+`"}`)
					}
				}
				return "[" + strings.Join(out, ",") + "]", nil
			}
			return "", nil
		},
		Run: func(_ string, args ...string) error {
			if args[0] == "close" {
				for _, a := range args[1:] {
					if _, ok := status[a]; ok && !refused[a] {
						status[a] = "closed"
					}
				}
			}
			return nil
		},
	}

	n, err := closeDescendantsViaCLI(bd, t.TempDir(), "gt-mol")
	if n != 2 {
		t.Errorf("closed count = %d, want 2 (gt-s2 was refused)", n)
	}
	var pe *beads.PartialCloseError
	if !errors.As(err, &pe) || len(pe.NotClosed) != 1 || pe.NotClosed[0] != "gt-s2" {
		t.Errorf("error = %v, want it to name gt-s2 as not closed", err)
	}
}
