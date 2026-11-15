package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The statements the swap sends around its renames, keyed so a test can script
// one answer and let the rest stand.
const (
	sqlCompactWorkTip     = "SELECT dolt_hashof('compact-work')"
	sqlMainPreSquashTip   = "SELECT dolt_hashof('main-pre-squash')"
	sqlMainPreSquashCount = "SELECT COUNT(*) FROM dolt_branches WHERE name = 'main-pre-squash'"
	sqlDoltBranch         = "CALL DOLT_BRANCH"
)

// The fixture commits the swap's tests read. hash renders n modulo its
// alphabet's length, so two values thirty-two apart name the same commit;
// these are distinct from each other.
const (
	verifiedCommit = 0x01
	swappedCommit  = 0x02
	oldTipCommit   = 0x03
	tailFirst      = 0x11
	tailSecond     = 0x12
	tailThird      = 0x13
	firstPick      = 0x14
	secondPick     = 0x15
	landedOnMain   = 0x16
	movedMainTip   = 0x17
)

// swapRenames is the swap's two rename statements as the design writes them,
// in order: main becomes main-pre-squash before compact-work becomes main, and
// both are forced. The SQL is spelled out here so the test pins what the run
// sends rather than the renderer that sends it.
var swapRenames = []string{
	"CALL DOLT_BRANCH('-m', '--force', 'main', 'main-pre-squash')",
	"CALL DOLT_BRANCH('-m', '--force', 'compact-work', 'main')",
}

// swapRenameBack is what a conflict sends when nothing landed on the new main:
// the two branches return to the names they held before the swap.
var swapRenameBack = []string{
	"CALL DOLT_BRANCH('-m', '--force', 'main', 'compact-work')",
	"CALL DOLT_BRANCH('-m', '--force', 'main-pre-squash', 'main')",
}

// pickedAs is what CALL DOLT_CHERRY_PICK returns for a commit it made: the new
// hash and three zero conflict counts.
func pickedAs(commit string) Rows {
	return Rows{
		Columns: []string{"hash", "data_conflicts", "schema_conflicts", "constraint_violations"},
		Data:    [][]string{{commit, "0", "0", "0"}},
	}
}

// pickedWithConflicts is what CALL DOLT_CHERRY_PICK returns for a pick that
// left a data conflict: no hash and a count per conflict kind.
func pickedWithConflicts() Rows {
	return Rows{
		Columns: []string{"hash", "data_conflicts", "schema_conflicts", "constraint_violations"},
		Data:    [][]string{{"", "1", "0", "0"}},
	}
}

// swapFake answers a swap's reads up to the old main's tail list: the session
// on compact-work before the renames and on main after them, compact-work's
// tip, the count of branches named main-pre-squash (none), and
// main-pre-squash's tip. A rename answers with no rows, as Dolt's branch
// procedures do.
func swapFake(swappedTip, oldTip string) *FakeExec {
	f := NewFakeExec()
	f.Respond(sqlActiveBranch, single("branch", "compact-work"))
	f.Respond(sqlActiveBranch, single("branch", "main"))
	f.Respond(sqlDoltBranch, Rows{})
	f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", swappedTip))
	f.Respond(sqlMainPreSquashCount, single("COUNT(*)", "0"))
	f.Respond(sqlMainPreSquashTip, single("dolt_hashof('main-pre-squash')", oldTip))
	return f
}

// branchStatements returns the DOLT_BRANCH statements the fake received, in
// order.
func branchStatements(f *FakeExec) []string {
	var renames []string
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, sqlDoltBranch) {
			renames = append(renames, statement)
		}
	}
	return renames
}

func TestSwapRenamesMainToMainPreSquashFirstAndBothForced(t *testing.T) {
	t.Parallel()
	verified, swapped := hash(verifiedCommit), hash(swappedCommit)
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, verified)

	got, err := Swap(context.Background(), f, verified, sleep)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if want := (SwapResult{
		SwappedTip: swapped,
		OldTip:     verified,
		Reason:     "swapped: no commit reached the old main after the verified tip",
	}); got != want {
		t.Errorf("Swap returned %+v, want %+v", got, want)
	}
	if got, want := branchStatements(f), swapRenames; !slices.Equal(got, want) {
		t.Errorf("renames = %q, want %q: main is renamed first and both are forced", got, want)
	}
	// The tip that becomes main is read before either rename, so a commit a
	// writer lands during the swap cannot be mistaken for the swapped content.
	want := []string{sqlActiveBranch, sqlCompactWorkTip, sqlMainPreSquashCount, swapRenames[0], swapRenames[1], sqlActiveBranch, sqlMainPreSquashTip}
	if got := f.Statements(); !slices.Equal(got, want) {
		t.Errorf("statements = %q, want %q", got, want)
	}
}

