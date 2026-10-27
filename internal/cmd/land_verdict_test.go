package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/land/landtest"
)

// The verdict command reads what the landing worker stored, and falls back to
// the bead's notes (gt-lh0sc). These tests drive landVerdict over an in-memory
// store and a beadsfake, and the town lookup through fakes, so no town, om or
// host state is read.

// verdictTestBeads is a beadsfake holding gt-abc with the given notes.
func verdictTestBeads(notes string) *beadsfake.Fake {
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{ID: "gt-abc", Title: "the work", Type: "task", Status: string(beads.StatusOpen), Notes: notes})
	return bd
}

// verdictTestRejection is a MERGE REJECTION note for gt-abc at head.
func verdictTestRejection(attempt int, head string, findings ...land.Finding) string {
	return land.FormatRejectionNote(land.RejectionNote{Attempt: attempt, Kind: "gate", Reason: "red",
		Branch: "b", Target: "main", MR: "gt-abc", Head: head, Findings: findings})
}

func TestLandVerdictPrintsTheNewestStoredVerdict(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t,
		landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM),
		landtest.Record("gt-abc", landtest.HeadC, landtest.T0.Add(time.Hour), landtest.NewOM))
	var out bytes.Buffer
	if err := landVerdict(&out, store, verdictTestBeads(""), "gastown", "gt-abc", "", false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Verdict for gt-abc @ " + landtest.HeadC, "approve, score 0.9100", "nit", "explain the nit", "a.go:3"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old round") {
		t.Errorf("output shows the older verdict:\n%s", got)
	}
}

func TestLandVerdictHeadSelectsOneHead(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t,
		landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM),
		landtest.Record("gt-abc", landtest.HeadB, landtest.T0.Add(time.Hour), landtest.NewOM))
	t.Run("unique prefix prints that head", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := landVerdict(&out, store, verdictTestBeads(""), "gastown", "gt-abc", landtest.HeadA[:12], false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "old round") || strings.Contains(out.String(), "new round") {
			t.Errorf("--head %s printed the wrong verdict:\n%s", landtest.HeadA[:12], out.String())
		}
	})
	t.Run("ambiguous prefix is refused", func(t *testing.T) {
		t.Parallel()
		err := landVerdict(&bytes.Buffer{}, store, verdictTestBeads(""), "gastown", "gt-abc", landtest.HeadA[:7], false)
		if !errors.Is(err, land.ErrAmbiguousHead) {
			t.Errorf("err = %v, want ErrAmbiguousHead", err)
		}
	})
	t.Run("missing prefix is refused", func(t *testing.T) {
		t.Parallel()
		err := landVerdict(&bytes.Buffer{}, store, verdictTestBeads(""), "gastown", "gt-abc", landtest.HeadC[:7], false)
		if !errors.Is(err, land.ErrNoVerdictForHead) {
			t.Errorf("err = %v, want ErrNoVerdictForHead", err)
		}
	})
}

func TestLandVerdictFallsBackToTheRejectionNotes(t *testing.T) {
	t.Parallel()
	notes := verdictTestRejection(1, landtest.HeadA, land.Finding{ID: "F1", Severity: "major", Path: "a.go", Line: 7, Title: "the noted finding"})
	var out bytes.Buffer
	if err := landVerdict(&out, fstest.MapFS{}, verdictTestBeads(notes), "gastown", "gt-abc", "", false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"No stored verdict for gt-abc", "not its detail, so no detail is shown", "the noted finding"} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback output lacks %q:\n%s", want, got)
		}
	}
}

// TestLandVerdictFallsBackForAHeadTheStoreLacks: the store holds a verdict for
// one head, and the notes hold a rejection of another. Asking for the other head
// reads its notes, as a head with no store write would need.
func TestLandVerdictFallsBackForAHeadTheStoreLacks(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.NewOM))
	notes := verdictTestRejection(1, landtest.HeadC, land.Finding{ID: "F1", Severity: "major", Path: "c.go", Line: 2, Title: "the head C finding"})

	var out bytes.Buffer
	if err := landVerdict(&out, store, verdictTestBeads(notes), "gastown", "gt-abc", landtest.HeadC[:12], false); err != nil {
		t.Fatalf("a head only the notes name was refused: %v", err)
	}
	if !strings.Contains(out.String(), "the head C finding") {
		t.Errorf("the notes for %s did not print:\n%s", landtest.HeadC, out.String())
	}

	err := landVerdict(&bytes.Buffer{}, store, verdictTestBeads(notes), "gastown", "gt-abc", "9999999", false)
	if !errors.Is(err, land.ErrNoVerdictForHead) {
		t.Errorf("a head neither source names: err = %v, want ErrNoVerdictForHead", err)
	}
}

