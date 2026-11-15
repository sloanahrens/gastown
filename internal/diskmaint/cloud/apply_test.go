package cloud

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// newApplyTarget builds a target whose daemon answers from f, and returns the
// ledger every apply of it writes to.
func newApplyTarget(t *testing.T, f *fakeDaemon) (*Target, *diskmaint.Ledger) {
	t.Helper()
	l := ledger(t)
	target, err := NewTarget(Config{DockerHost: dockerHost, Run: f.run, Ledger: l})
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	return target, l
}

// lastEntry is the one ledger line a run appended.
func lastEntry(t *testing.T, l *diskmaint.Ledger) diskmaint.LedgerEntry {
	t.Helper()
	entries, err := l.Tail(10)
	if err != nil {
		t.Fatalf("ledger tail: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger holds %d entries, want 1", len(entries))
	}
	return entries[0]
}

// removedIDs is every image id the fake daemon's removals took, in call order.
func removedIDs(f *fakeDaemon) []string {
	if f.docker == nil {
		return nil
	}
	return f.docker.removed
}

// held reports whether the fake daemon still holds an image id.
func (f *fakeDaemon) held(id string) bool {
	return f.docker == nil || !f.docker.gone[id]
}

// applying is a plan whose policy asks for an apply.
func applying(actions ...diskmaint.Action) diskmaint.Plan {
	return diskmaint.Plan{Target: planTarget, Policy: diskmaint.Policy{Apply: true}, Actions: actions}
}

// TestApplyRemovesTheImagesThePlanNames runs the target's own plan and then its
// apply, so the ids the daemon loses are the ids the plan decided on.
func TestApplyRemovesTheImagesThePlanNames(t *testing.T) {
	t.Parallel()
	images, digests := eightTags(t)
	f := &fakeDaemon{
		images:  images,
		digests: digests,
		labels:  "node:docker://code.forgejo.org/oci/node:lts\n",
		deploy:  cleanDeploy,
	}
	plan, err := newPlanTarget(t, f, labelsPath, deployPath).Plan(context.Background(), diskmaint.Policy{Apply: true})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions) != 3 {
		t.Fatalf("plan actions = %v, want the three tags outside the window", plan.Actions)
	}
	target, l := newApplyTarget(t, f)

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got, want := removedIDs(f), removed(plan); !slices.Equal(got, want) {
		t.Errorf("the daemon removed %v, want the plan's ids in order %v", got, want)
	}
	for _, a := range plan.Actions {
		if f.held(a.Ref) {
			t.Errorf("the daemon still holds %s, want it removed", a.Ref)
		}
	}
	if len(result.Done) != 3 || result.Done[0].Ref != plan.Actions[0].Ref {
		t.Errorf("Done = %+v, want the plan's actions in order", result.Done)
	}
	if want := int64(30_000_000 + 20_000_000 + 10_000_000); result.BytesFreed != want {
		t.Errorf("BytesFreed = %d, want %d (the three removed images)", result.BytesFreed, want)
	}

	entry := lastEntry(t, l)
	if entry.Mode != modeApply || entry.Planned != 3 || entry.Done != 3 || entry.BytesFreed != result.BytesFreed {
		t.Errorf("ledger entry = %+v, want an apply of 3 planned, 3 done", entry)
	}
	if len(entry.Errors) != 0 {
		t.Errorf("ledger errors = %q, want none", entry.Errors)
	}
}

// TestApplyRemovesAnImageSeveralTagsName drives an id that two candidate tags
// name: docker refuses that removal unless it is forced, and the plan decided
// every one of those tags leaves with the id.
func TestApplyRemovesAnImageSeveralTagsName(t *testing.T) {
	t.Parallel()
	id := imgID('1')
	f := &fakeDaemon{tags: map[string][]string{id: {fractalsRepo + ":t1", fractalsRepo + ":t2"}}}
	target, _ := newApplyTarget(t, f)
	plan := applying(diskmaint.Action{Kind: actionRemove, Ref: id, Bytes: 10})

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(result.Done) != 1 || len(result.Failed) != 0 {
		t.Errorf("Done=%+v Failed=%+v, want the image removed", result.Done, result.Failed)
	}
	if f.held(id) {
		t.Errorf("the daemon still holds %s, want it removed", id)
	}
}