func TestSwapReplaysTheTailInOrderAndRetriesUncommittedChanges(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1, c2, c3 := hash(tailFirst), hash(tailSecond), hash(tailThird)
	held := errors.New("cannot cherry-pick with uncommitted changes")
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows(c1, c2, c3))
	f.RespondErr(pickStatement(c1), held)
	f.Respond(pickStatement(c1), pickedAs(hash(firstPick)))
	f.Respond(pickStatement(c2), pickedAs(hash(secondPick)))
	f.Respond(pickStatement(c3), pickedAs(hash(movedMainTip)))

	got, err := Swap(context.Background(), f, verified, sleep)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	want := SwapResult{
		SwappedTip: swapped,
		OldTip:     oldTip,
		Tail:       3,
		Replayed:   3,
		Retries:    1,
		Reason:     "swapped and replayed 3 commits",
	}
	if got != want {
		t.Errorf("Swap returned %+v, want %+v", got, want)
	}
	// The held pick is sent once more for the retry, before the next commit.
	if got, want := pickStatements(f), wantPicks(c1, c1, c2, c3); !slices.Equal(got, want) {
		t.Errorf("picks = %q, want %q: the tail is replayed oldest first", got, want)
	}
	if got, want := tailRanges(t, f), []string{verified + ".." + oldTip}; !slices.Equal(got, want) {
		t.Errorf("the replay read the ranges %q, want %q: the tail starts at the verified tip", got, want)
	}
	if want := []time.Duration{catchupWait(0)}; !slices.Equal(sleep.waits, want) {
		t.Errorf("waits = %v, want %v: a held working set is retried", sleep.waits, want)
	}
}

// TestSwapKeepsBothBranchesWhenAConflictMeetsACommitLandedOnTheNewMain drives
// the case that forbids renaming back: a writer's commit reached the new main
// while the replay was picking, so the branches must stay where the swap put
// them and the result must name the commits an operator has to resolve.
func TestSwapKeepsBothBranchesWhenAConflictMeetsACommitLandedOnTheNewMain(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1, c2, c3 := hash(tailFirst), hash(tailSecond), hash(tailThird)
	p1, landed := hash(firstPick), hash(landedOnMain)
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows(c1, c2, c3))
	f.Respond(pickStatement(c1), pickedAs(p1))
	f.Respond(pickStatement(c2), pickedWithConflicts())
	f.Respond(sqlMainTip, single("dolt_hashof('main')", landed))
	f.Respond(tailStatement(swapped, landed), tailRows(p1, landed))

	got, err := Swap(context.Background(), f, verified, sleep)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	want := &SwapConflict{Commit: c2, Unpicked: []string{c2, c3}, Landed: []string{landed}}
	if got.Conflict == nil || got.Conflict.Commit != want.Commit ||
		!slices.Equal(got.Conflict.Unpicked, want.Unpicked) || !slices.Equal(got.Conflict.Landed, want.Landed) {
		t.Fatalf("Swap returned the conflict %+v, want %+v", got.Conflict, want)
	}
	if got.RenamedBack {
		t.Error("Swap renamed back with a commit landed on the new main: that commit would be moved off main")
	}
	if got.Tail != 3 || got.Replayed != 2 {
		t.Errorf("Swap replayed %d of a tail of %d, want the pick that conflicted counted as attempted", got.Replayed, got.Tail)
	}
	for _, commit := range []string{c2, c3, landed} {
		if !strings.Contains(got.Reason, commit) {
			t.Errorf("Reason %q does not hold %q: the item must name the commits", got.Reason, commit)
		}
	}
	if got, want := branchStatements(f), swapRenames; !slices.Equal(got, want) {
		t.Errorf("renames = %q, want %q: a landed commit keeps both branches as the swap left them", got, want)
	}
}

