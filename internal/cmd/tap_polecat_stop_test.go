package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

const polecatStopTestBranch = "polecat/test/gt-ksnv@abc123"

func TestPolecatStopPendingWork(t *testing.T) {
	t.Parallel()
	t.Run("clean feature branch has no pending work", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})

	t.Run("non-runtime dirty work is pending", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "internal/cmd/work.go", "package cmd\n")

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if !pending {
			t.Fatal("pending = false, want true")
		}
		if !strings.Contains(reason, "non-runtime dirty") {
			t.Fatalf("reason = %q, want non-runtime dirty work", reason)
		}
	})

	t.Run("runtime-only dirty work is ignored", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, ".opencode/state.json", "{}\n")

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})

	t.Run("branch stash is pending", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "stash-work.txt", "saved work\n")
		runPolecatStopTestGit(t, repo, "stash", "push", "-u", "-m", "branch stash")

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if !pending {
			t.Fatal("pending = false, want true")
		}
		if !strings.Contains(reason, "branch stash") {
			t.Fatalf("reason = %q, want branch stash", reason)
		}
	})

	t.Run("pushed source branch still pending until target contains it", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "submitted.go", "package main\n")
		runPolecatStopTestGit(t, repo, "add", "submitted.go")
		runPolecatStopTestGit(t, repo, "commit", "-m", "add submitted work")
		runPolecatStopTestGit(t, repo, "push", "origin", "HEAD:"+polecatStopTestBranch)

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if !pending {
			t.Fatal("pending = false, want true")
		}
		if !strings.Contains(reason, "unsubmitted commit") {
			t.Fatalf("reason = %q, want unsubmitted commit", reason)
		}
	})

	t.Run("target-contained commit has no pending work", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "merged.go", "package main\n")
		runPolecatStopTestGit(t, repo, "add", "merged.go")
		runPolecatStopTestGit(t, repo, "commit", "-m", "add merged work")
		runPolecatStopTestGit(t, repo, "checkout", "main")
		runPolecatStopTestGit(t, repo, "merge", "--ff-only", polecatStopTestBranch)
		runPolecatStopTestGit(t, repo, "push", "origin", "main")
		runPolecatStopTestGit(t, repo, "checkout", polecatStopTestBranch)

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})

	t.Run("invalid repo fails closed", func(t *testing.T) {
		pending, reason, err := polecatStopPendingWork(t.TempDir(), polecatStopTestBranch)
		if err == nil {
			t.Fatal("polecatStopPendingWork error = nil, want error")
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})
}

// TestPolecatStopVerificationRunning guards the turn-boundary fix for
// gt-couv: the Stop hook must not treat "my own slot-wrapped suite is still
// running" as abandonment, but a slot held by an unrelated polecat/rig must
// not falsely suppress the auto-done either.
//
// Serial because the first subtest counts lister calls: a counting stub is a
// value no peer test can share, so it cannot be installed once and left
// (gt-k317).
func TestPolecatStopVerificationRunning(t *testing.T) {
	t.Run("no slot held", func(t *testing.T) {
		townRoot := t.TempDir()

		// This runs at every turn boundary and reads only the flock, so it
		// must not shell out to `docker ps` (gt-a8kx).
		var calls int
		restore := slot.SetContainerListerForTest(func() ([]string, error) {
			calls++
			return nil, nil
		})
		defer restore()

		busy, reason := polecatStopVerificationRunning(townRoot, "gastown", "coral")
		if busy {
			t.Fatalf("busy = true (%s), want false when nothing holds the slot", reason)
		}
		if calls != 0 {
			t.Fatalf("polecatStopVerificationRunning probed docker %d time(s); it reads only the flock", calls)
		}
	})

	t.Run("slot held by this polecat", func(t *testing.T) {
		stubNoContainers(t)
		townRoot := t.TempDir()

		handle, err := slot.Acquire(townRoot, "gastown/coral", time.Second)
		if err != nil {
			t.Fatalf("slot.Acquire: %v", err)
		}
		defer handle.Release()

		busy, reason := polecatStopVerificationRunning(townRoot, "gastown", "coral")
		if !busy {
			t.Fatal("busy = false, want true when this polecat holds the slot")
		}
		if !strings.Contains(reason, "gastown/coral") {
			t.Fatalf("reason = %q, want it to name the holder gastown/coral", reason)
		}
	})

	t.Run("slot held by a different polecat does not suppress", func(t *testing.T) {
		stubNoContainers(t)
		townRoot := t.TempDir()

		handle, err := slot.Acquire(townRoot, "gastown/citrine", time.Second)
		if err != nil {
			t.Fatalf("slot.Acquire: %v", err)
		}
		defer handle.Release()

		busy, reason := polecatStopVerificationRunning(townRoot, "gastown", "coral")
		if busy {
			t.Fatalf("busy = true (%s), want false when a different polecat holds the slot", reason)
		}
	})
}

