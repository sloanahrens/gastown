package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// slingEstimateRoutes are the two hand-sling routes that hook a bead: the rig
// target, which the engine hooks, and the inline target, which hooks it itself.
var slingEstimateRoutes = []struct {
	name   string
	target string
}{
	{name: "rig target", target: "gastown"},
	{name: "inline target", target: "gastown/crew/mel"},
}

// estimateThrough wires the harness's estimate through the lint's seam over
// store and stub, as production does, and logs each estimate run it starts.
func (h *slingHarness) estimateThrough(store specEstimateStore, stub *specEstimateStub) {
	run := specLintEstimateRunFor(
		func(string) specEstimateStore { return store },
		func(string) specEstimateRunner { return specEstimateRunnerFor(stub) })
	h.run.estimateFirst = func(ctx context.Context, _, beadID string) (specEstimateOutcome, error) {
		h.record("estimate %s", beadID)
		return run(ctx, beadID)
	}
}

// TestSlingEstimateRunsOnceBeforeHook: a sling of a bead with no sp-minutes
// label runs the estimator once, writes the estimate's labels, and only then
// hooks the bead, on either route.
func TestSlingEstimateRunsOnceBeforeHook(t *testing.T) {
	t.Parallel()
	for _, route := range slingEstimateRoutes {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			db := specEstimateSeed()
			stub := specEstimateOK()
			h.estimateThrough(db, stub)
			h.addBead("gt-est", beadInfo{})

			if err := h.sling("gt-est", route.target); err != nil {
				t.Fatalf("sling: %v", err)
			}
			if stub.calls != 1 {
				t.Fatalf("the estimator ran %d times, want once", stub.calls)
			}
			labels := strings.Join(specEstimateLabelsOn(t, db), " ")
			for _, want := range []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3"} {
				if !strings.Contains(labels, want) {
					t.Errorf("labels %q do not carry %q", labels, want)
				}
			}
			calls := h.log()
			est, hook := indexOfCall(calls, "estimate gt-est"), indexOfCall(calls, "hook gt-est")
			if est < 0 || hook < 0 || est > hook {
				t.Fatalf("want the estimate before the hook, log:\n  %s", strings.Join(calls, "\n  "))
			}
			if !strings.Contains(h.out.String(), "Estimated gt-est: sp:5 sp-minutes:6.3") {
				t.Errorf("output %q does not print the measured sp-minutes on one line", h.out.String())
			}
		})
	}
}

// TestSlingEstimateRefusesWithoutHook: an estimate that fails, makes none, or
// makes one that cannot be labeled refuses the sling with the cause in the
// message, and the bead is not hooked nor a polecat spawned.
func TestSlingEstimateRefusesWithoutHook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		stub *specEstimateStub
		// wantErr are phrases the refusal must name.
		wantErr []string
	}{
		{
			name:    "the estimator fails",
			stub:    &specEstimateStub{err: errors.New("jev battery cannot open")},
			wantErr: []string{"refusing to sling gt-est", "no story-point estimate", "jev battery cannot open"},
		},
		{
			name:    "the estimate cannot be labeled",
			stub:    &specEstimateStub{result: estimate.Result{Points: 99, Minutes: 6, Rung: "all"}},
			wantErr: []string{"refusing to sling gt-est", "not on the scale"},
		},
	} {
		for _, route := range slingEstimateRoutes {
			t.Run(tc.name+", "+route.name, func(t *testing.T) {
				t.Parallel()
				h := newSlingHarness(t)
				// Each route gets its own stub: the rows run in parallel, and a
				// shared one would count the other route's estimate too.
				stub := &specEstimateStub{result: tc.stub.result, err: tc.stub.err}
				h.estimateThrough(specEstimateSeed(), stub)
				h.addBead("gt-est", beadInfo{})

				err := h.sling("gt-est", route.target)
				if stub.calls != 1 {
					t.Fatalf("the estimator ran %d times, want once", stub.calls)
				}
				for _, want := range tc.wantErr {
					wantSlingErr(t, err, want)
				}
				h.wantNo("hook ")
				h.wantNo("spawn ")
				h.wantNo("resolve target")
			})
		}
	}
}

// slingCtxKey marks a caller's context so a test can tell it from a fresh
// Background.
type slingCtxKey struct{}