// TestSwapRenamesBackWhenAConflictMeetsNoCommitOnTheNewMain drives the first
// pick conflicting before any of the replay landed: the new main's tip is the
// swapped tip, so nothing has landed and renaming back loses nothing.
func TestSwapRenamesBackWhenAConflictMeetsNoCommitOnTheNewMain(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1 := hash(tailFirst)
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows(c1))
	f.Respond(pickStatement(c1), pickedWithConflicts())
	f.Respond(sqlMainTip, single("dolt_hashof('main')", swapped))

	got, err := Swap(context.Background(), f, verified, sleep)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.Conflict == nil || got.Conflict.Commit != c1 || !slices.Equal(got.Conflict.Unpicked, []string{c1}) {
		t.Fatalf("Swap returned the conflict %+v, want the pick of %s unpicked", got.Conflict, c1)
	}
	if len(got.Conflict.Landed) != 0 {
		t.Errorf("Conflict.Landed = %q, want none: the new main's tip is the swapped tip", got.Conflict.Landed)
	}
	if !got.RenamedBack {
		t.Error("Swap did not rename back though nothing landed on the new main")
	}
	if got.Replayed != 1 || got.Tail != 1 {
		t.Errorf("Swap returned Tail %d, Replayed %d, want 1 and 1", got.Tail, got.Replayed)
	}
	if got, want := branchStatements(f), append(slices.Clone(swapRenames), swapRenameBack...); !slices.Equal(got, want) {
		t.Errorf("renames = %q, want %q", got, want)
	}
	for _, word := range []string{"renamed the two back", c1} {
		if !strings.Contains(got.Reason, word) {
			t.Errorf("Reason %q does not hold %q", got.Reason, word)
		}
	}
}

// TestSwapDoesNotReadItsOwnPicksAsLandedCommits drives a conflict after picks
// of its own landed: the new main's log holds the replay's commits and nothing
// else, so they are not a reason to keep the branches, and renaming back is
// still the recovery.
func TestSwapDoesNotReadItsOwnPicksAsLandedCommits(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1, c2, c3 := hash(tailFirst), hash(tailSecond), hash(tailThird)
	p1, p2 := hash(firstPick), hash(secondPick)
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows(c1, c2, c3))
	f.Respond(pickStatement(c1), pickedAs(p1))
	f.Respond(pickStatement(c2), pickedAs(p2))
	f.Respond(pickStatement(c3), pickedWithConflicts())
	f.Respond(sqlMainTip, single("dolt_hashof('main')", p2))
	f.Respond(tailStatement(swapped, p2), tailRows(p1, p2))
	// The rename back moves that same tip to compact-work, so its read back
	// names the tip the check read.
	f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", p2))

	got, err := Swap(context.Background(), f, verified, sleep)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.Conflict == nil || got.Conflict.Commit != c3 {
		t.Fatalf("Swap returned the conflict %+v, want the pick of %s", got.Conflict, c3)
	}
	if len(got.Conflict.Landed) != 0 {
		t.Errorf("Conflict.Landed = %q, want none: every commit after the swapped tip is the replay's own", got.Conflict.Landed)
	}
	if !got.RenamedBack {
		t.Error("Swap did not rename back: the replay's own picks are not landed commits")
	}
	if got, want := branchStatements(f), append(slices.Clone(swapRenames), swapRenameBack...); !slices.Equal(got, want) {
		t.Errorf("renames = %q, want %q", got, want)
	}
}

func TestSwapRefusesAValueThatIsNotACommitHash(t *testing.T) {
	t.Parallel()
	verified, swapped := hash(verifiedCommit), hash(swappedCommit)
	tests := []struct {
		name   string
		script func(*FakeExec)
		start  string
		// sent is how many statements the refused run sends before it stops.
		sent int
		want string
	}{
		{
			name:   "the verified tip",
			script: func(*FakeExec) {},
			start:  "main",
			sent:   0,
			want:   "the verified tip",
		},
		{
			name: "compact-work's tip",
			script: func(f *FakeExec) {
				f.Respond(sqlActiveBranch, single("branch", "compact-work"))
				f.Respond(sqlCompactWorkTip, single("h", "HEAD"))
			},
			start: verified,
			sent:  2,
			want:  "compact-work's tip",
		},
		{
			name: "main-pre-squash's tip",
			script: func(f *FakeExec) {
				f.Respond(sqlActiveBranch, single("branch", "compact-work"))
				f.Respond(sqlActiveBranch, single("branch", "main"))
				f.Respond(sqlDoltBranch, Rows{})
				f.Respond(sqlCompactWorkTip, single("h", swapped))
				f.Respond(sqlMainPreSquashCount, single("COUNT(*)", "0"))
				f.Respond(sqlMainPreSquashTip, single("h", "HEAD"))
			},
			start: verified,
			sent:  7,
			want:  "main-pre-squash's tip",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := NewFakeExec()
			tc.script(f)
			_, err := Swap(context.Background(), f, tc.start, &testSleeper{clk: newTestClock()})
			if err == nil {
				t.Fatalf("Swap returned no error for %s that is not a commit hash", tc.name)
			}
			for _, want := range []string{tc.want, "want a 32-character Dolt commit hash"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not hold %q", err, want)
				}
			}
			if picks := pickStatements(f); len(picks) != 0 {
				t.Errorf("picks = %q, want none: nothing is picked from a value the run cannot trust", picks)
			}
			if got := len(f.Statements()); got != tc.sent {
				t.Errorf("the refused run sent %d statements, want %d: it stopped at the value it could not trust", got, tc.sent)
			}
		})
	}
}

