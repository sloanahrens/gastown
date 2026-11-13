package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// fakeProcess records the native call and answers with its fields.
type fakeProcess struct {
	ran  bool
	argv []string
	env  []string
	dir  string
	exit int
	err  error
}

func (f *fakeProcess) Run(argv []string, env []string, dir string) (int, error) {
	f.ran, f.argv, f.env, f.dir = true, argv, env, dir
	return f.exit, f.err
}

// nativeOm is the absolute path the tests give the native om. No file is written
// there: withNativeOm makes the harness's stat report it as an executable file.
const nativeOm = "/opt/om/bin/om"

// fakeFile is a file of the given mode, for a stat the test answers.
type fakeFile struct {
	fs.FileInfo
	mode fs.FileMode
}

func (f fakeFile) Mode() fs.FileMode { return f.mode }

// nativeBd is the absolute path the tests give the native bd, as nativeOm is for om.
const nativeBd = "/opt/bd/bin/bd"

// nativeGt is the absolute path the tests give the native gt, as nativeOm is for om.
const nativeGt = "/opt/gt/bin/gt"

// withNativeOm answers stat for nativeOm as an executable file and leaves every
// other path to the harness's stat, so the target still finds its town.
func withNativeOm(h *harness) {
	withNativeBinary(h, nativeOm)
}

// withNativeBinary answers stat for path as an executable file, for a native tool.
func withNativeBinary(h *harness, path string) {
	stat := h.surface.stat
	h.surface.stat = func(p string) (os.FileInfo, error) {
		if p == path {
			return fakeFile{mode: 0o755}, nil
		}
		return stat(p)
	}
}

// nativeConfig is a config file that targets docker and carries the native map.
func nativeConfig(t *testing.T, native map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"target": "docker", "native": native})
	if err != nil {
		t.Fatalf("marshal the config: %v", err)
	}
	return string(data)
}

func omNative(path string, subcommands ...string) map[string]any {
	return map[string]any{"om": map[string]any{"path": path, "subcommands": subcommands}}
}

func TestRunOmReviewUsesNativePath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	native := nativeOm
	withNativeOm(h)
	h.writeConfig(t, nativeConfig(t, omNative(native, "review")))
	caller := t.TempDir()
	env := []string{"HOME=" + h.home, "PATH=/usr/bin", "CLAUDE_CODE_OAUTH_TOKEN=x", "GT_TOWN_ROOT=" + h.town}
	h.surface.getwd = func() (string, error) { return caller, nil }
	h.surface.environ = func() []string { return env }
	// The target finds its town through GT_TOWN_ROOT; the native call never reads it.
	getenv := h.surface.getenv
	h.surface.getenv = func(name string) string {
		if name == "GT_TOWN_ROOT" {
			return h.town
		}
		return getenv(name)
	}
	fp := &fakeProcess{}
	h.surface.native = fp

	if exit := h.run("om", "review", "-base", "origin/main"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !fp.ran {
		t.Fatal("the native om did not run")
	}
	if want := []string{native, "review", "-base", "origin/main"}; !slices.Equal(fp.argv, want) {
		t.Fatalf("native argv = %q, want %q", fp.argv, want)
	}
	if fp.dir != caller {
		t.Fatalf("native ran in %q, want the caller's directory %q", fp.dir, caller)
	}
	if !slices.Equal(fp.env, env) {
		t.Fatalf("native env = %q, want the caller's whole environment %q", fp.env, env)
	}
	if h.fake.launched || h.targetSet {
		t.Fatal("a native om review reached the target")
	}

	if exit := h.run("om", "status"); exit != 0 {
		t.Fatalf("run(om status) = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.fake.launched {
		t.Fatal("om status did not reach the target")
	}
	if !slices.Equal(h.fake.call.Argv, []string{"om", "status"}) {
		t.Fatalf("target launched %q, want om status", h.fake.call.Argv)
	}
}

func TestRunNativePassesTheExitStatusThrough(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withNativeOm(h)
	h.writeConfig(t, nativeConfig(t, omNative(nativeOm, "review")))
	h.surface.native = &fakeProcess{exit: 2}
	if exit := h.run("om", "review"); exit != 2 {
		t.Fatalf("run = %d, want the native om's 2", exit)
	}
}

func TestRunNativeRefusesWhenNoRunnerIsWired(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withNativeOm(h)
	h.writeConfig(t, nativeConfig(t, omNative(nativeOm, "review")))
	if exit := h.run("om", "review"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if h.fake.launched {
		t.Fatal("the call reached the target with no native runner wired")
	}
}

func TestConfigNativeRefusesBadEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  func(t *testing.T) string
		wantKey string
	}{
		{
			name: "empty path",
			config: func(t *testing.T) string {
				return nativeConfig(t, omNative("", "review"))
			},
			wantKey: "native.om.path",
		},
		{
			name: "relative path",
			config: func(t *testing.T) string {
				return nativeConfig(t, omNative("bin/om", "review"))
			},
			wantKey: "native.om.path",
		},
		{
			name: "missing file",
			config: func(t *testing.T) string {
				return nativeConfig(t, omNative(filepath.Join(t.TempDir(), "om"), "review"))
			},
			wantKey: "native.om.path",
		},
		{
			name: "non-executable file",
			config: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "om")
				if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
					t.Fatalf("write the file: %v", err)
				}
				return nativeConfig(t, omNative(path, "review"))
			},
			wantKey: "native.om.path",
		},
		{
			name: "a tool other than om, bd and gt",
			config: func(t *testing.T) string {
				return nativeConfig(t, map[string]any{
					"hm": map[string]any{"path": nativeOm, "subcommands": []string{"status"}},
				})
			},
			wantKey: "native.hm",
		},
		{
			name: "no subcommand",
			config: func(t *testing.T) string {
				return nativeConfig(t, omNative(nativeOm))
			},
			wantKey: "native.om.subcommands",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.writeConfig(t, tt.config(t))
			fp := &fakeProcess{}
			h.surface.native = fp
			if exit := h.run("om", "review"); exit != 1 {
				t.Fatalf("run = %d, want 1", exit)
			}
			if !strings.Contains(h.stderr.String(), tt.wantKey) {
				t.Fatalf("stderr %q does not name %q", h.stderr, tt.wantKey)
			}
			if n := strings.Count(h.stderr.String(), "\n"); n != 1 {
				t.Fatalf("stderr is %d lines, want one: %q", n, h.stderr)
			}
			if fp.ran {
				t.Fatal("the native om ran from a config that refused it")
			}
			if h.fake.launched {
				t.Fatal("the call fell back to the target")
			}
		})
	}
}

