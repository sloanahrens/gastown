package main

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ci/imagepublish"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

const (
	green    = "0123456789abcdef0123456789abcdef01234567"
	tag      = "0123456789ab"
	registry = "forgejo:3000"
	owner    = "example-owner"
	daemon   = "gastown-daemon"
	polecat  = "gastown-polecat"
	bd       = "gastown-bd"
)

// digest is the registry digest the fake answers for every image.
var digest = "sha256:" + strings.Repeat("3c", 32)

// labelMarker and digestMarker tell the label read and the digest read apart by
// the text of their inspect format, so the test does not copy the package's
// format constants.
const (
	labelMarker  = "gt-commit"
	digestMarker = "RepoDigests"
)

// call is one recorded docker invocation.
type call struct {
	name string
	args []string
}

// fakeDocker answers each docker call the way a healthy registry would for the
// fixture images, and keeps every call it was asked for.
type fakeDocker struct {
	calls []call
	// label is what the label read answers; the default is the green commit.
	label string
	// pushFails makes every push exit non-zero.
	pushFails bool
}

func (f *fakeDocker) run(c imagepublish.Command) imagepublish.Result {
	f.calls = append(f.calls, call{name: c.Name, args: append([]string(nil), c.Args...)})
	switch {
	case c.Args[0] == "push":
		if f.pushFails {
			return imagepublish.Result{Exit: 1, Stderr: "push refused\n"}
		}
		return imagepublish.Result{}
	case c.Args[0] == "pull":
		return imagepublish.Result{}
	case strings.Contains(strings.Join(c.Args, " "), labelMarker):
		return imagepublish.Result{Stdout: f.labelOr() + "\n"}
	case strings.Contains(strings.Join(c.Args, " "), digestMarker):
		ref := c.Args[len(c.Args)-1]
		base := ref[:strings.LastIndex(ref, ":")]
		return imagepublish.Result{Stdout: base + "@" + digest + "\n"}
	}
	return imagepublish.Result{Exit: 2, Stderr: "unexpected call\n"}
}

func (f *fakeDocker) labelOr() string {
	if f.label != "" {
		return f.label
	}
	return green
}

// pushedRefs is every image reference a docker push was asked for.
func (f *fakeDocker) pushedRefs() []string {
	var refs []string
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "push" {
			refs = append(refs, c.args[len(c.args)-1])
		}
	}
	return refs
}

// flags is the command line for the fixture job; extra is appended, so a test
// names -bd or leaves it out.
func flags(extra ...string) []string {
	base := []string{
		"-registry", registry,
		"-owner", owner,
		"-daemon", daemon,
		"-polecat", polecat,
		"-tag", tag,
		"-sha", green,
	}
	return append(base, extra...)
}

// TestPushWithBdFlagPushesTheBdImage holds the -bd wiring on push: the named bd
// image is pushed beside the other two and its digest line is printed.
func TestPushWithBdFlagPushesTheBdImage(t *testing.T) {
	t.Parallel()
	fake := &fakeDocker{}
	var stdout, stderr bytes.Buffer

	code := run(append([]string{"push"}, flags("-bd", bd)...), &stdout, &stderr, fake.run)
	if code != 0 {
		t.Fatalf("push exited %d, stderr %q", code, stderr.String())
	}
	want := registry + "/" + owner + "/" + bd + ":" + tag
	if !slices.Contains(fake.pushedRefs(), want) {
		t.Errorf("pushed %v, want %s among them", fake.pushedRefs(), want)
	}
	if !strings.Contains(stdout.String(), "IMAGE bd "+tag+" "+digest) {
		t.Errorf("stdout %q has no IMAGE line for the bd image", stdout.String())
	}
}

// TestPushWithoutBdFlagBuiltNoBdImage holds the flag's default: left out, the
// commit is treated as having built no bd image, so nothing named bd is pushed
// and the NOIMAGE line is printed.
func TestPushWithoutBdFlagBuiltNoBdImage(t *testing.T) {
	t.Parallel()
	fake := &fakeDocker{}
	var stdout, stderr bytes.Buffer

	code := run(append([]string{"push"}, flags()...), &stdout, &stderr, fake.run)
	if code != 0 {
		t.Fatalf("push exited %d, stderr %q", code, stderr.String())
	}
	for _, ref := range fake.pushedRefs() {
		if strings.Contains(ref, "/"+bd+":") {
			t.Errorf("pushed %s without -bd", ref)
		}
	}
	if !strings.Contains(stdout.String(), "NOIMAGE bd "+tag) {
		t.Errorf("stdout %q has no NOIMAGE line for bd", stdout.String())
	}
}

// TestPushWithBdFlagStopsWhenAPushFails holds the refusal branch: a refused
// push exits non-zero and prints no IMAGE line, bd or otherwise.
func TestPushWithBdFlagStopsWhenAPushFails(t *testing.T) {
	t.Parallel()
	fake := &fakeDocker{pushFails: true}
	var stdout, stderr bytes.Buffer

	code := run(append([]string{"push"}, flags("-bd", bd)...), &stdout, &stderr, fake.run)
	if code == 0 {
		t.Fatalf("push passed with a refused push, stdout %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "IMAGE bd") {
		t.Errorf("stdout %q prints a bd digest it never read", stdout.String())
	}
}

// TestCheckWithBdFlagReadsTheBdLabel holds the -bd wiring on check: the bd
// image's commit label is read back, and a label off the green commit exits
// non-zero and names the bd reference.
func TestCheckWithBdFlagReadsTheBdLabel(t *testing.T) {
	t.Parallel()
	fake := &fakeDocker{label: "fedcba9876543210fedcba9876543210fedcba98"}
	var stdout, stderr bytes.Buffer

	code := run(append([]string{"check"}, flags("-bd", bd)...), &stdout, &stderr, fake.run)
	if code == 0 {
		t.Fatalf("check passed with a bd image off the commit, stdout %q", stdout.String())
	}
	want := registry + "/" + owner + "/" + bd + ":" + tag
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not name the bd reference %s", stderr.String(), want)
	}
}
