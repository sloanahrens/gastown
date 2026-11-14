package imagepublish_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ci/imagepublish"
)

const (
	// testSHA is the green commit both images must carry.
	testSHA = "0123456789abcdef0123456789abcdef01234567"
	// testTag is its twelve-character tag.
	testTag = "0123456789ab"
	// otherSHA is a commit neither image may carry.
	otherSHA = "fedcba9876543210fedcba9876543210fedcba98"
)

var (
	daemonDigest  = "sha256:" + strings.Repeat("1a", 32)
	polecatDigest = "sha256:" + strings.Repeat("2b", 32)
	bdDigest      = "sha256:" + strings.Repeat("3c", 32)
)

// bdName is the bd gateway image's name, passed as Options.Bd on a commit that
// built one.
const bdName = "gastown-bd"

// daemonBase and polecatBase are the images' repository names without a tag.
func daemonBase() string  { return "forgejo:3000/example-owner/gastown-daemon" }
func polecatBase() string { return "forgejo:3000/example-owner/gastown-polecat" }
func bdBase() string      { return "forgejo:3000/example-owner/" + bdName }

// daemonRef and polecatRef are the tagged references the job pushes.
func daemonRef() string  { return daemonBase() + ":" + testTag }
func polecatRef() string { return polecatBase() + ":" + testTag }

// bdRef is the bd image's tagged reference, pushed after the other two.
func bdRef() string { return bdBase() + ":" + testTag }

// call is one recorded docker invocation.
type call struct {
	name string
	args []string
}

// last is the call's final argument: the image reference every docker call
// here ends with.
func (c call) last() string {
	if len(c.args) == 0 {
		return ""
	}
	return c.args[len(c.args)-1]
}

// recorder answers docker calls through the test's script and keeps every
// call, answered or not.
type recorder struct {
	calls []call
	reply func(call) imagepublish.Result
}

func (r *recorder) run(c imagepublish.Command) imagepublish.Result {
	call := call{name: c.Name, args: append([]string(nil), c.Args...)}
	r.calls = append(r.calls, call)
	if r.reply == nil {
		return imagepublish.Result{}
	}
	return r.reply(call)
}

// testOptions is a complete Options at the job's fixture values, with Docker
// left empty so every test also holds the "docker" default.
func testOptions(t *testing.T) (imagepublish.Options, *recorder, *strings.Builder, *strings.Builder) {
	t.Helper()
	rec := &recorder{}
	out := &strings.Builder{}
	errs := &strings.Builder{}
	o := imagepublish.Options{
		Registry: "forgejo:3000",
		Owner:    "example-owner",
		Daemon:   "gastown-daemon",
		Polecat:  "gastown-polecat",
		Tag:      testTag,
		SHA:      testSHA,
		Run:      rec.run,
		Out:      out,
		Err:      errs,
	}
	return o, rec, out, errs
}

// digestReply answers the three docker calls a push makes for each image, with
// the digest the registry lists for that image's repository.
func digestReply(t *testing.T, digests map[string]string) func(call) imagepublish.Result {
	t.Helper()
	return func(c call) imagepublish.Result {
		switch {
		case slices.Contains([]string{"push", "pull"}, c.args[0]):
			return imagepublish.Result{}
		case c.args[0] == "image":
			base, ok := strings.CutSuffix(c.last(), ":"+testTag)
			if !ok {
				t.Fatalf("docker image inspect %s: no tag %s", c.last(), testTag)
			}
			digest, ok := digests[base]
			if !ok {
				t.Fatalf("docker image inspect %s: no digest in the test's table", c.last())
			}
			return imagepublish.Result{Stdout: base + "@" + digest + "\n"}
		}
		t.Fatalf("unexpected docker call %v", c.args)
		return imagepublish.Result{}
	}
}

// labelReply answers the label read with the commit each image carries.
func labelReply(t *testing.T, labels map[string]string) func(call) imagepublish.Result {
	t.Helper()
	return func(c call) imagepublish.Result {
		if c.args[0] != "image" {
			t.Fatalf("unexpected docker call %v", c.args)
		}
		label, ok := labels[c.last()]
		if !ok {
			t.Fatalf("docker image inspect %s: no label in the test's table", c.last())
		}
		return imagepublish.Result{Stdout: label}
	}
}

