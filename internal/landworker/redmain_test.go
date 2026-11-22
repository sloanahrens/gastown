package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	pkgA = "github.com/steveyegge/gastown/internal/a"
	pkgB = "github.com/steveyegge/gastown/internal/b"
)

type redMainHarness struct {
	bd     *beadsfake.Fake
	r      *RedMain
	reruns []string
	// rerunExit is each package's rerun exit code; absent means 0.
	rerunExit map[string]int
	// rerunErr makes a package's rerun fail to run at all.
	rerunErr map[string]error
	status   []string
}

func newRedMainHarness(t *testing.T) *redMainHarness {
	t.Helper()
	h := &redMainHarness{bd: beadsfake.New(beadsfake.WithPrefix("gt")), rerunExit: map[string]int{}, rerunErr: map[string]error{}}
	h.r = &RedMain{Rig: "gastown", Beads: h.bd, Logf: t.Logf,
		Rerun: func(_ context.Context, cmd, pkg string, pl PostLand) PostLandResult {
			h.reruns = append(h.reruns, cmd+" "+pkg+"@"+pl.Commit)
			code := h.rerunExit[pkg]
			return PostLandResult{ExitCode: code, Err: h.rerunErr[pkg], Tail: "--- FAIL: TestX in " + pkg}
		},
		Status: func(line string) { h.status = append(h.status, line) },
	}
	return h
}

func (h *redMainHarness) open(t *testing.T) map[string]*beads.Issue {
	t.Helper()
	issues, err := h.bd.List(beads.ListOptions{Status: "open", Label: LabelRedMain, Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*beads.Issue{}
	for _, is := range issues {
		out[is.Title] = is
	}
	return out
}

func (h *redMainHarness) lastStatus() string { return h.status[len(h.status)-1] }

func pkgs(passed map[string]bool) []land.PackageResult {
	var out []land.PackageResult
	for _, p := range []string{pkgA, pkgB} {
		if v, ok := passed[p]; ok {
			out = append(out, land.PackageResult{Package: p, Passed: v})
		}
	}
	return out
}

func TestRedMainRerunsOnceAndFilesOneBeadPerStillRedPackage(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.rerunExit[pkgA] = 1 // A fails again; B passes its rerun
	pl := PostLand{BeadID: "gt-land1", Commit: "c1c1c1c1c1c1"}
	res := PostLandResult{ExitCode: 2, Packages: append(pkgs(map[string]bool{pkgA: false, pkgB: false}), land.PackageResult{Package: pkgA})}
	h.r.Red(context.Background(), "make test-slow", pl, res)

	if got := strings.Join(h.reruns, "|"); got != "make test-slow "+pkgA+"@c1c1c1c1c1c1|make test-slow "+pkgB+"@c1c1c1c1c1c1" {
		t.Fatalf("reruns %s; want one per failing package", got)
	}
	open := h.open(t)
	a := open[RedMainTitle("gastown", pkgA)]
	if len(open) != 1 || a == nil || a.Priority != 1 || !strings.Contains(a.Description, "gt-land1") || !strings.Contains(a.Description, "--- FAIL: TestX in "+pkgA) {
		t.Fatalf("open red-main beads %+v; want only %s's, citing the landing and the rerun's output", open, pkgA)
	}
	if s := h.lastStatus(); !strings.HasPrefix(s, "main RED at c1c1c1c1 (landed by gt-land1): "+pkgA+" ["+a.ID+"]") || !strings.HasSuffix(s, "flaky (passed on rerun): "+pkgB) {
		t.Fatalf("status %q", s)
	}

	// Red again at a later landing: the open bead gets a comment, not a twin.
	h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-land2", Commit: "c2c2c2c2"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
	if open := h.open(t); len(open) != 1 {
		t.Fatalf("second red filed a duplicate: %+v", open)
	}
	cs, _ := h.bd.Comments(a.ID)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Text, "still red: ") || !strings.Contains(cs[0].Text, "gt-land2") {
		t.Fatalf("comments on %s: %+v", a.ID, cs)
	}
}

func TestRedMainAllFlakyFilesNothing(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-land1", Commit: "c1c1c1c1c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
	if open := h.open(t); len(open) != 0 {
		t.Fatalf("flaky package filed %+v", open)
	}
	if s := h.lastStatus(); s != "main GREEN at c1c1c1c1 after rerun (landed by gt-land1); flaky (passed on rerun): "+pkgA {
		t.Fatalf("status %q", s)
	}
}

func TestRedMainClosesBeadsForPackagesThatPass(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.rerunExit[pkgA], h.rerunExit[pkgB] = 1, 1
	ctx := context.Background()
	h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false, pkgB: false})})
	// A red run that names no package files its own bead and reruns nothing.
	h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l2", Commit: "c2"}, PostLandResult{ExitCode: 2, Tail: "shell tests FAILED"})
	if len(h.reruns) != 2 || len(h.open(t)) != 3 {
		t.Fatalf("reruns %v open %v", h.reruns, h.open(t))
	}
	// Someone else's bead on another rig is not ours to close.
	other, err := h.bd.Create(beads.CreateOptions{Title: RedMainTitle("hm", pkgA), Labels: []string{LabelRedMain}})
	if err != nil {
		t.Fatal(err)
	}

	// B passes while A stays red: only B's bead closes.
	h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l3", Commit: "c3"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false, pkgB: true})})
	open := h.open(t)
	if open[RedMainTitle("gastown", pkgB)] != nil || open[RedMainTitle("gastown", pkgA)] == nil || open[RedMainTitle("gastown", redMainNoPackage)] == nil {
		t.Fatalf("after B passed: %v", open)
	}

	// Green closes every bead of this rig, the no-package one included.
	h.r.Green(ctx, "make test-slow", PostLand{BeadID: "gt-l4", Commit: "c4c4c4c4c4"}, PostLandResult{})
	open = h.open(t)
	if len(open) != 1 || open[other.Title] == nil {
		t.Fatalf("after green: %v; want only the other rig's bead", open)
	}
	if s := h.lastStatus(); s != "main GREEN at c4c4c4c4 (landed by gt-l4)" {
		t.Fatalf("status %q", s)
	}
}