func TestSwapRefusesATailCommitThatIsNotAHash(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows("HEAD"))

	_, err := Swap(context.Background(), f, verified, sleep)
	if err == nil {
		t.Fatal("Swap returned no error for a tail commit that is not a hash")
	}
	for _, want := range []string{"a commit in the old main's tail", "want a 32-character Dolt commit hash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if picks := pickStatements(f); len(picks) != 0 {
		t.Errorf("picks = %q, want none", picks)
	}
}

func TestSwapRefusesAnEmptyTailAtAMovedOldTip(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows())

	_, err := Swap(context.Background(), f, verified, sleep)
	if err == nil {
		t.Fatal("Swap returned no error for an empty tail at a tip it never verified")
	}
	for _, want := range []string{verified, oldTip, "left unpicked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
}

// TestSwapStopsOnAFailedStatement drives an error at each read the swap makes
// before and during its picks: every one stops the run where it happened and
// the error names the statement's position.
func TestSwapStopsOnAFailedStatement(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1 := hash(tailFirst)
	refused := errors.New("the connection was closed")
	// Statement positions: 1 the first branch check, 2 compact-work's tip, 3
	// the main-pre-squash count, 4 and 5 the renames, 6 the second branch
	// check, 7 the old main's tip, 8 the tail list, 9 the first pick.
	for failAt := 1; failAt <= 9; failAt++ {
		t.Run(fmt.Sprintf("statement_%d", failAt), func(t *testing.T) {
			t.Parallel()
			sleep := &testSleeper{clk: newTestClock()}
			f := swapFake(swapped, oldTip)
			f.Respond(tailStatement(verified, oldTip), tailRows(c1))
			f.Respond(sqlCherryPick, pickedAs(hash(firstPick)))
			f.FailAt(failAt, refused)

			_, err := Swap(context.Background(), f, verified, sleep)
			if !errors.Is(err, refused) {
				t.Fatalf("Swap returned %v, want the injected error", err)
			}
			if want := fmt.Sprintf("statement %d (", failAt); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if got := len(f.Statements()); got != failAt {
				t.Errorf("the run sent %d statements, want %d: it stopped at the failed one", got, failAt)
			}
		})
	}
}

// TestSwapStopsOnAPickErrorThatIsNotAConflict keeps the recovery rule narrow: a
// pick that failed for another reason is not the answer the recovery table has
// a state for, so the branches stay as the swap left them.
func TestSwapStopsOnAPickErrorThatIsNotAConflict(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1 := hash(tailFirst)
	refused := errors.New("the connection was closed")
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows(c1))
	f.RespondErr(pickStatement(c1), refused)

	_, err := Swap(context.Background(), f, verified, sleep)
	if !errors.Is(err, refused) {
		t.Fatalf("Swap returned %v, want the injected error", err)
	}
	if got, want := branchStatements(f), swapRenames; !slices.Equal(got, want) {
		t.Errorf("renames = %q, want %q: only a conflict has a recovery", got, want)
	}
}