// TestPushPrintsTheRegistryDigestForEachImage holds the output rule: each image
// is pushed, its registry digest read back, and only then its IMAGE line
// printed.
func TestPushPrintsTheRegistryDigestForEachImage(t *testing.T) {
	t.Parallel()
	o, rec, out, errs := testOptions(t)
	rec.reply = digestReply(t, map[string]string{
		daemonBase():  daemonDigest,
		polecatBase(): polecatDigest,
	})

	if err := imagepublish.Push(o); err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := "IMAGE daemon " + testTag + " " + daemonDigest + "\n" +
		"IMAGE polecat " + testTag + " " + polecatDigest + "\n" +
		"NOIMAGE bd " + testTag + "\n"
	if got := out.String(); got != want {
		t.Errorf("Push printed %q, want %q", got, want)
	}
	if errs.Len() != 0 {
		t.Errorf("Push wrote %q to stderr", errs.String())
	}
	wantCalls := []string{
		"push " + daemonRef(),
		"pull " + daemonRef(),
		"image " + daemonRef(),
		"push " + polecatRef(),
		"pull " + polecatRef(),
		"image " + polecatRef(),
	}
	var gotCalls []string
	for _, c := range rec.calls {
		if c.name != "docker" {
			t.Errorf("the push ran %q, want docker", c.name)
		}
		gotCalls = append(gotCalls, c.args[0]+" "+c.last())
	}
	if !slices.Equal(gotCalls, wantCalls) {
		t.Errorf("the push made the calls %q, want %q", gotCalls, wantCalls)
	}
}

// TestPushRefusesWhenTheRegistryListsNoSingleDigest holds the digest read's
// refusal: the registry must answer exactly one sha256 digest for the image.
func TestPushRefusesWhenTheRegistryListsNoSingleDigest(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, digestOutput, want string }{
		{"no digest", "", "listed 0 digests"},
		{"two digests", daemonBase() + "@" + daemonDigest + "\n" + daemonBase() + "@" + polecatDigest + "\n", "listed 2 digests"},
		{"another repository's digest", "forgejo:3000/example-owner/other@" + daemonDigest + "\n", "listed 0 digests"},
		{"a digest that is not sha256 hex", daemonBase() + "@sha256:xyz\n", "is not sha256:<64 lowercase hex>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, rec, out, _ := testOptions(t)
			rec.reply = func(c call) imagepublish.Result {
				if c.args[0] == "image" {
					return imagepublish.Result{Stdout: tc.digestOutput}
				}
				return imagepublish.Result{}
			}

			err := imagepublish.Push(o)
			if err == nil {
				t.Fatal("Push accepted a registry answer with no single digest")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Push error = %q, want it to contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), daemonBase()) {
				t.Errorf("Push error = %q, want it to name %s", err, daemonBase())
			}
			if out.Len() != 0 {
				t.Errorf("Push printed %q for an image whose digest it could not read", out.String())
			}
		})
	}
}

// TestPushStopsAtTheFirstFailedCommand holds the failure rule: a failed docker
// call fails the step, and no IMAGE line follows it.
func TestPushStopsAtTheFirstFailedCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		fail      func(call) bool
		wantCalls int
		wantOut   string
	}{
		{
			name:      "the daemon push",
			fail:      func(c call) bool { return c.args[0] == "push" && c.last() == daemonRef() },
			wantCalls: 1,
		},
		{
			name:      "the daemon digest pull",
			fail:      func(c call) bool { return c.args[0] == "pull" && c.last() == daemonRef() },
			wantCalls: 2,
		},
		{
			name:      "the daemon digest read",
			fail:      func(c call) bool { return c.args[0] == "image" && c.last() == daemonRef() },
			wantCalls: 3,
		},
		{
			name:      "the polecat push",
			fail:      func(c call) bool { return c.args[0] == "push" && c.last() == polecatRef() },
			wantCalls: 4,
			wantOut:   "IMAGE daemon " + testTag + " " + daemonDigest + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, rec, out, errs := testOptions(t)
			ok := digestReply(t, map[string]string{
				daemonBase():  daemonDigest,
				polecatBase(): polecatDigest,
			})
			rec.reply = func(c call) imagepublish.Result {
				if tc.fail(c) {
					return imagepublish.Result{Stderr: "boom", Exit: 1}
				}
				return ok(c)
			}

			err := imagepublish.Push(o)
			if err == nil {
				t.Fatalf("Push passed a failed call (%s)", tc.name)
			}
			if strings.Contains(err.Error(), "boom") {
				t.Errorf("Push error = %q, which carries captured output", err)
			}
			if !strings.Contains(errs.String(), "boom") {
				t.Errorf("Push did not relay the failed command's stderr; got %q", errs.String())
			}
			if len(rec.calls) != tc.wantCalls {
				t.Errorf("the failed push made %d calls, want %d", len(rec.calls), tc.wantCalls)
			}
			if got := out.String(); got != tc.wantOut {
				t.Errorf("Push printed %q, want %q", got, tc.wantOut)
			}
		})
	}
}

