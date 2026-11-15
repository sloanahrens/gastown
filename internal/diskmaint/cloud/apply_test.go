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

// doneRefs is the ref of each step a result reports as done, in order.
func doneRefs(result diskmaint.Result) []string {
	out := make([]string, 0, len(result.Done))
	for _, a := range result.Done {
		out = append(out, a.Ref)
	}
	return out
}

// builderCalls is every build cache prune the target ran, in call order.
func builderCalls(f *fakeDaemon) []call {
	if f.docker == nil {
		return nil
	}
	return f.docker.builderCalls()
}

// applying is a plan whose policy asks for an apply.
func applying(actions ...diskmaint.Action) diskmaint.Plan {
	return diskmaint.Plan{Target: planTarget, Policy: diskmaint.Policy{Apply: true}, Actions: actions}
}

// TestApplyPrunesTheBuildCacheItKeeps drives an apply and pins the one build
// cache prune it runs: --keep-storage names the target's constant and --force
// keeps the prune from waiting for a prompt, over the same daemon the removals
// go to. No prune names --all, so a full prune never runs.
func TestApplyPrunesTheBuildCacheItKeeps(t *testing.T) {
	t.Parallel()
	id := imgID('1')
	f := &fakeDaemon{}
	target, _ := newApplyTarget(t, f)
	plan := applying(diskmaint.Action{Kind: actionRemove, Ref: id, Bytes: 10})

	if _, err := target.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	prunes := builderCalls(f)
	want := []string{"builder", "prune", "--keep-storage", "8GB", "--force"}
	if len(prunes) != 1 || !slices.Equal(prunes[0].args, want) {
		t.Fatalf("build cache prunes = %v, want one %v: the size to keep is the target's constant", prunes, want)
	}
	if wantEnv := []string{"DOCKER_HOST=" + dockerHost}; !slices.Equal(prunes[0].env, wantEnv) {
		t.Errorf("the prune ran with env %v, want %v: the prune goes to the cloud daemon", prunes[0].env, wantEnv)
	}
	for _, c := range prunes {
		if slices.Contains(c.args, "--all") || slices.Contains(c.args, "-a") {
			t.Errorf("build cache prune %v names --all, want --keep-storage only", c.args)
		}
	}
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
	if got, want := doneRefs(result), append(removed(plan), "builder"); !slices.Equal(got, want) {
		t.Errorf("Done = %v, want the plan's ids then the build cache prune", got)
	}
	if want := int64(30_000_000 + 20_000_000 + 10_000_000); result.BytesFreed != want {
		t.Errorf("BytesFreed = %d, want %d (the three removed images)", result.BytesFreed, want)
	}

	entry := lastEntry(t, l)
	if entry.Mode != modeApply || entry.Planned != 3 || entry.Done != 4 || entry.BytesFreed != result.BytesFreed {
		t.Errorf("ledger entry = %+v, want an apply of 3 planned and 4 done, the prune among them", entry)
	}
	if len(entry.Errors) != 0 {
		t.Errorf("ledger errors = %q, want none", entry.Errors)
	}
}

// TestApplyCountsTheBuildCacheInItsLedger drives a prune that reports what it
// reclaimed, in each of the two summaries docker prints, and asserts the run's
// one ledger line adds that space to what the images freed.
func TestApplyCountsTheBuildCacheInItsLedger(t *testing.T) {
	t.Parallel()
	const gb = 1_000_000_000
	cases := []struct {
		name string
		out  string
		want int64
	}{
		{
			name: "the classic builder's total",
			out:  "Deleted build cache objects:\nabc\ndef\nTotal reclaimed space: 1.5GB\n",
			want: 1_500_000_000,
		},
		{
			name: "the buildx total",
			out:  "ID\tRECLAIMABLE\tSIZE\tLAST ACCESSED\nabc\ttrue\t2GB\t3 days ago\nTotal:\t2GB\n",
			want: 2_000_000_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := imgID('1')
			f := &fakeDaemon{builderOut: tc.out}
			target, l := newApplyTarget(t, f)
			plan := applying(diskmaint.Action{Kind: actionRemove, Ref: id, Bytes: gb})

			result, err := target.Apply(context.Background(), plan)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if want := gb + tc.want; result.BytesFreed != want {
				t.Errorf("BytesFreed = %d, want %d (the image and the build cache)", result.BytesFreed, want)
			}
			if len(result.Done) != 2 || result.Done[1].Kind != actionPruneBuildCache || result.Done[1].Bytes != tc.want {
				t.Errorf("Done = %+v, want the image then the prune with %d bytes", result.Done, tc.want)
			}
			entry := lastEntry(t, l)
			if entry.Planned != 1 || entry.Done != 2 || entry.BytesFreed != gb+tc.want {
				t.Errorf("ledger entry = %+v, want 1 planned, 2 done, %d bytes", entry, gb+tc.want)
			}
			if len(entry.Errors) != 0 {
				t.Errorf("ledger errors = %q, want none", entry.Errors)
			}
		})
	}
}

