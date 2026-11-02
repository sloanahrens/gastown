package bdshim

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The dep handler's tests answer the two dep golden rows through Main, compare
// each answer with bd's, and drive every refusal the handler can make: a
// subcommand it does not answer, a cross-rig target, a flag, a wrong operand
// count, a store that cannot be opened, a bead the store cannot answer, and a
// stream that refuses the answer.

// TestHandlersDep answers every dep golden row the shim answers as bd does: dep
// add and dep list --direction down --json.
func TestHandlersDep(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "dep")
}

// TestHandlersDepRefusesTheCommandsItDoesNotAnswer: a dep subcommand the shim
// cannot answer, a cross-rig target, and a flag, direction or operand a golden
// row does not cover are refused by name before the store is opened, with
// nothing on stdout and exit 1.
func TestHandlersDepRefusesTheCommandsItDoesNotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "dep remove is refused by name", argv: []string{"dep", "remove", "fx-1", "fx-2"}, want: "dep remove is not implemented for tracker rig fx-"},
		{name: "dep rm is named as the command it aliases", argv: []string{"dep", "rm", "fx-1", "fx-2"}, want: "dep remove is not implemented for tracker rig fx-"},
		{name: "dep tree is refused by name", argv: []string{"dep", "tree", "fx-1"}, want: "dep tree is not implemented for tracker rig fx-"},
		{name: "dep cycles is refused by name", argv: []string{"dep", "cycles"}, want: "dep cycles is not implemented for tracker rig fx-"},
		{name: "dep relate is refused by name", argv: []string{"dep", "relate", "fx-1", "fx-2"}, want: "dep relate is not implemented for tracker rig fx-"},
		{name: "an undocumented subcommand is named", argv: []string{"dep", "export"}, want: "dep export is not implemented for tracker rig fx-"},
		{name: "a bare dep is refused by name", argv: []string{"dep"}, want: "dep is not implemented for tracker rig fx-"},
		{name: "a cross-rig target is refused by name", argv: []string{"dep", "add", "fx-1", "gt-1"}, want: "dep add of a cross-rig target gt-1 is not implemented for tracker rig fx-"},
		{name: "an external target is refused by name", argv: []string{"dep", "add", "fx-1", "external:gt:gt-1"}, want: "dep add of a cross-rig target external:gt:gt-1 is not implemented for tracker rig fx-"},
		{name: "a target with no prefix is cross-rig", argv: []string{"dep", "add", "fx-1", "loose"}, want: "dep add of a cross-rig target loose is not implemented for tracker rig fx-"},
		{name: "dep add with no ids", argv: []string{"dep", "add"}, want: "dep add needs an issue id and a depends-on id"},
		{name: "dep add with one id", argv: []string{"dep", "add", "fx-1"}, want: "dep add needs a depends-on id"},
		{name: "dep add with three ids", argv: []string{"dep", "add", "fx-1", "fx-2", "fx-parent"}, want: "dep add takes two ids"},
		{name: "dep add refuses --type", argv: []string{"dep", "add", "--type", "tracks", "fx-1", "fx-2"}, want: "flag --type is not implemented for tracker rig fx-"},
		{name: "dep add refuses --blocked-by", argv: []string{"dep", "add", "--blocked-by", "fx-2", "fx-1"}, want: "flag --blocked-by is not implemented for tracker rig fx-"},
		{name: "dep list refuses --type", argv: []string{"dep", "list", "--direction", "down", "--json", "--type", "blocks", "fx-1"}, want: "flag --type is not implemented for tracker rig fx-"},
		{name: "dep list without --json", argv: []string{"dep", "list", "--direction", "down", "fx-1"}, want: "dep list without --json"},
		{name: "dep list without a direction", argv: []string{"dep", "list", "--json", "fx-1"}, want: "dep list without --direction down"},
		{name: "dep list with direction up", argv: []string{"dep", "list", "--direction", "up", "--json", "fx-1"}, want: "dep list --direction up is not implemented for tracker rig fx-"},
		{name: "dep list with no id", argv: []string{"dep", "list", "--direction", "down", "--json"}, want: "dep list needs an issue id"},
		{name: "dep list with two ids", argv: []string{"dep", "list", "--direction", "down", "--json", "fx-1", "fx-2"}, want: "dep list takes one issue id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to contain %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", tc.argv, stdout.String())
			}
		})
	}
}

