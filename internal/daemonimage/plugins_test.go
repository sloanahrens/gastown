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

// TestBuildPluginsBuildsEachPluginForLinuxArm64: every plugin is cross-compiled
// once from the gt checkout at the resolved rev, for linux/arm64 and without
// cgo, and its binary lands at plugins/<name>/<name> under the build context.
func TestBuildPluginsBuildsEachPluginForLinuxArm64(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	paths, err := b.BuildPlugins(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildPlugins: %v", err)
	}
	want := make([]string, 0, len(imagePlugins))
	for _, p := range imagePlugins {
		want = append(want, filepath.Join(b.Out, "plugins", p.name, p.name))
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("BuildPlugins paths = %q, want %q", paths, want)
	}
	if len(run.calls) != len(imagePlugins) {
		t.Fatalf("recorded %d builds, want one per plugin (%d)", len(run.calls), len(imagePlugins))
	}
	if len(run.adds) != len(imagePlugins) {
		t.Fatalf("made %d worktrees, want one per plugin (%d)", len(run.adds), len(imagePlugins))
	}
	for i, p := range imagePlugins {
		call := run.calls[i]
		for _, want := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "GOENV=off"} {
			if !contains(call.env, want) {
				t.Errorf("%s build env = %q, want %s among them", p.name, call.env, want)
			}
		}
		wantArgv := []string{"go", "build", "-trimpath", "-o", "OUT", p.pkg}
		if got := withOut(withoutLdflags(call.argv)); !reflect.DeepEqual(got, wantArgv) {
			t.Errorf("%s build argv = %q, want %q", p.name, call.argv, wantArgv)
		}
		if got := filepath.Base(outOf(call.argv)); got != p.name {
			t.Errorf("%s build writes %q, want a file named %s", p.name, outOf(call.argv), p.name)
		}
		if add := run.adds[i]; add.src != b.GTSrc || add.commit != testRev {
			t.Errorf("%s was built in a worktree of %s at %s, want the gt checkout at %s", p.name, add.src, add.commit, testRev)
		}
		if run.calls[i].dir != run.adds[i].dir {
			t.Errorf("%s ran in %q, want its worktree %q", p.name, run.calls[i].dir, run.adds[i].dir)
		}
		if info, err := os.Stat(paths[i]); err != nil || !info.Mode().IsRegular() {
			t.Errorf("BuildPlugins returned %s, which is not a file: %v", paths[i], err)
		}
	}
	if !reflect.DeepEqual(run.removed, []string{run.adds[0].dir, run.adds[1].dir}) {
		t.Errorf("removed worktrees %q, want every worktree the builds added", run.removed)
	}
}

// TestBuildPluginsLeavesNothingOnFailure: a plugin build that fails returns its
// error and no paths, and nothing is left in the build context even when an
// earlier plugin built.
func TestBuildPluginsLeavesNothingOnFailure(t *testing.T) {
	t.Parallel()
	for _, p := range imagePlugins {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			b, run := builderFor(t)
			run.failPkg = p.pkg
			paths, err := b.BuildPlugins(context.Background(), testRev)
			if err == nil {
				t.Fatalf("a failed %s build returned no error (paths %q)", p.name, paths)
			}
			if len(paths) != 0 {
				t.Errorf("a failed %s build listed %q, want no paths", p.name, paths)
			}
			if !strings.Contains(err.Error(), p.pkg) {
				t.Errorf("the error %q does not name the %s build", err, p.name)
			}
			entries, err := os.ReadDir(b.Out)
			if err != nil {
				t.Fatalf("reading the build context: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("a failed %s build left %d entries in the build context, want none", p.name, len(entries))
			}
		})
	}
}