// gt-f2voh: a daemon stopping mid-rerun reaches no verdict, so it files
// nothing, but it must say so: the red run is superseded by the run the
// restarted worker makes at the untested tip, and the log names that.
func TestRedMainCanceledLogsTheSupersede(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		preCancel  bool
		wantReruns int
	}{
		{name: "before any rerun", preCancel: true},
		{name: "mid rerun", wantReruns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRedMainHarness(t)
			var logs []string
			h.r.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.preCancel {
				cancel()
			}
			rerun := h.r.Rerun
			h.r.Rerun = func(ctx context.Context, cmd, pkg string, pl PostLand) PostLandResult {
				rerun(ctx, cmd, pkg, pl)
				cancel() // the daemon stops; the killed rerun exits non-zero
				return PostLandResult{ExitCode: -1}
			}
			pl := PostLand{Commit: "c1c1c1c1c1", Direct: true, From: "c0"}
			h.r.Red(ctx, "make test-slow", pl, PostLandResult{ExitCode: 2, LogPath: "/logs/post-1/test.log", Packages: pkgs(map[string]bool{pkgA: false, pkgB: false})})
			if len(h.reruns) != tc.wantReruns || len(h.open(t)) != 0 {
				t.Fatalf("a stopping daemon reran %v, filed %v", h.reruns, h.open(t))
			}
			all := strings.Join(logs, "\n")
			if !strings.Contains(all, "RED at c1c1c1c1") || !strings.Contains(all, "superseded") || !strings.Contains(all, "/logs/post-1/test.log") {
				t.Fatalf("log does not record the supersede:\n%s", all)
			}
		})
	}
}

