package cloud

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
)

// labelsPath and deployPath are placeholder file paths, not the VM's own; the
// tests assert the runner is asked for the configured ones, so a plan with a
// constant path fails.
const (
	labelsPath = "/placeholder/gt-cloud/runner-labels"
	deployPath = "/placeholder/gt-cloud/deploy.yml"
)

// cleanDeploy is a deploy workflow that names no image of the repository, so a
// test that is about the labels reads past it.
const cleanDeploy = "name: deploy\n" +
	"on:\n" +
	"  push:\n" +
	"    tags: [\"v*\"]\n" +
	"jobs:\n" +
	"  deploy:\n" +
	"    runs-on: ci-base\n" +
	"    steps:\n" +
	"      - run: make deploy\n"

// fakeDaemon answers the plan's reads: docker's image list and image inspect,
// the runner label file and the deploy workflow, the files named as "cat <path>".
type fakeDaemon struct {
	images     string
	imagesErr  error
	digests    map[string][]string
	digestsErr error
	digestsRaw string
	labels     string
	labelsErr  error
	deploy     string
	deployErr  error
	reads      []call
}

func (f *fakeDaemon) run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if name != "cat" {
		return (&fakeDocker{
			images: f.images, imagesErr: f.imagesErr,
			digests: f.digests, digestsErr: f.digestsErr, digestsRaw: f.digestsRaw,
		}).run(ctx, env, name, args...)
	}
	f.reads = append(f.reads, call{env: slices.Clone(env), args: slices.Clone(args)})
	switch {
	case len(args) == 2 && args[1] == labelsPath:
		return []byte(f.labels), f.labelsErr
	case len(args) == 2 && args[1] == deployPath:
		return []byte(f.deploy), f.deployErr
	}
	return nil, errors.New("unexpected file read")
}

// registryDigest is the registry digest a fixture tag's image carries: a hash of
// the tag, which is never the image id, as a pushed image's digest never is.
func registryDigest(tag string) string {
	return "sha256:" + fmt.Sprintf("%x", sha256.Sum256([]byte(tag)))
}

// daemon renders fixture rows as docker's image list and the registry refs each
// image carries, keyed by image id. A row of the repository's is taken as pushed
// under its first tag; any other image carries none, as a local-only image does.
func daemon(t *testing.T, rows ...dockerimg.Image) (string, map[string][]string) {
	t.Helper()
	digests := make(map[string][]string, len(rows))
	for _, im := range rows {
		if im.Repository != fractalsRepo {
			continue
		}
		if _, ok := digests[im.ID]; !ok {
			digests[im.ID] = []string{fractalsRepo + "@" + registryDigest(im.Tag)}
		}
	}
	return imageLines(t, rows...), digests
}

// readPaths is the file path of every cat the fake ran, in call order.
func readPaths(calls []call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.args[len(c.args)-1])
	}
	return out
}

// at renders a creation time the way docker image ls prints it.
func at(day int) string { return fmt.Sprintf("2026-11-%02d 12:00:00 +0000 UTC", day) }

// fractals is one tag of the repository, listed on day with id c and size.
func fractals(tag string, c byte, day int, size string) dockerimg.Image {
	return dockerimg.Image{Repository: fractalsRepo, Tag: tag, ID: imgID(c), Size: size, CreatedAt: at(day)}
}

// newPlanTarget builds a target whose daemon and files answer from f.
func newPlanTarget(t *testing.T, f *fakeDaemon, labelsFile, deployFile string) *Target {
	t.Helper()
	target, err := NewTarget(Config{DockerHost: dockerHost, Run: f.run, Ledger: ledger(t), LabelsFile: labelsFile, DeployFile: deployFile})
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	return target
}

