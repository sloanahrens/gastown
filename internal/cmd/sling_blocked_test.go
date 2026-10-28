package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// blockedEdge is one dependency edge as a show reports it.
func blockedEdge(id, status, kind string) beads.IssueDep {
	return beads.IssueDep{ID: id, Status: status, DependencyType: kind}
}

// blockedBeadInfo is an open bead whose blocking edges are still open: work
// bd's ready board leaves off, so no dispatch should spend a seat on it
// (gt-e3hl2).
func blockedBeadInfo(deps ...beads.IssueDep) beadInfo {
	return beadInfo{Status: "open", Dependencies: deps}
}

// TestSlingRefusesABeadWithAnUnresolvedBlocker is the refusal itself, on both
// of gt sling's dispatch paths: a bare rig target (the engine) and an agent
// target (the inline path). Each names the blockers and their statuses, and
// neither reaches target resolution, a spawn, a branch push or a hook.
func TestSlingRefusesABeadWithAnUnresolvedBlocker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		mover  string // the first side effect the refused path must not reach
	}{
		{name: "rig target", target: "gastown", mover: "spawn"},
		{name: "agent target", target: "gastown/crew/sloan", mover: "resolve target"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, blockedBeadInfo(
				blockedEdge("gt-block", "open", "blocks"),
				blockedEdge("gt-done", "closed", "blocks"),
				blockedEdge("gt-later", "deferred", "blocks"),
			))

			err := h.sling(slingBead, tc.target)
			if err == nil {
				t.Fatalf("a blocked bead was slung to %s", tc.target)
			}
			for _, want := range []string{dispatch.SlingRefusalMarker, "gt-block (open)", "gt-later (deferred)"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "gt-done") {
				t.Errorf("err = %v names a closed blocker", err)
			}
			h.wantNo(tc.mover)
			h.wantNo("hook")
			h.wantNo("blocker override")
		})
	}
}

// TestSlingForceStillRefusesABlockedBead: the automated redispatch paths pass
// --force for the guards they need, so --force must not open this one. Only
// --ignore-blockers does, and it names itself in the record it leaves.
func TestSlingForceStillRefusesABlockedBead(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"gastown", "gastown/crew/sloan"} {
		target := target
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")))
			h.run.opts.force = true

			wantSlingErr(t, h.sling(slingBead, target), "gt-block (open)")
			h.wantNo("hook")
			h.wantNo("blocker override")
		})
	}
}

// TestSlingIgnoreBlockersSlingsAndRecordsTheOverride: the flag dispatches the
// bead and writes the comment naming the actor and every blocker it went over.
func TestSlingIgnoreBlockersSlingsAndRecordsTheOverride(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		target  string
		applied string // the call that proves the dispatch went through
	}{
		{name: "rig target", target: "gastown", applied: "spawn"},
		{name: "agent target", target: "gastown/crew/sloan", applied: "hook " + slingBead + " gastown/crew/sloan"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, blockedBeadInfo(
				blockedEdge("gt-block", "open", "blocks"),
				blockedEdge("gt-later", "deferred", "blocks"),
			))
			h.run.opts.ignoreBlockers = true

			if err := h.sling(slingBead, tc.target); err != nil {
				t.Fatalf("--ignore-blockers did not sling the bead: %v", err)
			}
			h.wantCalls("blocker override",
				"blocker override "+slingBead+" by gastown/crew/sloan: gt-block (open), gt-later (deferred)")
			if len(h.matching(tc.applied)) == 0 {
				t.Fatalf("the dispatch did not reach %q\nfull log:\n  %s", tc.applied, strings.Join(h.log(), "\n  "))
			}
		})
	}
}