// gt-f2voh: every red-main bead points at the full post-landing log.
func TestRedMainBeadsCiteTheFullLog(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	h.rerunExit[pkgA] = 1
	ctx := context.Background()
	h.r.Red(ctx, "make test-slow", PostLand{Commit: "c1"}, PostLandResult{ExitCode: 2, LogPath: "/logs/post-1/test.log", Packages: pkgs(map[string]bool{pkgA: false})})
	// A timeout or kill names no package: the bead says how the command exited.
	h.r.Red(ctx, "make test-slow", PostLand{Commit: "c2"}, PostLandResult{ExitCode: 2, LogPath: "/logs/post-2/test.log", Tail: "test-slow: FAILED in 1004s wall"})
	open := h.open(t)
	a, none := open[RedMainTitle("gastown", pkgA)], open[RedMainTitle("gastown", redMainNoPackage)]
	if a == nil || !strings.Contains(a.Description, "Full log: /logs/post-1/test.log") {
		t.Fatalf("package bead %+v; want the full log path", a)
	}
	if none == nil || !strings.Contains(none.Description, "exited 2") || !strings.Contains(none.Description, "Full log: /logs/post-2/test.log") ||
		!strings.Contains(none.Description, "FAILED in 1004s wall") {
		t.Fatalf("no-package bead %+v; want exit code, tail and full log path", none)
	}
}

// gt-7t9zrm: Q36 says a rerun that could not run files nothing for its package.
func TestRedMainRerunThatCouldNotRunFilesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		prior bool // an open bead for the package, from an earlier red run
		want  int
	}{{name: "no open bead"}, {name: "an open bead stays", prior: true, want: 1}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRedMainHarness(t)
			ctx := context.Background()
			if tc.prior {
				h.rerunExit[pkgA] = 1
				h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
				delete(h.rerunExit, pkgA)
			}
			h.rerunErr[pkgA] = errors.New("slot wait timed out")
			h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l2", Commit: "c2"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
			open := h.open(t)
			if len(open) != tc.want {
				t.Fatalf("open red-main beads %v; want %d", open, tc.want)
			}
			if a := open[RedMainTitle("gastown", pkgA)]; a != nil {
				cs, err := h.bd.Comments(a.ID)
				if err != nil || len(cs) != 0 {
					t.Fatalf("comments %+v (err %v); want the package's bead left as it is", cs, err)
				}
			}
		})
	}
}

// gt-7t9zrm: Q36 says no verdict is recorded when a failing package's rerun
// could not run. A package that passed its rerun does not make the commit
// green either: the one that could not run was never checked (om, attempt 1).
func TestRedMainRerunThatCouldNotRunRecordsNoVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		flaky bool // pkgB fails the first run and passes its rerun
	}{
		{name: "every rerun errored"},
		{name: "another package passed its rerun", flaky: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRedMainHarness(t)
			store := &MemoryMainState{}
			if err := store.Save(MainState{LastRun: "g0", LastGreen: "g0"}); err != nil {
				t.Fatal(err)
			}
			h.r.State = store
			h.rerunErr[pkgA] = errors.New("slot wait timed out")
			if !tc.flaky {
				h.rerunErr[pkgB] = errors.New("make: not found")
			}
			h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1c1c1c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false, pkgB: false})})
			st, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.LastRun != "g0" || st.LastGreen != "g0" {
				t.Fatalf("state %+v; want the previous verdict untouched", st)
			}
			if s := h.lastStatus(); !strings.HasPrefix(s, "main UNKNOWN at c1c1c1c1 ") || !strings.Contains(s, "no verdict") {
				t.Fatalf("status %q; want UNKNOWN and no verdict", s)
			}
		})
	}
}

// gt-4nz0bj: a main that cannot be judged for several runs in a row is
// counted, so an attention item can see a streak instead of one log line.
func TestRedMainUnknownRunsAreCounted(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	store := &MemoryMainState{}
	h.r.State = store
	// The state keeps the first line only, bounded to 160 bytes.
	h.rerunErr[pkgA] = errors.New(strings.Repeat("x", 200) + "\nthe rest of the stack")
	for i, commit := range []string{"c1", "c2", "c3"} {
		h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: fmt.Sprintf("gt-l%d", i+1), Commit: commit},
			PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.UnknownRuns != 3 {
		t.Fatalf("unknown runs %d; want 3 consecutive UNKNOWNs counted", st.UnknownRuns)
	}
	if st.UnknownCommit != "c3" {
		t.Fatalf("unknown commit %q; want the third run's commit", st.UnknownCommit)
	}
	if want := strings.Repeat("x", 160); st.UnknownCause != want {
		t.Fatalf("cause %q; want the first line cut to 160 bytes", st.UnknownCause)
	}
}

