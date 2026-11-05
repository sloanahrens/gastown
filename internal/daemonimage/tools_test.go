package daemonimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testHMCommit     = "abcdef0123456789abcdef0123456789abcdef01"
	testDevopsCommit = "1234567890abcdef1234567890abcdef12345678"
	// testOtherCommit is a commit neither checkout is pinned to.
	testOtherCommit = "9999999999999999999999999999999999999999"
)

// testDevopsTime is the committer time git reports for testDevopsCommit.
var testDevopsTime = time.Date(2026, 11, 5, 12, 30, 45, 0, time.UTC)

// checkout is one fake source checkout: the commits it holds, the commit its HEAD
// is at, whether its worktree has changes, and the time git reports for its
// commits. badTime makes that report something that is not a unix time. fail names
// the one read that errors for this checkout: HEAD, status or show.
type checkout struct {
	holds   []string
	head    string
	dirty   bool
	when    time.Time
	badTime bool
	fail    string
}

// toolsGit is a bdbuild.Git over checkouts keyed by directory. It answers the
// commands BuildTools and its builds run, records each worktree a build adds,
// and touches no file.
type toolsGit struct {
	checkouts map[string]checkout
	adds      []worktreeAdd
}

func (g *toolsGit) git(dir string, args ...string) ([]byte, error) {
	co, ok := g.checkouts[dir]
	if !ok {
		return nil, errors.New("fatal: not a git repository: " + dir)
	}
	switch {
	case args[0] == "worktree":
		if len(args) == 5 && args[1] == "add" {
			g.adds = append(g.adds, worktreeAdd{src: dir, dir: args[3], commit: args[4]})
		}
		return nil, nil
	case args[0] == "rev-parse" && args[1] == "--verify":
		rev := strings.TrimSuffix(args[2], "^{commit}")
		if rev == "HEAD" {
			if co.fail == "HEAD" {
				return nil, errors.New("fatal: HEAD is unreadable")
			}
			return []byte(co.head + "\n"), nil
		}
		if slices.Contains(co.holds, rev) {
			return []byte(rev + "\n"), nil
		}
		return nil, errors.New("fatal: bad revision '" + rev + "'")
	case args[0] == "status":
		if co.fail == "status" {
			return nil, errors.New("fatal: the index is unreadable")
		}
		if co.dirty {
			return []byte(" M internal/x.go\n"), nil
		}
		return nil, nil
	case args[0] == "show":
		if co.fail == "show" {
			return nil, errors.New("fatal: the commit is unreadable")
		}
		if co.badTime {
			return []byte("not-a-time\n"), nil
		}
		return []byte(strconv.FormatInt(co.when.Unix(), 10) + "\n"), nil
	}
	return nil, errors.New("fake git: unexpected " + strings.Join(args, " "))
}

// toolsBuilder is a Builder ready to build the tools: the hm and devops checkouts
// clean and at their pins, the output directory out, over run and the fake git.
func toolsBuilder(t *testing.T) (Builder, *fakeRunner, *toolsGit) {
	t.Helper()
	run := &fakeRunner{}
	g := &toolsGit{checkouts: map[string]checkout{
		"/src/hm":     {holds: []string{testHMCommit}, head: testHMCommit},
		"/src/devops": {holds: []string{testDevopsCommit}, head: testDevopsCommit, when: testDevopsTime},
	}}
	return Builder{
		Run:          run.run,
		Git:          g.git,
		Environ:      testEnv,
		Out:          t.TempDir(),
		HMSrc:        "/src/hm",
		HMCommit:     testHMCommit,
		DevopsSrc:    "/src/devops",
		DevopsCommit: testDevopsCommit,
	}, run, g
}

// entriesIn lists the names in dir, or nothing when dir cannot be read, so a
// refusal that names no directory can still assert that it wrote nothing.
func entriesIn(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// argAfter returns the argument after flag in argv, or "" when flag is absent.
func argAfter(argv []string, flag string) string {
	for i := range argv {
		if argv[i] == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func TestDeployBuildsHMAndReporters(t *testing.T) {
	t.Parallel()
	b, run, g := toolsBuilder(t)
	paths, err := b.BuildTools(context.Background())
	if err != nil {
		t.Fatalf("BuildTools: %v", err)
	}
	want := []string{filepath.Join(b.Out, "hm"), filepath.Join(b.Out, "devops-reporter"), filepath.Join(b.Out, "release-reporter")}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("BuildTools paths = %q, want %q", paths, want)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("BuildTools returned %s, which is not written: %v", p, err)
		}
	}
	got := outEntries(t, b.Out)
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"devops-reporter", "hm", "release-reporter"}) {
		t.Errorf("the build context holds %v after a build, want the three tools only", got)
	}

	if len(run.calls) != 3 {
		t.Fatalf("ran %d builds, want 3", len(run.calls))
	}
	for i, pkg := range []string{HMPackage, DevopsReporterPackage, ReleaseReporterPackage} {
		c := run.calls[i]
		if got := c.argv[len(c.argv)-1]; got != pkg {
			t.Errorf("build %d compiles %q, want %q", i, got, pkg)
		}
		if !slices.Contains(c.env, "GOOS=linux") || !slices.Contains(c.env, "GOARCH=arm64") {
			t.Errorf("build %d env = %q, want the linux/arm64 cross-compile", i, c.env)
		}
	}
	if got := argAfter(run.calls[2].argv, "-ldflags"); got != releaseReporterVersionFlag+"v0.1.4-0.20261105123045-"+testDevopsCommit[:12] {
		t.Errorf("release-reporter ldflags = %q, want the pseudo-version stamp of the devops commit", got)
	}
	if got := argAfter(run.calls[0].argv, "-ldflags"); got != "" {
		t.Errorf("hm ldflags = %q, want none", got)
	}

	if len(g.adds) != 3 {
		t.Fatalf("made %d worktrees, want 3", len(g.adds))
	}
	wantAdds := []struct{ src, commit string }{
		{"/src/hm", testHMCommit},
		{"/src/devops", testDevopsCommit},
		{"/src/devops", testDevopsCommit},
	}
	for i, w := range wantAdds {
		if g.adds[i].src != w.src || g.adds[i].commit != w.commit {
			t.Errorf("worktree %d = %+v, want a worktree of %s at %s", i, g.adds[i], w.src, w.commit)
		}
	}
}

