package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

const repo = "localhost:5001/gastown-polecat"

// imgID is a crictl image id: a sha256 digest, the shape registry.ValidDigest
// accepts.
func imgID(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// digestRef is the repo digest an image is pulled by; imageprune keeps an image
// a container names by digest.
func digestRef(c byte) string { return repo + "@sha256:" + strings.Repeat(string(c), 64) }

// nodeImage is one image a fake node reports.
type nodeImage struct {
	id      byte
	tag     string
	digests []string
	age     time.Duration
}

// call is one command the target ran.
type call struct {
	binary string
	args   []string
}

// fakeDocker answers the docker and crictl commands the target runs from canned
// data and records every one, so a test asserts what was asked for.
type fakeDocker struct {
	calls      []call
	list       string
	listErr    error
	images     map[string][]nodeImage
	imagesErr  map[string]bool
	containers map[string]string
	rmiErr     map[string]bool
}

func newFake(list string, nodes map[string][]nodeImage) *fakeDocker {
	f := &fakeDocker{
		list:       list,
		images:     map[string][]nodeImage{},
		imagesErr:  map[string]bool{},
		containers: map[string]string{},
		rmiErr:     map[string]bool{},
	}
	for node, imgs := range nodes {
		f.images[node] = imgs
	}
	return f
}

func (f *fakeDocker) run(_ context.Context, _ []string, binary string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{binary: binary, args: slices.Clone(args)})
	if len(args) < 4 || args[0] != "--context" {
		return nil, errors.New("unexpected docker arguments")
	}
	rest := args[2:]
	switch rest[0] {
	case "ps":
		if f.listErr != nil {
			return nil, f.listErr
		}
		return []byte(f.list), nil
	case "exec":
		if len(rest) < 5 || rest[2] != "crictl" {
			return nil, errors.New("unexpected docker arguments")
		}
		return f.crictl(rest[1], rest[3], rest[4:])
	}
	return nil, errors.New("unexpected docker arguments")
}

func (f *fakeDocker) crictl(node, verb string, rest []string) ([]byte, error) {
	switch verb {
	case "images":
		if f.imagesErr[node] {
			return nil, errors.New("crictl images failed")
		}
		out := make([]map[string]any, 0, len(f.images[node]))
		for _, im := range f.images[node] {
			out = append(out, map[string]any{
				"id":          imgID(im.id),
				"repoTags":    []string{repo + ":" + im.tag},
				"repoDigests": im.digests,
			})
		}
		return json.Marshal(map[string]any{"images": out})
	case "inspecti":
		if len(rest) == 0 {
			return nil, errors.New("crictl inspecti without an id")
		}
		return json.Marshal(map[string]any{
			"info": map[string]any{
				"imageSpec": map[string]string{
					"created": testNow.Add(-f.ageOf(rest[0])).Format(time.RFC3339Nano),
				},
			},
		})
	case "ps":
		body, ok := f.containers[node]
		if !ok {
			body = `{"containers":[]}`
		}
		return []byte(body), nil
	case "rmi":
		if len(rest) == 0 {
			return nil, errors.New("crictl rmi without an id")
		}
		if f.rmiErr[rest[0]] {
			return nil, errors.New("crictl rmi failed")
		}
		return nil, nil
	}
	return nil, errors.New("unexpected crictl verb")
}

// ageOf returns the age of the image with this id on any node; an id no node
// holds reads as created now.
func (f *fakeDocker) ageOf(id string) time.Duration {
	for _, imgs := range f.images {
		for _, im := range imgs {
			if imgID(im.id) == id {
				return im.age
			}
		}
	}
	return 0
}

// crictlArgs returns the arguments after crictl for every exec on node.
func (f *fakeDocker) crictlArgs(node string) [][]string {
	var out [][]string
	for _, c := range f.calls {
		a := c.args
		if len(a) >= 6 && a[0] == "--context" && a[2] == "exec" && a[3] == node && a[4] == "crictl" {
			out = append(out, a[5:])
		}
	}
	return out
}

// rmiIDs returns every image id crictl rmi named on node, in call order.
func (f *fakeDocker) rmiIDs(node string) []string {
	var out []string
	for _, a := range f.crictlArgs(node) {
		if a[0] == "rmi" && len(a) > 1 {
			out = append(out, a[1])
		}
	}
	return out
}

// everyRmi returns "<node>/<id>" for every rmi the target issued.
func (f *fakeDocker) everyRmi() []string {
	var out []string
	for _, c := range f.calls {
		a := c.args
		if len(a) >= 7 && a[0] == "--context" && a[2] == "exec" && a[4] == "crictl" && a[5] == "rmi" {
			out = append(out, a[3]+"/"+a[6])
		}
	}
	return out
}

