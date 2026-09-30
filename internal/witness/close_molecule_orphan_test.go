package witness

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeMoleculeBd models one molecule gt-mol with three open steps, where bd
// refuses to close gt-s2 unless the close carries --force. The returned
// closes lists every id a close call named, in order, so a test can see
// whether the root was closed.
func fakeMoleculeBd(forceWorks bool) (*BdCli, map[string]string, *[]string) {
	status := map[string]string{"gt-mol": "open", "gt-s1": "open", "gt-s2": "open", "gt-s3": "open"}
	var closes []string
	bd := &BdCli{
		Exec: func(_ string, args ...string) (string, error) {
			switch {
			case args[0] == "list" && args[1] == "--parent=gt-mol":
				return `[{"id":"gt-s1","status":"` + status["gt-s1"] + `"},{"id":"gt-s2","status":"` + status["gt-s2"] + `"},{"id":"gt-s3","status":"` + status["gt-s3"] + `"}]`, nil
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
			if args[0] != "close" {
				return nil
			}
			force := slices.Contains(args, "--force")
			closedAny := false
			for _, a := range args[1:] {
				if _, ok := status[a]; !ok {
					continue
				}
				closes = append(closes, a)
				if a == "gt-s2" && !(force && forceWorks) {
					continue // refused, like bd 1.2: skipped, exit 0 if others closed
				}
				status[a] = "closed"
				closedAny = true
			}
			if !closedAny {
				return errors.New("exit status 1")
			}
			return nil
		},
	}
	return bd, status, &closes
}

// TestCloseMoleculeWithDescendantsForceClosesRefusedSteps: the polecat that
// owned the molecule is gone, so a step bd refuses to close protects nothing.
// Closing the root over an open step leaves that step behind, and the next
// `bd mol wisp gc --closed --force` deletes the closed root and drops the
// step's parent edge: the step is then an open wisp under no molecule that
// no reaper query selects (gt-ogvp6).
func TestCloseMoleculeWithDescendantsForceClosesRefusedSteps(t *testing.T) {
	t.Parallel()
	bd, status, _ := fakeMoleculeBd(true)

	n, err := closeMoleculeWithDescendants(bd, t.TempDir(), "gt-mol")
	if err != nil {
		t.Fatalf("closeMoleculeWithDescendants: %v", err)
	}
	if n != 4 {
		t.Errorf("closed count = %d, want 4 (three steps and the root)", n)
	}
	for id, st := range status {
		if st != "closed" {
			t.Errorf("%s status %q, want closed", id, st)
		}
	}
}

// TestCloseMoleculeWithDescendantsKeepsRootOpenOverStuckStep: when even a
// forced close leaves a step open, the root stays open too. An open root is
// visible work; a closed root over an open step is a wisp gc will orphan.
func TestCloseMoleculeWithDescendantsKeepsRootOpenOverStuckStep(t *testing.T) {
	t.Parallel()
	bd, status, closes := fakeMoleculeBd(false)

	_, err := closeMoleculeWithDescendants(bd, t.TempDir(), "gt-mol")
	if err == nil {
		t.Fatal("closeMoleculeWithDescendants = nil, want an error naming the stuck step")
	}
	if !strings.Contains(err.Error(), "gt-s2") {
		t.Errorf("error = %v, want it to name gt-s2", err)
	}
	if status["gt-mol"] != "open" {
		t.Errorf("root status %q, want open while gt-s2 is open", status["gt-mol"])
	}
	if slices.Contains(*closes, "gt-mol") {
		t.Errorf("root was sent to bd close (%v); it must not be while a step is open", *closes)
	}
}