// TestApplyKeepsAnUntaggedParentImage drives --no-prune: a removal without it
// would take the untagged parent, which this plan keeps, since every row it does
// not name as a removal is one it keeps.
func TestApplyKeepsAnUntaggedParentImage(t *testing.T) {
	t.Parallel()
	child, parent := imgID('1'), imgID('2')
	f := &fakeDaemon{
		tags:     map[string][]string{child: {fractalsRepo + ":t1"}},
		parentOf: map[string]string{child: parent},
	}
	target, _ := newApplyTarget(t, f)
	plan := applying(diskmaint.Action{Kind: actionRemove, Ref: child, Bytes: 10})

	if _, err := target.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !f.held(parent) {
		t.Errorf("the daemon dropped the parent %s, want the untagged parent kept", parent)
	}
}

// TestApplyIsADryRunWithoutThePolicyFlag is the dry-run default: a plan whose
// policy does not ask for an apply removes nothing and runs no docker command.
func TestApplyIsADryRunWithoutThePolicyFlag(t *testing.T) {
	t.Parallel()
	images, digests := eightTags(t)
	f := &fakeDaemon{
		images:  images,
		digests: digests,
		labels:  "node:docker://code.forgejo.org/oci/node:lts\n",
		deploy:  cleanDeploy,
	}
	plan := planOf(t, f, diskmaint.Policy{})
	if len(plan.Actions) == 0 {
		t.Fatal("plan names no removal, so this test would prove nothing")
	}
	// The plan itself reads the daemon; only Apply's own commands are counted.
	before := len(f.dockerCalls())
	target, l := newApplyTarget(t, f)

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := len(f.dockerCalls()) - before; n != 0 {
		t.Errorf("a dry run ran %d docker commands, want none", n)
	}
	if len(result.Done) != 0 || result.BytesFreed != 0 {
		t.Errorf("Result = %+v, want nothing done and nothing freed", result)
	}
	entry := lastEntry(t, l)
	if entry.Mode != modeDryRun || entry.Planned != len(plan.Actions) || entry.Done != 0 {
		t.Errorf("ledger entry = %+v, want a dry-run of %d planned and none done", entry, len(plan.Actions))
	}
}