// nodesContacted returns the node of every docker exec, in call order.
func (f *fakeDocker) nodesContacted() []string {
	var out []string
	for _, c := range f.calls {
		a := c.args
		if len(a) >= 5 && a[0] == "--context" && a[2] == "exec" {
			out = append(out, a[3])
		}
	}
	return out
}

// containersJSON is a `crictl ps -a -o json` answer with these images in use.
func containersJSON(refs ...string) string {
	type container struct {
		Image struct {
			Image string `json:"image"`
		} `json:"image"`
		ImageRef string `json:"imageRef"`
	}
	list := make([]container, 0, len(refs))
	for _, r := range refs {
		var c container
		c.Image.Image = r
		c.ImageRef = r
		list = append(list, c)
	}
	body, err := json.Marshal(map[string]any{"containers": list})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// fakeLedger records the entries Apply writes; err makes every append fail.
type fakeLedger struct {
	entries []diskmaint.LedgerEntry
	err     error
}

func (l *fakeLedger) Append(e diskmaint.LedgerEntry) error {
	if l.err != nil {
		return l.err
	}
	l.entries = append(l.entries, e)
	return nil
}

// planRefs returns the Ref of every action, in plan order.
func planRefs(p diskmaint.Plan) []string {
	var refs []string
	for _, a := range p.Actions {
		refs = append(refs, a.Ref)
	}
	return refs
}

// image is a polecat image on a node, aged age before testNow.
func image(id byte, tag string, age time.Duration, digests ...string) nodeImage {
	return nodeImage{id: id, tag: tag, digests: digests, age: age}
}

func TestName(t *testing.T) {
	if got := NewTarget(Config{}).Name(); got != "nodes" {
		t.Fatalf("Name() = %q, want nodes", got)
	}
}

func TestNewTargetDefaultsContextAndPrefix(t *testing.T) {
	f := newFake("", nil)
	if _, err := NewTarget(Config{Run: f.run}).Measure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("commands = %d, want 1", len(f.calls))
	}
	c := f.calls[0]
	if c.binary != "docker" {
		t.Fatalf("binary = %q, want docker", c.binary)
	}
	if !slices.Equal(c.args, []string{"--context", "colima-gt-sbx", "ps", "--filter", "name=gt-sbx-", "--format", "{{.Names}}"}) {
		t.Fatalf("args = %v", c.args)
	}
}

func TestMeasureReportsNodeCountWithoutContactingNodes(t *testing.T) {
	f := newFake("gt-sbx-a\ngt-sbx-b\n", nil)
	m, err := NewTarget(Config{Run: f.run}).Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Used != 0 {
		t.Fatalf("Used = %d, want 0", m.Used)
	}
	if !strings.Contains(m.Detail, "2 kind nodes") {
		t.Fatalf("Detail = %q, want the node count", m.Detail)
	}
	if got := f.nodesContacted(); len(got) != 0 {
		t.Fatalf("measure contacted nodes %v", got)
	}
}

func TestMeasureFailsOnListingError(t *testing.T) {
	f := newFake("", nil)
	f.listErr = errors.New("cannot connect to the docker daemon")
	if _, err := NewTarget(Config{Run: f.run}).Measure(context.Background()); err == nil {
		t.Fatal("Measure succeeded on a failed listing")
	}
}