// gt-4nz0bj: the recorded cause is scrubbed, so a rerun error that quotes a
// credential never reaches the main state.
func TestRedMainUnknownCauseIsScrubbed(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	store := &MemoryMainState{}
	h.r.State = store
	h.rerunErr[pkgA] = errors.New("go test: token=ghp_0123456789abcdefghij failed")
	h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1"},
		PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st.UnknownCause, "ghp_0123456789") || !strings.Contains(st.UnknownCause, "[redacted]") {
		t.Fatalf("cause %q; want the credential masked", st.UnknownCause)
	}
}

// gt-4nz0bj: a decided verdict after UNKNOWNs clears the streak, so only
// consecutive UNKNOWNs are counted.
func TestRedMainDecidedVerdictResetsUnknownRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		decide func(h *redMainHarness)
	}{
		{name: "red", decide: func(h *redMainHarness) {
			delete(h.rerunErr, pkgA)
			h.rerunExit[pkgA] = 1
			h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-l3", Commit: "c3"},
				PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
		}},
		{name: "green", decide: func(h *redMainHarness) {
			h.r.Green(context.Background(), "make post-land", PostLand{BeadID: "gt-l3", Commit: "c3"}, PostLandResult{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRedMainHarness(t)
			store := &MemoryMainState{}
			h.r.State = store
			h.rerunErr[pkgA] = errors.New("slot wait timed out")
			for i, commit := range []string{"c1", "c2"} {
				h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: fmt.Sprintf("gt-l%d", i+1), Commit: commit},
					PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})})
			}
			if st, err := store.Load(); err != nil || st.UnknownRuns != 2 {
				t.Fatalf("after two UNKNOWNs: %+v, %v; want UnknownRuns 2", st, err)
			}
			tc.decide(h)
			st, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.UnknownRuns != 0 || st.UnknownCause != "" || st.UnknownCommit != "" {
				t.Fatalf("state %+v after a decided verdict; want the UNKNOWN streak cleared", st)
			}
		})
	}
}