func TestDoctorOmReportsNativePath(t *testing.T) {
	t.Parallel()
	t.Run("runnable", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		native := nativeOm
		withNativeOm(h)
		h.writeConfig(t, nativeConfig(t, omNative(native, "review")))
		h.fake.diag = gtwrap.Diagnosis{Rung: gtwrap.RungExec, Container: "gt-daemon", Image: "gt-daemon:1", Reason: "up"}
		if exit := h.run("om", "--doctor"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if want := "native: " + native + " (exists)\n"; !strings.HasPrefix(h.stdout.String(), want) {
			t.Fatalf("stdout %q does not start with %q", h.stdout, want)
		}
	})
	t.Run("not runnable", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		missing := filepath.Join(t.TempDir(), "om")
		h.writeConfig(t, nativeConfig(t, omNative(missing, "review")))
		if exit := h.run("om", "--doctor"); exit != 1 {
			t.Fatalf("run = %d, want 1", exit)
		}
		if !strings.Contains(h.stdout.String(), "native: \""+missing+"\" (not runnable)") {
			t.Fatalf("stdout %q does not report the native path as not runnable", h.stdout)
		}
	})
}

// selfConfig points the native om at the wrapper the harness is running as, the
// state after the cutover when the config still names the installed om.
func selfConfig(t *testing.T, h *harness) string {
	t.Helper()
	return selfConfigFor(t, h, omTool, "review")
}

// selfConfigFor is selfConfig for any native tool: its entry points at the wrapper.
func selfConfigFor(t *testing.T, h *harness, tool string, subcommands ...string) string {
	t.Helper()
	self, err := h.surface.executable()
	if err != nil {
		t.Fatalf("find the stand-in wrapper: %v", err)
	}
	return nativeConfig(t, map[string]any{tool: map[string]any{"path": self, "subcommands": subcommands}})
}

func TestRunNativeSelfPathRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, selfConfig(t, h))
	fp := &fakeProcess{}
	h.surface.native = fp

	if exit := h.run("om", "review", "-base", "origin/main"); exit != 1 {
		t.Fatalf("run = %d, want 1; stdout: %s", exit, h.stdout)
	}
	if !strings.Contains(h.stderr.String(), "native.om.path") || !strings.Contains(h.stderr.String(), "is this wrapper itself") {
		t.Errorf("stderr %q does not say the native path is the wrapper", h.stderr)
	}
	if fp.ran {
		t.Error("the wrapper started a process for a native path that is itself")
	}
	if h.fake.launched || h.targetSet {
		t.Error("a refused native call reached the target")
	}
}

func TestDoctorNativeSelfPathDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, selfConfig(t, h))
	h.fake.diag = gtwrap.Diagnosis{Rung: gtwrap.RungExec, Container: "gt-daemon", Image: "gt-daemon:1", Reason: "up"}
	if exit := h.run("om", "--doctor"); exit != 1 {
		t.Fatalf("run = %d, want 1; stderr: %s", exit, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "(not runnable)") || !strings.Contains(h.stdout.String(), "is this wrapper itself") {
		t.Errorf("stdout %q does not report the native path as the wrapper", h.stdout)
	}
}