// TestDeployRefusesUnpinnedOrDirtyHMAndDevopsSources: each refusal names the flag
// the operator must fix, and runs nothing and writes nothing.
func TestDeployRefusesUnpinnedOrDirtyHMAndDevopsSources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder, *toolsGit)
		want    string
	}{
		{"no --hm-commit", func(b *Builder, _ *toolsGit) { b.HMCommit = "" }, "--hm-commit is required"},
		{"no --devops-commit", func(b *Builder, _ *toolsGit) { b.DevopsCommit = "" }, "--devops-commit is required"},
		{"no --hm-src", func(b *Builder, _ *toolsGit) { b.HMSrc = "" }, "--hm-src is required"},
		{"no --devops-src", func(b *Builder, _ *toolsGit) { b.DevopsSrc = "" }, "--devops-src is required"},
		{"an --hm-commit that is not hex", func(b *Builder, _ *toolsGit) { b.HMCommit = "main" }, "--hm-commit \"main\" is not a 7+ character hex commit"},
		{"an --hm-commit the checkout does not hold", func(b *Builder, g *toolsGit) {
			co := g.checkouts["/src/hm"]
			co.holds = nil
			g.checkouts["/src/hm"] = co
		}, "--hm-commit " + testHMCommit + " is not a commit of /src/hm"},
		{"an hm checkout not at --hm-commit", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/hm"]
			co.head = testOtherCommit
			g.checkouts["/src/hm"] = co
		}, "--hm-commit"},
		{"a devops checkout not at --devops-commit", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/devops"]
			co.head = testOtherCommit
			g.checkouts["/src/devops"] = co
		}, "--devops-commit"},
		{"a dirty hm checkout", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/hm"]
			co.dirty = true
			g.checkouts["/src/hm"] = co
		}, "--hm-src"},
		{"a dirty devops checkout", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/devops"]
			co.dirty = true
			g.checkouts["/src/devops"] = co
		}, "--devops-src"},
		{"the canonical clone as --devops-src", func(b *Builder, _ *toolsGit) { b.DevopsSrc = "/town/rig/mayor/rig" }, "canonical clone"},
		{"a devops commit git does not expand to a full sha", func(b *Builder, g *toolsGit) {
			b.DevopsCommit = "abcdef1"
			g.checkouts["/src/devops"] = checkout{holds: []string{"abcdef1"}, head: "abcdef1", when: testDevopsTime}
		}, "too short to stamp release-reporter"},
		{"no runner", func(b *Builder, _ *toolsGit) { b.Run = nil }, "no runner"},
		{"no git", func(b *Builder, _ *toolsGit) { b.Git = nil }, "no git to resolve"},
		{"no output directory", func(b *Builder, _ *toolsGit) { b.Out = "" }, "no output directory for the tools"},
		{"a relative output directory", func(b *Builder, _ *toolsGit) { b.Out = filepath.Join("rel", "out") }, "is not absolute"},
		{"an hm checkout whose HEAD cannot be read", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/hm"]
			co.fail = "HEAD"
			g.checkouts["/src/hm"] = co
		}, "cannot read HEAD of the hm checkout"},
		{"a devops checkout whose status cannot be read", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/devops"]
			co.fail = "status"
			g.checkouts["/src/devops"] = co
		}, "cannot read the status of the devops checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run, g := toolsBuilder(t)
			tc.breakIt(&b, g)
			paths, err := b.BuildTools(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildTools = %q, %v; want an error naming %q", paths, err, tc.want)
			}
			if len(run.calls) != 0 {
				t.Errorf("a refused build ran %d commands", len(run.calls))
			}
			if got := entriesIn(b.Out); len(got) != 0 {
				t.Errorf("a refused build left %v in the build context", got)
			}
		})
	}
}