func TestLandVerdictFallbackHonoursHead(t *testing.T) {
	t.Parallel()
	notes := verdictTestRejection(1, landtest.HeadA) + "\n" + verdictTestRejection(2, landtest.HeadC)
	err := landVerdict(&bytes.Buffer{}, fstest.MapFS{}, verdictTestBeads(notes), "gastown", "gt-abc", landtest.HeadA[:12], false)
	if err != nil {
		t.Fatalf("a head the notes name was refused: %v", err)
	}
	err = landVerdict(&bytes.Buffer{}, fstest.MapFS{}, verdictTestBeads(notes), "gastown", "gt-abc", "9999999", false)
	if !errors.Is(err, land.ErrNoVerdictForHead) {
		t.Errorf("a head no note names: err = %v, want ErrNoVerdictForHead", err)
	}
}

// TestLandVerdictDoesNotFallBackOnAnUnreadableStore: a store that holds a file
// it cannot read is not an empty store, so the notes must not stand in for it.
func TestLandVerdictDoesNotFallBackOnAnUnreadableStore(t *testing.T) {
	t.Parallel()
	notes := verdictTestRejection(1, landtest.HeadA, land.Finding{ID: "F1", Severity: "major", Path: "a.go", Line: 7, Title: "a noted finding"})
	store := fstest.MapFS{
		"gastown/gt-abc/" + landtest.HeadA[:12] + ".json": &fstest.MapFile{Data: []byte("not a record")},
	}
	var out bytes.Buffer
	err := landVerdict(&out, store, verdictTestBeads(notes), "gastown", "gt-abc", "", false)
	if err == nil {
		t.Fatal("a corrupt stored verdict was shown as if the bead had none")
	}
	if out.Len() != 0 {
		t.Errorf("output on a refusal = %q; the notes must not print", out.String())
	}
}

func TestLandVerdictRefusesWhenNeitherSourceHasAVerdict(t *testing.T) {
	t.Parallel()
	err := landVerdict(&bytes.Buffer{}, fstest.MapFS{}, verdictTestBeads("Findings: none.\n"), "gastown", "gt-abc", "", false)
	if err == nil || !strings.Contains(err.Error(), "nothing to show") {
		t.Errorf("err = %v, want a refusal that there is nothing to show", err)
	}
}

func TestLandVerdictJSONPrintsTheStoredFileUnchanged(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.NewOM))
	want := store["gastown/gt-abc/"+landtest.HeadA[:12]+".json"].Data
	var out bytes.Buffer
	if err := landVerdict(&out, store, verdictTestBeads(""), "gastown", "gt-abc", "", true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("--json output is not the stored file:\n got %s\nwant %s", out.Bytes(), want)
	}
}

func TestLandVerdictJSONRefusesTheNotesFallback(t *testing.T) {
	t.Parallel()
	notes := verdictTestRejection(1, landtest.HeadA)
	var out bytes.Buffer
	err := landVerdict(&out, fstest.MapFS{}, verdictTestBeads(notes), "gastown", "gt-abc", "", true)
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("err = %v, want a refusal that --json needs a stored verdict", err)
	}
	if out.Len() != 0 {
		t.Errorf("--json wrote %q on a refusal; want nothing", out.String())
	}
}

// nilIssueBeads answers every Show with neither a bead nor an error.
type nilIssueBeads struct{}

func (nilIssueBeads) Show(string) (*beads.Issue, error) { return nil, nil }

// TestLandVerdictReportsABeadItCannotRead: when the fallback cannot read the
// bead, the command stops with the reason rather than printing nothing.
func TestLandVerdictReportsABeadItCannotRead(t *testing.T) {
	t.Parallel()
	failing := verdictTestBeads("")
	failing.FailWith("show", errors.New("bd is down"))
	for name, tc := range map[string]struct {
		bd   landVerdictBeads
		want string
	}{
		"the read fails":     {bd: failing, want: "bd is down"},
		"the bead is absent": {bd: beadsfake.New(beadsfake.WithPrefix("gt")), want: "reading gt-abc"},
		"the bead is nil":    {bd: nilIssueBeads{}, want: "no such bead"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := landVerdict(&out, fstest.MapFS{}, tc.bd, "gastown", "gt-abc", "", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("output on a refusal = %q; want nothing", out.String())
			}
		})
	}
}

