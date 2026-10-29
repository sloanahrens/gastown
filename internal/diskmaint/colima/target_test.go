package colima

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// dockerHost is a placeholder daemon socket; every test checks it reaches docker.
const dockerHost = "unix:///placeholder/.colima/gt-sbx/docker.sock"

// imgID is an image id: a sha256 digest, the shape registry.ValidDigest accepts.
func imgID(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// call is one command the target ran and the environment it ran with.
type call struct {
	env  []string
	args []string
}

// fakeDocker answers the docker commands the target runs from canned output and
// records every one, so a test asserts what was asked for and never reaches a
// real daemon.
type fakeDocker struct {
	calls         []call
	images        string
	imagesErr     error
	containers    string
	containersErr error
	inspect       string
	inspectErr    error
	rmiErr        map[string]error
}

func (f *fakeDocker) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{env: slices.Clone(env), args: slices.Clone(args)})
	if name != "docker" || len(args) == 0 {
		return nil, errors.New("unexpected command")
	}
	switch args[0] {
	case "image":
		return []byte(f.images), f.imagesErr
	case "ps":
		return []byte(f.containers), f.containersErr
	case "inspect":
		return []byte(f.inspect), f.inspectErr
	case "rmi":
		return nil, f.rmiErr[args[len(args)-1]]
	}
	return nil, errors.New("unexpected docker command")
}

// rmiCalls is the image ids the target asked docker to remove, in call order.
func (f *fakeDocker) rmiCalls() []call {
	var out []call
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "rmi" {
			out = append(out, c)
		}
	}
	return out
}

// imageLines renders images as docker image ls --format {{json .}} prints them.
func imageLines(t *testing.T, images ...image) string {
	t.Helper()
	var b strings.Builder
	for _, im := range images {
		line, err := json.Marshal(im)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func newTarget(t *testing.T, f *fakeDocker) (*Target, *diskmaint.Ledger) {
	t.Helper()
	ledger := diskmaint.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 0)
	return NewTarget(Config{DockerHost: dockerHost, Run: f.run, Ledger: ledger}), ledger
}

func lastEntry(t *testing.T, ledger *diskmaint.Ledger) diskmaint.LedgerEntry {
	t.Helper()
	entries, err := ledger.Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger holds %d entries, want 1", len(entries))
	}
	return entries[0]
}

func TestNameIsColima(t *testing.T) {
	t.Parallel()
	if got := NewTarget(Config{}).Name(); got != "colima" {
		t.Errorf("Name() = %q, want colima", got)
	}
}

func TestPlanRemovesOnlyUnusedDanglingImages(t *testing.T) {
	t.Parallel()
	const none = "<none>"
	unused, inUse := imgID('1'), imgID('2')
	f := &fakeDocker{
		images: imageLines(t,
			image{Repository: none, Tag: none, ID: unused, Size: "1.5GB"},
			image{Repository: none, Tag: none, ID: inUse, Size: "10MB"},
			image{Repository: "gastown/app", Tag: "v1", ID: imgID('3'), Size: "200MB"},
			image{Repository: none, Tag: "v9", ID: imgID('4'), Size: "5MB"},
			// A repeated row for the unused id must not plan its removal twice.
			image{Repository: none, Tag: none, ID: unused, Size: "1.5GB"},
		),
		containers: "abc123abc123\n",
		inspect:    inUse + "\n",
	}
	target, _ := newTarget(t, f)

	plan, err := target.Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Refused != "" {
		t.Fatalf("plan refused: %s", plan.Refused)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("planned %d actions, want 1: %+v", len(plan.Actions), plan.Actions)
	}
	if a := plan.Actions[0]; a.Kind != "remove-image" || a.Ref != unused || a.Bytes != 1_500_000_000 {
		t.Errorf("action = %+v, want remove-image of %s at 1500000000 bytes", a, unused)
	}
	if len(plan.Kept) != 3 {
		t.Fatalf("kept %d refs, want 3: %+v", len(plan.Kept), plan.Kept)
	}
	wantKept := []diskmaint.Kept{
		{Ref: none + "@" + inUse, Rule: ruleInUse},
		{Ref: "gastown/app:v1", Rule: ruleTagged},
		{Ref: none + ":v9", Rule: rulePartial},
	}
	for i, want := range wantKept {
		if plan.Kept[i] != want {
			t.Errorf("kept[%d] = %+v, want %+v", i, plan.Kept[i], want)
		}
	}
	if n := len(f.rmiCalls()); n != 0 {
		t.Errorf("Plan issued %d rmi calls, want 0 (a dry run removes nothing)", n)
	}
	for _, c := range f.calls {
		if !slices.Equal(c.env, []string{"DOCKER_HOST=" + dockerHost}) {
			t.Errorf("docker call %v ran with env %v, want DOCKER_HOST=%s", c.args, c.env, dockerHost)
		}
	}
}

