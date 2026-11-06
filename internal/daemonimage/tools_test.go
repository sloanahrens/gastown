package daemonimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	testHMCommit     = "abcdef0123456789abcdef0123456789abcdef01"
	testDevopsCommit = "1234567890abcdef1234567890abcdef12345678"
	// testOtherCommit is a commit neither checkout is pinned to.
	testOtherCommit = "9999999999999999999999999999999999999999"
)

// testReleaseVersion is the version the go command stamps release-reporter with
// when it builds from testDevopsCommit: a pseudo-version past a tag.
var testReleaseVersion = "v0.1.4-0.20261105123045-" + testDevopsCommit[:12]

// checkout is one fake source checkout: the commits it holds, the commit its HEAD
// is at, and whether its worktree has changes. fail names the one read that errors
// for this checkout: HEAD or status.
type checkout struct {
	holds []string
	head  string
	dirty bool
	fail  string
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
	}
	return nil, errors.New("fake git: unexpected " + strings.Join(args, " "))
}

// versionGo is a Go runner whose go version -m prints info and returns no error.
func versionGo(info string) func(string, []string, ...string) ([]byte, error) {
	return func(string, []string, ...string) ([]byte, error) { return []byte(info), nil }
}

// versionInfo is go version -m output for a release-reporter binary whose main
// module is at version: the header, the package path, and the mod line that
// carries the version, as the go command prints it.
func versionInfo(version string) string {
	return "release-reporter: go1.27.1\n" +
		"\tpath\tgt.local/devops/cmd/release-reporter\n" +
		"\tmod\tgt.local/devops\t" + version + "\t\n" +
		"\tbuild\t-buildmode=exe\n" +
		"\tbuild\tvcs.modified=false\n"
}

// toolsBuilder is a Builder ready to build the tools: the hm and devops checkouts
// clean and at their pins, the output directory out, over run and the fake git.
// Its go reads back testReleaseVersion, a pseudo-version of testDevopsCommit.
func toolsBuilder(t *testing.T) (Builder, *fakeRunner, *toolsGit) {
	t.Helper()
	run := &fakeRunner{}
	g := &toolsGit{checkouts: map[string]checkout{
		"/src/hm":     {holds: []string{testHMCommit}, head: testHMCommit},
		"/src/devops": {holds: []string{testDevopsCommit}, head: testDevopsCommit},
	}}
	return Builder{
		Run:          run.run,
		Git:          g.git,
		Go:           versionGo(versionInfo(testReleaseVersion)),
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

// TestDeployRefusesUnpinnedOrDirtyHMAndDevopsSources: each refusal names the pin
// or the checkout the operator must fix, and runs nothing and writes nothing.
func TestDeployRefusesUnpinnedOrDirtyHMAndDevopsSources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder, *toolsGit)
		want    string
	}{
		{"no hm pin", func(b *Builder, _ *toolsGit) { b.HMCommit = "" }, "no hm pin"},
		{"no devops pin", func(b *Builder, _ *toolsGit) { b.DevopsCommit = "" }, "no devops pin"},
		{"no --hm-src", func(b *Builder, _ *toolsGit) { b.HMSrc = "" }, "--hm-src is required"},
		{"no --devops-src", func(b *Builder, _ *toolsGit) { b.DevopsSrc = "" }, "--devops-src is required"},
		{"a hm pin that is not hex", func(b *Builder, _ *toolsGit) { b.HMCommit = "main" }, "hm pin \"main\" is not a 7+ character hex commit"},
		{"a hm pin the checkout does not hold", func(b *Builder, g *toolsGit) {
			co := g.checkouts["/src/hm"]
			co.holds = nil
			g.checkouts["/src/hm"] = co
		}, "hm pin " + testHMCommit + " is not a commit of /src/hm"},
		{"an hm checkout not at its pin", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/hm"]
			co.head = testOtherCommit
			g.checkouts["/src/hm"] = co
		}, "not its pin"},
		{"a devops checkout not at its pin", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/devops"]
			co.head = testOtherCommit
			g.checkouts["/src/devops"] = co
		}, "not its pin"},
		{"a dirty hm checkout", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/hm"]
			co.dirty = true
			g.checkouts["/src/hm"] = co
		}, "uncommitted changes"},
		{"a dirty devops checkout", func(_ *Builder, g *toolsGit) {
			co := g.checkouts["/src/devops"]
			co.dirty = true
			g.checkouts["/src/devops"] = co
		}, "uncommitted changes"},
		{"the canonical clone as --devops-src", func(b *Builder, _ *toolsGit) { b.DevopsSrc = "/town/rig/mayor/rig" }, "canonical clone"},
		{"no runner", func(b *Builder, _ *toolsGit) { b.Run = nil }, "no runner"},
		{"no git", func(b *Builder, _ *toolsGit) { b.Git = nil }, "no git to resolve"},
		{"no go to read the version", func(b *Builder, _ *toolsGit) { b.Go = nil }, "no go to read the release-reporter version"},
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

