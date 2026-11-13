package crew

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/rig"
)

// presetClone is the rig's git handle with a core.hooksPath already set in each
// clone it makes, as a config template or an earlier setup would leave it.
type presetClone struct {
	crewRepo
	f      *gitfake.Fake
	preset string
	// selfLoop makes the clone's .githooks a symlink to itself, so stat fails
	// with ELOOP: neither a directory nor a path that is absent.
	selfLoop bool
}

// CloneBranchFull and CloneFull are the two clones Add makes: CloneFull is the
// fallback Add takes when CloneBranchFull fails, as it does for an empty rig.
func (p presetClone) CloneBranchFull(url, dest, branch string) error {
	if err := p.crewRepo.CloneBranchFull(url, dest, branch); err != nil {
		return err
	}
	return p.prepare(dest)
}

func (p presetClone) CloneFull(url, dest string) error {
	if err := p.crewRepo.CloneFull(url, dest); err != nil {
		return err
	}
	return p.prepare(dest)
}

// prepare makes the state the clone at dest is handed over in.
func (p presetClone) prepare(dest string) error {
	if p.selfLoop {
		if err := os.Symlink(".githooks", filepath.Join(dest, ".githooks")); err != nil {
			return err
		}
	}
	if p.preset == "" {
		return nil
	}
	clone, ok := p.f.Open(dest).(crewRepo)
	if !ok {
		return errors.New("gitfake clone does not implement crewRepo")
	}
	return clone.ConfigSet("core.hooksPath", p.preset)
}

// failingConfig is a crew clone whose config read or write is refused.
type failingConfig struct {
	crewRepo
	failGet, failSet bool
}

func (c failingConfig) ConfigGet(key string) (string, error) {
	if c.failGet {
		return "", errors.New("config read refused")
	}
	return c.crewRepo.ConfigGet(key)
}

func (c failingConfig) ConfigSet(key, value string) error {
	if c.failSet {
		return errors.New("config write refused")
	}
	return c.crewRepo.ConfigSet(key, value)
}

// TestCrewAddSetsHooksPath covers gt crew add's core.hooksPath rule: a clone
// whose tree carries .githooks gets core.hooksPath=.githooks; a clone without it
// gets none; a value already naming a directory is kept; a dangling value is
// replaced; a value that cannot be read, or a write that is refused, fails the
// add and removes the clone.
func TestCrewAddSetsHooksPath(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		hasHooks   bool
		hooksFile  bool
		selfLoop   bool
		preset     func(t *testing.T) string
		keepPreset bool
		failGet    bool
		failSet    bool
		want       string
		wantErr    bool
	}{
		{
			name:     "unstattable githooks refuses the add",
			selfLoop: true,
			wantErr:  true,
		},
		{
			name:     "githooks tree, unset",
			hasHooks: true,
			want:     ".githooks",
		},
		{
			name: "no githooks tree",
			want: "",
		},
		{
			name:      "githooks file is not a directory",
			hooksFile: true,
			want:      "",
		},
		{
			name:     "existing directory is kept",
			hasHooks: true,
			preset: func(t *testing.T) string {
				return t.TempDir()
			},
			keepPreset: true,
		},
		{
			name:     "dangling value is replaced",
			hasHooks: true,
			preset: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "gone")
			},
			want: ".githooks",
		},
		{
			name:     "unreadable value refuses the add",
			hasHooks: true,
			preset: func(t *testing.T) string {
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(file, "hooks")
			},
			wantErr: true,
		},
		{
			name:     "refused read fails the add",
			hasHooks: true,
			failGet:  true,
			wantErr:  true,
		},
		{
			name:     "refused write fails the add",
			hasHooks: true,
			failSet:  true,
			wantErr:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := gitfake.New()
			tmpDir := t.TempDir()
			rigPath := filepath.Join(tmpDir, "test-rig")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			bare := filepath.Join(tmpDir, "bare.git")
			f.InitBare(t, bare)
			if tt.hasHooks {
				f.Commit(t, bare, "main", "add hooks", map[string]string{".githooks/pre-push": "pre-push guard\n"})
			}
			if tt.hooksFile {
				f.Commit(t, bare, "main", "add a file named .githooks", map[string]string{".githooks": "not a directory\n"})
			}

			var preset string
			if tt.preset != nil {
				preset = tt.preset(t)
			}
			r := &rig.Rig{Name: "test-rig", Path: rigPath, GitURL: bare}
			mgr := fakeManager(t, f, r)
			mgr.git = presetClone{crewRepo: mgr.git, f: f, preset: preset, selfLoop: tt.selfLoop}
			if tt.failGet || tt.failSet {
				open := mgr.openGit
				mgr.openGit = func(dir string) crewRepo {
					return failingConfig{crewRepo: open(dir), failGet: tt.failGet, failSet: tt.failSet}
				}
			}

			worker, err := mgr.Add("hooks", false)
			crewPath := filepath.Join(rigPath, "crew", "hooks")
			if tt.wantErr {
				if err == nil {
					t.Fatal("Add succeeded, want an error")
				}
				if _, statErr := os.Stat(crewPath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("clone left behind after refused add: stat err %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			want := tt.want
			if tt.keepPreset {
				want = preset
			}
			got, err := f.Open(worker.ClonePath).(crewRepo).ConfigGet("core.hooksPath")
			if err != nil {
				t.Fatalf("ConfigGet: %v", err)
			}
			if got != want {
				t.Fatalf("core.hooksPath = %q, want %q", got, want)
			}
		})
	}
}