// TestSwapRefusesToRecoverFromAConflictItCannotRead drives the reads the
// conflict makes before it may rename back: a refused tip read, a refused log
// read, and a log that lists nothing while the tip moved. Each leaves the
// branches swapped, because renaming back against a commit the run could not
// name would move that commit off main unseen.
func TestSwapRefusesToRecoverFromAConflictItCannotRead(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1 := hash(tailFirst)
	moved := hash(movedMainTip)
	refused := errors.New("the connection was closed")
	// The conflict is the ninth statement; the tip read and the log read that
	// follow it are the tenth and eleventh.
	tests := []struct {
		name  string
		after func(*FakeExec)
		want  string
	}{
		{
			name:  "the new main's tip read fails",
			after: func(f *FakeExec) { f.FailAt(10, refused) },
			want:  "statement 10 (",
		},
		{
			name: "the landed commits read fails",
			after: func(f *FakeExec) {
				f.Respond(sqlMainTip, single("dolt_hashof('main')", moved))
				f.FailAt(11, refused)
			},
			want: "statement 11 (",
		},
		{
			name: "the landed commits read lists nothing at a moved tip",
			after: func(f *FakeExec) {
				f.Respond(sqlMainTip, single("dolt_hashof('main')", moved))
				f.Respond(tailStatement(swapped, moved), tailRows())
			},
			want: "a landed commit would be missed",
		},
		{
			name: "the new main's tip is not a hash",
			after: func(f *FakeExec) {
				f.Respond(sqlMainTip, single("dolt_hashof('main')", "HEAD"))
			},
			want: "the new main's tip",
		},
		{
			name: "a landed commit is not a hash",
			after: func(f *FakeExec) {
				f.Respond(sqlMainTip, single("dolt_hashof('main')", moved))
				f.Respond(tailStatement(swapped, moved), tailRows("HEAD"))
			},
			want: "a commit in the new main's log",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sleep := &testSleeper{clk: newTestClock()}
			f := swapFake(swapped, oldTip)
			f.Respond(tailStatement(verified, oldTip), tailRows(c1))
			f.Respond(pickStatement(c1), pickedWithConflicts())
			tc.after(f)

			_, err := Swap(context.Background(), f, verified, sleep)
			if err == nil {
				t.Fatalf("Swap returned no error, want one holding %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if got, want := branchStatements(f), swapRenames; !slices.Equal(got, want) {
				t.Errorf("renames = %q, want %q: a read the run cannot trust never renames back", got, want)
			}
		})
	}
}

// TestSwapStopsWhenTheRenameBackFails drives a rename-back statement that Dolt
// refuses: the run stops with that error and reports no recovery it did not
// complete, so the caller's item describes the branches as they actually are.
func TestSwapStopsWhenTheRenameBackFails(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1 := hash(tailFirst)
	refused := errors.New("the connection was closed")
	sleep := &testSleeper{clk: newTestClock()}
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows(c1))
	f.Respond(pickStatement(c1), pickedWithConflicts())
	f.Respond(sqlMainTip, single("dolt_hashof('main')", swapped))
	// The conflict is the ninth statement, the tip read is the tenth, and the
	// first rename back is the eleventh.
	f.FailAt(11, refused)

	got, err := Swap(context.Background(), f, verified, sleep)
	if !errors.Is(err, refused) {
		t.Fatalf("Swap returned %v, want the injected error", err)
	}
	if want := "statement 11 (CALL DOLT_BRANCH('-m', '--force', 'main', 'compact-work'))"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
	if got.RenamedBack {
		t.Error("Swap reports a rename back it did not complete")
	}
	if got, want := branchStatements(f), append(slices.Clone(swapRenames), swapRenameBack[0]); !slices.Equal(got, want) {
		t.Errorf("renames = %q, want %q: the run stopped at the first rename back", got, want)
	}
}

// conflictFake answers the swap of one tail commit whose pick leaves conflicts
// with the new main's tip still at the swapped tip: the check reads no commit
// landed, so the swap renames back.
func conflictFake(verified, swapped, oldTip, c1 string) *FakeExec {
	f := swapFake(swapped, oldTip)
	f.Respond(tailStatement(verified, oldTip), tailRows(c1))
	f.Respond(pickStatement(c1), pickedWithConflicts())
	f.Respond(sqlMainTip, single("dolt_hashof('main')", swapped))
	return f
}