// TestPushAndCheckRefuseAnIncompleteConfiguration holds the fail-closed rule:
// a step refuses an unwired value instead of reporting a success it did not
// establish.
func TestPushAndCheckRefuseAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	fields := []struct {
		name  string
		clear func(*imagepublish.Options)
		want  string
	}{
		{"a registry", func(o *imagepublish.Options) { o.Registry = "" }, "registry is required"},
		{"an owner", func(o *imagepublish.Options) { o.Owner = "" }, "owner is required"},
		{"a daemon image", func(o *imagepublish.Options) { o.Daemon = "" }, "daemon is required"},
		{"a polecat image", func(o *imagepublish.Options) { o.Polecat = "" }, "polecat is required"},
		{"a tag", func(o *imagepublish.Options) { o.Tag = "" }, "tag is required"},
		{"a sha", func(o *imagepublish.Options) { o.SHA = "" }, "sha is required"},
		{"a runner", func(o *imagepublish.Options) { o.Run = nil }, "no runner is wired"},
		{"an output", func(o *imagepublish.Options) { o.Out = nil }, "no output is wired"},
	}
	steps := []struct {
		name string
		call func(imagepublish.Options) error
	}{
		{"Push", imagepublish.Push},
		{"Check", imagepublish.Check},
	}
	for _, step := range steps {
		for _, field := range fields {
			t.Run(step.name+" without "+field.name, func(t *testing.T) {
				t.Parallel()
				o, rec, out, _ := testOptions(t)
				field.clear(&o)

				err := step.call(o)
				if err == nil {
					t.Fatalf("%s accepted an option without %s", step.name, field.name)
				}
				if !strings.Contains(err.Error(), field.want) {
					t.Errorf("%s error = %q, want it to contain %q", step.name, err, field.want)
				}
				if len(rec.calls) != 0 {
					t.Errorf("%s ran %d docker commands before refusing", step.name, len(rec.calls))
				}
				if out.Len() != 0 {
					t.Errorf("%s printed %q before refusing", step.name, out.String())
				}
			})
		}
	}
}

// TestCheckAcceptsBothImagesAtTheGreenCommit holds the passing branch: both
// images carry the one commit.
func TestCheckAcceptsBothImagesAtTheGreenCommit(t *testing.T) {
	t.Parallel()
	o, rec, out, _ := testOptions(t)
	rec.reply = labelReply(t, map[string]string{
		daemonRef():  testSHA,
		polecatRef(): testSHA,
	})

	if err := imagepublish.Check(o); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("Check printed %q", out.String())
	}
	if len(rec.calls) != 2 {
		t.Fatalf("Check made %d label reads, want one per image", len(rec.calls))
	}
	for i, want := range []string{daemonRef(), polecatRef()} {
		if got := rec.calls[i].last(); got != want {
			t.Errorf("label read %d is of %s, want %s", i, got, want)
		}
	}
}

