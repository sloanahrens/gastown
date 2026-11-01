package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/done"
)

// findingsTown lays out a town whose gt- beads route to the gastown rig and
// returns its root plus a store holding the source bead gt-finding-source.
func findingsTown(t *testing.T) (string, *beadsfake.Fake) {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{
		ID:       "gt-finding-source",
		Title:    "the task the finding is not about",
		Type:     "task",
		Status:   string(beads.StatusInProgress),
		Assignee: "gastown/polecats/nux",
	})
	return townRoot, db
}

// findingsStoreOpener opens db for any routed beads dir, standing in for the
// bd store the source bead routes to.
func findingsStoreOpener(db beads.Client) done.SourceStoreOpener {
	return func(string, string) beads.Client { return db }
}

// findingsComments returns the FINDING: comments on gt-finding-source.
func findingsComments(t *testing.T, db *beadsfake.Fake) []string {
	t.Helper()
	comments, err := db.Comments("gt-finding-source")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	var out []string
	for _, c := range comments {
		if strings.HasPrefix(c.Text, doneFindingMarker) {
			out = append(out, c.Text)
		}
	}
	return out
}

// TestDoneFindingWritesComments: every --finding is one FINDING: comment on
// the source bead, in the order the flags were given.
func TestDoneFindingWritesComments(t *testing.T) {
	t.Parallel()
	townRoot, db := findingsTown(t)
	var warn bytes.Buffer
	findings := []string{
		"the retry never resets its counter",
		"rig: gastown\nland worker retries forever on a missing branch",
	}

	recordDoneFindings(&warn, filepath.Join(townRoot, "gastown"), "gt-finding-source", findings, findingsStoreOpener(db))

	got := findingsComments(t, db)
	if len(got) != len(findings) {
		t.Fatalf("recorded %d FINDING comment(s), want %d: %v", len(got), len(findings), got)
	}
	for i, finding := range findings {
		want := doneFindingMarker + "\n" + finding
		if got[i] != want {
			t.Errorf("comment %d = %q, want %q", i, got[i], want)
		}
	}
	if warn.Len() != 0 {
		t.Errorf("a successful record warned: %q", warn.String())
	}
}

// TestDoneFindingLimits: a run records the first 5 findings and warns about
// the rest; every recorded finding is cut to 1200 characters with its C
// category characters removed.
func TestDoneFindingLimits(t *testing.T) {
	t.Parallel()
	townRoot, db := findingsTown(t)
	var warn bytes.Buffer
	findings := []string{"one", "two", "three", "four", "five", "six", "seventh finding is dropped"}

	recordDoneFindings(&warn, filepath.Join(townRoot, "gastown"), "gt-finding-source", findings, findingsStoreOpener(db))

	got := findingsComments(t, db)
	if len(got) != maxDoneFindings {
		t.Fatalf("recorded %d FINDING comment(s), want the cap %d: %v", len(got), maxDoneFindings, got)
	}
	for i, want := range findings[:maxDoneFindings] {
		if got[i] != doneFindingMarker+"\n"+want {
			t.Errorf("comment %d = %q, want %q", i, got[i], want)
		}
	}
	if !strings.Contains(warn.String(), fmt.Sprintf("recording the first %d", maxDoneFindings)) {
		t.Errorf("warnings %q do not say the extra findings were dropped", warn.String())
	}

	// A finding longer than the cap is cut, and one carrying the C category
	// keeps only the text: Cc, Cf (a bidi override, a zero-width joiner), Co
	// and Cs all go; tab and newline stay readable.
	long := strings.Repeat("x", maxDoneFindingLen+50)
	hostile := "keep\x00 this\r\tand\u202e this\u2066\nsecond\u200bline\u0600"
	cutWarn := bytes.Buffer{}
	cutDB := beadsfake.New(beadsfake.WithPrefix("gt"))
	cutDB.Seed(beads.Issue{ID: "gt-finding-source", Title: "source", Type: "task", Status: string(beads.StatusInProgress)})

	recordDoneFindings(&cutWarn, filepath.Join(townRoot, "gastown"), "gt-finding-source", []string{long, hostile}, findingsStoreOpener(cutDB))

	cut := findingsComments(t, cutDB)
	if len(cut) != 2 {
		t.Fatalf("recorded %d FINDING comment(s), want 2: %v", len(cut), cut)
	}
	if body := strings.TrimPrefix(cut[0], doneFindingMarker+"\n"); len([]rune(body)) != maxDoneFindingLen {
		t.Errorf("long finding recorded with %d runes, want %d", len([]rune(body)), maxDoneFindingLen)
	}
	wantHostile := doneFindingMarker + "\n" + "keep this\tand this\nsecondline"
	if cut[1] != wantHostile {
		t.Errorf("hostile finding recorded as %q, want %q", cut[1], wantHostile)
	}
}

