package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// specGraphFixture is the reads gt spec lint --graph works over, answered from
// one fake database, so a test drives the command's own code path without bd.
func specGraphFixture(db *beadsfake.Fake) specGraphReads {
	return specGraphReads{
		show:     func(id string) (*beads.Issue, error) { return db.Show(id) },
		children: func(parentID string) ([]*beads.Issue, error) { return db.Children(parentID) },
	}
}

// specGraphLines splits a human-form run's findings into lines.
func specGraphLines(out string) []string {
	trimmed := strings.TrimSuffix(out, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// findGraphFinding returns the one output line of kind, or fails when there is
// not exactly one.
func findGraphFinding(t *testing.T, lines []string, kind string) string {
	t.Helper()
	var found []string
	for _, line := range lines {
		if strings.HasPrefix(line, kind+": ") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("findings of %s = %d, want 1:\n%s", kind, len(found), strings.Join(lines, "\n"))
	}
	return found[0]
}

// TestSpecGraphExitCodes: a clean epic exits 0, an epic with a duplicate
// definition exits 1, and one whose only mistake is a duplicate test name
// prints it and still exits 0 — the design's advisory.
func TestSpecGraphExitCodes(t *testing.T) {
	t.Parallel()

	t.Run("clean epic exits 0", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.1", Title: "one", Parent: epic.ID,
			Description: "## Interfaces\nDefines: Alpha",
			Acceptance:  "- [ ] TestOne: first",
		})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.2", Title: "two", Parent: epic.ID,
			Description: "## Interfaces\nUses: Alpha",
			Acceptance:  "- [ ] TestTwo: second",
		})
		if err := db.AddDependency("gt-epic.2", "gt-epic.1"); err != nil {
			t.Fatal(err)
		}

		var out, errOut bytes.Buffer
		if err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, false); err != nil {
			t.Fatalf("runSpecGraphAt = %v, want exit 0\nstderr: %s", err, errOut.String())
		}
		if out.Len() != 0 {
			t.Errorf("clean epic printed %q, want no findings", out.String())
		}
	})

	t.Run("duplicate define exits 1", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.1", Title: "one", Parent: epic.ID,
			Description: "## Interfaces\nDefines: Alpha",
		})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.2", Title: "two", Parent: epic.ID,
			Description: "## Interfaces\nDefines: Alpha",
		})

		var out, errOut bytes.Buffer
		err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, false)
		if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
			t.Fatalf("runSpecGraphAt = %v, want a silent exit %d", err, specLintExitRefused)
		}
		if line := findGraphFinding(t, specGraphLines(out.String()), "duplicate-define"); !strings.Contains(line, "Alpha") {
			t.Errorf("finding = %q, want the duplicated name", line)
		}
	})

	t.Run("duplicate test alone prints and exits 0", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.1", Title: "one", Parent: epic.ID,
			Acceptance: "- [ ] TestSame: first",
		})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.2", Title: "two", Parent: epic.ID,
			Acceptance: "- [ ] TestSame: second",
		})

		var out, errOut bytes.Buffer
		if err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, false); err != nil {
			t.Fatalf("runSpecGraphAt = %v, want exit 0 for the advisory\nstderr: %s", err, errOut.String())
		}
		if line := findGraphFinding(t, specGraphLines(out.String()), "duplicate-test"); !strings.Contains(line, "TestSame") {
			t.Errorf("finding = %q, want the duplicated test name", line)
		}
	})
}