// gt-7t9zrm: a package that did rerun and failed still files its bead while
// another package's rerun could not run, and the commit is red.
func TestRedMainRerunThatCouldNotRunStillFilesTheOneThatFailed(t *testing.T) {
	t.Parallel()
	h := newRedMainHarness(t)
	store := &MemoryMainState{}
	h.r.State = store
	h.rerunErr[pkgA] = errors.New("slot wait timed out")
	h.rerunExit[pkgB] = 1
	h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1c1c1c1"}, PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false, pkgB: false})})
	open := h.open(t)
	if len(open) != 1 || open[RedMainTitle("gastown", pkgB)] == nil {
		t.Fatalf("open %v; want only %s's bead", open, pkgB)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRun != "c1c1c1c1" || st.LastGreen == "c1c1c1c1" {
		t.Fatalf("state %+v; want the red verdict recorded", st)
	}
}

// gt-tghszi: a red run the daemon's stop cut short neither counts toward nor
// resets the UNKNOWN streak, and a main state that cannot be read or saved is
// logged and changes no verdict.
func TestRedMainSupersededAndUnreadableState(t *testing.T) {
	t.Parallel()
	res := PostLandResult{ExitCode: 2, Packages: pkgs(map[string]bool{pkgA: false})}
	t.Run("a superseded run between two UNKNOWNs neither counts nor resets", func(t *testing.T) {
		t.Parallel()
		h := newRedMainHarness(t)
		var logs []string
		h.r.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
		store := &MemoryMainState{}
		if err := store.Save(MainState{LastRun: "g0", LastGreen: "g0"}); err != nil {
			t.Fatal(err)
		}
		h.r.State = store
		h.rerunErr[pkgA] = errors.New("slot wait timed out")
		h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-l1", Commit: "c1"}, res)
		// The daemon stops during the rerun: the context is cancelled and the
		// killed rerun returns a non-zero exit with no error.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rerun := h.r.Rerun
		h.r.Rerun = func(ctx context.Context, cmd, pkg string, pl PostLand) PostLandResult {
			rerun(ctx, cmd, pkg, pl)
			cancel()
			return PostLandResult{ExitCode: -1}
		}
		h.r.Red(ctx, "make test-slow", PostLand{BeadID: "gt-l2", Commit: "c2"}, res)
		h.r.Rerun = rerun
		h.r.Red(context.Background(), "make test-slow", PostLand{BeadID: "gt-l3", Commit: "c3"}, res)
		st, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if st.UnknownRuns != 2 || st.UnknownCommit != "c3" {
			t.Fatalf("state %+v after UNKNOWN, superseded, UNKNOWN; want UnknownRuns 2 at c3", st)
		}
		if st.LastRun != "g0" || st.LastGreen != "g0" {
			t.Fatalf("state %+v; want the seeded verdict untouched by the superseded run", st)
		}
		// UNKNOWN runs file nothing, and the superseded run emits no status
		// line: the first and third UNKNOWN are the only ones.
		if open := h.open(t); len(open) != 0 {
			t.Fatalf("open red-main beads %v; want none", open)
		}
		if len(h.status) != 2 || !strings.HasPrefix(h.status[1], "main UNKNOWN at c3 ") {
			t.Fatalf("status lines %q; want the first and third UNKNOWN only", h.status)
		}
		if all := strings.Join(logs, "\n"); !strings.Contains(all, "superseded") {
			t.Fatalf("log does not record the supersede:\n%s", all)
		}
	})
	t.Run("an unreadable or unsavable state changes no verdict", func(t *testing.T) {
		t.Parallel()
		readErr := errors.New("read main state: unexpected end of JSON input")
		saveErr := errors.New("write main state: disk full")
		for _, tc := range []struct {
			name       string
			loadErr    error
			saveErr    error
			rerunErr   bool // the rerun could not run: UNKNOWN; otherwise it failed: RED
			green      bool // a green verdict, which has no rerun
			wantStatus string
			wantLog    string
			wantOpen   int
		}{
			{name: "unreadable on UNKNOWN", loadErr: readErr, rerunErr: true,
				wantStatus: "main UNKNOWN at c1 ", wantLog: "reading the main state"},
			{name: "unsavable on UNKNOWN", saveErr: saveErr, rerunErr: true,
				wantStatus: "main UNKNOWN at c1 ", wantLog: "saving the main state"},
			{name: "unreadable on RED", loadErr: readErr,
				wantStatus: "main RED at c1 ", wantLog: "reading the main state", wantOpen: 1},
			{name: "unsavable on GREEN", saveErr: saveErr, green: true,
				wantStatus: "main GREEN at c1 ", wantLog: "saving the main state"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				h := newRedMainHarness(t)
				var logs []string
				h.r.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
				// A working store holding the seeded verdict: the failing read or
				// write must leave it as it was.
				store := &flakyMainState{}
				if err := store.inner.Save(MainState{LastRun: "g0", LastGreen: "g0"}); err != nil {
					t.Fatal(err)
				}
				store.failLoad(tc.loadErr)
				store.failSave(tc.saveErr)
				h.r.State = store
				pl := PostLand{BeadID: "gt-l1", Commit: "c1"}
				ctx := context.Background()
				switch {
				case tc.green:
					h.r.Green(ctx, "make post-land", pl, PostLandResult{})
				case tc.rerunErr:
					h.rerunErr[pkgA] = errors.New("slot wait timed out")
					h.r.Red(ctx, "make test-slow", pl, res)
				default:
					h.rerunExit[pkgA] = 1
					h.r.Red(ctx, "make test-slow", pl, res)
				}
				if s := h.lastStatus(); !strings.HasPrefix(s, tc.wantStatus) {
					t.Fatalf("status %q; want prefix %q", s, tc.wantStatus)
				}
				if all := strings.Join(logs, "\n"); !strings.Contains(all, tc.wantLog) {
					t.Fatalf("log does not record the state error %q:\n%s", tc.wantLog, all)
				}
				if got := len(h.open(t)); got != tc.wantOpen {
					t.Fatalf("open red-main beads %d; want %d", got, tc.wantOpen)
				}
				// Reads and writes are checked on the working store once the
				// failures are cleared: it still holds the seeded verdict.
				store.failLoad(nil)
				store.failSave(nil)
				st, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				if st.LastRun != "g0" || st.LastGreen != "g0" || st.UnknownRuns != 0 {
					t.Fatalf("state %+v after the failed run; want the seeded verdict untouched", st)
				}
			})
		}
	})
}
