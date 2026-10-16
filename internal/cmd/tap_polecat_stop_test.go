package cmd

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/slot"
)

const polecatStopTestBranch = "polecat/test/gt-ksnv@abc123"

// fakePolecatStopGit answers the pending-work check from canned git state.
type fakePolecatStopGit struct {
	status    git.UncommittedWorkStatus
	statusErr error
	target    git.BranchPreservationStatus
	targetErr error
	branch    string // the branch BranchTargetStatus was asked about
}

func (f *fakePolecatStopGit) CheckUncommittedWork() (*git.UncommittedWorkStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &f.status, nil
}

func (f *fakePolecatStopGit) BranchTargetStatus(branch, remote string, targets []string) (git.BranchPreservationStatus, error) {
	f.branch = branch
	return f.target, f.targetErr
}

// TestPolecatStopPendingWork covers the pending-work decision over canned
// git state; TestIntegrationPolecatStopPendingWork reads real repositories.
func TestPolecatStopPendingWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		g           fakePolecatStopGit
		wantPending bool
		wantReason  string
		wantErr     bool
	}{
		{name: "clean feature branch has no pending work", g: fakePolecatStopGit{target: git.BranchPreservationStatus{Preserved: true}}},
		{
			name:        "non-runtime dirty work is pending",
			g:           fakePolecatStopGit{status: git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{"internal/cmd/work.go"}}},
			wantPending: true,
			wantReason:  "1 non-runtime dirty file(s)",
		},
		{
			name: "runtime-only dirty work is ignored",
			g:    fakePolecatStopGit{status: git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".opencode/state.json"}}},
		},
		{
			name:        "branch stash is pending",
			g:           fakePolecatStopGit{status: git.UncommittedWorkStatus{StashCount: 1}},
			wantPending: true,
			wantReason:  "1 branch stash(es)",
		},
		{
			name:        "pushed source branch still pending until target contains it",
			g:           fakePolecatStopGit{target: git.BranchPreservationStatus{UnpreservedPatchCount: 1}},
			wantPending: true,
			wantReason:  "1 unsubmitted commit(s)",
		},
		{
			name: "target-contained commit has no pending work",
			g:    fakePolecatStopGit{target: git.BranchPreservationStatus{Preserved: true}},
		},
		{name: "unreadable worktree fails closed", g: fakePolecatStopGit{statusErr: errors.New("not a git repository")}, wantErr: true},
		{name: "unreadable target fails closed", g: fakePolecatStopGit{targetErr: errors.New("no origin")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.g
			pending, reason, err := polecatStopPendingWorkIn(&g, polecatStopTestBranch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if pending != tt.wantPending || reason != tt.wantReason {
				t.Fatalf("pending, reason = %v, %q, want %v, %q", pending, reason, tt.wantPending, tt.wantReason)
			}
			if !tt.wantErr && tt.g.status.StashCount == 0 && !tt.wantPending && g.branch != polecatStopTestBranch {
				t.Errorf("BranchTargetStatus asked about %q, want %q", g.branch, polecatStopTestBranch)
			}
		})
	}
}

// fakeStopCheckReads answers the live-suite check from canned evidence. The
// reads themselves are plumbing over tmux, ps and the pool, and the unit tier
// starts none of them (docs/testing.md); what is under test here is the
// decision over those reads and the message it carries.
type fakeStopCheckReads struct {
	rep      slot.Report
	repErr   error
	panes    []int
	panesErr error
	procs    []polecat.Proc
	procsErr error
}

func (f fakeStopCheckReads) Slots() (slot.Report, error)           { return f.rep, f.repErr }
func (f fakeStopCheckReads) PanePIDs(string) ([]int, error)        { return f.panes, f.panesErr }
func (f fakeStopCheckReads) ProcessTable() ([]polecat.Proc, error) { return f.procs, f.procsErr }

// heldSlot is a pool report holding slot 0 under role.
func heldSlot(role string) slot.Report {
	return slot.Report{Slots: []slot.SlotState{{Index: 0, Held: true, Owner: &slot.Owner{Role: role}}}}
}