// TestSpecGraphBuildsBeadsFromDescriptions: each field the command builds a
// GraphBead from reaches CheckGraph, which is what turns it into a finding.
// Defines and Uses come from Interfaces, Modify from Files, TestNames from the
// acceptance boxes, and DependsOn from the child's blocking edges.
func TestSpecGraphBuildsBeadsFromDescriptions(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
	mustBead(t, db, beads.CreateOptions{
		ID: "gt-epic.1", Title: "one", Parent: epic.ID,
		Description: "## Interfaces\nDefines: Alpha, Beta\nUses: Gamma\n\n## Files\ncreate: a.go\nmodify: shared.go",
		Acceptance:  "- [ ] TestSame: first",
	})
	mustBead(t, db, beads.CreateOptions{
		ID: "gt-epic.2", Title: "two", Parent: epic.ID,
		Description: "## Interfaces\nDefines: Alpha\n\n## Files\ncreate: b.go\nmodify: shared.go",
		Acceptance:  "- [ ] TestSame: second",
	})
	// The third child uses a name gt-epic.1 defines, and depends on it, so the
	// use is met: an unmet-use for Beta means the edge never reached GraphBead.
	mustBead(t, db, beads.CreateOptions{
		ID: "gt-epic.3", Title: "three", Parent: epic.ID,
		Description: "## Interfaces\nUses: Beta\n\n## Files\ncreate: c.go",
		Acceptance:  "- [ ] TestOther: third",
	})
	if err := db.AddDependency("gt-epic.3", "gt-epic.1"); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, false)
	if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
		t.Fatalf("runSpecGraphAt = %v, want a silent exit %d\nstderr: %s", err, specLintExitRefused, errOut.String())
	}
	lines := specGraphLines(out.String())
	if len(lines) != 4 {
		t.Fatalf("findings = %d, want 4:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []struct{ kind, name string }{
		{"duplicate-define", "Alpha"},
		{"unmet-use", "Gamma"},
		{"shared-modify", "shared.go"},
		{"duplicate-test", "TestSame"},
	} {
		line := findGraphFinding(t, lines, want.kind)
		if !strings.Contains(line, want.name) {
			t.Errorf("%s finding = %q, want it naming %q", want.kind, line, want.name)
		}
		// The pair findings name both beads; the unmet use names the user and
		// the name only.
		if want.kind != "unmet-use" && (!strings.Contains(line, "gt-epic.1") || !strings.Contains(line, "gt-epic.2")) {
			t.Errorf("%s finding = %q, want both child ids", want.kind, line)
		}
	}
	for _, line := range lines {
		if strings.Contains(line, "uses Beta") {
			t.Errorf("the dependency edge did not reach CheckGraph: %q", line)
		}
	}
}