// TestSlingIgnoresBlockersOnlyWhenTheRecordIsWritten: the comment is the
// condition the override is granted on, so a comment that cannot be written
// refuses the sling instead of dispatching work over blockers unrecorded.
func TestSlingIgnoresBlockersOnlyWhenTheRecordIsWritten(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"gastown", "gastown/crew/sloan"} {
		target := target
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")))
			h.run.opts.ignoreBlockers = true
			h.overrideErr = errors.New("bd comments add: database is read only")

			wantSlingErr(t, h.sling(slingBead, target), "database is read only")
			h.wantNo("spawn")
			h.wantNo("hook")
		})
	}
}

// TestSlingIgnoreBlockersIsNotRecordedByADryRun: a dry run reports what it
// would do, and the comment is part of what it would do — not something it
// leaves behind.
func TestSlingIgnoreBlockersIsNotRecordedByADryRun(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")))
	h.run.opts.ignoreBlockers = true
	h.run.opts.dryRun = true

	if err := h.sling(slingBead, "gastown"); err != nil {
		t.Fatalf("a dry run failed: %v", err)
	}
	h.wantNo("blocker override")
	h.wantNo("spawn")
	h.wantNo("hook")
}

// TestSlingDryRunReportsABlockedRefusal: --dry-run reports the refusal it
// would take, and takes nothing: no record, no spawn and no hook.
func TestSlingDryRunReportsABlockedRefusal(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, blockedBeadInfo(
		blockedEdge("gt-block", "open", "blocks"),
		blockedEdge("gt-later", "deferred", "blocks"),
	))
	h.run.opts.dryRun = true

	if err := h.sling(slingBead, "gastown"); err != nil {
		t.Fatalf("a dry run refused with an error instead of reporting: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"would be refused", "gt-block (open)", "gt-later (deferred)", "--ignore-blockers"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output %q does not name %q", out, want)
		}
	}
	h.wantNo("spawn")
	h.wantNo("hook")
	h.wantNo("blocker override")
}

// TestSlingDryRunOfARigTargetReportsABlockedRefusal is the rig-target dry run:
// it never reaches the engine, so the refusal is the inline path's, reported
// the same way.
func TestSlingDryRunOfARigTargetReportsABlockedRefusal(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")))
	h.run.opts.dryRun = true

	if err := h.sling(slingBead, "gastown"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, "gt-block (open)") {
		t.Errorf("dry-run output %q does not name the blocker", out)
	}
	h.wantNo("spawn")
	h.wantNo("hook")
}