// TestReleaseReporterBuildStampsNoLinkerVariable: release-reporter's version is the
// one the go command stamps from its checkout, so its compile carries no -X flag
// (a -X for a symbol the package lacks is dropped without an error) and does not
// turn VCS stamping off (which leaves the module version at (devel)).
func TestReleaseReporterBuildStampsNoLinkerVariable(t *testing.T) {
	t.Parallel()
	b, run, _ := toolsBuilder(t)
	if _, err := b.BuildTools(context.Background()); err != nil {
		t.Fatalf("BuildTools: %v", err)
	}
	if len(run.calls) != 3 {
		t.Fatalf("ran %d builds, want 3", len(run.calls))
	}
	argv := run.calls[2].argv
	if got := argv[len(argv)-1]; got != ReleaseReporterPackage {
		t.Fatalf("build 2 compiles %q, want %q", got, ReleaseReporterPackage)
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "-X") || strings.HasPrefix(a, "-buildvcs") {
			t.Errorf("release-reporter build argv %q carries %q: its version comes from the checkout, not a flag", argv, a)
		}
	}
}

// TestReleaseReporterVersionIsReadFromTheBinary: BuildTools reads the version the
// go command stamped into the release-reporter it built, with go version -m on that
// binary, and accepts only the vMAJOR.MINOR.PATCH[-pre] form run.sh accepts. A
// refusal names the binary and leaves the build context empty.
func TestReleaseReporterVersionIsReadFromTheBinary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		info   string
		goErr  bool
		refuse string
	}{
		{name: "a pseudo-version past a tag", info: versionInfo("v0.1.4-0.20261105123045-abcdef123456")},
		{name: "a tagged release", info: versionInfo("v0.1.3")},
		{name: "a pre-release", info: versionInfo("v1.2.3-rc.1")},
		{name: "an empty module version", info: versionInfo(""), refuse: "carries no main module version"},
		{name: "no mod line", info: "release-reporter: go1.27.1\n\tpath\tgt.local/devops/cmd/release-reporter\n", refuse: "carries no main module version"},
		{name: "(devel)", info: versionInfo("(devel)"), refuse: "is a (devel) build"},
		{name: "a dirty build", info: versionInfo("v0.1.4-0.20261105123045-abcdef123456+dirty"), refuse: "not vMAJOR.MINOR.PATCH[-pre]"},
		{name: "a version with no v", info: versionInfo("0.1.4"), refuse: "not vMAJOR.MINOR.PATCH[-pre]"},
		{name: "a version with no patch", info: versionInfo("v0.1"), refuse: "not vMAJOR.MINOR.PATCH[-pre]"},
		{name: "go version fails", goErr: true, refuse: "cannot read the version of release-reporter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run, _ := toolsBuilder(t)
			var readPath string
			b.Go = func(_ string, _ []string, args ...string) ([]byte, error) {
				readPath = args[len(args)-1]
				if tc.goErr {
					return nil, errors.New("go: not found")
				}
				return []byte(tc.info), nil
			}
			paths, err := b.BuildTools(context.Background())
			if built := argAfter(run.calls[2].argv, "-o"); readPath != built {
				t.Fatalf("go version -m read %q, want the binary the build wrote, %q", readPath, built)
			}
			if tc.refuse == "" {
				if err != nil {
					t.Fatalf("BuildTools: %v", err)
				}
				if len(paths) != 3 {
					t.Errorf("BuildTools returned %q, want the three tools", paths)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refuse) || !strings.Contains(err.Error(), "release-reporter") {
				t.Fatalf("BuildTools = %q, %v; want an error naming release-reporter and %q", paths, err, tc.refuse)
			}
			if paths != nil {
				t.Errorf("a refused version returned paths %q", paths)
			}
			if got := outEntries(t, b.Out); len(got) != 0 {
				t.Errorf("a refused version left %v in the build context", got)
			}
		})
	}
}

// The hm repo has no Go files at its root: its main package is cmd/hm. The other
// tests read HMPackage itself, so only this literal catches the constant being
// pointed back at the repository root (the first real image build failed with
// "no Go files" on it).
func TestHMPackageIsTheCmdHmMain(t *testing.T) {
	t.Parallel()
	if HMPackage != "./cmd/hm" {
		t.Fatalf("HMPackage = %q, want ./cmd/hm: the hm repo's main package", HMPackage)
	}
}
