package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

type commentLog struct {
	mu   sync.Mutex
	byID map[string][]string
}

func (c *commentLog) AddComment(id, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string][]string{}
	}
	c.byID[id] = append(c.byID[id], text)
	return nil
}

func (c *commentLog) get(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.byID[id]...)
}

// blockingRun records each run and blocks it until released.
type blockingRun struct {
	mu      sync.Mutex
	runs    []PostLand
	started chan struct{}
	release chan PostLandResult
}

func newBlockingRun() *blockingRun {
	return &blockingRun{started: make(chan struct{}, 16), release: make(chan PostLandResult)}
}

func (b *blockingRun) run(_ context.Context, _ string, pl PostLand) PostLandResult {
	b.mu.Lock()
	b.runs = append(b.runs, pl)
	b.mu.Unlock()
	b.started <- struct{}{}
	return <-b.release
}

func (b *blockingRun) commits() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, r := range b.runs {
		out = append(out, r.Commit)
	}
	return out
}

func TestPostLandCoalescesToTheNewestLanding(t *testing.T) {
	t.Parallel()
	b := newBlockingRun()
	p := &PostLandRunner{Rig: "gastown", Command: func() string { return "make test-slow" }, Run: b.run, Beads: &commentLog{}, Logf: t.Logf}
	ctx := context.Background()
	p.Trigger(ctx, PostLand{BeadID: "gt-1", Commit: "c1"})
	<-b.started
	// Three landings finish while c1 runs: only the newest runs next.
	p.Trigger(ctx, PostLand{BeadID: "gt-2", Commit: "c2"})
	p.Trigger(ctx, PostLand{BeadID: "gt-3", Commit: "c3"})
	p.Trigger(ctx, PostLand{BeadID: "gt-4", Commit: "c4"})
	b.release <- PostLandResult{}
	<-b.started
	b.release <- PostLandResult{}
	p.Wait()
	if got := strings.Join(b.commits(), ","); got != "c1,c4" {
		t.Fatalf("runs at %s; want c1 then one coalesced run at c4", got)
	}
	// Idle again: the next landing starts a fresh run.
	p.Trigger(ctx, PostLand{BeadID: "gt-5", Commit: "c5"})
	<-b.started
	b.release <- PostLandResult{}
	p.Wait()
	if got := strings.Join(b.commits(), ","); got != "c1,c4,c5" {
		t.Fatalf("runs at %s", got)
	}
}

// gt-gb4ij: Busy brackets the whole run, coalesced runs included, so the
// daemon can hold an upgrade restart until the verdict.
func TestPostLandBusyBracketsQueuedRuns(t *testing.T) {
	t.Parallel()
	b := newBlockingRun()
	var mu sync.Mutex
	var calls []bool
	p := &PostLandRunner{Rig: "gastown", Command: func() string { return "make test-slow" }, Run: b.run, Logf: t.Logf,
		Busy: func(busy bool) {
			mu.Lock()
			calls = append(calls, busy)
			mu.Unlock()
		}}
	got := func() string {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Sprint(calls)
	}
	ctx := context.Background()
	p.Trigger(ctx, PostLand{BeadID: "gt-1", Commit: "c1"})
	<-b.started
	p.Trigger(ctx, PostLand{BeadID: "gt-2", Commit: "c2"})
	b.release <- PostLandResult{}
	<-b.started
	if g := got(); g != "[true]" {
		t.Fatalf("Busy calls %s mid-queue; want [true]", g)
	}
	b.release <- PostLandResult{}
	p.Wait()
	if g := got(); g != "[true false]" {
		t.Fatalf("Busy calls %s; want [true false]", g)
	}
}

func TestPostLandExitCodeRouting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		res         PostLandResult
		wantComment bool
		wantRed     bool
		wantGreen   bool
	}{
		{name: "green", res: PostLandResult{ExitCode: 0, Tail: "ok all"}, wantGreen: true},
		{name: "red", res: PostLandResult{ExitCode: 2, Tail: strings.Repeat("noise\n", 20) + "--- FAIL: TestSlow\nFAIL\tpkg"}, wantComment: true, wantRed: true},
		{name: "could not run", res: PostLandResult{ExitCode: -1, Err: errors.New("slot wait timed out")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			comments := &commentLog{}
			var red, green []PostLand
			p := &PostLandRunner{Rig: "gastown", Command: func() string { return "make test-slow" }, Beads: comments, Logf: t.Logf,
				Run:     func(context.Context, string, PostLand) PostLandResult { return tc.res },
				OnRed:   func(_ context.Context, _ string, pl PostLand, _ PostLandResult) { red = append(red, pl) },
				OnGreen: func(_ context.Context, _ string, pl PostLand, _ PostLandResult) { green = append(green, pl) }}
			p.Trigger(context.Background(), PostLand{BeadID: "gt-1", Commit: "abc123def456"})
			p.Wait()
			cs := comments.get("gt-1")
			if (len(cs) == 1) != tc.wantComment || (len(red) == 1) != tc.wantRed || (len(green) == 1) != tc.wantGreen {
				t.Fatalf("comments %q red %v green %v", cs, red, green)
			}
			if tc.wantComment {
				c := cs[0]
				if !strings.HasPrefix(c, "post-landing check RED at abc123def456: ") || !strings.Contains(c, "--- FAIL: TestSlow") ||
					strings.Count(c, "noise") != postLandTailLines-2 {
					t.Fatalf("comment:\n%s", c)
				}
			}
		})
	}
}

