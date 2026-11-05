package daemonimage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	testMangoCommit = "aabbccddeeff00112233445566778899aabbccdd"
	// testMangoShort is the abbreviation mangoBuilder's git resolves to
	// testMangoCommit, so a test sees the stamp carry the resolved commit.
	testMangoShort = "aabbccd"
)

// mangoBuilder is a Builder ready to build mango: the fake git holds the mango
// commit, Out is a fresh directory, and the runner writes the binary the
// container's compile leaves in the staging directory it mounts at /out.
func mangoBuilder(t *testing.T) (Builder, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{}
	b := validBuilder(run, t.TempDir())
	b.Git = run.git(map[string]string{testMangoCommit: testMangoCommit, testMangoShort: testMangoCommit})
	b.MangoSrc = filepath.Join("src", "mango")
	b.MangoCommit = testMangoShort
	b.Run = func(ctx context.Context, dir string, env, argv []string) error {
		if err := run.run(ctx, dir, env, argv); err != nil {
			return err
		}
		stage := stageOf(argv)
		if stage == "" {
			return errors.New("the mango docker run mounts no staging directory at /out")
		}
		return os.WriteFile(filepath.Join(stage, "mango"), []byte("mango binary"), 0o600)
	}
	return b, run
}

// stageOf returns the host directory a mango argv mounts at /out.
func stageOf(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-v" {
			if stage, ok := strings.CutSuffix(argv[i+1], ":/out"); ok {
				return stage
			}
		}
	}
	return ""
}