func TestPlanTurnsPlannedImagesIntoActions(t *testing.T) {
	f := newFake("gt-sbx-a\n", map[string][]nodeImage{
		"gt-sbx-a": {
			image('a', "a", time.Hour),
			image('b', "b", 2*time.Hour),
			image('c', "c", 3*time.Hour),
			image('d', "d", 4*time.Hour),
			image('e', "dev", 5*time.Hour),
			image('f', "f", 6*time.Hour, digestRef('f')),
		},
	})
	f.containers["gt-sbx-a"] = containersJSON(repo+":f", digestRef('f'))

	plan, err := NewTarget(Config{Run: f.run}).Plan(context.Background(), diskmaint.Policy{Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Target != planTarget {
		t.Fatalf("Target = %q, want %q", plan.Target, planTarget)
	}
	if plan.Refused != "" {
		t.Fatalf("Refused = %q, want a plan", plan.Refused)
	}
	want := []string{"gt-sbx-a/" + imgID('c'), "gt-sbx-a/" + imgID('d')}
	if got := planRefs(plan); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	for _, a := range plan.Actions {
		if a.Kind != actionRemove {
			t.Errorf("kind = %q, want %q", a.Kind, actionRemove)
		}
		if a.Why != keepRule(2) {
			t.Errorf("why = %q, want the keep rule", a.Why)
		}
	}
	if len(plan.Kept) != 0 {
		t.Fatalf("Kept = %v; a kept image is not listed", plan.Kept)
	}
	if got := f.everyRmi(); len(got) != 0 {
		t.Fatalf("plan issued rmi: %v", got)
	}
}

func TestPlanLeavesAnUnreadableNodeAlone(t *testing.T) {
	f := newFake("gt-sbx-a\ngt-sbx-b\n", map[string][]nodeImage{
		"gt-sbx-a": {image('a', "a", time.Hour), image('c', "c", 3*time.Hour)},
		"gt-sbx-b": {image('b', "b", time.Hour), image('d', "d", 3*time.Hour)},
	})
	f.imagesErr["gt-sbx-b"] = true

	plan, err := NewTarget(Config{Run: f.run}).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gt-sbx-a/" + imgID('c')}
	if got := planRefs(plan); !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want only the readable node's %v", got, want)
	}
	if !slices.Equal(plan.Kept, []diskmaint.Kept{{Ref: "gt-sbx-b", Rule: ruleUnreadable}}) {
		t.Fatalf("Kept = %v, want the unreadable node", plan.Kept)
	}
	if got := f.rmiIDs("gt-sbx-b"); len(got) != 0 {
		t.Fatalf("unreadable node was pruned: %v", got)
	}
}

func TestPlanRefusesAnEmptyNodeList(t *testing.T) {
	f := newFake("", nil)
	plan, err := NewTarget(Config{Run: f.run}).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Refused == "" {
		t.Fatal("Refused is empty for an empty node list")
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("actions = %v, want none", planRefs(plan))
	}
}

func TestPlanRefusesAFailedListing(t *testing.T) {
	f := newFake("", nil)
	f.listErr = errors.New("cannot connect to the docker daemon")
	plan, err := NewTarget(Config{Run: f.run}).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Refused == "" {
		t.Fatal("Refused is empty for a failed listing")
	}
	if got := f.nodesContacted(); len(got) != 0 {
		t.Fatalf("a failed listing still contacted %v", got)
	}
}

func TestPlanRefusesWithoutARunner(t *testing.T) {
	plan, err := NewTarget(Config{}).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Refused != errNoRunner.Error() {
		t.Fatalf("Refused = %q, want %q", plan.Refused, errNoRunner.Error())
	}
}

// TestPlanKeepsOnlyNamesThatStartWithThePrefix covers the docker name filter:
// it is a substring match, so a container whose name merely contains gt-sbx-
// comes back too, and such a name must never be planned or contacted.
func TestPlanKeepsOnlyNamesThatStartWithThePrefix(t *testing.T) {
	f := newFake("gt-sbx-a\nother-gt-sbx-b\ngt-sbx-a;evil\n", map[string][]nodeImage{
		"gt-sbx-a":       {image('a', "a", time.Hour), image('c', "c", 3*time.Hour)},
		"other-gt-sbx-b": {image('b', "b", time.Hour), image('d', "d", 3*time.Hour)},
	})
	plan, err := NewTarget(Config{Run: f.run}).Plan(context.Background(), diskmaint.Policy{Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Refused != "" {
		t.Fatalf("Refused = %q, want a plan", plan.Refused)
	}
	if got, want := planRefs(plan), []string{"gt-sbx-a/" + imgID('c')}; !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	for _, node := range f.nodesContacted() {
		if node != "gt-sbx-a" {
			t.Fatalf("contacted %q, which the filter overmatched", node)
		}
	}
}

// TestPlanIssuesNoRmi is the dry-run rule: Policy.Apply is false, so a plan
// reads the nodes and removes nothing.
func TestPlanIssuesNoRmi(t *testing.T) {
	f := newFake("gt-sbx-a\n", map[string][]nodeImage{
		"gt-sbx-a": {image('a', "a", time.Hour), image('c', "c", 3*time.Hour)},
	})
	plan, err := NewTarget(Config{Run: f.run}).Plan(context.Background(), diskmaint.Policy{Keep: 1, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) == 0 {
		t.Fatal("no actions planned, so the rmi assertion would be vacuous")
	}
	if got := f.everyRmi(); len(got) != 0 {
		t.Fatalf("a dry run issued rmi: %v", got)
	}
	if len(f.crictlArgs("gt-sbx-a")) == 0 {
		t.Fatal("the dry run never read the node")
	}
}

// TestApplyRemovesOnlyThePlannedImages is the bound Apply must hold: the live
// state holds an old image the plan did not name, and Apply must leave it.
func TestApplyRemovesOnlyThePlannedImages(t *testing.T) {
	f := newFake("gt-sbx-a\n", map[string][]nodeImage{
		"gt-sbx-a": {
			image('a', "a", time.Hour),
			image('c', "c", 3*time.Hour),
			// Live state the plan was not built from: an old image that a
			// re-plan would remove.
			image('9', "9", 30*24*time.Hour),
		},
	})
	ledger := &fakeLedger{}
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{{
		Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned",
	}}}

	result, err := NewTarget(Config{Run: f.run, Ledger: ledger}).Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.everyRmi(), []string{"gt-sbx-a/" + imgID('c')}; !slices.Equal(got, want) {
		t.Fatalf("rmi = %v, want only the planned %v", got, want)
	}
	if len(result.Done) != 1 || result.Done[0].Ref != "gt-sbx-a/"+imgID('c') {
		t.Fatalf("Done = %v, want the planned action", result.Done)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", result.Failed)
	}
}

func TestApplyTouchesOnlyPlannedNodes(t *testing.T) {
	f := newFake("gt-sbx-a\ngt-sbx-b\n", map[string][]nodeImage{
		"gt-sbx-a": {image('a', "a", time.Hour), image('c', "c", 3*time.Hour)},
		"gt-sbx-b": {image('a', "a", time.Hour), image('c', "c", 3*time.Hour)},
	})
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{{
		Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned",
	}}}
	if _, err := NewTarget(Config{Run: f.run, Ledger: &fakeLedger{}}).Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, node := range f.nodesContacted() {
		if node != "gt-sbx-a" {
			t.Fatalf("contacted %q, which the plan does not name", node)
		}
	}
}

func TestApplyValidatesBeforeAnyCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action diskmaint.Action
	}{
		{"wrong kind", diskmaint.Action{Kind: "delete-manifest", Ref: "gt-sbx-a/" + imgID('c')}},
		{"short id", diskmaint.Action{Kind: actionRemove, Ref: "gt-sbx-a/not-a-digest"}},
		{"invalid node", diskmaint.Action{Kind: actionRemove, Ref: "gt-sbx a/" + imgID('c')}},
		{"no node", diskmaint.Action{Kind: actionRemove, Ref: imgID('c')}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("gt-sbx-a\n", nil)
			ledger := &fakeLedger{}
			plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{tc.action}}

			result, err := NewTarget(Config{Run: f.run, Ledger: ledger}).Apply(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("an invalid ref ran a command: %v", f.calls)
			}
			if len(result.Failed) != 1 {
				t.Fatalf("Failed = %v, want one", result.Failed)
			}
			if len(ledger.entries) != 1 || ledger.entries[0].Planned != 1 || ledger.entries[0].Done != 0 {
				t.Fatalf("ledger = %+v, want one line with the action planned and none done", ledger.entries)
			}
			for _, e := range ledger.entries[0].Errors {
				if strings.Contains(e, tc.action.Ref) {
					t.Fatalf("recorded error quotes the untrusted ref: %q", e)
				}
			}
		})
	}
}