// bdNative is the config entry for a native bd that names the given subcommands.
func bdNative(path string, subcommands ...string) map[string]any {
	return map[string]any{bdTool: map[string]any{"path": path, "subcommands": subcommands}}
}

// gtNative is the config entry for a native gt that names the given command paths.
func gtNative(path string, subcommands ...string) map[string]any {
	return map[string]any{gtTool: map[string]any{"path": path, "subcommands": subcommands}}
}

func TestConfigNativeAcceptsBdHooksOnly(t *testing.T) {
	t.Parallel()
	t.Run("hooks loads", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		withNativeBinary(h, nativeBd)
		h.writeConfig(t, nativeConfig(t, bdNative(nativeBd, "hooks")))
		fp := &fakeProcess{}
		h.surface.native = fp
		if exit := h.run("bd", "hooks", "run", "pre-commit"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if !fp.ran {
			t.Fatal("the native bd hooks run did not run")
		}
	})
	refusals := []struct {
		name    string
		native  map[string]any
		wantKey string
	}{
		{name: "a bd subcommand besides hooks", native: bdNative(nativeBd, "hooks", "show"), wantKey: "native.bd.subcommands"},
		{name: "bd with only a non-hooks subcommand", native: bdNative(nativeBd, "show"), wantKey: "native.bd.subcommands"},
		{name: "a tool other than om and bd", native: map[string]any{"hm": map[string]any{"path": nativeBd, "subcommands": []string{"hooks"}}}, wantKey: "native.hm"},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			withNativeBinary(h, nativeBd)
			h.writeConfig(t, nativeConfig(t, tt.native))
			fp := &fakeProcess{}
			h.surface.native = fp
			if exit := h.run("bd", "hooks", "run", "pre-commit"); exit != 1 {
				t.Fatalf("run = %d, want 1", exit)
			}
			if !strings.Contains(h.stderr.String(), tt.wantKey) {
				t.Fatalf("stderr %q does not name %q", h.stderr, tt.wantKey)
			}
			if fp.ran || h.fake.launched {
				t.Fatal("a call ran from a config that refused it")
			}
		})
	}
}

func TestRunBdHooksUseNativePath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withNativeBinary(h, nativeBd)
	h.writeConfig(t, nativeConfig(t, bdNative(nativeBd, "hooks")))
	caller := t.TempDir()
	env := []string{"HOME=" + h.home, "PATH=/usr/bin", "BD_ACTOR=toast", "GT_TOWN_ROOT=" + h.town}
	h.surface.getwd = func() (string, error) { return caller, nil }
	h.surface.environ = func() []string { return env }
	getenv := h.surface.getenv
	h.surface.getenv = func(name string) string {
		if name == "GT_TOWN_ROOT" {
			return h.town
		}
		return getenv(name)
	}
	fp := &fakeProcess{exit: 3}
	h.surface.native = fp

	if exit := h.run("bd", "hooks", "run", "prepare-commit-msg", ".git/COMMIT_EDITMSG"); exit != 3 {
		t.Fatalf("run = %d, want the native bd's 3; stderr: %s", exit, h.stderr)
	}
	if want := []string{nativeBd, "hooks", "run", "prepare-commit-msg", ".git/COMMIT_EDITMSG"}; !slices.Equal(fp.argv, want) {
		t.Fatalf("native argv = %q, want %q", fp.argv, want)
	}
	if fp.dir != caller {
		t.Fatalf("native ran in %q, want the caller's directory %q", fp.dir, caller)
	}
	if !slices.Equal(fp.env, env) {
		t.Fatalf("native env = %q, want the caller's whole environment %q", fp.env, env)
	}
	if h.fake.launched || h.targetSet {
		t.Fatal("bd hooks run reached the target")
	}

	for _, args := range [][]string{{"show", "x"}, {"hooks", "list"}} {
		fp.ran = false
		h.fake.launched = false
		argv := append([]string{"bd"}, args...)
		if exit := h.run(argv...); exit != 0 {
			t.Fatalf("run(%q) = %d, want 0; stderr: %s", argv, exit, h.stderr)
		}
		if fp.ran {
			t.Fatalf("%q ran natively; only hooks run is native", argv)
		}
		if !h.fake.launched || !slices.Equal(h.fake.call.Argv, argv) {
			t.Fatalf("%q did not reach the target", argv)
		}
	}
}

func TestDoctorBdNativeSelfPathDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, selfConfigFor(t, h, bdTool, "hooks"))
	h.fake.diag = gtwrap.Diagnosis{Rung: gtwrap.RungExec, Container: "gt-daemon", Image: "gt-daemon:1", Reason: "up"}
	if exit := h.run("bd", "--doctor"); exit != 1 {
		t.Fatalf("run = %d, want 1; stderr: %s", exit, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "native: ") || !strings.Contains(h.stdout.String(), "(not runnable)") || !strings.Contains(h.stdout.String(), "is this wrapper itself") {
		t.Errorf("stdout %q does not report the native bd path as the wrapper", h.stdout)
	}
}

func TestRunGtHostCommandsUseNativePath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withNativeBinary(h, nativeGt)
	h.writeConfig(t, nativeConfig(t, gtNative(nativeGt, "town forward plist", "maint plan", "maint apply")))
	caller := t.TempDir()
	env := []string{"HOME=" + h.home, "PATH=/usr/bin", "GT_TOWN_ROOT=" + h.town}
	h.surface.getwd = func() (string, error) { return caller, nil }
	h.surface.environ = func() []string { return env }
	getenv := h.surface.getenv
	h.surface.getenv = func(name string) string {
		if name == "GT_TOWN_ROOT" {
			return h.town
		}
		return getenv(name)
	}
	fp := &fakeProcess{exit: 4}
	h.surface.native = fp

	for _, args := range [][]string{
		{"town", "forward", "plist", "--service", "dashboard", "--context", "gt", "--port", "8080"},
		{"maint", "plan", "nodes"},
		{"maint", "apply", "worktree"},
	} {
		fp.ran = false
		h.fake.launched = false
		argv := append([]string{"gt"}, args...)
		if exit := h.run(argv...); exit != 4 {
			t.Fatalf("run(%q) = %d, want the native gt's 4; stderr: %s", argv, exit, h.stderr)
		}
		if !fp.ran {
			t.Fatalf("%q did not run natively", argv)
		}
		if want := append([]string{nativeGt}, args...); !slices.Equal(fp.argv, want) {
			t.Fatalf("native argv = %q, want %q", fp.argv, want)
		}
		if fp.dir != caller {
			t.Fatalf("native ran in %q, want the caller's directory %q", fp.dir, caller)
		}
		if !slices.Equal(fp.env, env) {
			t.Fatalf("native env = %q, want the caller's whole environment %q", fp.env, env)
		}
		if h.fake.launched {
			t.Fatalf("%q reached the target", argv)
		}
	}

	for _, args := range [][]string{{"pool", "show"}, {"status"}, {"town", "state"}} {
		fp.ran = false
		h.fake.launched = false
		argv := append([]string{"gt"}, args...)
		if exit := h.run(argv...); exit != 0 {
			t.Fatalf("run(%q) = %d, want 0; stderr: %s", argv, exit, h.stderr)
		}
		if fp.ran {
			t.Fatalf("%q ran natively; it is not in the host command set", argv)
		}
		if !h.fake.launched || !slices.Equal(h.fake.call.Argv, argv) {
			t.Fatalf("%q did not reach the target", argv)
		}
	}
}

func TestConfigNativeRefusesAGtPathOutsideTheHostSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		native  map[string]any
		wantKey string
	}{
		{name: "a gt leaf outside the set", native: gtNative(nativeGt, "status"), wantKey: "native.gt.subcommands"},
		{name: "the town forward group alone", native: gtNative(nativeGt, "town forward"), wantKey: "native.gt.subcommands"},
		{name: "one good path beside one outside the set", native: gtNative(nativeGt, "town forward plist", "maint"), wantKey: "native.gt.subcommands"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			withNativeBinary(h, nativeGt)
			h.writeConfig(t, nativeConfig(t, tt.native))
			fp := &fakeProcess{}
			h.surface.native = fp
			if exit := h.run("gt", "town", "forward", "plist"); exit != 1 {
				t.Fatalf("run = %d, want 1", exit)
			}
			if !strings.Contains(h.stderr.String(), tt.wantKey) {
				t.Fatalf("stderr %q does not name %q", h.stderr, tt.wantKey)
			}
			if fp.ran || h.fake.launched {
				t.Fatal("a call ran from a config that refused it")
			}
		})
	}
}

func TestDoctorGtNativeSelfPathDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, selfConfigFor(t, h, gtTool, "town forward plist"))
	h.fake.diag = gtwrap.Diagnosis{Rung: gtwrap.RungExec, Container: "gt-daemon", Image: "gt-daemon:1", Reason: "up"}
	if exit := h.run("gt", "--doctor"); exit != 1 {
		t.Fatalf("run = %d, want 1; stderr: %s", exit, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "native: ") || !strings.Contains(h.stdout.String(), "(not runnable)") || !strings.Contains(h.stdout.String(), "is this wrapper itself") {
		t.Errorf("stdout %q does not report the native gt path as the wrapper", h.stdout)
	}
}