// planOf runs one plan and fails on the error or on a refusal a case expected
// to be planned.
func planOf(t *testing.T, f *fakeDaemon, p diskmaint.Policy) diskmaint.Plan {
	t.Helper()
	plan, err := newPlanTarget(t, f, labelsPath, deployPath).Plan(context.Background(), p)
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

// eightTags is t8 newest through t1 oldest, one image per tag, so the newest
// five are a window and t3, t2 and t1 sit outside it.
func eightTags(t *testing.T) (string, map[string][]string) {
	t.Helper()
	images := make([]dockerimg.Image, 0, 8)
	for day := 8; day >= 1; day-- {
		tag := fmt.Sprintf("t%d", day)
		images = append(images, fractals(tag, byte('0'+day), day, fmt.Sprintf("%d0MB", day)))
	}
	return daemon(t, images...)
}

func TestPlanKeepsTheNewestFractalsTags(t *testing.T) {
	t.Parallel()
	images, digests := daemon(t,
		fractals("t7", '7', 7, "70MB"),
		fractals("t6", '6', 6, "60MB"),
		fractals("t5", '5', 5, "50MB"),
		fractals("t4", '4', 4, "40MB"),
		fractals("t3", '3', 3, "30MB"),
		fractals("t2", '2', 2, "20MB"),
		fractals("t1", '1', 1, "10MB"),
		// A second tag of the image t1 names: one removal names the id once.
		fractals("t1b", '1', 1, "10MB"),
	)
	f := &fakeDaemon{
		images:  images,
		digests: digests,
		labels:  "node:docker://code.forgejo.org/oci/node:lts\n",
		deploy:  cleanDeploy,
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
	// The removal is planned from t1 and drops t1b with it, so the action names
	// t1b too: the plan's rows and its actions otherwise never mention it.
	for _, a := range plan.Actions {
		if a.Ref == imgID('1') && !strings.Contains(a.Why, fractalsRepo+":t1b") {
			t.Errorf("the removal of %s says %q, want it to name %s, the tag it drops with the image", a.Ref, a.Why, fractalsRepo+":t1b")
		}
	}
}

func TestPlanKeepsTheTagsARunnerLabelNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels string
		kept   []string
		want   []string
	}{
		{
			name:   "the oldest tag",
			labels: "# the runner's labels\nself-hosted\nfractals-prod:docker://fractals-nextjs:t1\n",
			kept:   []string{"t1"},
			want:   []string{imgID('3'), imgID('2')},
		},
		{
			name:   "a registry-prefixed ref, and a different tag",
			labels: "fractals-prod:docker://registry.example:5000/team/fractals-nextjs:t2\n",
			kept:   []string{"t2"},
			want:   []string{imgID('3'), imgID('1')},
		},
		{
			// A registry digest names the image's content, which is not the id
			// docker lists for it: the image it is matched to, and every tag of
			// it, is kept.
			name:   "an image pinned by a registry digest",
			labels: "fractals-prod:docker://registry.example:5000/team/fractals-nextjs@" + registryDigest("t1") + "\n",
			kept:   []string{"t1"},
			want:   []string{imgID('3'), imgID('2')},
		},
		{
			name:   "an image named by its id",
			labels: "fractals-prod:docker://" + imgID('2') + "\n",
			kept:   []string{"t2"},
			want:   []string{imgID('3'), imgID('1')},
		},
		{
			// A ref naming a tag and a digest names two images: the tag's and the
			// digest's, and both are kept.
			name:   "a tag and a digest in one ref",
			labels: "fractals-prod:docker://fractals-nextjs:t2@" + registryDigest("t3") + "\n",
			kept:   []string{"t2", "t3"},
			want:   []string{imgID('1')},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			images, digests := eightTags(t)
			f := &fakeDaemon{images: images, digests: digests, labels: tc.labels, deploy: cleanDeploy}
			plan := planOf(t, f, diskmaint.Policy{})

			for _, tag := range tc.kept {
				ref := fractalsRepo + ":" + tag
				if got, want := rules(plan)[ref], ruleLabel; got != want {
					t.Errorf("Kept[%s] = %q, want %q", ref, got, want)
				}
			}
			if got := removed(plan); !slices.Equal(got, tc.want) {
				t.Errorf("removals = %v, want %v", got, tc.want)
			}
			if got := readPaths(f.reads); !slices.Equal(got, []string{labelsPath, deployPath}) {
				t.Errorf("file reads = %v, want the two configured paths", got)
			}
		})
	}
}