func TestPostLandDisabledRunsNothing(t *testing.T) {
	t.Parallel()
	p := &PostLandRunner{Command: func() string { return "  " }, Run: func(context.Context, string, PostLand) PostLandResult {
		t.Fatal("ran with no command configured")
		return PostLandResult{}
	}}
	p.Trigger(context.Background(), PostLand{BeadID: "gt-1", Commit: "c"})
	p.Wait()
}

type recordTrigger struct{ got []PostLand }

func (r *recordTrigger) Trigger(_ context.Context, pl PostLand) { r.got = append(r.got, pl) }

func TestPassTriggersPostLandForNewLandingsOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	trig := &recordTrigger{}
	h.w.PostLand = trig
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{LandedCommit: "landed1"}, nil }
	h.w.Pass(context.Background())
	if len(trig.got) != 1 || trig.got[0].Commit != "landed1" || trig.got[0].BeadID != "gt-abc" {
		t.Fatalf("triggers %+v", trig.got)
	}
	// A record repair is not a new landing.
	h2 := newHarness(t)
	trig2 := &recordTrigger{}
	h2.w.PostLand = trig2
	h2.seedReady(t, "gt-abc")
	h2.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Branch: branch, Head: "h", Target: "main", LandedCommit: "l1", Route: "daemon"}}
	h2.remote.contains["l1"] = true
	h2.w.startupDone = true
	h2.w.Pass(context.Background())
	if len(trig2.got) != 0 {
		t.Fatalf("repair triggered post-land: %+v", trig2.got)
	}
}

// gt-f2voh: a run cut short by the daemon stopping is no verdict. It goes to
// neither handler (a killed run would otherwise be filed as red main) and
// the log records that the restarted worker's run supersedes it.
func TestPostLandCanceledRunIsSupersededNotRed(t *testing.T) {
	t.Parallel()
	var logs []string
	var red []PostLand
	ctx, cancel := context.WithCancel(context.Background())
	p := &PostLandRunner{Rig: "gastown", Command: func() string { return "make test-slow" }, Beads: &commentLog{},
		Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		Run: func(context.Context, string, PostLand) PostLandResult {
			cancel()
			return PostLandResult{ExitCode: -1, LogPath: "/logs/post-9/test.log"}
		},
		OnRed: func(_ context.Context, _ string, pl PostLand, _ PostLandResult) { red = append(red, pl) }}
	p.Trigger(ctx, PostLand{Commit: "abc123def456", Direct: true, From: "c0"})
	p.Wait()
	all := strings.Join(logs, "\n")
	if len(red) != 0 || !strings.Contains(all, "superseded") || !strings.Contains(all, "/logs/post-9/test.log") {
		t.Fatalf("red %v; log:\n%s", red, all)
	}
}

// gt-f2voh: the RED log line names the failing packages and the full log,
// since its tail is usually the shell tests that ran after the Go suite.
func TestPostLandRedLogNamesPackagesAndLog(t *testing.T) {
	t.Parallel()
	var logs []string
	p := &PostLandRunner{Rig: "gastown", Command: func() string { return "make test-slow" },
		Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		Run: func(context.Context, string, PostLand) PostLandResult {
			return PostLandResult{ExitCode: 2, LogPath: "/logs/post-9/test.log", Tail: "makefile-gate_test: 33 passed",
				Packages: []land.PackageResult{{Package: "x/internal/cmd"}, {Package: "x/internal/ok", Passed: true}}}
		}}
	p.Trigger(context.Background(), PostLand{BeadID: "gt-1", Commit: "abc123def456"})
	p.Wait()
	all := strings.Join(logs, "\n")
	if !strings.Contains(all, "failing: x/internal/cmd;") || strings.Contains(all, "x/internal/ok") || !strings.Contains(all, "full log /logs/post-9/test.log") {
		t.Fatalf("log:\n%s", all)
	}
}