// TestCheckReportsEveryImageOffTheCommit holds the difference rule: a label
// that differs from the green commit fails the step, naming the image and the
// commit it carries.
func TestCheckReportsEveryImageOffTheCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		labels     map[string]string
		wantNamed  []string
		wantAbsent []string
	}{
		{
			name:       "the daemon image",
			labels:     map[string]string{daemonRef(): otherSHA, polecatRef(): testSHA},
			wantNamed:  []string{daemonRef(), otherSHA},
			wantAbsent: []string{polecatRef()},
		},
		{
			name:       "the polecat image",
			labels:     map[string]string{daemonRef(): testSHA, polecatRef(): otherSHA},
			wantNamed:  []string{polecatRef(), otherSHA},
			wantAbsent: []string{daemonRef()},
		},
		{
			name:      "both images",
			labels:    map[string]string{daemonRef(): otherSHA, polecatRef(): otherSHA},
			wantNamed: []string{daemonRef(), polecatRef()},
		},
		{
			name:       "an image with no label",
			labels:     map[string]string{daemonRef(): "", polecatRef(): testSHA},
			wantNamed:  []string{daemonRef(), "org.gastown.gt-commit=\"\""},
			wantAbsent: []string{polecatRef()},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, rec, out, _ := testOptions(t)
			rec.reply = labelReply(t, tc.labels)

			err := imagepublish.Check(o)
			if err == nil {
				t.Fatalf("Check passed with %s off the green commit", tc.name)
			}
			for _, want := range tc.wantNamed {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Check error = %q, want it to name %q", err, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("Check error = %q, want it not to name %q", err, absent)
				}
			}
			if len(rec.calls) != 2 {
				t.Errorf("Check made %d label reads, want one per image even after a mismatch", len(rec.calls))
			}
			if out.Len() != 0 {
				t.Errorf("Check printed %q", out.String())
			}
		})
	}
}

// TestCheckStopsWhenALabelCannotBeRead holds the refusal branch: an image whose
// label cannot be read fails the step before its label is compared.
func TestCheckStopsWhenALabelCannotBeRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		fail      func(call) bool
		wantCalls int
	}{
		{"the daemon label", func(c call) bool { return c.last() == daemonRef() }, 1},
		{"the polecat label", func(c call) bool { return c.last() == polecatRef() }, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, rec, _, errs := testOptions(t)
			ok := labelReply(t, map[string]string{daemonRef(): testSHA, polecatRef(): testSHA})
			rec.reply = func(c call) imagepublish.Result {
				if tc.fail(c) {
					return imagepublish.Result{Stderr: "boom", Exit: 1}
				}
				return ok(c)
			}

			err := imagepublish.Check(o)
			if err == nil {
				t.Fatalf("Check passed when it could not read %s", tc.name)
			}
			if strings.Contains(err.Error(), "boom") {
				t.Errorf("Check error = %q, which carries captured output", err)
			}
			if !strings.Contains(errs.String(), "boom") {
				t.Errorf("Check did not relay the failed command's stderr; got %q", errs.String())
			}
			if len(rec.calls) != tc.wantCalls {
				t.Errorf("Check made %d calls, want %d", len(rec.calls), tc.wantCalls)
			}
		})
	}
}

// TestPublishThreeImages holds the three-image commit: the bd image is pushed
// after the other two, its registry digest is printed as theirs is, and Check
// reads the commit label back from all three.
func TestPublishThreeImages(t *testing.T) {
	t.Parallel()
	o, rec, out, errs := testOptions(t)
	o.Bd = bdName
	rec.reply = digestReply(t, map[string]string{
		daemonBase():  daemonDigest,
		polecatBase(): polecatDigest,
		bdBase():      bdDigest,
	})

	if err := imagepublish.Push(o); err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := "IMAGE daemon " + testTag + " " + daemonDigest + "\n" +
		"IMAGE polecat " + testTag + " " + polecatDigest + "\n" +
		"IMAGE bd " + testTag + " " + bdDigest + "\n"
	if got := out.String(); got != want {
		t.Errorf("Push printed %q, want %q", got, want)
	}
	if errs.Len() != 0 {
		t.Errorf("Push wrote %q to stderr", errs.String())
	}
	wantCalls := []string{
		"push " + daemonRef(),
		"pull " + daemonRef(),
		"image " + daemonRef(),
		"push " + polecatRef(),
		"pull " + polecatRef(),
		"image " + polecatRef(),
		"push " + bdRef(),
		"pull " + bdRef(),
		"image " + bdRef(),
	}
	var gotCalls []string
	for _, c := range rec.calls {
		gotCalls = append(gotCalls, c.args[0]+" "+c.last())
	}
	if !slices.Equal(gotCalls, wantCalls) {
		t.Errorf("the push made the calls %q, want %q", gotCalls, wantCalls)
	}

	rec.calls = nil
	rec.reply = labelReply(t, map[string]string{
		daemonRef():  testSHA,
		polecatRef(): testSHA,
		bdRef():      testSHA,
	})
	if err := imagepublish.Check(o); err != nil {
		t.Fatalf("Check: %v", err)
	}
	wantLabels := []string{daemonRef(), polecatRef(), bdRef()}
	if len(rec.calls) != len(wantLabels) {
		t.Fatalf("Check made %d label reads, want one per image", len(rec.calls))
	}
	for i, want := range wantLabels {
		if got := rec.calls[i].last(); got != want {
			t.Errorf("label read %d is of %s, want %s", i, got, want)
		}
	}
}