// TestDoneFindingRecordsVerbatimNoDoneNoteSecretScreening: gt done's other
// notes screen nothing for secrets, so a finding is recorded word for word.
func TestDoneFindingRecordsVerbatimNoDoneNoteSecretScreening(t *testing.T) {
	t.Parallel()
	townRoot, db := findingsTown(t)
	var warn bytes.Buffer
	finding := "rig: gastown\ntoken sk-live-abc123 is in the config at line 4"

	recordDoneFindings(&warn, filepath.Join(townRoot, "gastown"), "gt-finding-source", []string{finding}, findingsStoreOpener(db))

	got := findingsComments(t, db)
	if len(got) != 1 || got[0] != doneFindingMarker+"\n"+finding {
		t.Fatalf("FINDING comments = %q, want the text verbatim", got)
	}
}

// failingCommentStore fails every comment write; the rest of the fake's bead
// surface works, so the run reaches the write.
type failingCommentStore struct {
	beads.Client
	err error
}

func (s failingCommentStore) AddComment(id, text string) error { return s.err }

// TestDoneFindingWriteFailureDoesNotRefuse: a comment write that fails is a
// warning and nothing else, so the polecat's work still submits.
func TestDoneFindingWriteFailureDoesNotRefuse(t *testing.T) {
	t.Parallel()
	townRoot, db := findingsTown(t)
	store := failingCommentStore{Client: db, err: errors.New("bd comment refused")}
	var warn bytes.Buffer

	recordDoneFindings(&warn, filepath.Join(townRoot, "gastown"), "gt-finding-source", []string{"the defect"}, findingsStoreOpener(store))

	if !strings.Contains(warn.String(), "could not record --finding 1 on gt-finding-source") {
		t.Errorf("warnings %q do not report the failed finding write", warn.String())
	}
}

// TestDoneFindingUnrecordableWarns: a finding gt done cannot record is said so
// on stderr and nothing else happens — a run with no source bead, a finding
// that is nothing but control characters, and a run with no store.
func TestDoneFindingUnrecordableWarns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		issueID string
		finding string
		open    func(beads.Client) done.SourceStoreOpener
		want    string
	}{
		{
			name:    "no source bead",
			issueID: "",
			finding: "the defect",
			want:    "resolved no source bead",
		},
		{
			name:    "control characters only",
			issueID: "gt-finding-source",
			finding: "\x00\x01\x02​",
			want:    "is empty once control characters are removed",
		},
		{
			name:    "no bead store",
			issueID: "gt-finding-source",
			finding: "the defect",
			open:    func(beads.Client) done.SourceStoreOpener { return func(string, string) beads.Client { return nil } },
			want:    "no bead store",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, db := findingsTown(t)
			var warn bytes.Buffer
			open := findingsStoreOpener(db)
			if tc.open != nil {
				open = tc.open(db)
			}

			recordDoneFindings(&warn, filepath.Join(townRoot, "gastown"), tc.issueID, []string{tc.finding}, open)

			if !strings.Contains(warn.String(), tc.want) {
				t.Errorf("warnings %q do not carry %q", warn.String(), tc.want)
			}
		})
	}
}

// TestDoneFindingWithoutFindingsWritesNothing: a run with no --finding writes
// no comment and warns about nothing.
func TestDoneFindingWithoutFindingsWritesNothing(t *testing.T) {
	t.Parallel()
	townRoot, db := findingsTown(t)
	var warn bytes.Buffer

	recordDoneFindings(&warn, filepath.Join(townRoot, "gastown"), "gt-finding-source", nil, findingsStoreOpener(db))

	if got := findingsComments(t, db); len(got) != 0 {
		t.Errorf("recorded %q without --finding", got)
	}
	if warn.Len() != 0 {
		t.Errorf("a run without findings warned: %q", warn.String())
	}
}

// TestDoneFindingFlagIsRepeatable: --finding is repeatable and each flag lands
// in doneFindings, the value runDone hands to Options.Findings, in order.
//
//testpolicy:allow parallel — doneFindings is the process-wide --finding flag variable
func TestDoneFindingFlagIsRepeatable(t *testing.T) {
	f := doneCmd.Flags().Lookup("finding")
	if f == nil {
		t.Fatal("gt done has no --finding flag")
	}
	old := doneFindings
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the flag variable the parse below set
		doneFindings = old
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			if err := sv.Replace(nil); err != nil {
				t.Errorf("reset --finding: %v", err)
			}
		}
		f.Changed = false
	})
	//testpolicy:allow no-global-swap — the --finding flag binds doneFindings; parsing it is the behaviour under test
	doneFindings = nil
	if err := doneCmd.Flags().Parse([]string{"--finding", "first", "--finding", "second"}); err != nil {
		t.Fatalf("parse --finding twice: %v", err)
	}
	if !reflect.DeepEqual(doneFindings, []string{"first", "second"}) {
		t.Fatalf("doneFindings = %q, want [first second]", doneFindings)
	}
}