// batchPreview is a batch dry run over beads whose blocker answer the test
// names: it prints what it would do and dispatches nothing.
func batchPreview(t *testing.T, beads map[string]beadInfo, opts func(*batchSlingOptions)) (string, []string) {
	t.Helper()
	f := newBatchSlingFake(t)
	var out bytes.Buffer
	f.b.out = &out
	f.b.opts.dryRun = true
	if opts != nil {
		opts(&f.b.opts)
	}
	f.b.beadInfo = func(id string) (*beadInfo, error) {
		b, ok := beads[id]
		if !ok {
			return nil, errors.New("bd show: no such bead")
		}
		return &b, nil
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	// A bead the map does not hold is one the preview cannot read, which is
	// its own case.
	if err := f.b.run([]string{"gt-aaa", "gt-bbb"}, "gastown", filepath.Join(townRoot, ".beads")); err != nil {
		t.Fatalf("batch dry run: %v", err)
	}
	return out.String(), f.executed
}

// TestBatchSlingPreviewNamesABeadTheDispatchWouldRefuse: a batch dry run
// promises a polecat per bead, so it must say when the real dispatch would
// refuse one instead of spawning it (gt-e3hl2).
func TestBatchSlingPreviewNamesABeadTheDispatchWouldRefuse(t *testing.T) {
	t.Parallel()
	out, executed := batchPreview(t, map[string]beadInfo{
		"gt-aaa": blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")),
		"gt-bbb": blockedBeadInfo(blockedEdge("gt-done", "closed", "blocks")),
	}, nil)

	if !strings.Contains(out, "gt-aaa") || !strings.Contains(out, "would refuse this bead") {
		t.Errorf("preview %q does not report the refusal", out)
	}
	if !strings.Contains(out, "gt-block (open)") {
		t.Errorf("preview %q does not name the blocker", out)
	}
	if strings.Contains(out, "gt-done") {
		t.Errorf("preview %q names a closed blocker: the bead is dispatchable", out)
	}
	if len(executed) != 0 {
		t.Errorf("a dry run dispatched %v", executed)
	}
}

// TestBatchSlingPreviewIsSilentUnderIgnoreBlockers: the flag makes the real
// dispatch go ahead, so the preview must not promise a refusal the batch will
// not take.
func TestBatchSlingPreviewIsSilentUnderIgnoreBlockers(t *testing.T) {
	t.Parallel()
	out, _ := batchPreview(t, map[string]beadInfo{
		"gt-aaa": blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")),
		"gt-bbb": blockedBeadInfo(),
	}, func(o *batchSlingOptions) { o.ignoreBlockers = true })

	if strings.Contains(out, "would refuse this bead") {
		t.Errorf("preview %q reports a refusal --ignore-blockers overrides", out)
	}
}

// TestBatchSlingPreviewReportsABeadItCouldNotRead: an unread bead is not a
// clean one, so the preview says so rather than promising it would dispatch.
func TestBatchSlingPreviewReportsABeadItCouldNotRead(t *testing.T) {
	t.Parallel()
	out, _ := batchPreview(t, map[string]beadInfo{
		"gt-bbb": blockedBeadInfo(),
	}, nil)

	if !strings.Contains(out, "could not check gt-aaa for blockers") {
		t.Errorf("preview %q does not report the unread bead", out)
	}
}

// TestScheduleBeadCarriesTheBlockersOverride: a sling a town defers to the
// capacity scheduler is dispatched later by the engine, so the flag has to be
// written into the sling context — an override the queue drops is an override
// the dispatch never had (gt-e3hl2).
func TestScheduleBeadCarriesTheBlockersOverride(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")))

	if err := h.run.scheduleSlingBead(slingBead, "gastown", ScheduleOptions{IgnoreBlockers: true}); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if len(h.contexts) != 1 {
		t.Fatalf("wrote %d sling context(s), want 1", len(h.contexts))
	}
	fields := beads.ParseSlingContextFields(h.contexts[0].Description)
	if fields == nil {
		t.Fatalf("context description %q is not sling context fields", h.contexts[0].Description)
	}
	if !fields.IgnoreBlockers {
		t.Errorf("sling context lost --ignore-blockers: %+v", fields)
	}
}

// TestScheduleBeadRefusesABlockedBeadBeforeTheContext: the deferred path
// writes a sling context the queue later dispatches, so a blocked bead is
// refused before that write. A dry run reports the refusal and writes nothing.
func TestScheduleBeadRefusesABlockedBeadBeforeTheContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		dryRun bool
	}{
		{name: "sling", dryRun: false},
		{name: "dry run", dryRun: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, blockedBeadInfo(blockedEdge("gt-block", "open", "blocks")))

			err := h.run.scheduleSlingBead(slingBead, "gastown", ScheduleOptions{DryRun: tc.dryRun})
			if tc.dryRun {
				if err != nil {
					t.Fatalf("dry run refused with an error: %v", err)
				}
			} else {
				wantSlingErr(t, err, "gt-block (open)")
			}
			if len(h.contexts) != 0 {
				t.Fatalf("a refused bead wrote %d sling context(s), want none", len(h.contexts))
			}
		})
	}
}

// TestSlingDispatchesABeadWhoseBlockersClosed: the guard refuses unresolved
// edges, not the presence of a dependency list.
func TestSlingDispatchesABeadWhoseBlockersClosed(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, blockedBeadInfo(blockedEdge("gt-done", "closed", "blocks")))

	if err := h.sling(slingBead, "gastown"); err != nil {
		t.Fatalf("a bead with only closed blockers was refused: %v", err)
	}
	h.wantCalls("spawn", "spawn gastown")
	h.wantNo("blocker override")
}