// TestPolecatStopCommittedWithinGrace guards the second turn-boundary signal
// for gt-couv: a commit that just landed means the polecat is very likely
// still mid-formula (about to build/lint/test), while an old commit carries
// no such signal.
func TestPolecatStopCommittedWithinGrace(t *testing.T) {
	t.Parallel()
	t.Run("fresh commit is within grace", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)

		recent, err := polecatStopCommittedWithinGrace(repo)
		if err != nil {
			t.Fatalf("polecatStopCommittedWithinGrace: %v", err)
		}
		if !recent {
			t.Fatal("recent = false, want true for a commit made moments ago")
		}
	})

	t.Run("old commit is outside grace", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		oldDate := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
		cmd := exec.Command("git", "commit", "--amend", "--no-edit", "--date", oldDate)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+oldDate)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit --amend: %v\n%s", err, out)
		}

		recent, err := polecatStopCommittedWithinGrace(repo)
		if err != nil {
			t.Fatalf("polecatStopCommittedWithinGrace: %v", err)
		}
		if recent {
			t.Fatal("recent = true, want false for a 10-minute-old commit")
		}
	})

	t.Run("invalid repo fails closed", func(t *testing.T) {
		recent, err := polecatStopCommittedWithinGrace(t.TempDir())
		if err == nil {
			t.Fatal("polecatStopCommittedWithinGrace error = nil, want error")
		}
		if recent {
			t.Fatal("recent = true, want false on error")
		}
	})
}

// polecatStopWrapperArgv is the real shape of a Claude Code background Bash
// process (captured from `ps -ww -eo pid,ppid,args` on the host running this
// suite): a snapshot preamble, then the command as a quoted eval argument, then
// a trailing cwd write. The command is the only part that matters to the pane
// scan, and it is the part a plain tokenization of the wrapper cannot see.
const polecatStopWrapperArgv = `/bin/zsh -c source /Users/sloan/gt/.claude-town/shell-snapshots/snapshot-zsh-1790075016638-nti8n3.sh 2>/dev/null || true && setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL 2>/dev/null || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'gt slot run --role gastown/diamond -- GOFLAGS=-p=8 make test' < /dev/null && pwd -P >| /tmp/claude-fac2-cwd`