// TestBuildPluginsMoveRollsBack: when a plugin cannot be placed after another
// was, the call puts the build context back as it found it instead of leaving a
// partial plugin set there.
func TestBuildPluginsMoveRollsBack(t *testing.T) {
	t.Parallel()
	first, second := imagePlugins[0], imagePlugins[1]
	// blockSecond puts a directory where the second plugin's binary goes, so its
	// rename into the build context fails after the first plugin's has succeeded.
	blockSecond := func(t *testing.T, b Builder) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(b.Out, "plugins", second.name, second.name), 0o755); err != nil {
			t.Fatalf("blocking the %s plugin: %v", second.name, err)
		}
	}
	refuse := func(t *testing.T, b Builder) {
		t.Helper()
		paths, err := b.BuildPlugins(context.Background(), testRev)
		if err == nil || !strings.Contains(err.Error(), "cannot move "+second.name) {
			t.Fatalf("BuildPlugins = %q, %v; want an error naming the %s move", paths, err, second.name)
		}
		if len(paths) != 0 {
			t.Errorf("a failed move listed %q, want no paths", paths)
		}
	}

	t.Run("the plugin placed before the failure is taken back out", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		blockSecond(t, b)
		refuse(t, b)
		dir := filepath.Join(b.Out, "plugins", first.name)
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the failed move left %s in the build context (stat: %v), want it taken back out", dir, err)
		}
	})

	t.Run("a previous build's binary is put back", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		binary := filepath.Join(b.Out, "plugins", first.name, first.name)
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			t.Fatalf("making the %s plugin directory: %v", first.name, err)
		}
		if err := os.WriteFile(binary, []byte("an earlier build"), 0o600); err != nil {
			t.Fatalf("writing the earlier %s binary: %v", first.name, err)
		}
		blockSecond(t, b)
		refuse(t, b)
		body, err := os.ReadFile(binary)
		if err != nil {
			t.Fatalf("reading the %s binary back: %v", first.name, err)
		}
		if got := string(body); got != "an earlier build" {
			t.Errorf("the %s binary reads %q after the failed move, want the earlier build's binary", first.name, got)
		}
	})
}

// TestBuildPluginsFailsWhenTheMoveCannotBeUndone drives every failure of the
// move and of its rollback through the filesystem seams: the call returns its
// error and no paths, and what it could not undo it names rather than dropping.
func TestBuildPluginsFailsWhenTheMoveCannotBeUndone(t *testing.T) {
	t.Parallel()
	first, second := imagePlugins[0], imagePlugins[1]
	firstPath := func(b Builder) string { return filepath.Join(b.Out, "plugins", first.name, first.name) }
	// blockSecond puts a directory where the second plugin's binary goes, so its
	// rename into the build context fails after the first plugin's succeeded.
	blockSecond := func(t *testing.T, b Builder) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(b.Out, "plugins", second.name, second.name), 0o755); err != nil {
			t.Fatalf("blocking the %s plugin: %v", second.name, err)
		}
	}
	// earlierBuild writes the first plugin's binary as a previous build left it,
	// the file a failed move has to put back.
	earlierBuild := func(t *testing.T, b Builder) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(firstPath(b)), 0o755); err != nil {
			t.Fatalf("making the %s plugin directory: %v", first.name, err)
		}
		if err := os.WriteFile(firstPath(b), []byte("an earlier build"), 0o600); err != nil {
			t.Fatalf("writing the earlier %s binary: %v", first.name, err)
		}
	}
	// renamesRefusedAt refuses the renames whose call numbers it is given and
	// performs the others, so a test can stop the move and its rollback at the
	// steps it names.
	renamesRefusedAt := func(ns ...int) func(string, string) error {
		refuse := make(map[int]bool, len(ns))
		for _, n := range ns {
			refuse[n] = true
		}
		calls := 0
		return func(oldpath, newpath string) error {
			calls++
			if refuse[calls] {
				return errors.New("rename refused")
			}
			return os.Rename(oldpath, newpath)
		}
	}
	wantEarlier := func(t *testing.T, b Builder) {
		t.Helper()
		body, err := os.ReadFile(firstPath(b))
		if err != nil {
			t.Fatalf("reading the %s binary back: %v", first.name, err)
		}
		if got := string(body); got != "an earlier build" {
			t.Errorf("the %s binary reads %q, want the earlier build's binary put back", first.name, got)
		}
	}
	refuse := func(t *testing.T, b Builder, want ...string) {
		t.Helper()
		paths, err := b.BuildPlugins(context.Background(), testRev)
		if err == nil {
			t.Fatalf("BuildPlugins = %q, nil; want an error", paths)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the error %q does not name %q", err, w)
			}
		}
		if len(paths) != 0 {
			t.Errorf("a failed move listed %q, want no paths", paths)
		}
	}

	t.Run("the file already at the path cannot be moved aside", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		earlierBuild(t, b)
		b.rename = func(oldpath, newpath string) error {
			if strings.Contains(newpath, "replaced") {
				return errors.New("rename refused")
			}
			return os.Rename(oldpath, newpath)
		}
		refuse(t, b, "cannot move the "+first.name+" already in", "rename refused")
		wantEarlier(t, b)
	})

	t.Run("the binary cannot be renamed into place after the file it replaces was set aside", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		earlierBuild(t, b)
		b.rename = renamesRefusedAt(2)
		refuse(t, b, "cannot move "+first.name+" into")
		wantEarlier(t, b)
	})

	t.Run("the file replaced cannot be put back", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		earlierBuild(t, b)
		b.rename = renamesRefusedAt(2, 3)
		refuse(t, b, "cannot move "+first.name+" into", "cannot undo the plugin move", "rename refused")
	})

	t.Run("a placed binary cannot be taken back out", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		blockSecond(t, b)
		b.remove = func(path string) error {
			if path == firstPath(b) {
				return errors.New("remove refused")
			}
			return os.Remove(path)
		}
		refuse(t, b, "cannot move "+second.name, "cannot undo the plugin move", "remove refused")
	})

	t.Run("a directory the move made cannot be removed", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		blockSecond(t, b)
		firstDir := filepath.Dir(firstPath(b))
		b.remove = func(path string) error {
			if path == firstDir {
				return errors.New("remove refused")
			}
			return os.Remove(path)
		}
		refuse(t, b, "cannot move "+second.name, "cannot undo the plugin move", "remove refused")
	})
}

