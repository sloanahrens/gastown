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

// withNativeOm answers stat for nativeOm as an executable file and leaves every
// other path to the harness's stat, so the target still finds its town.
func withNativeOm(h *harness) {
	stat := h.surface.stat
	h.surface.stat = func(path string) (os.FileInfo, error) {
		if path == nativeOm {
			return fakeFile{mode: 0o755}, nil
		}
		return stat(path)
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
			name: "a tool other than om",
			config: func(t *testing.T) string {
				return nativeConfig(t, map[string]any{
					"gt": map[string]any{"path": nativeOm, "subcommands": []string{"status"}},
				})
			},
			wantKey: "native.gt",
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
