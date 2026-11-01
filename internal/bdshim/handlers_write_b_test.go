package bdshim

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The second write handlers' tests answer each verb through Main against the
// fixture the goldens were recorded from, compare what bd printed with what the
// handler prints, and drive each refusal the handlers can make: a flag they do
// not answer, a store that does not hold the id, a store that refuses the write,
// a stream that refuses the answer, and the batches where one id fails and the
// rest still run.

// TestHandlersDelete answers every delete golden row.
func TestHandlersDelete(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "delete")
}

// TestHandlersDefer answers every defer golden row.
func TestHandlersDefer(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "defer")
}

// TestHandlersUndefer answers every undefer golden row.
func TestHandlersUndefer(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "undefer")
}

// TestHandlersUnclaim answers every unclaim golden row.
func TestHandlersUnclaim(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "unclaim")
}

// TestHandlersRefuseDeleteDeferUndeferUnclaim: a flag, an operand or a missing
// id the goldens do not cover is refused by name before any write, so a caller
// never gets a write it did not ask for.
func TestHandlersRefuseDeleteDeferUndeferUnclaim(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "delete without --force", argv: []string{"delete", "fx-1"}, want: "delete without --force"},
		{name: "delete refuses --cascade", argv: []string{"delete", "--cascade", "--force", "fx-1"}, want: "--cascade"},
		{name: "delete refuses --dry-run", argv: []string{"delete", "--dry-run", "--force", "fx-1"}, want: "--dry-run"},
		{name: "delete with no id", argv: []string{"delete", "--force"}, want: "delete needs an issue id"},
		{name: "defer refuses --until", argv: []string{"defer", "--until", "tomorrow", "fx-1"}, want: "--until"},
		{name: "defer with no id", argv: []string{"defer", "--reason", "later"}, want: "defer needs an issue id"},
		{name: "undefer refuses --reason", argv: []string{"undefer", "--reason", "later", "fx-1"}, want: "--reason"},
		{name: "undefer with no id", argv: []string{"undefer"}, want: "undefer needs an issue id"},
		{name: "unclaim refuses --force", argv: []string{"unclaim", "--force", "fx-1"}, want: "--force"},
		{name: "unclaim refuses --reason", argv: []string{"unclaim", "--reason", "gone", "fx-1"}, want: "--reason"},
		{name: "unclaim with no id", argv: []string{"unclaim"}, want: "unclaim needs an issue id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to name %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing before a refusal", tc.argv, stdout.String())
			}
		})
	}
}

// TestWriteBHandlersRefuseAStoreThatCannotBeOpened: a rig with no tracker
// endpoint is a store the handler cannot open, and the verb stops rather than
// reporting a write that never reached a store.
func TestWriteBHandlersRefuseAStoreThatCannotBeOpened(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
	}{
		{name: "delete", argv: []string{"delete", "--force", "fx-1"}},
		{name: "defer", argv: []string{"defer", "fx-1"}},
		{name: "undefer", argv: []string{"undefer", "fx-1"}},
		{name: "unclaim", argv: []string{"unclaim", "fx-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			env.Getenv = func(string) string { return "" }
			if code := Main(env); code != 1 {
				t.Errorf("%s with no TRACKER_URL: exit code is %d, want 1", tc.name, code)
			}
			if !strings.Contains(stderr.String(), "TRACKER_URL") {
				t.Errorf("%s with no TRACKER_URL: stderr = %q, want it to name TRACKER_URL", tc.name, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("%s with no TRACKER_URL: wrote %q, want nothing", tc.name, stdout.String())
			}
		})
	}
}