// TestPolecatStopVerificationRunning guards the turn-boundary fix for
// gt-couv: the Stop hook must not treat "my own slot-wrapped suite is still
// running" as abandonment, and gt-78n0: nor a suite still live in the polecat's
// own pane. A suite belonging to an unrelated polecat/rig must not falsely
// suppress the auto-done either.
func TestPolecatStopVerificationRunning(t *testing.T) {
	t.Parallel()
	const session = "gt-coral"
	// An idle pane: the polecat's agent process and nothing under it.
	idlePane := []polecat.Proc{{PID: 100, PPID: 1, Args: "claude --session-id a"}}

	t.Run("no suite running", func(t *testing.T) {
		busy, reason := polecatStopSuite(fakeStopCheckReads{panes: []int{100}, procs: idlePane},
			t.TempDir(), "gastown", "coral", session)
		if busy {
			t.Fatalf("busy = true (%s), want false when nothing holds the slot and the pane is idle", reason)
		}
	})

	t.Run("slot held by this polecat", func(t *testing.T) {
		busy, reason := polecatStopSuite(fakeStopCheckReads{rep: heldSlot("gastown/coral")},
			t.TempDir(), "gastown", "coral", session)
		if !busy {
			t.Fatal("busy = false, want true when this polecat holds the slot")
		}
		if !strings.Contains(reason, "gastown/coral") {
			t.Fatalf("reason = %q, want it to name the holder gastown/coral", reason)
		}
	})

	t.Run("slot held by a different polecat does not suppress", func(t *testing.T) {
		busy, reason := polecatStopSuite(fakeStopCheckReads{rep: heldSlot("gastown/citrine"), panes: []int{100}, procs: idlePane},
			t.TempDir(), "gastown", "coral", session)
		if busy {
			t.Fatalf("busy = true (%s), want false when a different polecat holds the slot", reason)
		}
	})

	t.Run("a suite still live in this polecat's pane", func(t *testing.T) {
		procs := append(slices.Clone(idlePane), polecat.Proc{PID: 101, PPID: 100, Args: "go test ./internal/cmd/"})
		busy, reason := polecatStopSuite(fakeStopCheckReads{panes: []int{100}, procs: procs},
			t.TempDir(), "gastown", "coral", session)
		if !busy {
			t.Fatal("busy = false, want true while go test runs in the polecat's own pane")
		}
		if !strings.Contains(reason, "go test") {
			t.Fatalf("reason = %q, want it to name the go test", reason)
		}
	})

	t.Run("an unreadable read is not busy", func(t *testing.T) {
		busy, reason := polecatStopSuite(fakeStopCheckReads{panesErr: errors.New("tmux is gone")},
			t.TempDir(), "gastown", "coral", session)
		if busy {
			t.Fatalf("busy = true (%s), want false: a turn boundary reads an unreadable state as not busy", reason)
		}
	})
}

