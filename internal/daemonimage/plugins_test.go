package daemonimage

import (
	"context"
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