// TestSlingEstimateRunsOnCallerContext: the estimate runs on the context the
// sling was given, not a fresh Background, on either route (gt-8oi0dz).
func TestSlingEstimateRunsOnCallerContext(t *testing.T) {
	t.Parallel()
	for _, route := range slingEstimateRoutes {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead("gt-est", beadInfo{})
			var seen any = "the estimate did not run"
			h.run.estimateFirst = func(ctx context.Context, _, _ string) (specEstimateOutcome, error) {
				seen = ctx.Value(slingCtxKey{})
				return specEstimateOutcome{Action: "estimated", Points: 5, Rung: "all", Minutes: 6}, nil
			}
			ctx := context.WithValue(context.Background(), slingCtxKey{}, "the caller")

			if err := h.run.run(ctx, nil, []string{"gt-est", route.target}); err != nil {
				t.Fatalf("sling: %v", err)
			}
			if seen != "the caller" {
				t.Errorf("the estimate ran on a context carrying %v, want the caller's", seen)
			}
		})
	}
}

// TestSlingRigTargetReadsBeadOnce: a rig-target sling reads the bead once: the
// held check and the estimate answer from the same read (gt-8oi0dz).
func TestSlingRigTargetReadsBeadOnce(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead("gt-est", beadInfo{})
	reads := 0
	read := h.run.beadInfo
	h.run.beadInfo = func(id string) (*beadInfo, error) {
		reads++
		return read(id)
	}

	if err := h.sling("gt-est", "gastown"); err != nil {
		t.Fatalf("sling: %v", err)
	}
	if reads != 1 {
		t.Errorf("the sling read the bead %d times, want once", reads)
	}
}

// TestSlingEstimateRefusesWhenBeadUnreadable: a rig sling whose bead cannot be
// read is refused before the estimate runs, and nothing is hooked.
func TestSlingEstimateRefusesWhenBeadUnreadable(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	stub := specEstimateOK()
	h.estimateThrough(specEstimateSeed(), stub)
	h.addBead("gt-est", beadInfo{})
	h.run.beadInfo = func(string) (*beadInfo, error) { return nil, errors.New("bd show failed") }
	h.run.opts.force = true

	wantSlingErr(t, h.sling("gt-est", "gastown"), "checking bead status")
	if stub.calls != 0 {
		t.Errorf("the estimator ran %d times, want none for an unreadable bead", stub.calls)
	}
	h.wantNo("hook ")
}

// TestSlingEstimateRefusesWhenStoreFails: a bead the estimate cannot read is a
// refusal that names the failed estimate, and the bead is not hooked.
func TestSlingEstimateRefusesWhenStoreFails(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.estimateThrough(unreadableEstimateStore{specEstimateSeed()}, specEstimateOK())
	h.addBead("gt-est", beadInfo{})

	err := h.sling("gt-est", "gastown")
	wantSlingErr(t, err, "the story-point estimate failed")
	h.wantNo("hook ")
}

// TestSlingEstimateSkipsEstimatedAndDryRun: a bead that already carries
// sp-minutes, and a dry run of any bead, call the estimator zero times and
// write no label.
func TestSlingEstimateSkipsEstimatedAndDryRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		labels []string
		dryRun bool
		// wantHook is whether the sling still hooks the bead.
		wantHook bool
	}{
		{name: "the bead already has an estimate", labels: []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3"}, wantHook: true},
		{name: "a dry run of an unestimated bead", dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			db := specEstimateSeed()
			counter := &specEstimateWriteCounter{specEstimateStore: db}
			stub := specEstimateOK()
			h.estimateThrough(counter, stub)
			h.addBead("gt-est", beadInfo{Labels: tc.labels})
			h.run.opts.dryRun = tc.dryRun

			if err := h.sling("gt-est", "gastown"); err != nil {
				t.Fatalf("sling: %v", err)
			}
			if stub.calls != 0 {
				t.Errorf("the estimator ran %d times, want none", stub.calls)
			}
			if counter.updates != 0 || counter.comments != 0 {
				t.Errorf("the run wrote %d label updates and %d comments, want none", counter.updates, counter.comments)
			}
			if got := len(h.matching("estimate ")); got != 0 {
				t.Errorf("%d estimate runs were started, want none", got)
			}
			if tc.wantHook {
				if got := h.matching("hook gt-est"); len(got) != 1 {
					t.Errorf("hook calls = %q, want the bead hooked once\nfull log:\n  %s", got, strings.Join(h.log(), "\n  "))
				}
			} else {
				h.wantNo("hook ")
			}
		})
	}
}

// indexOfCall is the position of the first logged call that starts with prefix,
// or -1.
func indexOfCall(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// unreadableEstimateStore fails the read the estimate starts with.
type unreadableEstimateStore struct {
	specEstimateStore
}

func (unreadableEstimateStore) Show(string) (*beads.Issue, error) {
	return nil, errors.New("bd show failed")
}