func TestPlanRefusesWhenAnInputIsUnreadable(t *testing.T) {
	t.Parallel()
	good := imageLines(t, image{Repository: "<none>", Tag: "<none>", ID: imgID('1'), Size: "1MB"})
	cases := []struct {
		name string
		f    fakeDocker
		want error
	}{
		{
			name: "unreadable image list",
			f:    fakeDocker{imagesErr: errors.New("SECRET stderr")},
			want: errListImages,
		},
		{
			name: "unreadable container list",
			f:    fakeDocker{images: good, containersErr: errors.New("SECRET stderr")},
			want: errListContainers,
		},
		{
			name: "unreadable container image lookup",
			f:    fakeDocker{images: good, containers: "abc123abc123\n", inspectErr: errors.New("SECRET stderr")},
			want: errInspect,
		},
		{
			name: "image line that is not JSON",
			f:    fakeDocker{images: "not json\n"},
			want: errParse,
		},
		{
			name: "image line with a bad id",
			f:    fakeDocker{images: imageLines(t, image{Repository: "<none>", Tag: "<none>", ID: "short"})},
			want: errParse,
		},
		{
			name: "container id that does not parse",
			f:    fakeDocker{images: good, containers: "zz; rm -rf /\n"},
			want: errParse,
		},
		{
			name: "container image line that does not parse",
			f:    fakeDocker{images: good, containers: "abc123abc123\n", inspect: "not-a-digest\n"},
			want: errParse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			target, _ := newTarget(t, &f)

			plan, err := target.Plan(context.Background(), diskmaint.Policy{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Refused != tc.want.Error() {
				t.Errorf("Refused = %q, want %q", plan.Refused, tc.want.Error())
			}
			if len(plan.Actions) != 0 {
				t.Errorf("refused plan carries %d actions, want none", len(plan.Actions))
			}
			if strings.Contains(plan.Refused, "SECRET") {
				t.Errorf("refusal echoes command output: %q", plan.Refused)
			}
			if n := len(f.rmiCalls()); n != 0 {
				t.Errorf("refused plan led to %d rmi calls", n)
			}
		})
	}
}

func TestPlanRefusesWithoutRunnerOrDockerHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{name: "no runner", cfg: Config{DockerHost: dockerHost}, want: errNoRunner},
		{name: "no docker host", cfg: Config{Run: (&fakeDocker{}).run}, want: errNoDockerHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := NewTarget(tc.cfg).Plan(context.Background(), diskmaint.Policy{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Refused != tc.want.Error() {
				t.Errorf("Refused = %q, want %q", plan.Refused, tc.want.Error())
			}
		})
	}
}