// TestHandlersDepRefusesAStoreItCannotOpen: a rig with no tracker endpoint
// cannot answer either subcommand, and the refusal names what is missing rather
// than printing an empty answer or a link it did not write.
func TestHandlersDepRefusesAStoreItCannotOpen(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	for _, argv := range [][]string{
		{"dep", "add", "fx-1", "fx-2"},
		{"dep", "list", "--direction", "down", "--json", "fx-1"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, argv)
			env.Getenv = func(string) string { return "" }
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", argv, code)
			}
			if !strings.Contains(stderr.String(), "TRACKER_URL") {
				t.Errorf("%v: stderr = %q, want it to name TRACKER_URL", argv, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", argv, stdout.String())
			}
		})
	}
}

// TestHandlersDepAddAnswersAsBd: the link is written and bd's line printed, with
// both beads' titles.
func TestHandlersDepAddAnswersAsBd(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	var written [2]string
	st := stubAuxDepStore{
		showMany: func(ids []string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{
				"fx-1": {ID: "fx-1", Title: "fixture bead one"},
				"fx-2": {ID: "fx-2", Title: "fixture bead two"},
			}, nil
		},
		add: func(issue, dependsOn string) error {
			written = [2]string{issue, dependsOn}
			return nil
		},
	}
	if code := addDep(env, st, "fx-1", "fx-2"); code != 0 {
		t.Fatalf("exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "✓ Added dependency: fx-1 (fixture bead one) depends on fx-2 (fixture bead two) (blocks)\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if written != [2]string{"fx-1", "fx-2"} {
		t.Errorf("wrote the link %v, want fx-1 depends on fx-2", written)
	}
}

// TestHandlersDepAddRefusesABeadTheStoreDoesNotHold: a bead the store cannot
// answer is refused before the write, so a link is never created for a bead the
// answer cannot name.
func TestHandlersDepAddRefusesABeadTheStoreDoesNotHold(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	written := false
	st := stubAuxDepStore{
		showMany: func([]string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{"fx-1": {ID: "fx-1", Title: "fixture bead one"}}, nil
		},
		add: func(string, string) error { written = true; return nil },
	}
	if code := addDep(env, st, "fx-1", "fx-2"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if written {
		t.Error("wrote the link for a bead the store did not answer")
	}
	if !strings.Contains(stderr.String(), "fx-2") {
		t.Errorf("stderr = %q, want it to name the missing bead", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing", stdout.String())
	}
}

// TestHandlersDepAddReportsAReadItCannotMake: a store that fails the read has
// not answered, and the shim reports the failure rather than adding the link.
func TestHandlersDepAddReportsAReadItCannotMake(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxDepStore{showMany: func([]string) (map[string]*beads.Issue, error) {
		return nil, errors.New("store unavailable")
	}}
	if code := addDep(env, st, "fx-1", "fx-2"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "store unavailable") {
		t.Errorf("stderr = %q, want the store's error reported", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing", stdout.String())
	}
}

// TestHandlersDepAddReportsAWriteThatFailed: bd's line is printed only after
// the store kept the link, so a failed write exits 1 and prints nothing.
func TestHandlersDepAddReportsAWriteThatFailed(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxDepStore{
		showMany: func([]string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{
				"fx-1": {ID: "fx-1", Title: "fixture bead one"},
				"fx-2": {ID: "fx-2", Title: "fixture bead two"},
			}, nil
		},
		add: func(string, string) error { return errors.New("write refused") },
	}
	if code := addDep(env, st, "fx-1", "fx-2"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "write refused") {
		t.Errorf("stderr = %q, want the store's error reported", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing for a link that was not written", stdout.String())
	}
}

// TestHandlersDepAddReportsAnAnswerItCannotWrite: a stream that refuses bd's
// line fails the verb rather than passing as a write that was reported.
func TestHandlersDepAddReportsAnAnswerItCannotWrite(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	env := Env{Stdout: failingWriter{}, Stderr: stderr}
	st := stubAuxDepStore{
		showMany: func([]string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{
				"fx-1": {ID: "fx-1", Title: "fixture bead one"},
				"fx-2": {ID: "fx-2", Title: "fixture bead two"},
			}, nil
		},
		add: func(string, string) error { return nil },
	}
	if code := addDep(env, st, "fx-1", "fx-2"); code != 1 {
		t.Errorf("exit code is %d, want 1 for an answer that was not written", code)
	}
	if !strings.Contains(stderr.String(), "writing bd's answer") {
		t.Errorf("stderr = %q, want the write failure named", stderr.String())
	}
}

// TestHandlersDepListReportsAReadItCannotMake: a store that cannot answer the
// issue has not said it depends on nothing, so the shim refuses.
func TestHandlersDepListReportsAReadItCannotMake(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxDepStore{show: func(string) (*beads.Issue, error) {
		return nil, fmt.Errorf("dep list: %w", beads.ErrNotFound)
	}}
	if code := listDeps(env, st, "fx-1"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing for a store that did not answer", stdout.String())
	}
	if !strings.Contains(stderr.String(), "fx-1") {
		t.Errorf("stderr = %q, want it to name the issue", stderr.String())
	}
}

// TestHandlersDepListRefusesALinkWithNoTarget: bd prints each row's fields from
// the target bead, so a link whose target the store did not answer is refused
// rather than printed with empty fields.
func TestHandlersDepListRefusesALinkWithNoTarget(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxDepStore{
		show: func(string) (*beads.Issue, error) {
			return &beads.Issue{ID: "fx-1", Dependencies: []beads.IssueDep{{ID: "fx-2", DependencyType: "blocks"}}}, nil
		},
		showMany: func(ids []string) (map[string]*beads.Issue, error) {
			if len(ids) != 1 || ids[0] != "fx-2" {
				t.Errorf("read targets %v, want fx-2", ids)
			}
			return map[string]*beads.Issue{}, nil
		},
	}
	if code := listDeps(env, st, "fx-1"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "did not answer the bead fx-2") {
		t.Errorf("stderr = %q, want the missing target named", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing", stdout.String())
	}
}

// TestHandlersDepListReportsAnAnswerItCannotWrite: a stream that refuses the
// answer fails the verb.
func TestHandlersDepListReportsAnAnswerItCannotWrite(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	env := Env{Stdout: failingWriter{}, Stderr: stderr}
	st := stubAuxDepStore{
		show: func(string) (*beads.Issue, error) {
			return &beads.Issue{ID: "fx-1", Dependencies: []beads.IssueDep{{ID: "fx-2", DependencyType: "blocks"}}}, nil
		},
		showMany: func([]string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{"fx-2": {ID: "fx-2", Title: "two"}}, nil
		},
	}
	if code := listDeps(env, st, "fx-1"); code != 1 {
		t.Errorf("exit code is %d, want 1 for an answer that was not written", code)
	}
	if !strings.Contains(stderr.String(), "writing bd's answer") {
		t.Errorf("stderr = %q, want the write failure named", stderr.String())
	}
}

// stubAuxDepStore is a dependency store a test scripts.
type stubAuxDepStore struct {
	show     func(string) (*beads.Issue, error)
	showMany func([]string) (map[string]*beads.Issue, error)
	add      func(issue, dependsOn string) error
}

func (s stubAuxDepStore) Show(id string) (*beads.Issue, error) {
	if s.show == nil {
		return nil, errors.New("stubAuxDepStore: Show was not scripted")
	}
	return s.show(id)
}

func (s stubAuxDepStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.showMany == nil {
		return nil, errors.New("stubAuxDepStore: ShowMultiple was not scripted")
	}
	return s.showMany(ids)
}

func (s stubAuxDepStore) AddDependency(issue, dependsOn string) error {
	if s.add == nil {
		return errors.New("stubAuxDepStore: AddDependency was not scripted")
	}
	return s.add(issue, dependsOn)
}