// TestBuildMangoCompilesInTheBuilderContainer: mango is compiled by one docker
// run of the builder image on the desktop-linux context, from a clean worktree
// of --mango-src at the resolved --mango-commit, with cgo and linux enabled and
// the VCS stamping off; the binary and the commit it was built from are
// published under Out/mango. The plan prints the same docker run and the same
// compile, so an operator reads the argv the build runs.
func TestBuildMangoCompilesInTheBuilderContainer(t *testing.T) {
	t.Parallel()
	b, run := mangoBuilder(t)
	paths, err := b.BuildMango(context.Background())
	if err != nil {
		t.Fatalf("BuildMango: %v", err)
	}
	want := []string{
		filepath.Join(b.Out, mangoDir, "mango"),
		filepath.Join(b.Out, mangoDir, "mango.sha"),
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("BuildMango paths = %q, want %q", paths, want)
	}
	if len(run.adds) != 1 {
		t.Fatalf("made %d worktrees, want the one mango worktree", len(run.adds))
	}
	add := run.adds[0]
	if add.src != b.MangoSrc || add.commit != testMangoCommit {
		t.Errorf("mango worktree = checkout %s at %s, want %s at the resolved %s", add.src, add.commit, b.MangoSrc, testMangoCommit)
	}
	if len(run.calls) != 1 {
		t.Fatalf("recorded %d commands, want the one docker run", len(run.calls))
	}
	call := run.calls[0]
	if call.dir != add.dir {
		t.Errorf("the docker run ran in %q, want the mango worktree %q", call.dir, add.dir)
	}
	if got, want := call.argv, mangoArgv(add.dir, stageOf(call.argv)); !reflect.DeepEqual(got, want) {
		t.Errorf("docker argv = %q, want %q", got, want)
	}
	for _, arg := range []string{"docker", "run", "--rm", "--platform", mangoPlatform, mangoBuildImage, "sh", "-c", add.dir + ":/src", stageOf(call.argv) + ":/out", "/src/" + mangoGoAPI} {
		if !contains(call.argv, arg) {
			t.Errorf("docker argv = %q, want %q among them", call.argv, arg)
		}
	}
	if got := call.argv[len(call.argv)-1]; got != mangoCompile {
		t.Errorf("the container runs %q, want %q", got, mangoCompile)
	}
	for _, want := range []string{"CGO_ENABLED=1", "GOOS=linux", "go build", "-buildvcs=false", "-o /out/mango", mangoPackage} {
		if !strings.Contains(mangoCompile, want) {
			t.Errorf("the mango compile %q lacks %q", mangoCompile, want)
		}
	}
	if i := indexOf(call.argv, "--context"); i < 0 || i+1 >= len(call.argv) || call.argv[i+1] != DockerContext {
		t.Errorf("docker argv = %q, want --context %s", call.argv, DockerContext)
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/builder"} {
		if !contains(call.env, want) {
			t.Errorf("docker env = %q, want %s carried through", call.env, want)
		}
	}
	for _, key := range []string{"GOFLAGS", "GOWORK"} {
		if hasKey(call.env, key) {
			t.Errorf("docker env = %q, want %s left out", call.env, key)
		}
	}
	stamp, err := os.ReadFile(filepath.Join(b.Out, mangoDir, "mango.sha"))
	if err != nil {
		t.Fatalf("reading the mango stamp: %v", err)
	}
	if got, want := string(stamp), testMangoCommit+"\n"; got != want {
		t.Errorf("mango.sha = %q, want the resolved commit %q", got, want)
	}
	if !reflect.DeepEqual(run.removed, []string{add.dir}) {
		t.Errorf("removed worktrees %q, want the mango worktree", run.removed)
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil || len(entries) != 1 || entries[0].Name() != mangoDir {
		t.Errorf("Out holds %v (err %v) after a mango build, want only %s", entries, err, mangoDir)
	}

	pb := planBuilder(t)
	lines, err := pb.Plan(testRev)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var mangoLine string
	for _, line := range lines {
		if strings.HasPrefix(line, "build mango: ") {
			mangoLine = line
		}
	}
	if mangoLine == "" {
		t.Fatalf("plan has no build mango: line:\n%s", strings.Join(lines, "\n"))
	}
	for _, want := range []string{
		"docker --context " + DockerContext + " run --rm --platform " + mangoPlatform,
		"-v " + planMangoWorktree + ":/src",
		"-v " + planMangoStage + ":/out",
		"-w /src/" + mangoGoAPI,
		mangoBuildImage,
		"CGO_ENABLED=1 GOOS=linux go build",
		"-buildvcs=false",
		"(clean worktree of /src/mango at " + testMangoCommit + ")",
	} {
		if !strings.Contains(mangoLine, want) {
			t.Errorf("the mango plan line = %q, want it to carry %q", mangoLine, want)
		}
	}
}

// TestBuildMangoFailureLeavesNoOutMango: a compile that fails returns its error
// and no paths, and leaves no mango directory and no staging directory behind.
func TestBuildMangoFailureLeavesNoOutMango(t *testing.T) {
	t.Parallel()
	b, run := mangoBuilder(t)
	b.Run = func(ctx context.Context, dir string, env, argv []string) error {
		_ = run.run(ctx, dir, env, argv)
		return errors.New("compile failed")
	}
	paths, err := b.BuildMango(context.Background())
	if err == nil || !strings.Contains(err.Error(), "building mango") {
		t.Fatalf("BuildMango = %q, %v; want an error naming the mango build", paths, err)
	}
	if paths != nil {
		t.Errorf("a failed mango build returned the paths %q", paths)
	}
	if _, err := os.Stat(filepath.Join(b.Out, mangoDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed mango build left %s (stat err %v)", filepath.Join(b.Out, mangoDir), err)
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil || len(entries) != 0 {
		t.Errorf("Out holds %v (err %v) after a failed mango build, want none", entries, err)
	}
}

// TestBuildMangoMissingBinaryLeavesNoOutMango: a container that exits zero
// without writing its binary is a refusal, not a published empty binary.
func TestBuildMangoMissingBinaryLeavesNoOutMango(t *testing.T) {
	t.Parallel()
	b, run := mangoBuilder(t)
	b.Run = run.run
	paths, err := b.BuildMango(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wrote no binary") {
		t.Fatalf("BuildMango = %q, %v; want an error naming the missing binary", paths, err)
	}
	if paths != nil {
		t.Errorf("a mango build with no binary returned the paths %q", paths)
	}
	if _, err := os.Stat(filepath.Join(b.Out, mangoDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a mango build with no binary left %s (stat err %v)", filepath.Join(b.Out, mangoDir), err)
	}
}

// TestBuildMangoFailedMoveKeepsThePreviousBuild: the directory is assembled in
// staging and swapped in last, so a move that fails puts back the directory a
// previous build published rather than leaving a half-published one.
func TestBuildMangoFailedMoveKeepsThePreviousBuild(t *testing.T) {
	t.Parallel()
	b, _ := mangoBuilder(t)
	prev := filepath.Join(b.Out, mangoDir)
	if err := os.Mkdir(prev, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"mango": "old binary", "mango.sha": "old stamp\n"} {
		if err := os.WriteFile(filepath.Join(prev, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	renames := 0
	b.rename = func(oldpath, newpath string) error {
		renames++
		// The third rename is the staged directory taking the published name.
		if renames == 3 {
			return errors.New("rename failed")
		}
		return os.Rename(oldpath, newpath)
	}
	if _, err := b.BuildMango(context.Background()); err == nil {
		t.Fatal("BuildMango with a failing final move returned no error")
	}
	for name, body := range map[string]string{"mango": "old binary", "mango.sha": "old stamp\n"} {
		got, err := os.ReadFile(filepath.Join(prev, name))
		if err != nil || string(got) != body {
			t.Errorf("the previous %s = %q (err %v), want %q put back", name, got, err, body)
		}
	}
}

// TestBuildMangoFailedMoveWithoutAPreviousBuildLeavesNothing: with nothing
// published before, a failed final move leaves no mango directory at all.
func TestBuildMangoFailedMoveWithoutAPreviousBuildLeavesNothing(t *testing.T) {
	t.Parallel()
	b, _ := mangoBuilder(t)
	renames := 0
	b.rename = func(oldpath, newpath string) error {
		renames++
		// The second rename takes the staged directory to its published name.
		if renames == 2 {
			return errors.New("rename failed")
		}
		return os.Rename(oldpath, newpath)
	}
	if _, err := b.BuildMango(context.Background()); err == nil {
		t.Fatal("BuildMango with a failing final move returned no error")
	}
	if _, err := os.Stat(filepath.Join(b.Out, mangoDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed mango move left %s (stat err %v)", filepath.Join(b.Out, mangoDir), err)
	}
}

// TestBuildMangoCannotStage: an output directory that cannot hold a staging
// directory is a refusal, not a build that silently writes nowhere.
func TestBuildMangoCannotStage(t *testing.T) {
	t.Parallel()
	b, _ := mangoBuilder(t)
	out := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(out, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b.Out = out
	paths, err := b.BuildMango(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot stage mango") {
		t.Fatalf("BuildMango = %q, %v; want an error naming the staging directory", paths, err)
	}
}

// TestBuildMangoCannotCheckOutTheCommit: a checkout git refuses to add a
// worktree to is a refusal, and leaves the output directory empty.
func TestBuildMangoCannotCheckOutTheCommit(t *testing.T) {
	t.Parallel()
	b, run := mangoBuilder(t)
	run.failAdd = true
	paths, err := b.BuildMango(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot check commit") {
		t.Fatalf("BuildMango = %q, %v; want an error naming the checkout", paths, err)
	}
	entries, readErr := os.ReadDir(b.Out)
	if readErr != nil || len(entries) != 0 {
		t.Errorf("Out holds %v (err %v) after a refused checkout, want none", entries, readErr)
	}
}

// TestBuildMangoWrittenThingIsNotARegularFile: a container that leaves a
// directory where the binary goes is a refusal, not a published mango.
func TestBuildMangoWrittenThingIsNotARegularFile(t *testing.T) {
	t.Parallel()
	b, run := mangoBuilder(t)
	b.Run = func(ctx context.Context, dir string, env, argv []string) error {
		if err := run.run(ctx, dir, env, argv); err != nil {
			return err
		}
		return os.Mkdir(filepath.Join(stageOf(argv), "mango"), 0o755)
	}
	paths, err := b.BuildMango(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("BuildMango = %q, %v; want an error naming the written directory", paths, err)
	}
	if _, statErr := os.Stat(filepath.Join(b.Out, mangoDir)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a refused mango build left %s (stat err %v)", filepath.Join(b.Out, mangoDir), statErr)
	}
}

// TestBuildMangoCannotPublish covers each step of the move into Out/mango: a
// failure returns its error and leaves no published mango behind.
func TestBuildMangoCannotPublish(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, b *Builder, run *fakeRunner)
		want string
	}{
		{
			name: "the staging directory name is taken",
			run: func(t *testing.T, b *Builder, run *fakeRunner) {
				b.Run = func(ctx context.Context, dir string, env, argv []string) error {
					if err := run.run(ctx, dir, env, argv); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(stageOf(argv), "mango"), []byte("mango binary"), 0o600); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(stageOf(argv), "mango-out"), nil, 0o600)
				}
			},
			want: "cannot make",
		},
		{
			name: "the binary cannot leave the staging directory",
			run: func(_ *testing.T, b *Builder, _ *fakeRunner) {
				b.rename = func(string, string) error { return errors.New("rename failed") }
			},
			want: "cannot move mango into",
		},
		{
			name: "the commit stamp cannot be written",
			run: func(_ *testing.T, b *Builder, _ *fakeRunner) {
				renames := 0
				b.rename = func(oldpath, newpath string) error {
					if err := os.Rename(oldpath, newpath); err != nil {
						return err
					}
					renames++
					if renames == 1 {
						return os.Mkdir(filepath.Join(filepath.Dir(newpath), "mango.sha"), 0o755)
					}
					return nil
				}
			},
			want: "cannot write the mango commit stamp",
		},
		{
			name: "the previous directory cannot be moved aside",
			run: func(t *testing.T, b *Builder, _ *fakeRunner) {
				if err := os.Mkdir(filepath.Join(b.Out, mangoDir), 0o755); err != nil {
					t.Fatal(err)
				}
				renames := 0
				b.rename = func(oldpath, newpath string) error {
					renames++
					if renames == 2 {
						return errors.New("rename failed")
					}
					return os.Rename(oldpath, newpath)
				}
			},
			want: "cannot move the mango directory already in",
		},
		{
			name: "the published directory cannot be read",
			run: func(_ *testing.T, b *Builder, _ *fakeRunner) {
				b.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("permission denied") }
			},
			want: "cannot read",
		},
		{
			name: "the previous directory cannot be put back",
			run: func(t *testing.T, b *Builder, _ *fakeRunner) {
				if err := os.Mkdir(filepath.Join(b.Out, mangoDir), 0o755); err != nil {
					t.Fatal(err)
				}
				renames := 0
				b.rename = func(oldpath, newpath string) error {
					renames++
					if renames >= 3 {
						return errors.New("rename failed")
					}
					return os.Rename(oldpath, newpath)
				}
			},
			want: "cannot put the previous mango directory back",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := mangoBuilder(t)
			tc.run(t, &b, run)
			paths, err := b.BuildMango(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildMango = %q, %v; want an error naming %q", paths, err, tc.want)
			}
			if paths != nil {
				t.Errorf("a failed publish returned the paths %q", paths)
			}
			if _, err := os.Stat(filepath.Join(b.Out, mangoDir, "mango")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a failed publish left %s (stat err %v)", filepath.Join(b.Out, mangoDir, "mango"), err)
			}
		})
	}
}

// TestBuildMangoRefusesBeforeItRuns covers every refusal: each comes before the
// worktree, the docker run and any write under Out.
func TestBuildMangoRefusesBeforeItRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder)
		want    string
	}{
		{"no runner", func(b *Builder) { b.Run = nil }, "no runner"},
		{"no git", func(b *Builder) { b.Git = nil }, "no git"},
		{"no output directory", func(b *Builder) { b.Out = "" }, "no output directory"},
		{"a relative output directory", func(b *Builder) { b.Out = filepath.Join("rel", "out") }, "is not absolute"},
		{"no mango source", func(b *Builder) { b.MangoSrc = "" }, "--mango-src is required"},
		{"no mango commit", func(b *Builder) { b.MangoCommit = "" }, "no mango commit"},
		{"a mango commit that is not hex", func(b *Builder) { b.MangoCommit = "main" }, "is not a 7+ character hex commit"},
		{"a canonical mango source", func(b *Builder) { b.MangoSrc = "/town/gastown/mayor/rig/mango" }, "canonical clone"},
		{"a canonical output directory", func(b *Builder) { b.Out = "/town/gastown/mayor/rig/out" }, "canonical clone"},
		{"a commit the checkout does not hold", func(b *Builder) { b.MangoCommit = "deadbeef" }, "does not hold commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := mangoBuilder(t)
			tc.breakIt(&b)
			paths, err := b.BuildMango(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildMango = %q, %v; want an error naming %q", paths, err, tc.want)
			}
			if paths != nil {
				t.Errorf("a refused mango build returned the paths %q", paths)
			}
			if len(run.calls) != 0 || len(run.adds) != 0 {
				t.Errorf("a refused mango build ran %d commands and made %d worktrees", len(run.calls), len(run.adds))
			}
		})
	}
}
