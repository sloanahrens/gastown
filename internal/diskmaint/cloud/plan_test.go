package cloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
)

// labelsPath is a placeholder file path, not the VM's own; the tests assert the
// runner is asked for the configured one, so a plan with a constant path fails.
const labelsPath = "/placeholder/gt-cloud/runner-labels"

// fakeDaemon answers the plan's two reads: docker's image list, and the runner
// label file, which the plan names as "cat <path>".
type fakeDaemon struct {
	images    string
	imagesErr error
	labels    string
	labelsErr error
	reads     []call
}

func (f *fakeDaemon) run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if name != "cat" {
		return (&fakeDocker{images: f.images, imagesErr: f.imagesErr}).run(ctx, env, name, args...)
	}
	f.reads = append(f.reads, call{env: slices.Clone(env), args: slices.Clone(args)})
	return []byte(f.labels), f.labelsErr
}

// at renders a creation time the way docker image ls prints it.
func at(day int) string { return fmt.Sprintf("2026-11-%02d 12:00:00 +0000 UTC", day) }

// fractals is one tag of the repository, listed on day with id c and size.
func fractals(tag string, c byte, day int, size string) dockerimg.Image {
	return dockerimg.Image{Repository: fractalsRepo, Tag: tag, ID: imgID(c), Size: size, CreatedAt: at(day)}
}

// newPlanTarget builds a target whose daemon and label file answer from f.
func newPlanTarget(t *testing.T, f *fakeDaemon, labelsFile string) *Target {
	t.Helper()
	target, err := NewTarget(Config{DockerHost: dockerHost, Run: f.run, Ledger: ledger(t), LabelsFile: labelsFile})
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	return target
}

// planOf runs one plan and fails on the error or on a refusal a case expected
// to be planned.
func planOf(t *testing.T, f *fakeDaemon, p diskmaint.Policy) diskmaint.Plan {
	t.Helper()
	plan, err := newPlanTarget(t, f, labelsPath).Plan(context.Background(), p)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Refused != "" {
		t.Fatalf("plan refused: %s", plan.Refused)
	}
	return plan
}

// rules maps each kept ref to the rule the plan says kept it.
func rules(plan diskmaint.Plan) map[string]string {
	out := make(map[string]string, len(plan.Kept))
	for _, k := range plan.Kept {
		out[k.Ref] = k.Rule
	}
	return out
}

// removed lists the refs of the plan's actions.
func removed(plan diskmaint.Plan) []string {
	var out []string
	for _, a := range plan.Actions {
		out = append(out, a.Ref)
	}
	return out
}

func TestPlanKeepsTheNewestFractalsTags(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{
		images: imageLines(t,
			fractals("t7", '7', 7, "70MB"),
			fractals("t6", '6', 6, "60MB"),
			fractals("t5", '5', 5, "50MB"),
			fractals("t4", '4', 4, "40MB"),
			fractals("t3", '3', 3, "30MB"),
			fractals("t2", '2', 2, "20MB"),
			fractals("t1", '1', 1, "10MB"),
			// A second tag of the image t1 names: one removal names the id once.
			fractals("t1b", '1', 1, "10MB"),
		),
		labels: "node:docker://code.forgejo.org/oci/node:lts\n",
	}
	// A policy that asks for a wider window: the count is the plan's own, so
	// only the five newest are kept.
	plan := planOf(t, f, diskmaint.Policy{Keep: 9, Apply: false})

	if plan.Target != "cloud" {
		t.Errorf("Target = %q, want cloud", plan.Target)
	}
	if plan.Policy.Keep != 9 || plan.Policy.Apply {
		t.Errorf("Policy = %+v, want the policy Plan was called with: Apply reads its dry-run flag", plan.Policy)
	}

	got := rules(plan)
	const rule = "among the newest 5 fractals-nextjs tags"
	for _, tag := range []string{"t7", "t6", "t5", "t4", "t3"} {
		if ref := fractalsRepo + ":" + tag; got[ref] != rule {
			t.Errorf("Kept[%s] = %q, want %q", ref, got[ref], rule)
		}
	}
	if len(plan.Kept) != 5 {
		t.Errorf("Kept = %v, want exactly the five newest tags", plan.Kept)
	}

	want := []string{imgID('2'), imgID('1')}
	if !slices.Equal(removed(plan), want) {
		t.Errorf("removals = %v, want %v", removed(plan), want)
	}
	if plan.Actions[0].Kind != actionRemove || plan.Actions[0].Bytes != 20_000_000 {
		t.Errorf("action = %+v, want a %s of 20MB", plan.Actions[0], actionRemove)
	}
}

func TestPlanKeepsTheTagsARunnerLabelNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels string
		kept   string
		want   []string
	}{
		{
			name:   "the oldest tag",
			labels: "# the runner's labels\nself-hosted\nfractals-prod:docker://fractals-nextjs:t1\n",
			kept:   "t1",
			want:   []string{imgID('3'), imgID('2')},
		},
		{
			name:   "a registry-prefixed ref, and a different tag",
			labels: "fractals-prod:docker://registry.example:5000/team/fractals-nextjs:t2\n",
			kept:   "t2",
			want:   []string{imgID('3'), imgID('1')},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeDaemon{
				images: imageLines(t,
					fractals("t8", '8', 8, "80MB"),
					fractals("t7", '7', 7, "70MB"),
					fractals("t6", '6', 6, "60MB"),
					fractals("t5", '5', 5, "50MB"),
					fractals("t4", '4', 4, "40MB"),
					fractals("t3", '3', 3, "30MB"),
					fractals("t2", '2', 2, "20MB"),
					fractals("t1", '1', 1, "10MB"),
				),
				labels: tc.labels,
			}
			plan := planOf(t, f, diskmaint.Policy{})

			ref := fractalsRepo + ":" + tc.kept
			if got, want := rules(plan)[ref], "named by a runner label or deploy workflow"; got != want {
				t.Errorf("Kept[%s] = %q, want %q", ref, got, want)
			}
			if got := removed(plan); !slices.Equal(got, tc.want) {
				t.Errorf("removals = %v, want %v", got, tc.want)
			}
			if len(f.reads) != 1 || !slices.Equal(f.reads[0].args, []string{"--", labelsPath}) {
				t.Errorf("label reads = %v, want one cat of %s", f.reads, labelsPath)
			}
		})
	}
}

func TestPlanRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	good := imageLines(t, fractals("t1", '1', 1, "10MB"))
	cases := []struct {
		name       string
		labelsFile string
		f          fakeDaemon
		want       string
	}{
		{
			name:       "no label list configured",
			labelsFile: "",
			f:          fakeDaemon{images: good},
			want:       refuseNoLabels,
		},
		{
			name:       "unreadable label list",
			labelsFile: labelsPath,
			f:          fakeDaemon{images: good, labelsErr: errors.New("cat: SECRET: no such file")},
			want:       refuseLabelRead,
		},
		{
			name:       "label naming the repository with no tag",
			labelsFile: labelsPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://fractals-nextjs\n"},
			want:       refuseLabelLine,
		},
		{
			name:       "unreadable image list",
			labelsFile: labelsPath,
			f:          fakeDaemon{imagesErr: errors.New("SECRET stderr"), labels: ""},
			want:       dockerimg.ErrListImages.Error(),
		},
		{
			name:       "fractals tag with no creation time",
			labelsFile: labelsPath,
			f: fakeDaemon{images: imageLines(t,
				dockerimg.Image{Repository: fractalsRepo, Tag: "t1", ID: imgID('1'), Size: "10MB"})},
			want: refuseUndated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.f
			plan, err := newPlanTarget(t, &f, tc.labelsFile).Plan(context.Background(), diskmaint.Policy{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.Refused != tc.want {
				t.Fatalf("Refused = %q, want %q", plan.Refused, tc.want)
			}
			if len(plan.Actions) != 0 || len(plan.Kept) != 0 {
				t.Errorf("plan = %+v, want no actions and no kept list: a refused plan decides nothing", plan)
			}
			if strings.Contains(plan.Refused, "SECRET") {
				t.Errorf("refusal echoes command output: %s", plan.Refused)
			}
		})
	}
}

func TestPlanKeepsEveryImageItDoesNotRemove(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{
		images: imageLines(t,
			// Another repository's image and a dangling one are never this plan's.
			dockerimg.Image{Repository: "gt-cloud", Tag: "latest", ID: imgID('9'), Size: "10MB", CreatedAt: at(9)},
			dockerimg.Image{Repository: noneField, Tag: noneField, ID: imgID('8'), Size: "1MB", CreatedAt: at(8)},
			// Five tags fill the window, and `old` is a second tag of the oldest
			// kept image: the removal names the id, so it is kept with it.
			fractals("t5", '5', 6, "50MB"),
			fractals("t4", '4', 5, "40MB"),
			fractals("t3", '3', 4, "30MB"),
			fractals("t2", '2', 3, "20MB"),
			fractals("t1", '1', 2, "10MB"),
			fractals("old", '1', 1, "5MB"),
		),
		labels: "node:docker://code.forgejo.org/oci/node:lts\n",
	}
	plan := planOf(t, f, diskmaint.Policy{})

	if got := removed(plan); len(got) != 0 {
		t.Errorf("removals = %v, want none", got)
	}
	got := rules(plan)
	for ref, want := range map[string]string{
		"gt-cloud:latest":            "not a fractals-nextjs tag; this plan removes none",
		noneField + "@" + imgID('8'): "not a fractals-nextjs tag; this plan removes none",
		fractalsRepo + ":old":        "shares an image a rule keeps",
		fractalsRepo + ":t1":         "among the newest 5 fractals-nextjs tags",
	} {
		if got[ref] != want {
			t.Errorf("Kept[%s] = %q, want %q", ref, got[ref], want)
		}
	}
}

func TestPlanKeepsAnImageAnotherRepositoryAlsoTags(t *testing.T) {
	t.Parallel()
	// Six tags put t1 outside the window, and the same image also carries a
	// gt-cloud tag: removing the id would drop that tag, so the image stays.
	f := &fakeDaemon{
		images: imageLines(t,
			fractals("t6", '6', 6, "60MB"),
			fractals("t5", '5', 5, "50MB"),
			fractals("t4", '4', 4, "40MB"),
			fractals("t3", '3', 3, "30MB"),
			fractals("t2", '2', 2, "20MB"),
			fractals("t1", '1', 1, "10MB"),
			dockerimg.Image{Repository: "gt-cloud", Tag: "old", ID: imgID('1'), Size: "10MB", CreatedAt: at(1)},
		),
		labels: "node:docker://code.forgejo.org/oci/node:lts\n",
	}
	plan := planOf(t, f, diskmaint.Policy{})

	if got := removed(plan); len(got) != 0 {
		t.Errorf("removals = %v, want none", got)
	}
	if got, want := rules(plan)[fractalsRepo+":t1"], "shares an image a rule keeps"; got != want {
		t.Errorf("Kept[%s] = %q, want %q", fractalsRepo+":t1", got, want)
	}
}
