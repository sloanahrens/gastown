package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// firedTimer is a timer whose timeout has already passed, so a shadow estimate
// that has not answered takes the timeout branch at once, with no wall clock.
func firedTimer(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

// neverTimer is a timer that does not fire, so the estimate's own answer wins.
func neverTimer(time.Duration) <-chan time.Time {
	return make(chan time.Time)
}

// storyPointNotices returns the tick's notices that the shadow estimate wrote.
func storyPointNotices(notices []string) []string {
	var out []string
	for _, n := range notices {
		if strings.Contains(n, "story points shadow") {
			out = append(out, n)
		}
	}
	return out
}

// TestSpecDispatchStoryPointShadowOffNeverEstimates: with the setting off the
// tick attempts no estimate, and the beads are slung as they were.
func TestSpecDispatchStoryPointShadowOffNeverEstimates(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"), cleanSpec("gt-b", 1, "2026-09-29T11:00:00Z"))
	env := f.env()
	env.PerTick = 2
	estimates := 0
	env.StoryPointShadow = false
	env.StoryPointEstimate = func(context.Context, string) error {
		estimates++
		return nil
	}
	r := runSpecDispatchCycle(env)

	if estimates != 0 {
		t.Fatalf("estimated %d bead(s) with the setting off, want none", estimates)
	}
	if got := strings.Join(f.slung, " "); got != "gt-a gt-b" {
		t.Fatalf("slung %q, want both beads", got)
	}
	if n := storyPointNotices(r.Notices); len(n) != 0 {
		t.Errorf("notices %q with the setting off, want none", n)
	}
}

// TestSpecDispatchStoryPointShadowEstimatesEachBeadBeforeItsSling: with the
// setting on, every bead the tick slings is estimated first, and the sling
// follows its own estimate.
func TestSpecDispatchStoryPointShadowEstimatesEachBeadBeforeItsSling(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"), cleanSpec("gt-b", 1, "2026-09-29T11:00:00Z"))
	env := f.env()
	env.PerTick = 2

	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	env.StoryPointShadow = true
	env.StoryPointEstimate = func(_ context.Context, id string) error {
		record("estimate:" + id)
		return nil
	}
	sling := env.Sling
	env.Sling = func(c specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
		record("sling:" + c.Spec.ID)
		return sling(c, resume, seat)
	}
	r := runSpecDispatchCycle(env)

	if len(r.Dispatched) != 2 {
		t.Fatalf("dispatched %d, want 2: %+v", len(r.Dispatched), r)
	}
	pos := map[string]int{}
	for i, e := range events {
		pos[e] = i
	}
	for _, id := range []string{"gt-a", "gt-b"} {
		est, okE := pos["estimate:"+id]
		slg, okS := pos["sling:"+id]
		if !okE || !okS {
			t.Fatalf("events %v: %s needs an estimate and a sling", events, id)
		}
		if est > slg {
			t.Errorf("events %v: %s slung before it was estimated", events, id)
		}
	}
	if n := storyPointNotices(r.Notices); len(n) != 0 {
		t.Errorf("notices %q on a clean estimate, want none", n)
	}
}

// TestSpecDispatchStoryPointShadowErrorLogsOneLineAndSlings: an estimate that
// fails logs one line, and the bead is still slung.
func TestSpecDispatchStoryPointShadowErrorLogsOneLineAndSlings(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointShadow = true
	env.After = neverTimer
	env.StoryPointEstimate = func(context.Context, string) error {
		return errors.New("embedder refused")
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q after an estimate error, want gt-a", got)
	}
	n := storyPointNotices(r.Notices)
	if len(n) != 1 {
		t.Fatalf("story points notices = %q, want exactly one line", n)
	}
	if !strings.Contains(n[0], "gt-a") || !strings.Contains(n[0], "embedder refused") {
		t.Errorf("notice %q does not name the bead and the error", n[0])
	}
}

// TestSpecDispatchStoryPointShadowTimeoutLogsOneLineAndSlings: an estimate that
// runs past its timeout logs one line, is canceled, and the bead is still
// slung.
func TestSpecDispatchStoryPointShadowTimeoutLogsOneLineAndSlings(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointShadow = true
	env.After = firedTimer
	// The estimate hands its context out, then waits for it to be canceled.
	// The tick cancels before it returns, so the context is done by the time
	// the cycle is over, and nothing waits on a clock.
	handed := make(chan context.Context, 1)
	env.StoryPointEstimate = func(ctx context.Context, _ string) error {
		handed <- ctx
		<-ctx.Done()
		return ctx.Err()
	}
	r := runSpecDispatchCycle(env)

	if ctx := <-handed; ctx.Err() == nil {
		t.Fatal("the timed-out estimate's context was not canceled")
	}
	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q after a timeout, want gt-a", got)
	}
	n := storyPointNotices(r.Notices)
	if len(n) != 1 {
		t.Fatalf("story points notices = %q, want exactly one line", n)
	}
	if !strings.Contains(n[0], "no estimate within") {
		t.Errorf("notice %q does not say the estimate timed out", n[0])
	}
}

// TestSpecDispatchStoryPointShadowVerdict: the production shadow estimate fails
// on a run error and on a bead with no estimate, with the reason, and passes an
// estimate that was written or was already current.
func TestSpecDispatchStoryPointShadowVerdict(t *testing.T) {
	t.Parallel()
	runErr := errors.New("bd write refused")
	cases := []struct {
		name    string
		outcome specEstimateOutcome
		err     error
		wantErr error
		wantMsg string
	}{
		{name: "run error", err: runErr, wantErr: runErr},
		{name: "no estimate names the reason", outcome: specEstimateOutcome{Action: "none", Reason: "embedder unreachable"}, wantMsg: "no estimate: embedder unreachable"},
		{name: "estimated passes", outcome: specEstimateOutcome{Action: "estimated", Points: 3}},
		{name: "unchanged passes", outcome: specEstimateOutcome{Action: "unchanged"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := specShadowErr(tc.outcome, tc.err)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(got, tc.wantErr) {
					t.Fatalf("specShadowErr = %v, want %v", got, tc.wantErr)
				}
			case tc.wantMsg != "":
				if got == nil || got.Error() != tc.wantMsg {
					t.Fatalf("specShadowErr = %v, want %q", got, tc.wantMsg)
				}
			default:
				if got != nil {
					t.Fatalf("specShadowErr = %v, want nil", got)
				}
			}
		})
	}
}