// TestDeployRefusesAnAbsentBuildContext: the tools are staged inside the build
// context, so a context that does not exist stops the build before any compile.
func TestDeployRefusesAnAbsentBuildContext(t *testing.T) {
	t.Parallel()
	b, run, _ := toolsBuilder(t)
	b.Out = filepath.Join(t.TempDir(), "absent")
	_, err := b.BuildTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot stage the tools") {
		t.Fatalf("BuildTools error = %v, want one naming the staging failure", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("a build with no context ran %d commands", len(run.calls))
	}
}

// TestDeployNamesAToolItCannotMoveIntoTheContext: a path already holding a
// directory cannot take the binary, and the error names the tool.
func TestDeployNamesAToolItCannotMoveIntoTheContext(t *testing.T) {
	t.Parallel()
	b, _, _ := toolsBuilder(t)
	if err := os.MkdirAll(filepath.Join(b.Out, "hm", "stuck"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths, err := b.BuildTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot move hm into") {
		t.Fatalf("BuildTools = %q, %v; want an error naming hm's move", paths, err)
	}
	if paths != nil {
		t.Errorf("a failed move returned paths %q", paths)
	}
}

// TestDeployRefusesAnUnreadableDevopsCommitTime: the release-reporter stamp needs
// the commit's time, so a time git does not print as a unix time stops the build
// before anything runs.
func TestDeployRefusesAnUnreadableDevopsCommitTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*checkout)
		want string
	}{
		{"git prints something that is not a unix time", func(c *checkout) { c.badTime = true }, "not a unix time"},
		{"git cannot read the commit", func(c *checkout) { c.fail = "show" }, "cannot read the commit time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run, g := toolsBuilder(t)
			co := g.checkouts["/src/devops"]
			tc.edit(&co)
			g.checkouts["/src/devops"] = co
			_, err := b.BuildTools(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildTools error = %v, want one containing %q", err, tc.want)
			}
			if len(run.calls) != 0 {
				t.Errorf("a build with no commit time ran %d commands", len(run.calls))
			}
		})
	}
}

// TestDeployBuildFailureLeavesNothing: a build that fails returns its error and no
// paths, and leaves no tool in the build context.
func TestDeployBuildFailureLeavesNothing(t *testing.T) {
	t.Parallel()
	b, run, _ := toolsBuilder(t)
	run.failPkg = ReleaseReporterPackage
	paths, err := b.BuildTools(context.Background())
	if err == nil {
		t.Fatalf("BuildTools = %q, nil; want the failed build's error", paths)
	}
	if paths != nil {
		t.Errorf("a failed build returned paths %q", paths)
	}
	if got := outEntries(t, b.Out); len(got) != 0 {
		t.Errorf("a failed build left %v in the build context", got)
	}
}

// TestDockerfileCopiesHMAndReporters: the image copies each tool from the build
// context root to /usr/local/bin, once, with mode 0755.
func TestDockerfileCopiesHMAndReporters(t *testing.T) {
	t.Parallel()
	var got []string
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd != "COPY" {
			continue
		}
		_, src, _ := copyOperands(in.args)
		if slices.Contains(toolNames, src) {
			got = append(got, in.cmd+" "+strings.Join(in.args, " "))
		}
	}
	var want []string
	for _, name := range toolNames {
		want = append(want, "COPY --chmod=0755 "+name+" /usr/local/bin/"+name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s copies the tools as %q, want %q", DockerfilePath, got, want)
	}
}

// TestReleaseReporterVersionIsAcceptedByRunSh: the stamp has the vMAJOR.MINOR.PATCH[-pre]
// shape the release-reporter plugin's run.sh accepts, and its core is above MIN_VERSION.
// The shape is copied here because run.sh is not in this repo.
func TestReleaseReporterVersionIsAcceptedByRunSh(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?$`)
	v := releaseReporterVersion(testDevopsTime, testDevopsCommit)
	got := shape.FindStringSubmatch(v)
	if got == nil {
		t.Fatalf("the stamp %q is not vMAJOR.MINOR.PATCH[-pre]", v)
	}
	minVersion := shape.FindStringSubmatch(releaseReporterMinVersion)
	if minVersion == nil {
		t.Fatalf("MIN_VERSION %q is not vMAJOR.MINOR.PATCH[-pre]", releaseReporterMinVersion)
	}
	for i := 1; i <= 3; i++ {
		a, err := strconv.Atoi(got[i])
		if err != nil {
			t.Fatalf("the stamp's core %q: %v", got[i], err)
		}
		m, err := strconv.Atoi(minVersion[i])
		if err != nil {
			t.Fatalf("MIN_VERSION's core %q: %v", minVersion[i], err)
		}
		if a != m {
			if a < m {
				t.Errorf("the stamp %q is below MIN_VERSION %s", v, releaseReporterMinVersion)
			}
			return
		}
	}
	t.Errorf("the stamp %q has MIN_VERSION's core %s; it must sort above it", v, releaseReporterMinVersion)
}