// TestPolecatStopVerificationArgv guards the gt-78n0 pane scan: the commands
// that mean "this polecat is still verifying" must be recognized wherever the
// wrapper buries them, and the ones that only mention a gate must not be.
func TestPolecatStopVerificationArgv(t *testing.T) {
	t.Parallel()

	gates := []stopCheckGate{
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
		want    stopCheckGate
		ok      bool
	}{
		{command: "GOFLAGS=-p=8 make test", want: stopCheckGate{Program: "make", Target: "test"}, ok: true},
		{command: "make test", want: stopCheckGate{Program: "make", Target: "test"}, ok: true},
		{command: "make -j4 test", want: stopCheckGate{Program: "make", Target: "test"}, ok: true},
		{command: "/usr/local/bin/make lint", want: stopCheckGate{Program: "make", Target: "lint"}, ok: true},
		{command: "go test ./...", want: stopCheckGate{Program: "go", Target: "test"}, ok: true},
		{command: "pytest -q", want: stopCheckGate{Program: "pytest"}, ok: true},
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

// TestStopCheckDescendantArgvs guards the tree walk that scopes the scan to
// the polecat's own pane: descendants of a pane must be found at any depth,
// and another pane's processes must never be.
func TestStopCheckDescendantArgvs(t *testing.T) {
	t.Parallel()

	// Pane 100's agent spawns a wrapper (110) that spawns the suite (111);
	// pane 200 is a different polecat and shares no ancestry.
	procs := []stopCheckProc{
		{PID: 100, PPID: 1, Args: "claude --session-id a"},
		{PID: 110, PPID: 100, Args: "zsh -c eval 'make test'"},
		{PID: 111, PPID: 110, Args: "make test"},
		{PID: 200, PPID: 1, Args: "claude --session-id b"},
		{PID: 210, PPID: 200, Args: "make lint"},
	}

	got := stopCheckDescendantArgvs([]int{100}, procs)
	if len(got) != 2 {
		t.Fatalf("descendants of pane 100 = %v, want the wrapper and its suite", got)
	}
	for _, argv := range got {
		if strings.Contains(argv, "make lint") {
			t.Fatalf("descendants of pane 100 included another pane's process: %q", argv)
		}
	}
	if len(stopCheckDescendantArgvs([]int{999}, procs)) != 0 {
		t.Fatal("a pane with no processes must yield no descendants")
	}
}

// TestParsePolecatStopPanes guards the pane lookup: only the polecat's own
// session's panes are returned, and every pane of it, not just the first.
func TestParsePolecatStopPanes(t *testing.T) {
	t.Parallel()

	out := "gt-diamond\t42057\ngt-diamond\t42058\ngt-refinery\t66606\nhq-mayor\t81505\n"
	got := parsePolecatStopPanes(out, "gt-diamond")
	if len(got) != 2 || got[0] != 42057 || got[1] != 42058 {
		t.Fatalf("parsePolecatStopPanes = %v, want [42057 42058]", got)
	}
	if len(parsePolecatStopPanes(out, "gt-missing")) != 0 {
		t.Fatal("an unknown session must yield no panes")
	}
	if len(parsePolecatStopPanes("no tabs here\n", "gt-diamond")) != 0 {
		t.Fatal("malformed output must yield no panes")
	}
}

// TestParseStopCheckProcessTable guards the ps parse: argv keeps its spaces,
// and the header and malformed rows are skipped.
func TestParseStopCheckProcessTable(t *testing.T) {
	t.Parallel()

	out := "  PID  PPID ARGS\n" +
		"42057     1 claude --session-id b49c75ca\n" +
		"59375 59370 sh -c echo hi; sleep 60\n" +
		"notanumber 1 junk\n"
	got := parseStopCheckProcessTable(out)
	if len(got) != 2 {
		t.Fatalf("parseStopCheckProcessTable returned %d rows, want 2: %+v", len(got), got)
	}
	if got[1].PID != 59375 || got[1].PPID != 59370 || got[1].Args != "sh -c echo hi; sleep 60" {
		t.Fatalf("second row = %+v, want the wrapper's pid, parent and full argv", got[1])
	}
}

func initPolecatStopTestRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	origin := filepath.Join(tmp, "origin.git")

	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runPolecatStopTestGit(t, repo, "init")
	runPolecatStopTestGit(t, repo, "branch", "-M", "main")
	runPolecatStopTestGit(t, repo, "config", "user.email", "test@test.com")
	runPolecatStopTestGit(t, repo, "config", "user.name", "Test User")
	writePolecatStopTestFile(t, repo, "README.md", "# Test\n")
	runPolecatStopTestGit(t, repo, "add", "README.md")
	runPolecatStopTestGit(t, repo, "commit", "-m", "initial")

	runPolecatStopTestGit(t, tmp, "init", "--bare", origin)
	runPolecatStopTestGit(t, repo, "remote", "add", "origin", origin)
	runPolecatStopTestGit(t, repo, "push", "-u", "origin", "main")
	runPolecatStopTestGit(t, repo, "checkout", "-b", polecatStopTestBranch)

	return repo
}

func writePolecatStopTestFile(t *testing.T, repo, rel, contents string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func runPolecatStopTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	fullArgs := append([]string{"-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