// TestSwapRefusesAnExistingMainPreSquash drives the branch count the swap reads
// before its first rename: a branch that already holds main-pre-squash would be
// overwritten by the forced rename, so the run refuses before either rename.
func TestSwapRefusesAnExistingMainPreSquash(t *testing.T) {
	t.Parallel()
	verified, swapped := hash(verifiedCommit), hash(swappedCommit)
	sleep := &testSleeper{clk: newTestClock()}
	f := NewFakeExec()
	f.Respond(sqlActiveBranch, single("branch", "compact-work"))
	f.Respond(sqlCompactWorkTip, single("h", swapped))
	f.Respond(sqlMainPreSquashCount, single("COUNT(*)", "1"))

	_, err := Swap(context.Background(), f, verified, sleep)
	if err == nil {
		t.Fatal("Swap returned no error for an existing main-pre-squash")
	}
	for _, want := range []string{"main-pre-squash", "want none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if renames := branchStatements(f); len(renames) != 0 {
		t.Errorf("renames = %q, want none: a forced rename would overwrite the branch", renames)
	}
	if picks := pickStatements(f); len(picks) != 0 {
		t.Errorf("picks = %q, want none", picks)
	}
}

// TestSwapUndoesTheRenameBackWhenACommitLandsInTheWindow drives the race the
// check cannot see: a commit reaches the new main after the check read the tip
// and before the rename back moves it. Reading back the tip the renames moved
// names that commit, so the renames are undone and both branches stay as the
// swap left them.
func TestSwapUndoesTheRenameBackWhenACommitLandsInTheWindow(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1, landed := hash(tailFirst), hash(landedOnMain)
	sleep := &testSleeper{clk: newTestClock()}
	f := conflictFake(verified, swapped, oldTip, c1)
	f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", landed))
	f.Respond(tailStatement(swapped, landed), tailRows(landed))

	got, err := Swap(context.Background(), f, verified, sleep)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got.RenamedBack {
		t.Error("Swap reports a rename back though a commit landed in the window: that commit would be moved off main")
	}
	want := &SwapConflict{Commit: c1, Unpicked: []string{c1}, Landed: []string{landed}}
	if got.Conflict == nil || got.Conflict.Commit != want.Commit ||
		!slices.Equal(got.Conflict.Unpicked, want.Unpicked) || !slices.Equal(got.Conflict.Landed, want.Landed) {
		t.Fatalf("Swap returned the conflict %+v, want %+v", got.Conflict, want)
	}
	if !strings.Contains(got.Reason, landed) {
		t.Errorf("Reason %q does not hold %q: the item must name the commit that landed", got.Reason, landed)
	}
	if got, want := branchStatements(f), slices.Concat(swapRenames, swapRenameBack, swapRenames); !slices.Equal(got, want) {
		t.Errorf("renames = %q, want %q: the rename back is undone", got, want)
	}
}

// TestSwapRefusesToFinishARenameBackItCannotVerify drives the reads the swap
// makes after the rename back: it cannot tell a commit that landed in the
// window from the tip the check read, so it stops and reports no rename back it
// did not verify. The cases where the swap already renamed forth again leave
// both branches as the swap left them.
func TestSwapRefusesToFinishARenameBackItCannotVerify(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1, landed := hash(tailFirst), hash(landedOnMain)
	refused := errors.New("the connection was closed")
	// The rename back is statements 11 and 12, the read of the tip it moved is
	// statement 13, the undo is 14 and 15, and the log of the commits it moved
	// is 16.
	tests := []struct {
		name  string
		after func(*FakeExec)
		want  string
		// renames is how many branch renames the refused run sends, the swap's
		// two included, so 4 is the two renames back and no undo.
		renames int
	}{
		{
			name:    "the moved tip read fails",
			after:   func(f *FakeExec) { f.FailAt(13, refused) },
			want:    "a commit that landed between the check and the renames is not ruled out",
			renames: 4,
		},
		{
			name: "the moved tip is not a hash",
			after: func(f *FakeExec) {
				f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", "HEAD"))
			},
			want:    "the tip they moved cannot be trusted",
			renames: 4,
		},
		{
			name: "the moved log read fails",
			after: func(f *FakeExec) {
				f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", landed))
				f.FailAt(16, refused)
			},
			want:    "statement 16 (",
			renames: 6,
		},
		{
			name: "the moved log lists nothing",
			after: func(f *FakeExec) {
				f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", landed))
				f.Respond(tailStatement(swapped, landed), tailRows())
			},
			want:    "a landed commit would be missed",
			renames: 6,
		},
		{
			name: "a moved commit is not a hash",
			after: func(f *FakeExec) {
				f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", landed))
				f.Respond(tailStatement(swapped, landed), tailRows("HEAD"))
			},
			want:    "a commit that landed between the check and the renames",
			renames: 6,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sleep := &testSleeper{clk: newTestClock()}
			f := conflictFake(verified, swapped, oldTip, c1)
			tc.after(f)

			got, err := Swap(context.Background(), f, verified, sleep)
			if err == nil {
				t.Fatalf("Swap returned no error, want one holding %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if got.RenamedBack {
				t.Error("Swap reports a rename back it could not verify")
			}
			if got := len(branchStatements(f)); got != tc.renames {
				t.Errorf("the refused run sent %d renames, want %d", got, tc.renames)
			}
		})
	}
}