func TestPlanKeepsTheTagsADeployWorkflowNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		deploy  string
		kept    []string
		removed []string
	}{
		{
			// t1 is named through an env var and t2 by a pull, both outside the
			// newest five: only an image the workflow does not name goes.
			name: "the app image in an env var and a pulled tag",
			deploy: "name: deploy\n" +
				"env:\n" +
				"  APP_IMAGE: registry.example:5000/team/fractals-nextjs:t1\n" +
				"jobs:\n" +
				"  deploy:\n" +
				"    steps:\n" +
				"      - run: docker pull fractals-nextjs:t2\n" +
				"      - run: kubectl set image deployment/fractals app=$APP_IMAGE\n",
			kept:    []string{"t1", "t2"},
			removed: []string{imgID('3')},
		},
		{
			// An assignment is the other way a workflow line names its image.
			name: "the app image in a shell assignment",
			deploy: "name: deploy\n" +
				"jobs:\n" +
				"  deploy:\n" +
				"    steps:\n" +
				"      - run: export IMAGE=fractals-nextjs:t2 && make deploy\n",
			kept:    []string{"t2"},
			removed: []string{imgID('3'), imgID('1')},
		},
		{
			name: "an image pinned by a registry digest",
			deploy: "name: deploy\n" +
				"env:\n" +
				"  APP_IMAGE: fractals-nextjs@" + registryDigest("t1") + "\n",
			kept:    []string{"t1"},
			removed: []string{imgID('3'), imgID('2')},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			images, digests := eightTags(t)
			f := &fakeDaemon{
				images:  images,
				digests: digests,
				labels:  "node:docker://code.forgejo.org/oci/node:lts\n",
				deploy:  tc.deploy,
			}
			plan := planOf(t, f, diskmaint.Policy{})

			for _, tag := range tc.kept {
				ref := fractalsRepo + ":" + tag
				if got, want := rules(plan)[ref], ruleLabel; got != want {
					t.Errorf("Kept[%s] = %q, want %q", ref, got, want)
				}
			}
			if got := removed(plan); !slices.Equal(got, tc.removed) {
				t.Errorf("removals = %v, want %v", got, tc.removed)
			}
			if got := readPaths(f.reads); !slices.Equal(got, []string{labelsPath, deployPath}) {
				t.Errorf("file reads = %v, want the two configured paths", got)
			}
		})
	}
}

func TestPlanRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	good, goodDigests := daemon(t, fractals("t1", '1', 1, "10MB"))
	nodeLabels := "node:docker://code.forgejo.org/oci/node:lts\n"
	// pinning is a label list that pins an image the good daemon carries, so a
	// row about the digest read reaches it.
	pinning := "fractals-prod:docker://fractals-nextjs@" + registryDigest("t1") + "\n"
	two := imageLines(t, fractals("t1", '1', 1, "10MB"), fractals("t2", '2', 2, "20MB"))
	cases := []struct {
		name       string
		labelsFile string
		deployFile string
		f          fakeDaemon
		want       string
	}{
		{
			name:       "no label list configured",
			labelsFile: "",
			deployFile: deployPath,
			f:          fakeDaemon{images: good, deploy: cleanDeploy},
			want:       refuseNoLabels,
		},
		{
			name:       "unreadable label list",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labelsErr: errors.New("cat: SECRET: no such file"), deploy: cleanDeploy},
			want:       refuseLabelRead,
		},
		{
			name:       "label file with no label line",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "\n# the runner's labels are minted at boot\n", deploy: cleanDeploy},
			want:       refuseEmptyLabels,
		},
		{
			name:       "label naming the repository with no tag",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://fractals-nextjs\n", deploy: cleanDeploy},
			want:       refuseLabelLine,
		},
		{
			// The port is not read as a tag, so the ref still names no tag and
			// is refused rather than skipped.
			name:       "label with a registry port and no tag",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://registry.example:5000/team/fractals-nextjs\n", deploy: cleanDeploy},
			want:       refuseLabelLine,
		},
		{
			name:       "label with a templated tag",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://fractals-nextjs:${{ github.ref_name }}\n", deploy: cleanDeploy},
			want:       refuseLabelName,
		},
		{
			name:       "label with a malformed digest",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://fractals-nextjs@sha256:not-a-digest\n", deploy: cleanDeploy},
			want:       refuseLabelName,
		},
		{
			// A line without its newline is a read cut short, not a file that
			// names fewer labels.
			name:       "label list with no newline at the end",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: strings.TrimSuffix(nodeLabels, "\n"), deploy: cleanDeploy},
			want:       refuseLabelShort,
		},
		{
			// Two tags in one ref: the file says it names our image and which one
			// is unreadable, so the ref is named for the operator to find.
			name:       "label naming a ref docker cannot read",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://fractals-nextjs:t1:t2\n", deploy: cleanDeploy},
			want:       fmt.Sprintf(refuseLabelRef, "fractals-nextjs:t1:t2"),
		},
		{
			name:       "label naming a ref with no digest",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://fractals-nextjs@\n", deploy: cleanDeploy},
			want:       fmt.Sprintf(refuseLabelRef, "fractals-nextjs@"),
		},
		{
			// Nothing says the pinned image is gone rather than read past, so the
			// pin is named and the plan refuses.
			name:       "label pinning a digest no image carries",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: "fractals-prod:docker://fractals-nextjs@" + registryDigest("gone") + "\n", deploy: cleanDeploy},
			want:       fmt.Sprintf(refuseUnpinned, registryDigest("gone")),
		},
		{
			name:       "daemon whose registry digests are unreadable",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, digestsErr: errors.New("SECRET stderr"), labels: pinning, deploy: cleanDeploy},
			want:       refuseDigestRead,
		},
		{
			name:       "daemon whose registry ref does not parse",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, digests: map[string][]string{imgID('1'): {"fractals-nextjs@nonsense"}}, labels: pinning, deploy: cleanDeploy},
			want:       refuseDigestRead,
		},
		{
			// The daemon answered for one image of the two it lists: a partial
			// read cannot show the pin's image is not the one it skipped.
			name:       "daemon answering for fewer images than it lists",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f: fakeDaemon{
				images: two,
				digestsRaw: fmt.Sprintf("%s %s\n", imgID('1'),
					`["`+fractalsRepo+`@`+registryDigest("t1")+`"]`),
				labels: pinning,
				deploy: cleanDeploy,
			},
			want: refuseDigestRead,
		},
		{
			name:       "no deploy workflow configured",
			labelsFile: labelsPath,
			deployFile: "",
			f:          fakeDaemon{images: good, labels: nodeLabels},
			want:       refuseNoDeploy,
		},
		{
			name:       "unreadable deploy workflow",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: nodeLabels, deployErr: errors.New("cat: SECRET: no such file")},
			want:       refuseDeployRead,
		},
		{
			name:       "empty deploy workflow",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: nodeLabels, deploy: "\n \n"},
			want:       refuseEmptyDeploy,
		},
		{
			name:       "deploy workflow with a templated tag",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: nodeLabels, deploy: "      image: fractals-nextjs:${{ github.ref_name }}\n"},
			want:       refuseDeployName,
		},
		{
			name:       "deploy workflow with a malformed digest",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: nodeLabels, deploy: "      image: fractals-nextjs@sha256:short\n"},
			want:       refuseDeployName,
		},
		{
			name:       "deploy workflow naming a ref docker cannot read",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{images: good, labels: nodeLabels, deploy: "      image: fractals-nextjs:t1:t2\n"},
			want:       fmt.Sprintf(refuseDeployRef, "fractals-nextjs:t1:t2"),
		},
		{
			name:       "unreadable image list",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f:          fakeDaemon{imagesErr: errors.New("SECRET stderr"), labels: nodeLabels, deploy: cleanDeploy},
			want:       dockerimg.ErrListImages.Error(),
		},
		{
			name:       "fractals tag with no creation time",
			labelsFile: labelsPath,
			deployFile: deployPath,
			f: fakeDaemon{images: imageLines(t,
				dockerimg.Image{Repository: fractalsRepo, Tag: "t1", ID: imgID('1'), Size: "10MB"}), labels: nodeLabels, deploy: cleanDeploy},
			want: refuseUndated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.f
			if f.digests == nil {
				// The daemon carries the good images' registry digests unless a
				// row is about the digest read itself.
				f.digests = goodDigests
			}
			plan, err := newPlanTarget(t, &f, tc.labelsFile, tc.deployFile).Plan(context.Background(), diskmaint.Policy{})
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

// TestPlanNamesABoundedRefInItsRefusal checks that a refused ref is named so the
// operator can find the line, and cut at refNameMax so one malformed line cannot
// put a whole file into a log.
func TestPlanNamesABoundedRefInItsRefusal(t *testing.T) {
	t.Parallel()
	cases := []string{
		"fractals-nextjs:t1:t2",
		"fractals-nextjs:t1:" + strings.Repeat("t", 200),
	}
	for _, ref := range cases {
		t.Run(fmt.Sprintf("%d bytes", len(ref)), func(t *testing.T) {
			t.Parallel()
			images, digests := daemon(t, fractals("t1", '1', 1, "10MB"))
			f := &fakeDaemon{
				images:  images,
				digests: digests,
				labels:  "fractals-prod:docker://" + ref + "\n",
				deploy:  cleanDeploy,
			}
			plan, err := newPlanTarget(t, f, labelsPath, deployPath).Plan(context.Background(), diskmaint.Policy{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			want := ref
			if len(want) > refNameMax {
				want = want[:refNameMax]
			}
			if !strings.Contains(plan.Refused, want) {
				t.Errorf("Refused = %q, want it to name the ref %q", plan.Refused, want)
			}
			if want != ref && strings.Contains(plan.Refused, ref) {
				t.Errorf("Refused = %q, want the ref cut at %d bytes", plan.Refused, refNameMax)
			}
		})
	}
}

func TestPlanKeepsEveryImageItDoesNotRemove(t *testing.T) {
	t.Parallel()
	images, digests := daemon(t,
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
	)
	f := &fakeDaemon{
		images:  images,
		digests: digests,
		labels:  "node:docker://code.forgejo.org/oci/node:lts\n",
		deploy:  cleanDeploy,
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
	images, digests := daemon(t,
		fractals("t6", '6', 6, "60MB"),
		fractals("t5", '5', 5, "50MB"),
		fractals("t4", '4', 4, "40MB"),
		fractals("t3", '3', 3, "30MB"),
		fractals("t2", '2', 2, "20MB"),
		fractals("t1", '1', 1, "10MB"),
		dockerimg.Image{Repository: "gt-cloud", Tag: "old", ID: imgID('1'), Size: "10MB", CreatedAt: at(1)},
	)
	f := &fakeDaemon{
		images:  images,
		digests: digests,
		labels:  "node:docker://code.forgejo.org/oci/node:lts\n",
		deploy:  cleanDeploy,
	}
	plan := planOf(t, f, diskmaint.Policy{})

	if got := removed(plan); len(got) != 0 {
		t.Errorf("removals = %v, want none", got)
	}
	if got, want := rules(plan)[fractalsRepo+":t1"], "shares an image a rule keeps"; got != want {
		t.Errorf("Kept[%s] = %q, want %q", fractalsRepo+":t1", got, want)
	}
}

// TestParseDeployReadsRefsGluedToPunctuation holds the deploy keep rule to the
// refs a workflow glues to JSON, quotes or a list (the rework of gt-p427gz): the
// tag is kept wherever its punctuation sits, and a templated tag still refuses.
func TestParseDeployReadsRefsGluedToPunctuation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		tag  string
	}{
		{"a ref in a JSON object", `{"image":"fractals-nextjs:t7"}` + "\n", "t7"},
		{"a ref in a quoted list", `images: ['reg.example/team/fractals-nextjs:t8', "other:x"]` + "\n", "t8"},
		{"a ref after a long flag and a comma", `--image=fractals-nextjs:t9,` + "\n", "t9"},
		{"a ref in a shell assignment with a semicolon", `IMAGE=fractals-nextjs:t10; make deploy` + "\n", "t10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, why := parseDeploy(tc.text)
			if why != "" {
				t.Fatalf("parseDeploy refused: %s", why)
			}
			if !got.tags[tc.tag] {
				t.Errorf("tags = %v, want %q kept", got.tags, tc.tag)
			}
		})
	}
	if _, why := parseDeploy("image: fractals-nextjs:${{ github.sha }}\n"); why != refuseDeployName {
		t.Errorf("a templated tag: refusal = %q, want %q", why, refuseDeployName)
	}
}

// TestParseLabelsRefusesWhatItCannotUse pins the two label-file edges om raised
// in the third review of gt-p427gz: a registry with a port and no tag is refused,
// not skipped, and a file with only comments is an empty list, which refuses.
func TestParseLabelsRefusesWhatItCannotUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want string
	}{
		{"a registry with a port and no tag", "ubuntu:docker://registry.example:5000/team/fractals-nextjs\n", refuseLabelLine},
		{"a file of comments only", "# runner labels\n# none yet\n", refuseEmptyLabels},
		{"an empty file", "", refuseEmptyLabels},
		{"a file cut short", "ci-base:docker://fractals-nextjs:t1", refuseLabelShort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, why := parseLabels(tc.text); why != tc.want {
				t.Errorf("parseLabels refusal = %q, want %q", why, tc.want)
			}
		})
	}
}