func TestApplyRemovesEachImageAndKeepsGoingAfterFailure(t *testing.T) {
	t.Parallel()
	a1, a2, a3 := imgID('1'), imgID('2'), imgID('3')
	f := &fakeDocker{rmiErr: map[string]error{a2: errors.New("SECRET stderr")}}
	target, ledger := newTarget(t, f)
	plan := diskmaint.Plan{Target: "colima", Actions: []diskmaint.Action{
		{Kind: "remove-image", Ref: a1, Bytes: 100},
		{Kind: "remove-image", Ref: a2, Bytes: 250},
		{Kind: "remove-image", Ref: a3, Bytes: 7},
	}}

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	rmi := f.rmiCalls()
	if len(rmi) != 3 {
		t.Fatalf("rmi ran %d times, want once per id", len(rmi))
	}
	for i, id := range []string{a1, a2, a3} {
		if !slices.Equal(rmi[i].args, []string{"rmi", id}) {
			t.Errorf("rmi call %d args = %v, want [rmi %s] (no prune, no -f)", i, rmi[i].args, id)
		}
		if !slices.Equal(rmi[i].env, []string{"DOCKER_HOST=" + dockerHost}) {
			t.Errorf("rmi call %d env = %v, want DOCKER_HOST=%s", i, rmi[i].env, dockerHost)
		}
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

	entry := lastEntry(t, ledger)
	if entry.Mode != "apply" || entry.Planned != 3 || entry.Done != 2 || entry.BytesFreed != 107 {
		t.Errorf("ledger entry = %+v, want apply, 3 planned, 2 done, 107 bytes", entry)
	}
	if len(entry.Errors) != 1 || strings.Contains(entry.Errors[0], "SECRET") {
		t.Errorf("ledger errors = %q, want one entry that does not echo command output", entry.Errors)
	}
}

func TestApplyRefusesBadActionsBeforeAnyCommand(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{}
	target, ledger := newTarget(t, f)
	plan := diskmaint.Plan{Target: "colima", Actions: []diskmaint.Action{
		{Kind: "remove-image", Ref: "sha256:short"},
		{Kind: "delete-manifest", Ref: imgID('1')},
	}}

	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls); n != 0 {
		t.Errorf("bad actions led to %d docker calls, want none", n)
	}
	if len(result.Done) != 0 || len(result.Failed) != 2 {
		t.Errorf("Done=%d Failed=%d, want 0 and 2", len(result.Done), len(result.Failed))
	}
	if entry := lastEntry(t, ledger); len(entry.Errors) != 2 {
		t.Errorf("ledger errors = %q, want one per bad action", entry.Errors)
	}
}

func TestApplyRemovesNothingForRefusedOrEmptyPlan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		plan    diskmaint.Plan
		wantErr error
	}{
		{name: "refused", plan: diskmaint.Plan{Target: "colima", Refused: errListImages.Error()}, wantErr: errPlanRefused},
		{name: "refused with actions", plan: diskmaint.Plan{Target: "colima", Refused: errInspect.Error(),
			Actions: []diskmaint.Action{{Kind: "remove-image", Ref: imgID('1')}}}, wantErr: errPlanRefused},
		{name: "empty", plan: diskmaint.Plan{Target: "colima"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDocker{}
			target, ledger := newTarget(t, f)

			result, err := target.Apply(context.Background(), tc.plan)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Apply err = %v, want %v", err, tc.wantErr)
			}
			if len(f.calls) != 0 || len(result.Done) != 0 {
				t.Errorf("made %d calls and %d removals, want none", len(f.calls), len(result.Done))
			}
			if entry := lastEntry(t, ledger); entry.Refused != tc.plan.Refused {
				t.Errorf("ledger Refused = %q, want %q", entry.Refused, tc.plan.Refused)
			}
		})
	}
}