// TestCommitWithinGrace covers the grace decision over git log's %ct;
// TestIntegrationPolecatStopCommittedWithinGrace reads real commits.
func TestCommitWithinGrace(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	ct := func(d time.Duration) string { return strconv.FormatInt(now.Add(-d).Unix(), 10) + "\n" }
	tests := []struct {
		name    string
		ct      string
		want    bool
		wantErr bool
	}{
		{name: "fresh commit is within grace", ct: ct(0), want: true},
		{name: "commit just inside the grace period", ct: ct(pendingWorkGracePeriod - time.Second), want: true},
		{name: "old commit is outside grace", ct: ct(10 * time.Minute)},
		{name: "no commit is not within grace", ct: ""},
		{name: "unparseable output fails closed", ct: "yesterday", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := commitWithinGrace(tt.ct, now)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("commitWithinGrace(%q) = %v, %v, want %v, error %v", tt.ct, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// polecatStopWrapperArgv is the real shape of a Claude Code background Bash
// process (captured from `ps -ww -eo pid,ppid,args` on the host running this
// suite): a snapshot preamble, then the command as a quoted eval argument, then
// a trailing cwd write. The command is the only part that matters to the pane
// scan, and it is the part a plain tokenization of the wrapper cannot see.
const polecatStopWrapperArgv = `/bin/zsh -c source /Users/sloan/gt/.claude-town/shell-snapshots/snapshot-zsh-1790075016638-nti8n3.sh 2>/dev/null || true && setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL 2>/dev/null || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'gt slot run --role gastown/diamond -- GOFLAGS=-p=8 make test' < /dev/null && pwd -P >| /tmp/claude-fac2-cwd`

// TestPolecatStopVerificationArgv guards the gt-78n0 pane scan: the commands
// that mean "this polecat is still verifying" must be recognized wherever the
// wrapper buries them, and the ones that only mention a gate must not be. The
// matcher is polecat.SuiteArgv; what is pinned here is this guard's tokenizer
// and the invocations it feeds the matcher.
func TestPolecatStopVerificationArgv(t *testing.T) {
	t.Parallel()

	gates := []polecat.Invocation{
		{Program: "make", Target: "test"},
		{Program: "make", Target: "lint"},
		{Program: "make", Target: "build"},
	}

	cases := []struct {
		name   string
		argv   string
		want   string
		expect bool
	}{
		{
			name:   "slot run queued behind another rig, as the formula writes it",
			argv:   "gt slot run --role gastown/diamond -- GOFLAGS=-p=8 make test",
			want:   "gt slot run",
			expect: true,
		},
		{
			name:   "slot run inside a background wrapper",
			argv:   polecatStopWrapperArgv,
			want:   "gt slot run",
			expect: true,
		},
		{
			name:   "make test at the absolute path it was exec'd with",
			argv:   "/Library/Developer/CommandLineTools/usr/bin/make test",
			want:   "make test",
			expect: true,
		},
		{
			name:   "configured lint gate",
			argv:   "/Library/Developer/CommandLineTools/usr/bin/make lint",
			want:   "make lint",
			expect: true,
		},
		{
			name:   "bare go test the rig config never names",
			argv:   "go test ./internal/polecat/ -run TestOne -count=1",
			want:   "go test",
			expect: true,
		},
		{
			name:   "bare go vet from the polecat completion protocol",
			argv:   "go vet ./...",
			want:   "go vet",
			expect: true,
		},
		{
			name:   "go vet at the absolute path it was exec'd with",
			argv:   "/usr/local/go/bin/go vet ./internal/cmd/...",
			want:   "go vet",
			expect: true,
		},
		{
			name:   "go test behind an env prefix the formula uses",
			argv:   "gt slot run --role gastown/diamond -- env GT_TEST_DOCKER=1 go test ./internal/beads/",
			want:   "gt slot run",
			expect: true,
		},
		{
			name:   "a backgrounded gt done still running in the pane",
			argv:   "gt done",
			want:   "gt done",
			expect: true,
		},
		{
			name:   "gt done at the absolute path it was exec'd with",
			argv:   "/usr/local/bin/gt done",
			want:   "gt done",
			expect: true,
		},
		{
			name:   "gt done inside a background wrapper",
			argv:   `/bin/zsh -c source /Users/sloan/gt/.claude-town/shell-snapshots/snapshot-zsh-1790075016638-nti8n3.sh 2>/dev/null || true && eval 'gt done' < /dev/null && pwd -P >| /tmp/claude-fac2-cwd`,
			want:   "gt done",
			expect: true,
		},
		{
			name:   "make test as a shell -c body",
			argv:   `sh -c 'GOFLAGS=-p=8 make test'`,
			want:   "make test",
			expect: true,
		},
		{
			name:   "make test as a login shell's short-flag cluster",
			argv:   `bash -lc 'make test'`,
			want:   "make test",
			expect: true,
		},
		{
			name:   "a make test after && in an eval body is a live suite",
			argv:   `eval 'make clean && make test'`,
			want:   "make test",
			expect: true,
		},
		{
			name:   "a test that only follows && in an eval body is data",
			argv:   `eval 'make clean && git add test'`,
			expect: false,
		},
		{
			name:   "a test after a semicolon in a shell -c body is data",
			argv:   `sh -c 'make clean; git add test'`,
			expect: false,
		},
		{
			name:   "an unrelated background sleep is not a suite",
			argv:   "sleep 60",
			expect: false,
		},
		{
			name:   "the keep-awake process every pane carries is not a suite",
			argv:   "caffeinate -i -t 300",
			expect: false,
		},
		{
			name:   "gt slot status is a read, not a suite",
			argv:   "gt slot status",
			expect: false,
		},
		{
			name:   "a grep that only mentions the gate stays data",
			argv:   `grep -rn "make test" docs/`,
			expect: false,
		},
		{
			name:   "grep -c is a count, not a shell running a command",
			argv:   `grep -c "make test" docs/writing-for-agents.md`,
			expect: false,
		},
		{
			name:   "a grep for the slot wrapper stays data",
			argv:   `grep -rn "gt slot run" internal/`,
			expect: false,
		},
		{
			name:   "a grep for gt done stays data",
			argv:   `grep -rn "gt done" internal/`,
			expect: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := polecatStopVerificationArgv(tc.argv, gates)
			if ok != tc.expect {
				t.Fatalf("polecatStopVerificationArgv(%q) = (%q, %v), want ok=%v", tc.argv, got, ok, tc.expect)
			}
			if ok && got != tc.want {
				t.Fatalf("polecatStopVerificationArgv(%q) named %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// TestStopCheckGateFromCommand guards the rig-config half of the pane scan:
// the configured gate must reduce to the pair the process table carries, with
// the env prefix and the flag arguments out of the way.
func TestStopCheckGateFromCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		command string
		want    polecat.Invocation
		ok      bool
	}{
		{command: "GOFLAGS=-p=8 make test", want: polecat.Invocation{Program: "make", Target: "test"}, ok: true},
		{command: "make test", want: polecat.Invocation{Program: "make", Target: "test"}, ok: true},
		{command: "make -j4 test", want: polecat.Invocation{Program: "make", Target: "test"}, ok: true},
		{command: "/usr/local/bin/make lint", want: polecat.Invocation{Program: "make", Target: "lint"}, ok: true},
		{command: "go test ./...", want: polecat.Invocation{Program: "go", Target: "test"}, ok: true},
		{command: "pytest -q", want: polecat.Invocation{Program: "pytest"}, ok: true},
		{command: "", ok: false},
		{command: "   ", ok: false},
	}

	for _, tc := range cases {
		got, ok := stopCheckGateFromCommand(tc.command)
		if ok != tc.ok {
			t.Fatalf("stopCheckGateFromCommand(%q) ok = %v, want %v", tc.command, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("stopCheckGateFromCommand(%q) = %+v, want %+v", tc.command, got, tc.want)
		}
	}
}

// The pane listing parse, the ps parse and the descendant walk this guard's
// reads use are polecat.ParsePanes, polecat.ParseProcessTable and
// polecat.DescendantArgvs, tested where they live.