// TestApplyRecordsAFailedBuildCachePrune holds that a prune that failed does not
// stop the run: the image the plan removed stays done, the prune is a Failure,
// and the ledger line says why without quoting what docker printed.
func TestApplyRecordsAFailedBuildCachePrune(t *testing.T) {
	t.Parallel()
	id := imgID('1')
	f := &fakeDaemon{builderErr: errors.New("SECRET stderr")}
	target, l := newApplyTarget(t, f)
	plan := applying(diskmaint.Action{Kind: actionRemove, Ref: id, Bytes: 100})

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v, want the removal to carry on past a failed prune", err)
	}
	if f.held(id) {
		t.Errorf("the daemon still holds %s, want the removal done", id)
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Kind != actionPruneBuildCache || result.Failed[0].Err != pruneFailed {
		t.Errorf("Failed = %+v, want the prune alone with the fixed reason", result.Failed)
	}
	entry := lastEntry(t, l)
	if !slices.Equal(entry.Errors, []string{pruneFailed}) {
		t.Errorf("ledger errors = %q, want the prune's fixed reason alone", entry.Errors)
	}
}

// TestApplyRecordsABuildCachePruneWithNoTotal holds that a prune which ran but
// printed no reclaimed total is recorded as done with the bytes unknown, never
// as a prune that freed nothing: the reason reaches the ledger line.
func TestApplyRecordsABuildCachePruneWithNoTotal(t *testing.T) {
	t.Parallel()
	id := imgID('1')
	f := &fakeDaemon{builderOut: "the cache is rebuilt\n"}
	target, l := newApplyTarget(t, f)
	plan := applying(diskmaint.Action{Kind: actionRemove, Ref: id, Bytes: 100})

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.BytesFreed != 100 {
		t.Errorf("BytesFreed = %d, want 100: the unread total is not a freed byte", result.BytesFreed)
	}
	if got, want := doneRefs(result), []string{id, "builder"}; !slices.Equal(got, want) {
		t.Errorf("Done = %v, want %v: the prune ran", got, want)
	}
	entry := lastEntry(t, l)
	if !slices.Equal(entry.Errors, []string{pruneUnread}) {
		t.Errorf("ledger errors = %q, want %q", entry.Errors, pruneUnread)
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
	if len(result.Failed) != 0 {
		t.Errorf("Failed = %+v, want none", result.Failed)
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
// policy does not ask for an apply removes nothing, prunes nothing, and runs no
// docker command.
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
		t.Errorf("a dry run ran %d docker commands, want none, the build cache prune among them", n)
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
	if got, want := doneRefs(result), []string{a1, a3, "builder"}; !slices.Equal(got, want) {
		t.Errorf("Done = %v, want the first and third images then the prune", got)
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Ref != a2 || result.Failed[0].Err != removeFailed {
		t.Errorf("Failed = %+v, want the second image with the fixed reason", result.Failed)
	}
	if result.BytesFreed != 107 {
		t.Errorf("BytesFreed = %d, want 107 (the two removed images)", result.BytesFreed)
	}
	entry := lastEntry(t, l)
	if entry.Mode != modeApply || entry.Planned != 3 || entry.Done != 3 || entry.BytesFreed != 107 {
		t.Errorf("ledger entry = %+v, want apply, 3 planned, 3 done, 107 bytes", entry)
	}
	if len(entry.Errors) != 1 || strings.Contains(entry.Errors[0], "SECRET") {
		t.Errorf("ledger errors = %q, want one entry that does not echo command output", entry.Errors)
	}
}

// TestApplyRefusesBadActionsBeforeAnyDockerCall proves an action's ref never
// reaches docker unvalidated: the bad actions send no command of their own, and
// the only command the apply runs is the build cache prune, which names none of
// them.
func TestApplyRefusesBadActionsBeforeAnyDockerCall(t *testing.T) {
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
	if !errors.Is(err, errNothingRemoved) {
		t.Fatalf("Apply error = %v, want errNothingRemoved: no action was usable", err)
	}
	if got, want := doneRefs(result), []string{"builder"}; !slices.Equal(got, want) {
		t.Errorf("Done = %v, want %v: a bad action leaves nothing done", got, want)
	}
	for _, c := range f.dockerCalls() {
		if len(c.args) < 2 || c.args[0] != "builder" || c.args[1] != "prune" {
			t.Errorf("bad actions led to the command %v, want only the build cache prune", c.args)
		}
	}
	if len(result.Failed) != 3 {
		t.Errorf("Failed = %d, want 3", len(result.Failed))
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

func TestApplyRemovesNothingForARefusedPlan(t *testing.T) {
	t.Parallel()
	const refused = "the image list is unreadable"
	blocked := applying(diskmaint.Action{Kind: actionRemove, Ref: imgID('1')})
	blocked.Refused = refused
	cases := []struct {
		name string
		plan diskmaint.Plan
	}{
		{
			name: "refused",
			plan: diskmaint.Plan{Target: planTarget, Policy: diskmaint.Policy{Apply: true}, Refused: refused},
		},
		{name: "refused with actions", plan: blocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeDaemon{}
			target, l := newApplyTarget(t, f)

			result, err := target.Apply(context.Background(), tc.plan)
			if !errors.Is(err, errPlanRefused) {
				t.Fatalf("Apply err = %v, want %v", err, errPlanRefused)
			}
			if n := len(f.dockerCalls()); n != 0 || len(result.Done) != 0 {
				t.Errorf("made %d calls and %d removals, want none: a refusal changes nothing", n, len(result.Done))
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

// TestApplyOfAnEmptyPlanPrunesTheBuildCache holds that a plan with no image to
// remove still trims the cache: the build cache is what a quiet daemon fills,
// and the plan's empty action list says nothing about it.
func TestApplyOfAnEmptyPlanPrunesTheBuildCache(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{}
	target, l := newApplyTarget(t, f)

	result, err := target.Apply(context.Background(), applying())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := doneRefs(result), []string{"builder"}; !slices.Equal(got, want) {
		t.Errorf("Done = %v, want %v", got, want)
	}
	if len(result.Failed) != 0 {
		t.Errorf("Failed = %+v, want none", result.Failed)
	}
	entry := lastEntry(t, l)
	if entry.Planned != 0 || entry.Done != 1 {
		t.Errorf("ledger entry = %+v, want 0 planned and the prune done", entry)
	}
}

// TestApplyErrorsWhenThePruneFailedWithoutImages holds the fail-closed case: a run
// with no image to remove whose one step failed removed nothing, so it must not
// read as a success.
func TestApplyErrorsWhenThePruneFailedWithoutImages(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{builderErr: errors.New("denied")}
	target, l := newApplyTarget(t, f)

	result, err := target.Apply(context.Background(), applying())
	if !errors.Is(err, errNothingRemoved) {
		t.Fatalf("Apply error = %v, want errNothingRemoved", err)
	}
	if len(result.Done) != 0 || len(result.Failed) != 1 {
		t.Errorf("Done=%+v Failed=%+v, want nothing done and the prune failed", result.Done, result.Failed)
	}
	entry := lastEntry(t, l)
	if entry.Done != 0 || !slices.Equal(entry.Errors, []string{pruneFailed}) {
		t.Errorf("ledger entry = %+v, want nothing done and the prune's reason", entry)
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
	if len(result.Done) == 0 || result.Done[0].Ref != imgID('1') {
		t.Errorf("Done = %+v, want the removal it could not record", result.Done)
	}
}

// TestApplyReturnsAnErrorWhenEveryRemovalFailed holds the third review's major
// (gt-367d34): a run in which no image was removed is an error as well as a ledger
// line, while one failure among successes (the test above) is not. The build
// cache prune ran, and it does not turn the run into a success.
func TestApplyReturnsAnErrorWhenEveryRemovalFailed(t *testing.T) {
	t.Parallel()
	a1, a2 := imgID('1'), imgID('2')
	f := &fakeDaemon{rmErr: map[string]error{a1: errors.New("denied"), a2: errors.New("denied")}}
	target, l := newApplyTarget(t, f)
	plan := applying(
		diskmaint.Action{Kind: actionRemove, Ref: a1, Bytes: 100},
		diskmaint.Action{Kind: actionRemove, Ref: a2, Bytes: 250},
	)

	result, err := target.Apply(context.Background(), plan)
	if !errors.Is(err, errNothingRemoved) {
		t.Fatalf("Apply error = %v, want errNothingRemoved", err)
	}
	if got, want := doneRefs(result), []string{"builder"}; !slices.Equal(got, want) {
		t.Errorf("Done = %v, want %v alone: no image was removed", got, want)
	}
	if len(result.Failed) != 2 || result.BytesFreed != 0 {
		t.Errorf("result = %+v, want both image failures and no bytes freed", result)
	}
	entry := lastEntry(t, l)
	if entry.Mode != modeApply || entry.Planned != 2 || entry.Done != 1 || len(entry.Errors) != 2 {
		t.Errorf("ledger entry = %+v, want an apply line, 2 planned, the prune done and both errors", entry)
	}
}