// TestSpecGraphJSON: --json prints the epic and the findings array.
func TestSpecGraphJSON(t *testing.T) {
	t.Parallel()

	t.Run("findings", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.1", Title: "one", Parent: epic.ID,
			Description: "## Interfaces\nDefines: Alpha",
		})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.2", Title: "two", Parent: epic.ID,
			Description: "## Interfaces\nDefines: Alpha",
		})

		var out, errOut bytes.Buffer
		err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, true)
		if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
			t.Fatalf("runSpecGraphAt = %v, want a silent exit %d", err, specLintExitRefused)
		}
		var report struct {
			Epic     string `json:"epic"`
			Findings []struct {
				Kind   string `json:"kind"`
				Bead   string `json:"bead"`
				Detail string `json:"detail"`
			} `json:"findings"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("--json output is not JSON (%v):\n%s", err, out.String())
		}
		if report.Epic != epic.ID {
			t.Errorf("epic = %q, want %q", report.Epic, epic.ID)
		}
		if len(report.Findings) != 1 {
			t.Fatalf("findings = %+v, want one", report.Findings)
		}
		finding := report.Findings[0]
		if finding.Kind != "duplicate-define" || !strings.Contains(finding.Detail, "Alpha") {
			t.Errorf("finding = %+v, want the duplicate-define for Alpha", finding)
		}
		if !strings.Contains(finding.Detail, "gt-epic.1") || !strings.Contains(finding.Detail, "gt-epic.2") {
			t.Errorf("finding detail = %q, want both child ids", finding.Detail)
		}
	})

	t.Run("clean epic keeps an empty array", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{ID: "gt-epic.1", Title: "one", Parent: epic.ID})

		var out, errOut bytes.Buffer
		if err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, true); err != nil {
			t.Fatalf("runSpecGraphAt = %v, want exit 0", err)
		}
		if !strings.Contains(out.String(), `"findings":[]`) {
			t.Errorf("--json output = %s, want an empty findings array", out.String())
		}
	})
}

// TestSpecGraphJSONWriteFailure: a stdout that refuses the report fails the
// run rather than reporting a graph whose answer was never delivered. The epic
// has one child, since an epic with none is refused before any report is made.
func TestSpecGraphJSONWriteFailure(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
	mustBead(t, db, beads.CreateOptions{ID: "gt-epic.1", Title: "one", Parent: epic.ID})
	writeErr := errors.New("stdout refused")

	var errOut bytes.Buffer
	err := runSpecGraphAt(refusingWriter{err: writeErr}, &errOut, specGraphFixture(db), epic.ID, true)
	if !errors.Is(err, writeErr) {
		t.Errorf("runSpecGraphAt = %v, want the writer's error", err)
	}
}

// TestSpecGraphUnreadableChildFailsClosed: an epic that cannot be read, a
// child set that cannot be read, and a child whose own read or Files or
// Interfaces section fails each exit 1 naming the bead. None is read as an
// empty graph.
func TestSpecGraphUnreadableChildFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		description  string
		unknownEpic  bool
		epicNil      bool
		childrenErr  bool
		childShowErr bool
		childNil     bool
		wantNamed    string
	}{
		{name: "unknown epic", unknownEpic: true, wantNamed: "gt-missing"},
		{name: "epic read returns no bead", epicNil: true, wantNamed: "gt-epic"},
		{name: "children read fails", childrenErr: true, wantNamed: "gt-epic"},
		{name: "child cannot be read", childShowErr: true, wantNamed: "gt-epic.2"},
		{name: "child read returns no bead", childNil: true, wantNamed: "gt-epic.2"},
		{name: "files section does not parse", description: "## Files\ncreate no colon", wantNamed: "gt-epic.2"},
		{name: "interfaces section does not parse", description: "## Interfaces\nDefines: Alpha\nleftover text", wantNamed: "gt-epic.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
			mustBead(t, db, beads.CreateOptions{ID: "gt-epic.1", Title: "good", Parent: epic.ID})
			mustBead(t, db, beads.CreateOptions{
				ID: "gt-epic.2", Title: "bad", Parent: epic.ID, Description: tc.description,
			})
			reads := specGraphFixture(db)
			if tc.childrenErr {
				reads.children = func(string) ([]*beads.Issue, error) {
					return nil, errors.New("children read refused")
				}
			}
			if tc.childShowErr || tc.childNil || tc.epicNil {
				reads.show = func(id string) (*beads.Issue, error) {
					if id == "gt-epic.2" && tc.childShowErr {
						return nil, errors.New("show refused")
					}
					if id == "gt-epic.2" && tc.childNil {
						return nil, nil
					}
					if id == "gt-epic" && tc.epicNil {
						return nil, nil
					}
					return db.Show(id)
				}
			}
			epicID := epic.ID
			if tc.unknownEpic {
				epicID = "gt-missing"
			}

			var out, errOut bytes.Buffer
			err := runSpecGraphAt(&out, &errOut, reads, epicID, false)
			if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
				t.Fatalf("runSpecGraphAt = %v, want a silent exit %d", err, specLintExitRefused)
			}
			if !strings.Contains(errOut.String(), tc.wantNamed) {
				t.Errorf("stderr = %q, want it naming %s", errOut.String(), tc.wantNamed)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want no findings for a graph that could not be read", out.String())
			}
		})
	}
}

// TestSpecGraphEmptyEpicFailsClosed: an epic with no children is refused and
// named, never printed as a clean graph.
func TestSpecGraphEmptyEpicFailsClosed(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})

	var out, errOut bytes.Buffer
	err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, false)
	if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
		t.Fatalf("runSpecGraphAt = %v, want a silent exit %d\nstderr: %s", err, specLintExitRefused, errOut.String())
	}
	if !strings.Contains(errOut.String(), "gt-epic has no children") {
		t.Errorf("stderr = %q, want it naming the childless epic", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want no findings for an epic with no children", out.String())
	}
}

// TestSpecGraphCreateCollisionRefuses: two unordered beads that create the same
// file exit 1 naming both beads and the path, and the same pair with an edge
// between them is not a collision.
func TestSpecGraphCreateCollisionRefuses(t *testing.T) {
	t.Parallel()

	t.Run("unordered pair exits 1", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.1", Title: "one", Parent: epic.ID,
			Description: "## Files\ncreate: shared.go",
		})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.2", Title: "two", Parent: epic.ID,
			Description: "## Files\ncreate: shared.go",
		})

		var out, errOut bytes.Buffer
		err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, false)
		if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
			t.Fatalf("runSpecGraphAt = %v, want a silent exit %d", err, specLintExitRefused)
		}
		line := findGraphFinding(t, specGraphLines(out.String()), "shared-create")
		for _, want := range []string{"gt-epic.1", "gt-epic.2", "shared.go"} {
			if !strings.Contains(line, want) {
				t.Errorf("finding = %q, want it naming %q", line, want)
			}
		}
	})

	t.Run("ordered pair exits 0", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.1", Title: "one", Parent: epic.ID,
			Description: "## Files\ncreate: shared.go",
		})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.2", Title: "two", Parent: epic.ID,
			Description: "## Files\ncreate: shared.go",
		})
		if err := db.AddDependency("gt-epic.2", "gt-epic.1"); err != nil {
			t.Fatal(err)
		}

		var out, errOut bytes.Buffer
		if err := runSpecGraphAt(&out, &errOut, specGraphFixture(db), epic.ID, false); err != nil {
			t.Fatalf("runSpecGraphAt = %v, want exit 0 for an ordered pair\nstderr: %s", err, errOut.String())
		}
		if out.Len() != 0 {
			t.Errorf("ordered pair printed %q, want no findings", out.String())
		}
	})
}

// TestSpecGraphProductionReads: runSpecGraph reads the epic and its children
// through the stores it is given, which is the path a bd-backed run takes. The
// fake stores answer from one database, so the findings a run prints are the
// ones the production reads produced.
func TestSpecGraphProductionReads(t *testing.T) {
	t.Parallel()

	t.Run("epic and children come through the stores", func(t *testing.T) {
		t.Parallel()
		db := beadsfake.New()
		epic := mustBead(t, db, beads.CreateOptions{ID: "gt-epic", Title: "epic"})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.1", Title: "one", Parent: epic.ID,
			Description: "## Interfaces\nDefines: Alpha",
		})
		mustBead(t, db, beads.CreateOptions{
			ID: "gt-epic.2", Title: "two", Parent: epic.ID,
			Description: "## Interfaces\nDefines: Alpha",
		})
		// routed is an empty database: a read that falls through to it fails
		// the run, so a finding here means the pinned store answered.
		stores := slingStores{
			pinned: func(string) slingStore { return db },
			routed: func(string) slingStore { return beadsfake.New() },
		}

		var out, errOut bytes.Buffer
		err := runSpecGraph(&out, &errOut, stores, t.TempDir(), epic.ID, false)
		if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
			t.Fatalf("runSpecGraph = %v, want a silent exit %d\nstderr: %s", err, specLintExitRefused, errOut.String())
		}
		findGraphFinding(t, specGraphLines(out.String()), "duplicate-define")
	})

	t.Run("an epic no store has is refused and named", func(t *testing.T) {
		t.Parallel()
		stores := slingStores{
			pinned: func(string) slingStore { return beadsfake.New() },
			routed: func(string) slingStore { return beadsfake.New() },
		}

		var out, errOut bytes.Buffer
		err := runSpecGraph(&out, &errOut, stores, t.TempDir(), "gt-missing", false)
		if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
			t.Fatalf("runSpecGraph = %v, want a silent exit %d", err, specLintExitRefused)
		}
		if !strings.Contains(errOut.String(), "gt-missing") {
			t.Errorf("stderr = %q, want it naming gt-missing", errOut.String())
		}
	})
}

// TestSpecGraphDoesNotChangePerBeadLint: the flag defaults to off, so a run
// without it is the per-bead shape lint and nothing else.
func TestSpecGraphDoesNotChangePerBeadLint(t *testing.T) {
	t.Parallel()
	flag := specLintCmd.Flags().Lookup("graph")
	if flag == nil {
		t.Fatal("gt spec lint has no --graph flag")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--graph default = %q, want false: a run without it must stay the per-bead lint", flag.DefValue)
	}

	spec := cleanSpec("gt-shape", 2, "2026-01-01")
	var out, errOut bytes.Buffer
	if err := runSpecLintAt(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now()); err != nil {
		t.Fatalf("per-bead lint of a clean spec = %v, want exit 0", err)
	}
	if lines := specGraphLines(out.String()); len(lines) != 1 || !strings.Contains(lines[0], "gt-shape") {
		t.Errorf("per-bead lint printed %q, want its one-line verdict", out.String())
	}

	spec.Description = ""
	spec.Acceptance = ""
	err := runSpecLintAt(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now())
	if code, silent := IsSilentExit(err); !silent || code != specLintExitRefused {
		t.Errorf("per-bead lint of a bare spec = %v, want a silent exit %d", err, specLintExitRefused)
	}
}
