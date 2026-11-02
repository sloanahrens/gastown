package bdshim

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
)

// The write handlers' tests answer each verb through Main against the fixture
// the goldens were recorded from, compare what bd printed with what the handler
// prints, and drive each refusal the handler can make: a flag it does not
// answer, a store that refuses, bd's guard exit code and a partial close.

// TestHandlersCreate answers every create golden row.
func TestHandlersCreate(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "create")
}

// TestHandlersUpdate answers every update golden row.
func TestHandlersUpdate(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "update")
}

// TestHandlersClose answers every close golden row.
func TestHandlersClose(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "close")
}

// TestHandlersReopen answers every reopen golden row.
func TestHandlersReopen(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "reopen")
}

// TestHandlersRefuseAWriteTheyCannotMake: a flag, an operand or a value the
// goldens do not cover is refused by name before any write, so a caller never
// gets a write it did not ask for.
func TestHandlersRefuseAWriteTheyCannotMake(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "create without --json", argv: []string{"create", "--title", "x"}, want: "create without --json"},
		{name: "create with no title", argv: []string{"create", "--json"}, want: "create needs --title"},
		{name: "create refuses --description", argv: []string{"create", "--json", "--title", "x", "--description", "y"}, want: "--description"},
		{name: "create refuses an operand", argv: []string{"create", "--json", "--title", "x", "extra"}, want: "create takes no operands"},
		{name: "create refuses a type the store would misfile", argv: []string{"create", "--json", "--title", "x", "--type", "bug"}, want: "create --type bug"},
		{name: "create refuses a priority outside 0-4", argv: []string{"create", "--json", "--title", "x", "--priority", "9"}, want: "--priority"},
		{name: "update with no id", argv: []string{"update", "--status", "open"}, want: "update needs an issue id"},
		{name: "update without --status", argv: []string{"update", "fx-1"}, want: "update without --status"},
		{name: "update refuses --json", argv: []string{"update", "--json", "--status", "open", "fx-1"}, want: "--json"},
		{name: "close with no id", argv: []string{"close", "--reason", "done"}, want: "close needs an issue id"},
		{name: "close refuses --force", argv: []string{"close", "--force", "fx-1"}, want: "--force"},
		{name: "reopen with no id", argv: []string{"reopen"}, want: "reopen needs an issue id"},
		{name: "reopen refuses --reason", argv: []string{"reopen", "--reason", "x", "fx-1"}, want: "--reason"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to name %q", tc.argv, stderr.String(), tc.want)
			}
		})
	}
}

// TestHandlersRefuseAStoreThatCannotAnswer: a store the handler cannot open, or
// one that refuses the write, stops the verb rather than reporting a write that
// did not happen.
func TestHandlersRefuseAStoreThatCannotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	t.Run("no tracker endpoint", func(t *testing.T) {
		env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"create", "--json", "--title", "x"})
		env.Getenv = func(string) string { return "" }
		if code := Main(env); code != 1 {
			t.Errorf("create with no TRACKER_URL: exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "TRACKER_URL") {
			t.Errorf("create with no TRACKER_URL: stderr = %q, want it to name TRACKER_URL", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("create with no TRACKER_URL: wrote %q, want nothing", stdout.String())
		}
	})
	t.Run("a title the store refuses", func(t *testing.T) {
		// The tracker client refuses a title that opens with a dash, bd's own
		// flag-title guard, so the create answer must not be printed.
		env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"create", "--json", "--title=-not-a-title"})
		if code := Main(env); code != 1 {
			t.Errorf("create with a flag-like title: exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), beads.ErrFlagTitle.Error()) {
			t.Errorf("create with a flag-like title: stderr = %q, want the store's refusal %q", stderr.String(), beads.ErrFlagTitle)
		}
		if stdout.Len() != 0 {
			t.Errorf("create with a flag-like title: wrote %q, want nothing", stdout.String())
		}
	})
}