func TestApplyRemovesEachImageAndKeepsGoingAfterAFailure(t *testing.T) {
	t.Parallel()
	a1, a2, a3 := imgID('1'), imgID('2'), imgID('3')
	f := &fakeDaemon{rmErr: map[string]error{a2: errors.New("SECRET stderr")}}
	target, l := newApplyTarget(t, f)
	plan := applying(
		diskmaint.Action{Kind: actionRemove, Ref: a1, Bytes: 100},
		diskmaint.Action{Kind: actionRemove, Ref: a2, Bytes: 250},
		diskmaint.Action{Kind: actionRemove, Ref: a3, Bytes: 7},
	)

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := removedIDs(f), []string{a1, a3}; !slices.Equal(got, want) {
		t.Errorf("the daemon removed %v, want %v: one failure does not stop the rest", got, want)
	}
	if !f.held(a2) {
		t.Errorf("the daemon dropped %s, whose removal failed", a2)
	}
	if len(result.Done) != 2 || result.Done[0].Ref != a1 || result.Done[1].Ref != a3 {
		t.Errorf("Done = %+v, want the first and third images", result.Done)
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Ref != a2 || result.Failed[0].Err != removeFailed {
		t.Errorf("Failed = %+v, want the second image with the fixed reason", result.Failed)
	}
	if result.BytesFreed != 107 {
		t.Errorf("BytesFreed = %d, want 107 (the two removed images)", result.BytesFreed)
	}
	entry := lastEntry(t, l)
	if entry.Mode != modeApply || entry.Planned != 3 || entry.Done != 2 || entry.BytesFreed != 107 {
		t.Errorf("ledger entry = %+v, want apply, 3 planned, 2 done, 107 bytes", entry)
	}
	if len(entry.Errors) != 1 || strings.Contains(entry.Errors[0], "SECRET") {
		t.Errorf("ledger errors = %q, want one entry that does not echo command output", entry.Errors)
	}
}

func TestApplyRefusesBadActionsBeforeAnyCommand(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{}
	target, l := newApplyTarget(t, f)
	plan := applying(
		diskmaint.Action{Kind: actionRemove, Ref: "sha256:short"},
		diskmaint.Action{Kind: "delete-manifest", Ref: imgID('1')},
		// A tag is not an id: a removal names the image the plan decided about.
		diskmaint.Action{Kind: actionRemove, Ref: fractalsRepo + ":t1"},
	)

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := len(f.dockerCalls()); n != 0 {
		t.Errorf("bad actions led to %d docker calls, want none", n)
	}
	if len(result.Done) != 0 || len(result.Failed) != 3 {
		t.Errorf("Done=%d Failed=%d, want 0 and 3", len(result.Done), len(result.Failed))
	}
	entry := lastEntry(t, l)
	if len(entry.Errors) != 3 {
		t.Errorf("ledger errors = %q, want one per bad action", entry.Errors)
	}
	for _, e := range entry.Errors {
		if strings.Contains(e, "t1") {
			t.Errorf("ledger error quotes the unvalidated ref: %s", e)
		}
	}
}

func TestApplyRemovesNothingForARefusedOrEmptyPlan(t *testing.T) {
	t.Parallel()
	const refused = "the image list is unreadable"
	blocked := applying(diskmaint.Action{Kind: actionRemove, Ref: imgID('1')})
	blocked.Refused = refused
	cases := []struct {
		name    string
		plan    diskmaint.Plan
		wantErr error
	}{
		{
			name:    "refused",
			plan:    diskmaint.Plan{Target: planTarget, Policy: diskmaint.Policy{Apply: true}, Refused: refused},
			wantErr: errPlanRefused,
		},
		{name: "refused with actions", plan: blocked, wantErr: errPlanRefused},
		{name: "empty", plan: applying()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeDaemon{}
			target, l := newApplyTarget(t, f)

			result, err := target.Apply(context.Background(), tc.plan)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Apply err = %v, want %v", err, tc.wantErr)
			}
			if n := len(f.dockerCalls()); n != 0 || len(result.Done) != 0 {
				t.Errorf("made %d calls and %d removals, want none", n, len(result.Done))
			}
			entry := lastEntry(t, l)
			if entry.Refused != tc.plan.Refused {
				t.Errorf("ledger Refused = %q, want %q", entry.Refused, tc.plan.Refused)
			}
			if entry.Mode != modeApply {
				t.Errorf("ledger Mode = %q, want %q: the policy asked for an apply", entry.Mode, modeApply)
			}
		})
	}
}

func TestApplyRequiresALedger(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{}
	// A zero Target is the one config NewTarget refuses, so the nil check is
	// only reachable by building one directly.
	target := &Target{}
	plan := applying(diskmaint.Action{Kind: actionRemove, Ref: imgID('1')})

	if _, err := target.Apply(context.Background(), plan); !errors.Is(err, errNoLedger) {
		t.Fatalf("Apply err = %v, want %v", err, errNoLedger)
	}
	if n := len(f.dockerCalls()); n != 0 {
		t.Errorf("Apply without a ledger made %d docker calls, want none", n)
	}
}

func TestApplyReportsALedgerThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{}
	// A ledger under a directory that does not exist cannot be opened.
	l := diskmaint.NewLedger(filepath.Join(t.TempDir(), "missing", "ledger.jsonl"), 0)
	target, err := NewTarget(Config{DockerHost: dockerHost, Run: f.run, Ledger: l})
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	plan := applying(diskmaint.Action{Kind: actionRemove, Ref: imgID('1')})

	result, err := target.Apply(context.Background(), plan)
	if err == nil {
		t.Fatal("Apply reported no error for a ledger it could not write")
	}
	if !strings.Contains(err.Error(), "record the apply") {
		t.Errorf("err = %v, want it to name the record", err)
	}
	if len(result.Done) != 1 {
		t.Errorf("Done = %+v, want the removal it could not record", result.Done)
	}
}