// TestDeleteRefusesAnIDTheStoreDoesNotHold: the batch is all-or-nothing, so an
// id the store cannot read stops the whole delete before anything is written.
func TestDeleteRefusesAnIDTheStoreDoesNotHold(t *testing.T) {
	t.Parallel()
	deleted := false
	st := stubDeleteStore{
		show: func(id string) (*beads.Issue, error) {
			if id == "fx-1" {
				return &beads.Issue{ID: id}, nil
			}
			return nil, beads.ErrNotFound
		},
		delete: func(...string) error { deleted = true; return nil },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := deleteIssues(env, st, []string{"fx-1", "fx-missing"}); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if deleted {
		t.Error("a batch naming a missing id deleted the ids it could read")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused batch", stdout.String())
	}
	if !strings.Contains(stderr.String(), "fx-missing") {
		t.Errorf("stderr = %q, want it to name the id the store does not hold", stderr.String())
	}
}

// TestDeleteMarksTheReferencesOfTheLinkedIssues: bd marks the text references
// to a deleted id in the issues a link joins to it, and the answer counts the
// issues it changed. The delete runs after the marking, so an issue the caller
// never reached is not deleted with its references untouched.
func TestDeleteMarksTheReferencesOfTheLinkedIssues(t *testing.T) {
	t.Parallel()
	updates := map[string]beads.UpdateOptions{}
	var order []string
	st := stubDeleteStore{
		show: func(id string) (*beads.Issue, error) {
			return &beads.Issue{
				ID:           id,
				Dependencies: []beads.IssueDep{{ID: "fx-2"}, {ID: "fx-parent"}},
			}, nil
		},
		showMultiple: func(ids []string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{
				"fx-2":      {ID: "fx-2", Description: "The bead fx-1 waits on."},
				"fx-parent": {ID: "fx-parent", Description: "An issue about fx-10, not fx-1."},
			}, nil
		},
		update: func(id string, opts beads.UpdateOptions) error {
			updates[id] = opts
			order = append(order, id)
			return nil
		},
		delete: func(ids ...string) error {
			if len(ids) != 1 || ids[0] != "fx-1" {
				t.Errorf("deleted %v, want only fx-1", ids)
			}
			return nil
		},
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := deleteIssues(env, st, []string{"fx-1"}); code != 0 {
		t.Fatalf("delete: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if len(order) != 2 {
		t.Fatalf("updated %v, want fx-2 and fx-parent", order)
	}
	got := ""
	if opts := updates["fx-2"]; opts.Description != nil {
		got = *opts.Description
	}
	if want := "The bead [deleted:fx-1] waits on."; got != want {
		t.Errorf("fx-2 description = %q, want %q", got, want)
	}
	// fx-10 is a different id and keeps its text, so the parent issue is only
	// marked for the reference it really holds.
	got = ""
	if opts := updates["fx-parent"]; opts.Description != nil {
		got = *opts.Description
	}
	if want := "An issue about fx-10, not [deleted:fx-1]."; got != want {
		t.Errorf("fx-parent description = %q, want %q", got, want)
	}
	want := "✓ Deleted fx-1\n  Removed 2 dependency link(s)\n  Updated text references in 2 issue(s)\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestDeleteSaysWhenNoLinkedIssueNamesTheBatch: an id nothing refers to is a
// delete bd reports without a count, so the handler says that rather than
// printing a zero the golden's shape does not have.
func TestDeleteSaysWhenNoLinkedIssueNamesTheBatch(t *testing.T) {
	t.Parallel()
	st := stubDeleteStore{
		show: func(id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, Dependents: []beads.IssueDep{{ID: "fx-2"}}}, nil
		},
		showMultiple: func([]string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{"fx-2": {ID: "fx-2", Description: "nothing to see"}}, nil
		},
		delete: func(...string) error { return nil },
	}
	stdout := &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if code := deleteIssues(env, st, []string{"fx-1"}); code != 0 {
		t.Fatalf("delete: exit code is %d, want 0", code)
	}
	want := "✓ Deleted fx-1\n  Removed 1 dependency link(s)\n  (none have text references)\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestDeleteRefusesAStoreThatCannotMarkTheReferences: a delete whose marking
// failed must not go on to remove the issue, or the caller reads a delete of
// references the store never marked.
func TestDeleteRefusesAStoreThatCannotMarkTheReferences(t *testing.T) {
	t.Parallel()
	deleted := false
	st := stubDeleteStore{
		show: func(id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, Dependencies: []beads.IssueDep{{ID: "fx-2"}}}, nil
		},
		showMultiple: func([]string) (map[string]*beads.Issue, error) {
			return map[string]*beads.Issue{"fx-2": {ID: "fx-2", Description: "fx-1"}}, nil
		},
		update: func(string, beads.UpdateOptions) error { return errors.New("the store is down") },
		delete: func(...string) error { deleted = true; return nil },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := deleteIssues(env, st, []string{"fx-1"}); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if deleted {
		t.Error("the delete ran although the references could not be marked")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused delete", stdout.String())
	}
	if !strings.Contains(stderr.String(), "fx-1") {
		t.Errorf("stderr = %q, want the shim's message", stderr.String())
	}
}

// TestDeleteRefusesAStoreThatCannotReadTheLinkedIssues: a linked-issue read
// the store refuses is a failure, and the delete does not run past it, so no
// issue is removed with its references unmarked.
func TestDeleteRefusesAStoreThatCannotReadTheLinkedIssues(t *testing.T) {
	t.Parallel()
	deleted := false
	st := stubDeleteStore{
		show: func(id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, Dependencies: []beads.IssueDep{{ID: "fx-2"}}}, nil
		},
		showMultiple: func([]string) (map[string]*beads.Issue, error) {
			return nil, errors.New("the store is down")
		},
		delete: func(...string) error { deleted = true; return nil },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := deleteIssues(env, st, []string{"fx-1"}); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if deleted {
		t.Error("the delete ran although the linked issues could not be read")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused delete", stdout.String())
	}
	if !strings.Contains(stderr.String(), "fx-1") {
		t.Errorf("stderr = %q, want the shim's message", stderr.String())
	}
}

// TestDeleteRefusesAStoreThatCannotDelete: a delete the store refuses is a
// failure, and nothing of bd's answer is printed for it.
func TestDeleteRefusesAStoreThatCannotDelete(t *testing.T) {
	t.Parallel()
	st := stubDeleteStore{
		show: func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
		delete: func(...string) error {
			return errors.New("the store is down")
		},
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := deleteIssues(env, st, []string{"fx-1"}); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused delete", stdout.String())
	}
	if !strings.Contains(stderr.String(), "the store is down") {
		t.Errorf("stderr = %q, want the store's refusal", stderr.String())
	}
}

// TestDeferAppendsTheReasonToTheNotes: bd's --reason is appended to the notes in
// the same write that defers the issue, and the answer names the issue.
func TestDeferAppendsTheReasonToTheNotes(t *testing.T) {
	t.Parallel()
	var got beads.UpdateOptions
	st := stubClaimStore{update: func(_ string, opts beads.UpdateOptions) error { got = opts; return nil }}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := deferIssues(env, st, []string{"fx-1"}, "waiting on the API"); code != 0 {
		t.Fatalf("defer: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got.Status == nil || *got.Status != deferredStatus {
		t.Errorf("status written = %v, want %q", got.Status, deferredStatus)
	}
	if got.AppendNotes != "waiting on the API" {
		t.Errorf("notes appended = %q, want the reason", got.AppendNotes)
	}
	if want := "* Deferred fx-1\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestDeferReportsARefusedIssue: a defer the store refuses is reported and
// named, exits 1, and is not answered as a defer that happened.
func TestDeferReportsARefusedIssue(t *testing.T) {
	t.Parallel()
	st := stubClaimStore{update: func(string, beads.UpdateOptions) error { return beads.ErrNotFound }}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := deferIssues(env, st, []string{"fx-missing"}, ""); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused defer", stdout.String())
	}
	if !strings.Contains(stderr.String(), "fx-missing") {
		t.Errorf("stderr = %q, want it to name the refused id", stderr.String())
	}
}

// TestUndeferReopensADeferredIssue: a deferred issue goes back to open and the
// answer says so.
func TestUndeferReopensADeferredIssue(t *testing.T) {
	t.Parallel()
	var got beads.UpdateOptions
	st := stubClaimStore{
		show: func(id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, Status: deferredStatus}, nil
		},
		update: func(_ string, opts beads.UpdateOptions) error { got = opts; return nil },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := undeferIssues(env, st, []string{"fx-1"}, soon()); code != 0 {
		t.Fatalf("undefer: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got.Status == nil || *got.Status != openStatus {
		t.Errorf("status written = %v, want %q", got.Status, openStatus)
	}
	if want := "* Undeferred fx-1 (now open)\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestUndeferRefusesAnIssueThatIsNotDeferred: bd exits 1 and prints nothing for
// an issue that is not deferred (the golden), so the handler must not write.
func TestUndeferRefusesAnIssueThatIsNotDeferred(t *testing.T) {
	t.Parallel()
	wrote := false
	st := stubClaimStore{
		show: func(id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, Status: openStatus}, nil
		},
		update: func(string, beads.UpdateOptions) error { wrote = true; return nil },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := undeferIssues(env, st, []string{"fx-1"}, soon()); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if wrote {
		t.Error("an issue that is not deferred was written")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused undefer", stdout.String())
	}
	if !strings.Contains(stderr.String(), "is not deferred") {
		t.Errorf("stderr = %q, want bd's refusal", stderr.String())
	}
}

// TestUndeferHandlesADeferral: the seam's update cannot clear a deferral, so a
// bead snoozed into the future is refused rather than reopened into a state
// that keeps it off the ready board, and a deferral that has passed is reopened
// (it no longer hides the bead). A deferral the handler cannot read is refused
// too: guessing would be the same silent miss.
func TestUndeferHandlesADeferral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		until   string
		code    int
		wantOut bool
	}{
		{name: "a snooze still in the future", until: "2999-01-01T00:00:00Z", code: bdFailureExit},
		{name: "a snooze that has passed", until: "2000-01-01T00:00:00Z", code: 0, wantOut: true},
		{name: "a deferral the handler cannot read", until: "tomorrow", code: bdFailureExit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrote := false
			st := stubClaimStore{
				show: func(id string) (*beads.Issue, error) {
					return &beads.Issue{ID: id, Status: deferredStatus, DeferUntil: tc.until}, nil
				},
				update: func(string, beads.UpdateOptions) error { wrote = true; return nil },
			}
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			env := Env{Stdout: stdout, Stderr: stderr}
			code := undeferIssues(env, st, []string{"fx-1"}, soon())
			if code != tc.code {
				t.Errorf("exit code is %d, want %d (stderr %q)", code, tc.code, stderr.String())
			}
			if wrote != tc.wantOut {
				t.Errorf("wrote = %v, want %v", wrote, tc.wantOut)
			}
			if got := stdout.Len() > 0; got != tc.wantOut {
				t.Errorf("stdout = %q, want an answer only when reopened", stdout.String())
			}
		})
	}
}

// TestUnclaimRefusesAnUnassignedIssue: bd exits 1 and prints nothing for an
// issue nobody holds (the golden), so the handler must not release.
func TestUnclaimRefusesAnUnassignedIssue(t *testing.T) {
	t.Parallel()
	released := false
	st := stubClaimStore{
		show:    func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
		release: func(string) error { released = true; return nil },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := unclaimIssues(env, st, []string{"fx-1"}); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if released {
		t.Error("an issue nobody holds was released")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused unclaim", stdout.String())
	}
	if !strings.Contains(stderr.String(), "is not assigned") {
		t.Errorf("stderr = %q, want bd's refusal", stderr.String())
	}
}

// TestUnclaimReleasesAClaimedIssue: a held issue is released and the answer
// names it.
func TestUnclaimReleasesAClaimedIssue(t *testing.T) {
	t.Parallel()
	released := ""
	st := stubClaimStore{
		show:    func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id, Assignee: "alice"}, nil },
		release: func(id string) error { released = id; return nil },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := unclaimIssues(env, st, []string{"fx-1"}); code != 0 {
		t.Fatalf("unclaim: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if released != "fx-1" {
		t.Errorf("released %q, want fx-1", released)
	}
	if want := "* Unclaimed fx-1\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestUnclaimReportsARefusedRelease: the store's holder guard refusing a release
// (another actor holds the issue, and --force is not covered) is a failure, not
// a release.
func TestUnclaimReportsARefusedRelease(t *testing.T) {
	t.Parallel()
	st := stubClaimStore{
		show:    func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id, Assignee: "alice"}, nil },
		release: func(string) error { return errors.New("fx-1 is assigned to another actor") },
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	if code := unclaimIssues(env, st, []string{"fx-1"}); code != bdFailureExit {
		t.Errorf("exit code is %d, want %d", code, bdFailureExit)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing for a refused release", stdout.String())
	}
	if !strings.Contains(stderr.String(), "assigned to another actor") {
		t.Errorf("stderr = %q, want the store's refusal", stderr.String())
	}
}

// TestWriteBAnswersReportAStreamTheyCannotWrite: an answer the caller cannot
// read is the shim's error, not a write it reports as answered.
func TestWriteBAnswersReportAStreamTheyCannotWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler func(Env) int
	}{
		{
			name: "delete",
			handler: func(env Env) int {
				return deleteIssues(env, stubDeleteStore{
					show:   func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
					delete: func(...string) error { return nil },
				}, []string{"fx-1"})
			},
		},
		{
			name: "defer",
			handler: func(env Env) int {
				return deferIssues(env, stubClaimStore{update: allowsClaimUpdate}, []string{"fx-1"}, "")
			},
		},
		{
			name: "undefer",
			handler: func(env Env) int {
				return undeferIssues(env, stubClaimStore{
					show:   func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id, Status: deferredStatus}, nil },
					update: allowsClaimUpdate,
				}, []string{"fx-1"}, soon())
			},
		},
		{
			name: "unclaim",
			handler: func(env Env) int {
				return unclaimIssues(env, stubClaimStore{
					show:    func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id, Assignee: "alice"}, nil },
					release: func(string) error { return nil },
				}, []string{"fx-1"})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stderr := writeEnv(failingWriter{})
			if code := tc.handler(env); code != bdFailureExit {
				t.Errorf("%s with an unwritable answer: exit code is %d, want %d", tc.name, code, bdFailureExit)
			}
			if !strings.Contains(stderr.String(), "writing bd's answer") {
				t.Errorf("%s with an unwritable answer: stderr = %q, want the write failure", tc.name, stderr.String())
			}
		})
	}
}