func TestApplyRecordsAFailedRemovalAndKeepsGoing(t *testing.T) {
	f := newFake("gt-sbx-a\n", map[string][]nodeImage{
		"gt-sbx-a": {image('c', "c", 3*time.Hour), image('d', "d", 4*time.Hour)},
	})
	f.rmiErr[imgID('c')] = true
	ledger := &fakeLedger{}
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
		{Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned"},
		{Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('d'), Why: "planned"},
	}}

	result, err := NewTarget(Config{Run: f.run, Ledger: ledger}).Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Ref != "gt-sbx-a/"+imgID('c') {
		t.Fatalf("Failed = %v, want the failing action", result.Failed)
	}
	if len(result.Done) != 1 || result.Done[0].Ref != "gt-sbx-a/"+imgID('d') {
		t.Fatalf("Done = %v, want the other action", result.Done)
	}
	if len(ledger.entries) != 1 || ledger.entries[0].Done != 1 || len(ledger.entries[0].Errors) != 1 {
		t.Fatalf("ledger = %+v, want one done and one error", ledger.entries)
	}
}

func TestApplyRecordsARefusedPlanAndRunsNothing(t *testing.T) {
	f := newFake("gt-sbx-a\n", nil)
	ledger := &fakeLedger{}
	plan := diskmaint.Plan{Target: planTarget, Refused: "no kind nodes"}

	if _, err := NewTarget(Config{Run: f.run, Ledger: ledger}).Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused plan ran %v", f.calls)
	}
	if len(ledger.entries) != 1 || ledger.entries[0].Refused != "no kind nodes" {
		t.Fatalf("ledger = %+v, want the refusal recorded", ledger.entries)
	}
}

