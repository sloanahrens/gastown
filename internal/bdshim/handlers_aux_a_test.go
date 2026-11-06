package bdshim

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The auxiliary handlers' tests answer each verb through Main against the
// fixture the goldens were recorded from, compare what bd printed with what the
// handler prints, and drive each refusal the handlers can make: a flag they do
// not answer, a subcommand they do not answer, a missing operand, a store that
// does not hold the id, a store that refuses the write, and a stream that
// refuses the answer.

// TestHandlersComments answers every comments golden row.
func TestHandlersComments(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "comments")
}

// TestHandlersLabel answers every label golden row.
func TestHandlersLabel(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "label")
}

// TestHandlersRefuseAnAuxCommandTheyCannotAnswer: a flag, a subcommand or an
// operand the goldens do not cover is refused by name before any read or
// write, so a caller never gets an answer wider than it asked for.
func TestHandlersRefuseAnAuxCommandTheyCannotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "comments refuses --author", argv: []string{"comments", "--json", "--author", "fixture", "fx-1"}, want: "--author"},
		{name: "comments without --json", argv: []string{"comments", "fx-1"}, want: "comments without --json"},
		{name: "comments with no id", argv: []string{"comments", "--json"}, want: "comments needs an issue id"},
		{name: "comments with two ids", argv: []string{"comments", "--json", "fx-1", "fx-2"}, want: "comments takes one issue id"},
		{name: "comments add is refused by name", argv: []string{"comments", "add", "--author", "fixture", "fx-1"}, want: "comments add is not implemented"},
		{name: "label remove is refused by name", argv: []string{"label", "remove", "fx-1", "fixture-label"}, want: "label remove is not implemented"},
		{name: "a bare label is refused by name", argv: []string{"label"}, want: "label is not implemented"},
		{name: "label list refuses --json=false", argv: []string{"label", "list", "--json=false", "fx-1"}, want: "--json takes no value"},
		{name: "label list without --json", argv: []string{"label", "list", "fx-1"}, want: "label list without --json"},
		{name: "label list with no id", argv: []string{"label", "list", "--json"}, want: "label list needs an issue id"},
		{name: "label list with two ids", argv: []string{"label", "list", "--json", "fx-1", "fx-2"}, want: "label list takes one issue id"},
		// bd's operand table gives the last operand to the label slot, so a
		// command with one operand names a label and no issue.
		{name: "label add with only a label", argv: []string{"label", "add", "fixture-label"}, want: "label add needs an issue id"},
		{name: "label add with an empty label", argv: []string{"label", "add", "fx-1", ""}, want: "label add needs a label"},
		{name: "label add refuses --json", argv: []string{"label", "add", "--json", "fx-1", "fixture-label"}, want: "--json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to hold %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing before a refusal", tc.argv, stdout.String())
			}
		})
	}
}

// TestHandlersRefuseAnAuxCommandWhoseStoreCannotAnswer: a store that cannot be
// opened, or one that does not hold the id, stops the verb rather than printing
// an answer or a write line for work that did not happen.
func TestHandlersRefuseAnAuxCommandWhoseStoreCannotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	for _, argv := range [][]string{
		{"comments", "--json", "fx-9"},
		{"label", "list", "--json", "fx-9"},
		{"label", "add", "fx-9", "a-label"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", argv, code)
			}
			if stderr.Len() == 0 {
				t.Errorf("%v: stderr is empty, want the shim's refusal", argv)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing for an id the store does not hold", argv, stdout.String())
			}
		})
	}
	t.Run("no tracker endpoint", func(t *testing.T) {
		t.Parallel()
		env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"comments", "--json", "fx-1"})
		env.Getenv = func(string) string { return "" }
		if code := Main(env); code != 1 {
			t.Errorf("comments with no TRACKER_URL: exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "TRACKER_URL") {
			t.Errorf("comments with no TRACKER_URL: stderr = %q, want it to name TRACKER_URL", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("comments with no TRACKER_URL: wrote %q, want nothing", stdout.String())
		}
	})
}