// TestSwapStopsWhenTheUndoOfTheRenameBackFails drives an undo statement that
// Dolt refuses: the run stops with that error, so the caller's item describes
// the branches as they actually are.
func TestSwapStopsWhenTheUndoOfTheRenameBackFails(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1, landed := hash(tailFirst), hash(landedOnMain)
	refused := errors.New("the connection was closed")
	for failAt := 14; failAt <= 15; failAt++ {
		t.Run(fmt.Sprintf("statement_%d", failAt), func(t *testing.T) {
			t.Parallel()
			sleep := &testSleeper{clk: newTestClock()}
			f := conflictFake(verified, swapped, oldTip, c1)
			f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", landed))
			f.FailAt(failAt, refused)

			got, err := Swap(context.Background(), f, verified, sleep)
			if !errors.Is(err, refused) {
				t.Fatalf("Swap returned %v, want the injected error", err)
			}
			if want := fmt.Sprintf("statement %d (CALL DOLT_BRANCH", failAt); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if got.RenamedBack {
				t.Error("Swap reports a rename back it did not keep")
			}
			// The swap's two renames, the rename back's two, and the undo's up
			// to the one that failed.
			if got, want := len(branchStatements(f)), len(swapRenames)+len(swapRenameBack)+failAt-13; got != want {
				t.Errorf("the run sent %d renames, want %d: it stopped at the failed one", got, want)
			}
		})
	}
}

// TestSwapRefusesASessionOnAnotherBranch drives both branch checks: the run
// must pick on compact-work before the swap and on main after it, because a
// pick on another branch rewrites that branch.
func TestSwapRefusesASessionOnAnotherBranch(t *testing.T) {
	t.Parallel()
	verified, swapped, oldTip := hash(verifiedCommit), hash(swappedCommit), hash(oldTipCommit)
	c1 := hash(tailFirst)
	tests := []struct {
		name     string
		branches []string
		want     string
		// sent is how many statements the refused run may send.
		sent int
		// renames is how many branch renames it may send.
		renames int
	}{
		{
			name:     "before the swap",
			branches: []string{"main", "main"},
			want:     `answered "main", want compact-work`,
			sent:     1,
			renames:  0,
		},
		{
			name:     "after the swap",
			branches: []string{"compact-work", "main-pre-squash"},
			want:     `answered "main-pre-squash", want main`,
			sent:     6,
			renames:  2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sleep := &testSleeper{clk: newTestClock()}
			f := NewFakeExec()
			for _, branch := range tc.branches {
				f.Respond(sqlActiveBranch, single("branch", branch))
			}
			f.Respond(sqlDoltBranch, Rows{})
			f.Respond(sqlCompactWorkTip, single("h", swapped))
			f.Respond(sqlMainPreSquashCount, single("COUNT(*)", "0"))
			f.Respond(sqlMainPreSquashTip, single("h", oldTip))
			f.Respond(tailStatement(verified, oldTip), tailRows(c1))
			f.Respond(sqlCherryPick, pickedAs(hash(firstPick)))

			_, err := Swap(context.Background(), f, verified, sleep)
			if err == nil {
				t.Fatal("Swap returned no error on a session on another branch")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if picks := pickStatements(f); len(picks) != 0 {
				t.Errorf("picks = %q, want none: no pick lands on a branch the step did not choose", picks)
			}
			if got := len(f.Statements()); got != tc.sent {
				t.Errorf("the refused run sent %d statements, want %d", got, tc.sent)
			}
			if got := len(branchStatements(f)); got != tc.renames {
				t.Errorf("the refused run sent %d renames, want %d", got, tc.renames)
			}
		})
	}
}