// TestPublishWithoutBd holds the commit that built no bd image: the two images
// publish as before, the absence is reported on the output, and no error comes
// back from Push or Check.
func TestPublishWithoutBd(t *testing.T) {
	t.Parallel()
	o, rec, out, errs := testOptions(t)
	rec.reply = digestReply(t, map[string]string{
		daemonBase():  daemonDigest,
		polecatBase(): polecatDigest,
	})

	if err := imagepublish.Push(o); err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := "IMAGE daemon " + testTag + " " + daemonDigest + "\n" +
		"IMAGE polecat " + testTag + " " + polecatDigest + "\n" +
		"NOIMAGE bd " + testTag + "\n"
	if got := out.String(); got != want {
		t.Errorf("Push printed %q, want %q", got, want)
	}
	if errs.Len() != 0 {
		t.Errorf("Push wrote %q to stderr", errs.String())
	}
	for _, c := range rec.calls {
		if strings.HasPrefix(c.last(), bdBase()+":") {
			t.Errorf("Push made a %v call for the bd image, which the commit did not build", c.args)
		}
	}

	rec.calls = nil
	rec.reply = labelReply(t, map[string]string{
		daemonRef():  testSHA,
		polecatRef(): testSHA,
	})
	if err := imagepublish.Check(o); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(rec.calls) != 2 {
		t.Errorf("Check made %d label reads, want one per built image", len(rec.calls))
	}
}

// answerImage answers every image inspect with stdout and every push or pull
// with nothing, so a test controls only the registry read-back.
func answerImage(stdout string) func(call) imagepublish.Result {
	return func(c call) imagepublish.Result {
		if c.args[0] == "image" {
			return imagepublish.Result{Stdout: stdout}
		}
		return imagepublish.Result{}
	}
}

// TestPublishBdImageChecks holds the bd image to the two checks the other images
// get: its registry read-back must list exactly one sha256 digest, and its commit
// label must be the green commit. Either failure fails the publish and names the
// bd image.
func TestPublishBdImageChecks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// push selects Push, with bd answering the push's docker calls; otherwise
		// Check runs with the bd image's commit label set to label.
		push  bool
		bd    func(call) imagepublish.Result
		label string
		want  []string
	}{
		{
			name: "a registry read-back with no digest",
			push: true,
			bd:   answerImage(""),
			want: []string{bdBase(), "listed 0 digests"},
		},
		{
			name: "a registry read-back with two digests",
			push: true,
			bd:   answerImage(bdBase() + "@" + bdDigest + "\n" + bdBase() + "@" + polecatDigest + "\n"),
			want: []string{bdBase(), "listed 2 digests"},
		},
		{
			name: "a registry read-back that is not sha256 hex",
			push: true,
			bd:   answerImage(bdBase() + "@sha256:xyz\n"),
			want: []string{bdBase(), "is not sha256:<64 lowercase hex>"},
		},
		{
			name:  "a commit label off the green commit",
			label: otherSHA,
			want:  []string{bdRef(), otherSHA},
		},
		{
			name:  "a commit label that is empty",
			label: "",
			want:  []string{bdRef(), `org.gastown.gt-commit=""`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, rec, out, _ := testOptions(t)
			o.Bd = bdName

			var err error
			if tc.push {
				good := digestReply(t, map[string]string{
					daemonBase():  daemonDigest,
					polecatBase(): polecatDigest,
				})
				rec.reply = func(c call) imagepublish.Result {
					if strings.HasPrefix(c.last(), bdBase()+":") {
						return tc.bd(c)
					}
					return good(c)
				}
				err = imagepublish.Push(o)
				if strings.Contains(out.String(), "IMAGE bd") {
					t.Errorf("Push printed a bd IMAGE line for a read-back it refused: %q", out.String())
				}
			} else {
				rec.reply = labelReply(t, map[string]string{
					daemonRef():  testSHA,
					polecatRef(): testSHA,
					bdRef():      tc.label,
				})
				err = imagepublish.Check(o)
			}
			if err == nil {
				t.Fatalf("the step passed with %s", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