// TestCreateAnswersNoOwnerWithoutAnIdentity: bd stamps the git author email as
// a bead's owner, and a seat that carries none leaves the shim nothing to
// stamp. The answer then omits the member rather than printing an empty owner
// bd never wrote.
func TestCreateAnswersNoOwnerWithoutAnIdentity(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	env, stdout, _ := fixtureEnv(t, town, fixture, []string{"create", "--json", "--title", "fixture-created", "--type", "task", "--priority", "3"})
	inner := env.Getenv
	env.Getenv = func(name string) string {
		if name == ownerEmailEnv {
			return ""
		}
		return inner(name)
	}
	if code := Main(env); code != 0 {
		t.Fatalf("create without an identity: exit code is %d, want 0", code)
	}
	if strings.Contains(stdout.String(), `"owner"`) {
		t.Errorf("create without an identity: answer = %q, want no owner member", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"created_by": "bdshim-record"`) {
		t.Errorf("create without an identity: answer = %q, want the store's creator kept", stdout.String())
	}
}

// TestWriteAnswersReportAStreamTheyCannotWrite: a write that the answer cannot
// reach the caller through is the shim's error, not a silent success.
func TestWriteAnswersReportAStreamTheyCannotWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		st      writeStore
		handler func(Env, writeStore) int
	}{
		{
			name:    "update",
			st:      stubWriteStore{show: showIssue("fx-1", "open"), update: allowsUpdate},
			handler: func(env Env, st writeStore) int { return updateIssues(env, st, []string{"fx-1"}, "in_progress") },
		},
		{
			name:    "close",
			st:      stubWriteStore{close: allowsClose},
			handler: func(env Env, st writeStore) int { return closeIssues(env, st, []string{"fx-1"}, "done") },
		},
		{
			name:    "reopen",
			st:      stubWriteStore{show: showIssue("fx-1", "closed"), update: allowsUpdate},
			handler: func(env Env, st writeStore) int { return reopenIssues(env, st, []string{"fx-1"}) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stderr := writeEnv(failingWriter{})
			if code := tc.handler(env, tc.st); code != 1 {
				t.Errorf("%s with an unwritable answer: exit code is %d, want 1", tc.name, code)
			}
			if !strings.Contains(stderr.String(), "writing bd's answer") {
				t.Errorf("%s with an unwritable answer: stderr = %q, want the write failure", tc.name, stderr.String())
			}
		})
	}
}

// TestUpdateIssuesAnswersBdsGuardExitCode: bd exits 13 when every failure was a
// guard that did not hold and nothing was written, and 1 for any other refusal.
func TestUpdateIssuesAnswersBdsGuardExitCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		st   writeStore
		ids  []string
		code int
		want string // a message the shim must report
	}{
		{
			name: "a guard that did not hold",
			st:   stubWriteStore{show: showIssue("fx-1", "open"), update: failsUpdate(api.GuardFailed("fx-1 has status open"))},
			ids:  []string{"fx-1"},
			code: bdGuardExit,
			want: "guard_failed",
		},
		{
			name: "another refusal",
			st:   stubWriteStore{show: showIssue("fx-1", "open"), update: failsUpdate(api.Conflict("fx-1 is held by another seat"))},
			ids:  []string{"fx-1"},
			code: bdFailureExit,
			want: "fx-1 is held by another seat",
		},
		{
			name: "an id the store does not hold",
			st:   stubWriteStore{show: failsShow(beads.ErrNotFound)},
			ids:  []string{"fx-1"},
			code: bdFailureExit,
			want: "fx-1",
		},
		{
			name: "a guard beside another refusal",
			st: stubWriteStore{
				show: func(id string) (*beads.Issue, error) {
					if id == "fx-1" {
						return nil, beads.ErrNotFound
					}
					return &beads.Issue{ID: id, Title: "fixture bead two", Status: "open"}, nil
				},
				update: failsUpdate(api.GuardFailed("fx-2 has status open")),
			},
			ids:  []string{"fx-1", "fx-2"},
			code: bdFailureExit,
			want: "fx-2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stderr := writeEnv(&bytes.Buffer{})
			if code := updateIssues(env, tc.st, tc.ids, "in_progress"); code != tc.code {
				t.Errorf("exit code is %d, want bd's %d", code, tc.code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to hold %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestUpdateIssuesPrintsBdsLinePerUpdatedBead: bd names each bead it updated,
// and a refused bead does not stop the rest of the batch.
func TestUpdateIssuesPrintsBdsLinePerUpdatedBead(t *testing.T) {
	t.Parallel()
	st := stubWriteStore{
		show: func(id string) (*beads.Issue, error) {
			if id == "fx-2" {
				return nil, api.Conflict("fx-2 is pinned")
			}
			return &beads.Issue{ID: id, Title: "fixture bead one", Status: "open"}, nil
		},
		update: allowsUpdate,
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Args: []string{"update"}, Stdout: stdout, Stderr: stderr}
	if code := updateIssues(env, st, []string{"fx-1", "fx-2"}, "in_progress"); code != bdFailureExit {
		t.Errorf("a batch with one refusal: exit code is %d, want %d", code, bdFailureExit)
	}
	if want := "✓ Updated issue: fx-1 — fixture bead one\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "fx-2") {
		t.Errorf("stderr = %q, want it to name the refused bead", stderr.String())
	}
}

// TestCloseIssuesExitsZeroOnAPartialClose: bd closes every id it can and exits
// 0 when some of a batch closed, naming the ids it did not close.
func TestCloseIssuesExitsZeroOnAPartialClose(t *testing.T) {
	t.Parallel()
	bound := "fx-1 depends on fx-2"
	st := stubWriteStore{close: func(_ string, _ ...string) error {
		return &beads.PartialCloseError{Closed: []string{"fx-1"}, NotClosed: []string{"fx-2"}, Err: errors.New(bound)}
	}}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Args: []string{"close"}, Stdout: stdout, Stderr: stderr}
	if code := closeIssues(env, st, []string{"fx-1", "fx-2"}, "fixture-close"); code != 0 {
		t.Errorf("partial close: exit code is %d, want bd's 0", code)
	}
	if want := "✓ Closed fx-1: fixture-close\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "fx-2") || !strings.Contains(stderr.String(), bound) {
		t.Errorf("stderr = %q, want it to name the id that stayed open and why", stderr.String())
	}
}

// TestCloseIssuesReportsARefusalWhenTheAnswerCannotBeWritten: a partial close
// whose answer stream refuses the write still names the ids that stayed open
// and why, and exits 1 rather than reporting a write that did not happen.
func TestCloseIssuesReportsARefusalWhenTheAnswerCannotBeWritten(t *testing.T) {
	t.Parallel()
	bound := "fx-1 depends on fx-2"
	st := stubWriteStore{close: func(_ string, _ ...string) error {
		return &beads.PartialCloseError{Closed: []string{"fx-1"}, NotClosed: []string{"fx-2"}, Err: errors.New(bound)}
	}}
	env, stderr := writeEnv(failingWriter{})
	if code := closeIssues(env, st, []string{"fx-1", "fx-2"}, "fixture-close"); code != 1 {
		t.Errorf("partial close with an unwritable answer: exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "fx-2") || !strings.Contains(stderr.String(), bound) {
		t.Errorf("stderr = %q, want it to name the id that stayed open and why", stderr.String())
	}
}

// TestCloseIssuesAnswersBdsExitCodes: a batch where nothing closed is a
// failure, and a guard that did not hold is bd's guard exit code.
func TestCloseIssuesAnswersBdsExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		code int
		want string
	}{
		{name: "every id refused", err: api.Conflict("fx-1 has an open child"), code: bdFailureExit, want: "fx-1"},
		{name: "a guard that did not hold", err: api.GuardFailed("fx-1 has status open"), code: bdGuardExit, want: "guard_failed"},
		{name: "a partial close that closed nothing", err: &beads.PartialCloseError{NotClosed: []string{"fx-1"}}, code: bdFailureExit, want: "close refused"},
		{name: "a partial close carrying a guard", err: &beads.PartialCloseError{NotClosed: []string{"fx-1"}, Err: api.GuardFailed("fx-1 is held")}, code: bdGuardExit, want: "guard_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := stubWriteStore{close: func(_ string, _ ...string) error { return tc.err }}
			env, stderr := writeEnv(&bytes.Buffer{})
			code := closeIssues(env, st, []string{"fx-1"}, "done")
			if code != tc.code {
				t.Errorf("exit code is %d, want %d", code, tc.code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to hold %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestReopenIssuesLeavesAnOpenBeadAlone: bd reports nothing for a bead that is
// already open (the recorded golden), and reopens one that is not.
func TestReopenIssuesLeavesAnOpenBeadAlone(t *testing.T) {
	t.Parallel()
	updates := 0
	st := stubWriteStore{
		show: func(id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, Status: map[string]string{"fx-open": "open", "fx-closed": "closed"}[id]}, nil
		},
		update: func(string, beads.UpdateOptions) error { updates++; return nil },
	}
	stdout := &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if code := reopenIssues(env, st, []string{"fx-open", "fx-closed"}); code != 0 {
		t.Fatalf("reopen: exit code is %d, want 0", code)
	}
	if updates != 1 {
		t.Errorf("reopen wrote %d times, want one write for the closed bead only", updates)
	}
	if want := "✓ Reopened fx-closed\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestReopenIssuesAnswersBdsExitCodes: a reopen bd cannot make is a failure.
func TestReopenIssuesAnswersBdsExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		st   writeStore
		code int
	}{
		{name: "an id the store does not hold", st: stubWriteStore{show: failsShow(beads.ErrNotFound)}, code: bdFailureExit},
		{name: "a write the store refuses", st: stubWriteStore{show: showIssue("fx-1", "closed"), update: failsUpdate(api.Conflict("fx-1 is held"))}, code: bdFailureExit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stderr := writeEnv(&bytes.Buffer{})
			if code := reopenIssues(env, tc.st, []string{"fx-1"}); code != tc.code {
				t.Errorf("exit code is %d, want %d", code, tc.code)
			}
			if stderr.Len() == 0 {
				t.Error("the refusal was not reported")
			}
		})
	}
}

// writeEnv is the process surface a write helper reads: the answer on stdout
// and the shim's own messages on stderr.
func writeEnv(stdout io.Writer) (Env, *bytes.Buffer) {
	stderr := &bytes.Buffer{}
	return Env{Stdout: stdout, Stderr: stderr}, stderr
}

// stubWriteStore is a writeStore whose every method is the test's choice, so a
// refusal is driven without a tracker server. A method the test never expected
// to run fails the test rather than answering.
type stubWriteStore struct {
	create func(beads.CreateOptions) (*beads.Issue, error)
	show   func(string) (*beads.Issue, error)
	update func(string, beads.UpdateOptions) error
	close  func(string, ...string) error
}

func (s stubWriteStore) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	if s.create == nil {
		panic("the handler created a bead the test did not expect")
	}
	return s.create(opts)
}

func (s stubWriteStore) Show(id string) (*beads.Issue, error) { return s.show(id) }

func (s stubWriteStore) Update(id string, opts beads.UpdateOptions) error {
	if s.update == nil {
		panic("the handler updated a bead the test did not expect")
	}
	return s.update(id, opts)
}

func (s stubWriteStore) CloseWithReason(reason string, ids ...string) error {
	if s.close == nil {
		panic("the handler closed beads the test did not expect")
	}
	return s.close(reason, ids...)
}

// showIssue answers one issue in status.
func showIssue(id, status string) func(string) (*beads.Issue, error) {
	return func(got string) (*beads.Issue, error) {
		if got != id {
			return nil, beads.ErrNotFound
		}
		return &beads.Issue{ID: id, Title: "fixture bead one", Status: status}, nil
	}
}

// failsShow is a read the store refuses.
func failsShow(err error) func(string) (*beads.Issue, error) {
	return func(string) (*beads.Issue, error) { return nil, err }
}

// allowsUpdate is a write the store accepts.
func allowsUpdate(string, beads.UpdateOptions) error { return nil }

// allowsClose is a close the store accepts.
func allowsClose(string, ...string) error { return nil }

// failsUpdate is a write the store refuses with err.
func failsUpdate(err error) func(string, beads.UpdateOptions) error {
	return func(string, beads.UpdateOptions) error { return err }
}