// TestBuildPluginsRefusesBeforeAnyBuild covers every refusal: each one comes
// before the first build, so a refused call runs nothing and returns no paths.
func TestBuildPluginsRefusesBeforeAnyBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder)
		rev     string
		want    string
	}{
		{name: "no runner", breakIt: func(b *Builder) { b.Run = nil }, rev: testRev, want: "no runner to build the plugins with"},
		{name: "no git", breakIt: func(b *Builder) { b.Git = nil }, rev: testRev, want: "no git to resolve the commits with"},
		{name: "no output directory", breakIt: func(b *Builder) { b.Out = "" }, rev: testRev, want: "no output directory"},
		{name: "a relative output directory", breakIt: func(b *Builder) { b.Out = filepath.Join("rel", "out") }, rev: testRev, want: `output directory "rel/out" is not absolute`},
		{name: "no rev", breakIt: func(*Builder) {}, rev: " ", want: "no rev"},
		{name: "a rev that is an option", breakIt: func(*Builder) {}, rev: "--upload-pack=sh", want: `rev "--upload-pack=sh" is not`},
		{name: "a rev the gt checkout does not hold", breakIt: func(*Builder) {}, rev: "1234567", want: "does not hold commit 1234567"},
		{name: "no gt source", breakIt: func(b *Builder) { b.GTSrc = "" }, rev: testRev, want: "no gt source"},
		{name: "an output directory that does not exist", breakIt: func(b *Builder) { b.Out = filepath.Join(b.Out, "missing") }, rev: testRev, want: "cannot stage the plugins"},
		{name: "the canonical clone as the output directory", breakIt: func(b *Builder) { b.Out = filepath.Join(b.Out, "mayor", "rig") }, rev: testRev, want: "canonical clone"},
		{name: "the canonical clone as the gt source", breakIt: func(b *Builder) { b.GTSrc = "/src/mayor/rig/gt" }, rev: testRev, want: "canonical clone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{}
			b := validBuilder(run, t.TempDir())
			tc.breakIt(&b)
			paths, err := b.BuildPlugins(context.Background(), tc.rev)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildPlugins = %q, %v; want an error naming %q", paths, err, tc.want)
			}
			if len(paths) != 0 {
				t.Errorf("a refused build listed %q, want no paths", paths)
			}
			if len(run.calls) != 0 {
				t.Errorf("a refused build ran %d builds, want none", len(run.calls))
			}
		})
	}
}

// TestBuildPluginsFailsWhenAPluginCannotBeMoved: a built plugin that cannot be
// made room for in the build context, or cannot be renamed into it, fails the
// call and returns no paths.
func TestBuildPluginsFailsWhenAPluginCannotBeMoved(t *testing.T) {
	t.Parallel()
	first := imagePlugins[0]
	for _, tc := range []struct {
		name  string
		block func(t *testing.T, b Builder)
		want  string
	}{
		{
			name: "the plugin directory cannot be made",
			block: func(t *testing.T, b Builder) {
				if err := os.WriteFile(filepath.Join(b.Out, "plugins"), []byte("x"), 0o600); err != nil {
					t.Fatalf("writing the blocking file: %v", err)
				}
			},
			want: "cannot make the plugin directory",
		},
		{
			name: "the plugin binary cannot be renamed",
			block: func(t *testing.T, b Builder) {
				if err := os.MkdirAll(filepath.Join(b.Out, "plugins", first.name, first.name), 0o755); err != nil {
					t.Fatalf("making the blocking directory: %v", err)
				}
			},
			want: "cannot move " + first.name,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, _ := builderFor(t)
			tc.block(t, b)
			paths, err := b.BuildPlugins(context.Background(), testRev)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildPlugins = %q, %v; want an error naming %q", paths, err, tc.want)
			}
			if len(paths) != 0 {
				t.Errorf("a failed move listed %q, want no paths", paths)
			}
		})
	}
}
