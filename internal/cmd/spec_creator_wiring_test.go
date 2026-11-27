package cmd

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// TestSpecCreatorRefused is creator cite 4/4: both gt spec lint and the tick's
// pre-sling check fill each cite's state through specWithBlockerCreates, so a
// bead whose cited creator is open and not a blocker is refused on both paths,
// and passes both once the dependency edge exists. The bead has no Files
// section: the cite cue needs none.
func TestSpecCreatorRefused(t *testing.T) {
	t.Parallel()
	bead := cleanSpec("gt-work", 2, "2026-09-29T10:00:00Z")
	bead.Description = "## Goal\nAdd it, created by bead gt-creator.\n\n## Constraints\nGo.\n\n## Out of scope\nNone.\n\n## Gate\nmake gate\n\n## Size\none worker, one MR"
	const refusal = "creator cite gt-creator is open and not a blocker"

	cases := []struct {
		name    string
		blocker bool
	}{
		{"open creator the bead does not depend on is refused", false},
		{"a creator the bead depends on passes", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			blockers := map[string][]string{}
			if tc.blocker {
				blockers["gt-work"] = []string{"gt-creator"}
			}
			read := blockerGraph(map[string]string{
				"gt-work":    bead.Description,
				"gt-creator": "## Goal\nMakes the file.\n",
			}, blockers)

			// gt spec lint.
			spec, err := specForLint(t.TempDir(), "gt-work",
				func(string) (specdispatch.Spec, error) { return bead, nil }, read, openRigTree)
			if err != nil {
				t.Fatalf("specForLint: %v", err)
			}
			var out, errOut bytes.Buffer
			lintErr := runSpecLintAt(&out, &errOut, "", "gt-work", spec, nil, "", nil, false, specTestNow)
			if _, refused := IsSilentExit(lintErr); refused == tc.blocker {
				t.Fatalf("gt spec lint refused = %v, want %v; line %q", refused, !tc.blocker, out.String())
			}
			if got := strings.Contains(out.String(), refusal); got == tc.blocker {
				t.Errorf("lint line %q names the creator refusal = %v, want %v", out.String(), got, !tc.blocker)
			}

			// The tick's pre-sling check.
			f := newFakeSpecTown(bead)
			env := f.env()
			env.BlockerRead = read
			r := runSpecDispatchCycle(env)
			if tc.blocker {
				if len(r.Refused) != 0 {
					t.Fatalf("tick refused %+v, want the creator edge to pass", r.Refused)
				}
				if len(f.slung) != 1 || f.slung[0] != "gt-work" {
					t.Fatalf("slung %v, want gt-work; skipped %+v", f.slung, r.Skipped)
				}
				return
			}
			if len(f.slung) != 0 {
				t.Fatalf("slung %v, want the bead held", f.slung)
			}
			if len(r.Refused) != 1 || !strings.Contains(r.Refused[0].Line, refusal) {
				t.Fatalf("refused = %+v, want the creator refusal", r.Refused)
			}
		})
	}
}

// TestSpecCreatorRetiredThroughTheWiring is the close-reason half of the same
// wiring: a closed cite is retired, with its reason, only if specFromIssue
// carries the close reason into the spec the graph read answers.
func TestSpecCreatorRetiredThroughTheWiring(t *testing.T) {
	t.Parallel()
	bead := cleanSpec("gt-work", 2, "2026-09-29T10:00:00Z")
	bead.Description = "## Goal\nAdd it, created by bead gt-creator.\n"
	issues := map[string]*beads.Issue{
		"gt-work":    {ID: "gt-work", Status: "open", Description: bead.Description},
		"gt-creator": {ID: "gt-creator", Status: "closed", CloseReason: "split into gt-new"},
	}
	read := func(id string) (specdispatch.Spec, []string, error) {
		issue, ok := issues[id]
		if !ok {
			return specdispatch.Spec{}, nil, errors.New("no bead " + id)
		}
		return specFromIssue(issue), beads.UnresolvedBlockerIDs(issue.Dependencies), nil
	}

	got, err := specWithBlockerCreates(bead, read, "gt-work")
	if err != nil {
		t.Fatalf("specWithBlockerCreates: %v", err)
	}
	want := []specdispatch.CreatorCite{{ID: "gt-creator", State: specdispatch.CreatorRetired, Reason: "split into gt-new"}}
	if !slices.Equal(got.CreatorCites, want) {
		t.Errorf("CreatorCites = %+v, want %+v", got.CreatorCites, want)
	}
}