func TestApplyRecordsAnEmptyPlan(t *testing.T) {
	f := newFake("gt-sbx-a\n", nil)
	ledger := &fakeLedger{}
	if _, err := NewTarget(Config{Run: f.run, Ledger: ledger}).Apply(context.Background(), diskmaint.Plan{Target: planTarget}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("an empty plan ran %v", f.calls)
	}
	if len(ledger.entries) != 1 || ledger.entries[0].Planned != 0 {
		t.Fatalf("ledger = %+v, want one empty run", ledger.entries)
	}
}

func TestApplyWithoutALedgerRefuses(t *testing.T) {
	f := newFake("gt-sbx-a\n", nil)
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{{
		Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned",
	}}}
	if _, err := NewTarget(Config{Run: f.run}).Apply(context.Background(), plan); !errors.Is(err, errNoLedger) {
		t.Fatalf("error = %v, want errNoLedger", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("an apply without a ledger ran %v", f.calls)
	}
}

func TestApplyWithoutARunnerFails(t *testing.T) {
	ledger := &fakeLedger{}
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{{
		Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned",
	}}}
	if _, err := NewTarget(Config{Ledger: ledger}).Apply(context.Background(), plan); !errors.Is(err, errNoRunner) {
		t.Fatalf("error = %v, want errNoRunner", err)
	}
	if len(ledger.entries) != 1 {
		t.Fatalf("ledger = %+v, want the failed run recorded", ledger.entries)
	}
}

func TestApplyReturnsALedgerFailure(t *testing.T) {
	f := newFake("gt-sbx-a\n", map[string][]nodeImage{
		"gt-sbx-a": {image('c', "c", 3*time.Hour)},
	})
	ledger := &fakeLedger{err: errors.New("no space left on device")}
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{{
		Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned",
	}}}

	result, err := NewTarget(Config{Run: f.run, Ledger: ledger}).Apply(context.Background(), plan)
	if err == nil {
		t.Fatal("a failed ledger append did not fail the apply")
	}
	if len(result.Done) != 1 {
		t.Fatalf("Done = %v, want the removal reported even though the record failed", result.Done)
	}
}

func TestApplyStampsTheLedgerWithTheConfiguredClock(t *testing.T) {
	f := newFake("gt-sbx-a\n", map[string][]nodeImage{
		"gt-sbx-a": {image('c', "c", 3*time.Hour)},
	})
	ledger := &fakeLedger{}
	clock := func() time.Time { return testNow }
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{{
		Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned",
	}}}

	if _, err := NewTarget(Config{Run: f.run, Ledger: ledger, Now: clock}).Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(ledger.entries) != 1 || !ledger.entries[0].TS.Equal(testNow) {
		t.Fatalf("TS = %v, want %v", ledger.entries, testNow)
	}
	if ledger.entries[0].Target != planTarget || ledger.entries[0].Mode != "apply" {
		t.Fatalf("entry = %+v, want target %q in apply mode", ledger.entries[0], planTarget)
	}
}

func TestApplyWritesOneLedgerLinePerRun(t *testing.T) {
	f := newFake("gt-sbx-a\n", map[string][]nodeImage{
		"gt-sbx-a": {image('c', "c", 3*time.Hour), image('d', "d", 4*time.Hour)},
	})
	ledger := &fakeLedger{}
	target := NewTarget(Config{Run: f.run, Ledger: ledger})
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{{
		Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c'), Why: "planned",
	}}}

	for range 2 {
		if _, err := target.Apply(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if len(ledger.entries) != 2 {
		t.Fatalf("ledger entries = %d, want one per apply", len(ledger.entries))
	}
}

// TestActionImageAcceptsThePlannedRef pins the parser Apply relies on: a
// planned ref yields exactly the node and the image it names.
func TestActionImageAcceptsThePlannedRef(t *testing.T) {
	node, im, err := actionImage(diskmaint.Action{Kind: actionRemove, Ref: "gt-sbx-a/" + imgID('c')})
	if err != nil {
		t.Fatal(err)
	}
	if node != "gt-sbx-a" || im.ID != imgID('c') {
		t.Fatalf("parsed (%q, %+v), want the node and the image", node, im)
	}
}