// failingTown is a town lookup that finds no town, so a command run outside
// one stops before it reads anything.
var failingTown = verdictTown{
	find:   func(string) (string, error) { return "", errors.New("no town above here") },
	rigFor: func(string, string) string { return "gastown" },
}

// TestLandVerdictFromCwdRefusesWhenItCannotNameTheDirectory: a working
// directory the process cannot resolve stops the command before any read.
func TestLandVerdictFromCwdRefusesWhenItCannotNameTheDirectory(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "", errors.New("the directory was removed") }
	err := landVerdictFromCwd(&bytes.Buffer{}, getwd, failingTown, "gt-abc", "", false)
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Errorf("err = %v, want a refusal naming the working directory", err)
	}
}

// TestLandVerdictFromCwdRefusesOutsideATown: the command, run from a directory
// with no town above it, stops at the town before it reads anything.
func TestLandVerdictFromCwdRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "/nowhere/gastown", nil }
	err := landVerdictFromCwd(&bytes.Buffer{}, getwd, failingTown, "gt-abc", "", false)
	if err == nil || !strings.Contains(err.Error(), "resolving the town") {
		t.Errorf("err = %v, want a refusal to resolve the town", err)
	}
}

// TestResolveVerdictSourceRefusesOutsideATown: a directory with no town above
// it has no verdict store to read.
func TestResolveVerdictSourceRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	_, err := resolveVerdictSource(failingTown, "/nowhere/gastown", "gt-abc")
	if err == nil || !strings.Contains(err.Error(), "resolving the town") {
		t.Errorf("err = %v, want a refusal to resolve the town", err)
	}
}

// TestResolveVerdictSourceRefusesABeadWithNoRig: a town whose routes name no
// rig for the bead's prefix has no rig to read the verdict under.
func TestResolveVerdictSourceRefusesABeadWithNoRig(t *testing.T) {
	t.Parallel()
	town := verdictTown{
		find:   func(string) (string, error) { return "/town", nil },
		rigFor: func(string, string) string { return "" },
	}
	_, err := resolveVerdictSource(town, "/town", "gt-abc")
	if err == nil || !strings.Contains(err.Error(), "cannot resolve the rig") {
		t.Errorf("err = %v, want a refusal to resolve the rig", err)
	}
}

// TestLandVerdictWritesNothing reads the store and the bead on both paths and
// checks that neither changed: no file added or altered, and the bead's issue
// the same afterwards.
func TestLandVerdictWritesNothing(t *testing.T) {
	t.Parallel()
	notes := verdictTestRejection(1, landtest.HeadA)
	for name, tc := range map[string]struct {
		store  fstest.MapFS
		asJSON bool
	}{
		"stored verdict":         {store: landtest.Store(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.NewOM))},
		"stored verdict as JSON": {store: landtest.Store(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.NewOM)), asJSON: true},
		"notes fallback":         {store: fstest.MapFS{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bd := verdictTestBeads(notes)
			before, err := bd.Show("gt-abc")
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{}
			for p, f := range tc.store {
				files[p] = string(f.Data)
			}
			if err := landVerdict(&bytes.Buffer{}, tc.store, bd, "gastown", "gt-abc", "", tc.asJSON); err != nil {
				t.Fatal(err)
			}
			after, err := bd.Show("gt-abc")
			if err != nil {
				t.Fatal(err)
			}
			if after.Notes != before.Notes || after.Status != before.Status || len(after.Labels) != len(before.Labels) {
				t.Errorf("the bead changed: before %+v, after %+v", before, after)
			}
			if len(tc.store) != len(files) {
				t.Errorf("the store gained or lost files: %d before, %d after", len(files), len(tc.store))
			}
			for p, body := range files {
				if string(tc.store[p].Data) != body {
					t.Errorf("the store file %s changed", p)
				}
			}
		})
	}
}

func TestLandVerdictIsRegisteredUnderLand(t *testing.T) {
	t.Parallel()
	var found *cobra.Command
	for _, c := range landCmd.Commands() {
		if c.Name() == "verdict" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("gt land has no verdict subcommand")
	}
	if found.Short == "" || found.Long == "" {
		t.Errorf("verdict subcommand lacks help text: short %q", found.Short)
	}
	if found.Flags().Lookup("head") == nil || found.Flags().Lookup("json") == nil {
		t.Errorf("verdict subcommand lacks the --head or --json flag")
	}
}