// soon is the clock the undefer tests pass: a fixed instant their past and
// future deferrals sit on either side of.
func soon() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// allowsClaimUpdate is a write the store accepts.
func allowsClaimUpdate(string, beads.UpdateOptions) error { return nil }

// stubDeleteStore is a deleteStore whose every method is the test's choice, so
// a refusal is driven without a tracker server. A method the test never
// expected to run fails the test rather than answering.
type stubDeleteStore struct {
	show         func(string) (*beads.Issue, error)
	showMultiple func([]string) (map[string]*beads.Issue, error)
	update       func(string, beads.UpdateOptions) error
	delete       func(...string) error
}

func (s stubDeleteStore) Show(id string) (*beads.Issue, error) {
	if s.show == nil {
		panic(fmt.Sprintf("the handler read %s, which the test did not expect", id))
	}
	return s.show(id)
}

func (s stubDeleteStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.showMultiple == nil {
		panic(fmt.Sprintf("the handler read %v, which the test did not expect", ids))
	}
	return s.showMultiple(ids)
}

func (s stubDeleteStore) Update(id string, opts beads.UpdateOptions) error {
	if s.update == nil {
		panic(fmt.Sprintf("the handler updated %s, which the test did not expect", id))
	}
	return s.update(id, opts)
}

func (s stubDeleteStore) DeleteIssues(ids ...string) error {
	if s.delete == nil {
		panic(fmt.Sprintf("the handler deleted %v, which the test did not expect", ids))
	}
	return s.delete(ids...)
}

// stubClaimStore is a claimStore whose every method is the test's choice, with
// the same rule: a method the test never expected to run fails the test.
type stubClaimStore struct {
	show    func(string) (*beads.Issue, error)
	update  func(string, beads.UpdateOptions) error
	release func(string) error
}

func (s stubClaimStore) Show(id string) (*beads.Issue, error) {
	if s.show == nil {
		panic(fmt.Sprintf("the handler read %s, which the test did not expect", id))
	}
	return s.show(id)
}

func (s stubClaimStore) Update(id string, opts beads.UpdateOptions) error {
	if s.update == nil {
		panic(fmt.Sprintf("the handler updated %s, which the test did not expect", id))
	}
	return s.update(id, opts)
}

func (s stubClaimStore) Release(id string) error {
	if s.release == nil {
		panic(fmt.Sprintf("the handler released %s, which the test did not expect", id))
	}
	return s.release(id)
}