// TestLabelListAnswersNoLabelsAsAnEmptyArray: an issue with no labels answers
// bd's empty array, not a null the caller would have to tell apart.
func TestLabelListAnswersNoLabelsAsAnEmptyArray(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"label", "list", "--json", "fx-2"})
	if code := Main(env); code != 0 {
		t.Fatalf("label list fx-2: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "{\n  \"data\": [],\n  \"schema_version\": 1\n}\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestLabelAddWritesTheLabelItReports: the line the handler prints is the write
// it made, so the label is in the store when it is printed.
func TestLabelAddWritesTheLabelItReports(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	env, stdout, stderr, st := fixtureEnvWithStore(t, town, fixture, []string{"label", "add", "fx-2", "added-by-test"})
	if code := Main(env); code != 0 {
		t.Fatalf("label add: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "✓ Added label 'added-by-test' to fx-2\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	is, err := st.GetIssues(t.Context(), []string{"fx-2"})
	if err != nil {
		t.Fatalf("reading fx-2 back: %v", err)
	}
	if labels := is["fx-2"].Labels; len(labels) != 1 || labels[0] != "added-by-test" {
		t.Errorf("fx-2 labels = %v, want [added-by-test]", labels)
	}
}

// TestAddLabelsReportsAnAnswerItCannotWrite: bd's line for each label is the
// write's receipt, so a stream that refuses it fails the verb.
func TestAddLabelsReportsAnAnswerItCannotWrite(t *testing.T) {
	t.Parallel()
	st := stubAuxLabelStore{update: func(string, beads.UpdateOptions) error { return nil }}
	stderr := &bytes.Buffer{}
	env := Env{Stdout: failingWriter{}, Stderr: stderr}
	if code := addLabels(env, st, []string{"fx-1"}, []string{"a"}); code != 1 {
		t.Errorf("exit code is %d, want 1 for a write that failed", code)
	}
	if !strings.Contains(stderr.String(), "writing bd's answer") {
		t.Errorf("stderr = %q, want the write failure named", stderr.String())
	}
}

// TestAddLabelsReportsEachRefusedIdAndKeepsGoing: bd writes the ids one at a
// time, so one the store refuses is reported, the rest still get their line,
// and the verb exits 1.
func TestAddLabelsReportsEachRefusedIdAndKeepsGoing(t *testing.T) {
	t.Parallel()
	st := stubAuxLabelStore{update: func(id string, _ beads.UpdateOptions) error {
		if id == "fx-9" {
			return errors.New("refused")
		}
		return nil
	}}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := addLabels(env, st, []string{"fx-9", "fx-1"}, []string{"a"}); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if !strings.Contains(stderr.String(), "fx-9") {
		t.Errorf("stderr = %q, want the refused id named", stderr.String())
	}
	if want := "✓ Added label 'a' to fx-1\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestAddLabelsRefusesAnEmptyLabelSet: a call naming no label writes nothing
// and exits 1, so it is never reported as a label the caller added.
func TestAddLabelsRefusesAnEmptyLabelSet(t *testing.T) {
	t.Parallel()
	st := stubAuxLabelStore{update: func(string, beads.UpdateOptions) error {
		t.Error("a label set the call did not name was written")
		return nil
	}}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := addLabels(env, st, []string{"fx-1"}, nil); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "label add needs a label") {
		t.Errorf("stderr = %q, want it to say a label is missing", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

// stubAuxLabelStore is a store whose label write a test scripts.
type stubAuxLabelStore struct {
	show   func(string) (*beads.Issue, error)
	update func(string, beads.UpdateOptions) error
}

func (s stubAuxLabelStore) Show(id string) (*beads.Issue, error) {
	if s.show == nil {
		return nil, errors.New("stubAuxLabelStore: Show was not scripted")
	}
	return s.show(id)
}

func (s stubAuxLabelStore) Update(id string, opts beads.UpdateOptions) error {
	return s.update(id, opts)
}