func TestApplyRefusesWithoutDockerHost(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{}
	ledger := diskmaint.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 0)
	target := NewTarget(Config{Run: f.run, Ledger: ledger})
	plan := diskmaint.Plan{Target: "colima", Actions: []diskmaint.Action{{Kind: "remove-image", Ref: imgID('1')}}}

	if _, err := target.Apply(context.Background(), plan); !errors.Is(err, errNoDockerHost) {
		t.Fatalf("Apply err = %v, want errNoDockerHost", err)
	}
	if n := len(f.calls); n != 0 {
		t.Errorf("Apply without a docker host made %d calls, want none", n)
	}
}

func TestApplyRequiresLedger(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{}
	target := NewTarget(Config{DockerHost: dockerHost, Run: f.run})
	plan := diskmaint.Plan{Target: "colima", Actions: []diskmaint.Action{{Kind: "remove-image", Ref: imgID('1')}}}

	if _, err := target.Apply(context.Background(), plan); !errors.Is(err, errNoLedger) {
		t.Fatalf("Apply err = %v, want errNoLedger", err)
	}
	if n := len(f.calls); n != 0 {
		t.Errorf("Apply without a ledger made %d calls, want none", n)
	}
}

func TestApplyReportsLedgerWriteFailure(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{}
	// The ledger's path is a directory, so opening it for an append fails on the
	// filesystem itself and no seam is needed. It does not depend on whether the
	// ledger creates missing parent directories.
	ledger := diskmaint.NewLedger(t.TempDir(), 0)
	target := NewTarget(Config{DockerHost: dockerHost, Run: f.run, Ledger: ledger})

	_, err := target.Apply(context.Background(), diskmaint.Plan{Target: "colima"})
	if err == nil || !strings.Contains(err.Error(), "record the apply") {
		t.Fatalf("Apply err = %v, want the ledger write failure", err)
	}
}

func TestMeasureSumsEveryImage(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{images: imageLines(t,
		image{Repository: "<none>", Tag: "<none>", ID: imgID('1'), Size: "1.5GB"},
		image{Repository: "gastown/app", Tag: "v1", ID: imgID('2'), Size: "200MB"},
		image{Repository: "<none>", Tag: "<none>", ID: imgID('3'), Size: "0B"},
	)}
	target, _ := newTarget(t, f)

	m, err := target.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Used != 1_700_000_000 {
		t.Errorf("Used = %d, want 1700000000", m.Used)
	}
}

func TestMeasureCountsAnImageOnceForEachTag(t *testing.T) {
	t.Parallel()
	// docker prints one row per tag, each carrying the image's id and size.
	f := &fakeDocker{images: imageLines(t,
		image{Repository: "gastown/app", Tag: "v1", ID: imgID('1'), Size: "200MB"},
		image{Repository: "gastown/app", Tag: "latest", ID: imgID('1'), Size: "200MB"},
		image{Repository: "<none>", Tag: "<none>", ID: imgID('2'), Size: "50MB"},
	)}
	target, _ := newTarget(t, f)

	m, err := target.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Used != 250_000_000 {
		t.Errorf("Used = %d, want 250000000 (the shared image once, plus the dangling one)", m.Used)
	}
	if m.Detail != "2 images on the colima daemon" {
		t.Errorf("Detail = %q, want the count of distinct images", m.Detail)
	}
}

func TestMeasureRefusesUnreadableList(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{imagesErr: errors.New("SECRET stderr")}
	target, _ := newTarget(t, f)

	if _, err := target.Measure(context.Background()); !errors.Is(err, errListImages) {
		t.Fatalf("Measure err = %v, want errListImages", err)
	}
}

func TestSizeParsing(t *testing.T) {
	t.Parallel()
	cases := map[string]int64{
		"1.5GB":   1_500_000_000,
		"200MB":   200_000_000,
		"3kB":     3_000,
		"10KiB":   10_240,
		"0B":      0,
		"":        0,
		"garbage": 0,
		"12XB":    0,
		"-1MB":    0,
	}
	for size, want := range cases {
		if got := (image{Size: size}).bytes(); got != want {
			t.Errorf("bytes(%q) = %d, want %d", size, got, want)
		}
	}
}
