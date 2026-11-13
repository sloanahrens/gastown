package rig

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

const (
	retireTown = "/town"
	retireRig  = "gastown"
)

var (
	retireRigPath = filepath.Join(retireTown, retireRig)
	retireFrom    = filepath.Join(retireRigPath, constants.DirMayor, constants.DirRig)
	retireTo      = retireFrom + ".retired-20261113"
	retireRoutes  = filepath.Join(retireTown, constants.DirBeads, "routes.jsonl")
)

// retireClock is a fixed clock at noon local time, so its local date is the same
// in every time zone.
func retireClock() time.Time {
	return time.Date(2026, time.November, 13, 12, 0, 0, 0, time.Local)
}

// fakeRetireFS is an in-memory RetireFS. Renames are recorded, never applied.
type fakeRetireFS struct {
	dirs  map[string]bool
	files map[string]string
	// links holds symlinks whose targets do not exist: Lstat sees them, Exists
	// and Canonical do not.
	links map[string]bool
	// canon maps a symlinked prefix to the real path it stands for.
	canon   map[string]string
	errs    map[string]error
	renames []string
}

func newFakeRetireFS() *fakeRetireFS {
	return &fakeRetireFS{
		dirs:  map[string]bool{},
		files: map[string]string{},
		links: map[string]bool{},
		canon: map[string]string{},
		errs:  map[string]error{},
	}
}

func (f *fakeRetireFS) Exists(path string) (bool, error) {
	if err := f.errs[path]; err != nil {
		return false, err
	}
	_, isFile := f.files[path]
	return f.dirs[path] || isFile, nil
}

func (f *fakeRetireFS) Lstat(path string) (bool, bool, error) {
	if err := f.errs[path]; err != nil {
		return false, false, err
	}
	_, isFile := f.files[path]
	return f.dirs[path] || isFile || f.links[path], f.dirs[path], nil
}

// Canonical applies the canon prefix mappings, or cleans the path.
func (f *fakeRetireFS) Canonical(path string) (string, error) {
	if err := f.errs[path]; err != nil {
		return "", err
	}
	for prefix, target := range f.canon {
		if rest, ok := strings.CutPrefix(path, prefix); ok && (rest == "" || rest[0] == filepath.Separator) {
			return filepath.Clean(target + rest), nil
		}
	}
	return filepath.Clean(path), nil
}

func (f *fakeRetireFS) ReadFile(path string) ([]byte, error) {
	if err := f.errs[path]; err != nil {
		return nil, err
	}
	content, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return []byte(content), nil
}

// SubDirs lists the first path element under path of every directory it holds,
// so a directory's parents count as directories even when only the child was
// added.
func (f *fakeRetireFS) SubDirs(path string) ([]string, error) {
	if err := f.errs[path]; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for d := range f.dirs {
		rel, err := filepath.Rel(path, d)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		name, _, _ := strings.Cut(rel, string(filepath.Separator))
		if !strings.HasPrefix(name, ".") {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (f *fakeRetireFS) Rename(oldPath, newPath string) error {
	f.renames = append(f.renames, oldPath+" -> "+newPath)
	return nil
}

// retireBaseWorld is a rig whose mayor/rig can be retired: the directory exists,
// the beads have moved to <rig>/.beads with metadata.json, and nothing points
// at the old path.
func retireBaseWorld(f *fakeRetireFS) {
	f.dirs[retireRigPath] = true
	f.dirs[retireFrom] = true
	f.dirs[filepath.Join(retireRigPath, constants.DirBeads)] = true
	f.files[filepath.Join(retireRigPath, constants.DirBeads, "metadata.json")] = "{}"
}

func TestRetireMayorPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		setup              func(f *fakeRetireFS)
		rig                string
		wantAlreadyRetired bool
		wantRefusal        string // a substring of one refusal; empty means none
		wantErr            string // a substring of the error; empty means none
	}{
		{
			name:  "clean rig plans the rename to the dated destination",
			setup: retireBaseWorld,
			rig:   retireRig,
		},
		{
			name:               "rig directory without mayor/rig is already retired",
			setup:              func(f *fakeRetireFS) { f.dirs[retireRigPath] = true },
			rig:                retireRig,
			wantAlreadyRetired: true,
		},
		{
			name:    "nonexistent rig name is an error naming the rig and its path",
			setup:   func(f *fakeRetireFS) {},
			rig:     retireRig,
			wantErr: "rig gastown: no directory " + retireRigPath,
		},
		{
			name:    "typo'd rig name is an error naming the rig and its path",
			setup:   retireBaseWorld,
			rig:     "gastwn",
			wantErr: "rig gastwn: no directory " + filepath.Join(retireTown, "gastwn"),
		},
		{
			name: "dangling symlink at mayor/rig is present and renamed",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				delete(f.dirs, retireFrom)
				f.links[retireFrom] = true
			},
			rig: retireRig,
		},
		{
			name: "dangling symlink at mayor/rig still runs the redirect scan",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				delete(f.dirs, retireFrom)
				f.links[retireFrom] = true
				f.files[filepath.Join(retireRigPath, constants.DirBeads, "redirect")] = "mayor/rig/.beads"
			},
			rig:         retireRig,
			wantRefusal: filepath.Join(retireRigPath, constants.DirBeads, "redirect"),
		},
		{
			name: "existing destination is refused and named",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.dirs[retireTo] = true
			},
			rig:         retireRig,
			wantRefusal: retireTo,
		},
		{
			name: "beads dir without metadata.json is refused and named",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				delete(f.files, filepath.Join(retireRigPath, constants.DirBeads, "metadata.json"))
			},
			rig:         retireRig,
			wantRefusal: filepath.Join(retireRigPath, constants.DirBeads),
		},
		{
			name: "missing beads dir is refused and named",
			setup: func(f *fakeRetireFS) {
				f.dirs[retireRigPath] = true
				f.dirs[retireFrom] = true
			},
			rig:         retireRig,
			wantRefusal: filepath.Join(retireRigPath, constants.DirBeads),
		},
		{
			name: "crew worktree redirect into mayor/rig is refused and named",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				crew := filepath.Join(retireRigPath, constants.DirCrew, "max")
				f.dirs[crew] = true
				f.files[filepath.Join(crew, constants.DirBeads, "redirect")] = "../../mayor/rig/.beads"
			},
			rig:         retireRig,
			wantRefusal: filepath.Join(retireRigPath, constants.DirCrew, "max", constants.DirBeads, "redirect"),
		},
		{
			name: "rig-level redirect into mayor/rig is refused and named",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[filepath.Join(retireRigPath, constants.DirBeads, "redirect")] = "mayor/rig/.beads"
			},
			rig:         retireRig,
			wantRefusal: filepath.Join(retireRigPath, constants.DirBeads, "redirect"),
		},
		{
			name: "polecat worktree redirect with an absolute target is refused and named",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				worktree := filepath.Join(retireRigPath, constants.DirPolecats, "ace", retireRig)
				f.dirs[worktree] = true
				f.files[filepath.Join(worktree, constants.DirBeads, "redirect")] = filepath.Join(retireFrom, constants.DirBeads)
			},
			rig:         retireRig,
			wantRefusal: filepath.Join(retireRigPath, constants.DirPolecats, "ace", retireRig, constants.DirBeads, "redirect"),
		},
		{
			name: "redirect to a sibling of mayor/rig is not a refusal",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				crew := filepath.Join(retireRigPath, constants.DirCrew, "max")
				f.dirs[crew] = true
				f.files[filepath.Join(crew, constants.DirBeads, "redirect")] = "../../hm/.beads"
			},
			rig: retireRig,
		},
		{
			name: "routes line to this rig's mayor/rig is refused and named by line",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[retireRoutes] = "{\"path\":\".\",\"prefix\":\"hq-\"}\n{\"path\":\"gastown/mayor/rig\",\"prefix\":\"gt-\"}\n"
			},
			rig:         retireRig,
			wantRefusal: retireRoutes + ":2",
		},
		{
			name: "routes line to another rig's mayor/rig is not a refusal",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[retireRoutes] = "{\"path\":\"beads/mayor/rig\",\"prefix\":\"be-\"}\n"
			},
			rig: retireRig,
		},
		{
			name: "absolute routes line through a symlink to this rig is refused and named by line",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.canon[filepath.Join(retireTown, "alias")] = retireRigPath
				f.files[retireRoutes] = "{\"path\":\"/town/alias/mayor/rig\",\"prefix\":\"gt-\"}\n"
			},
			rig:         retireRig,
			wantRefusal: retireRoutes + ":1",
		},
		{
			name: "absolute routes line ending in this rig's mayor/rig is refused whatever its prefix",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[retireRoutes] = "{\"path\":\"/elsewhere/gastown/mayor/rig\",\"prefix\":\"gt-\"}\n"
			},
			rig:         retireRig,
			wantRefusal: retireRoutes + ":1",
		},
		{
			name: "routes line to this rig's mayor/rig through a symlinked rig directory is refused",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.canon[retireRigPath] = "/elsewhere/rigdir"
				f.files[retireRoutes] = "{\"path\":\"gastown/mayor/rig\",\"prefix\":\"gt-\"}\n"
			},
			rig:         retireRig,
			wantRefusal: retireRoutes + ":1",
		},
		{
			name: "routes line with .. segments into this rig's mayor/rig is refused and named by line",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[retireRoutes] = "{\"path\":\"beads/../gastown/mayor/rig\",\"prefix\":\"gt-\"}\n"
			},
			rig:         retireRig,
			wantRefusal: retireRoutes + ":1",
		},
		{
			name: "routes line that cannot be resolved is an error, not a clean plan",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[retireRoutes] = "{\"path\":\"beads/mayor/rig\",\"prefix\":\"be-\"}\n"
				f.errs[filepath.Join(retireTown, "beads", "mayor", "rig")] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "unreadable routes file is an error, not a clean plan",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.errs[retireRoutes] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "malformed routes line is an error naming its line",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[retireRoutes] = "not json\n"
			},
			rig:     retireRig,
			wantErr: retireRoutes + ":1",
		},
		{
			name: "unreadable redirect is an error, not a clean plan",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				redirect := filepath.Join(retireRigPath, constants.DirBeads, "redirect")
				f.errs[redirect] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "unreadable metadata.json is an error, not a missing one",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.errs[filepath.Join(retireRigPath, constants.DirBeads, "metadata.json")] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "unreadable mayor/rig is an error, not a missing one",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.errs[retireFrom] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "unreadable destination is an error, not a free one",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.errs[retireTo] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "unreadable crew directory is an error, not an empty one",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.errs[filepath.Join(retireRigPath, constants.DirCrew)] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "unreadable polecats directory is an error, not an empty one",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.errs[filepath.Join(retireRigPath, constants.DirPolecats)] = errors.New("permission denied")
			},
			rig:     retireRig,
			wantErr: "permission denied",
		},
		{
			name: "routes line with no path is an error naming its line",
			setup: func(f *fakeRetireFS) {
				retireBaseWorld(f)
				f.files[retireRoutes] = "{\"prefix\":\"gt-\"}\n"
			},
			rig:     retireRig,
			wantErr: retireRoutes + ":1",
		},
		{
			name:    "empty rig name is an error",
			setup:   retireBaseWorld,
			rig:     "",
			wantErr: "invalid rig name",
		},
		{
			name:    "rig name with a separator is an error",
			setup:   retireBaseWorld,
			rig:     "../gastown",
			wantErr: "invalid rig name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRetireFS()
			tt.setup(f)

			plan, err := PlanRetireMayor(f, retireTown, tt.rig, retireClock)
			if len(f.renames) != 0 {
				t.Errorf("planning renamed %v; only apply may rename", f.renames)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("PlanRetireMayor error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanRetireMayor: %v", err)
			}
			if plan.AlreadyRetired != tt.wantAlreadyRetired {
				t.Errorf("AlreadyRetired = %v, want %v", plan.AlreadyRetired, tt.wantAlreadyRetired)
			}
			if tt.wantAlreadyRetired {
				if len(plan.Refusals) != 0 {
					t.Errorf("already retired plan has refusals %v", plan.Refusals)
				}
				return
			}
			if plan.From != retireFrom || plan.To != retireTo {
				t.Errorf("plan renames %q to %q, want %q to %q", plan.From, plan.To, retireFrom, retireTo)
			}
			if tt.wantRefusal == "" {
				if len(plan.Refusals) != 0 {
					t.Errorf("Refusals = %v, want none", plan.Refusals)
				}
				return
			}
			if !slices.ContainsFunc(plan.Refusals, func(r string) bool { return strings.Contains(r, tt.wantRefusal) }) {
				t.Errorf("Refusals = %v, want one naming %q", plan.Refusals, tt.wantRefusal)
			}
		})
	}
}

// TestRetireMayorApply checks that only Apply renames, and that it renames to the
// planned destination exactly once.
func TestRetireMayorApply(t *testing.T) {
	t.Parallel()
	t.Run("apply renames mayor/rig to the dated destination", func(t *testing.T) {
		f := newFakeRetireFS()
		retireBaseWorld(f)
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		if err := plan.Apply(f); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if want := []string{retireFrom + " -> " + retireTo}; !slices.Equal(f.renames, want) {
			t.Errorf("renames = %v, want %v", f.renames, want)
		}
	})

	t.Run("apply refuses a plan with refusals and renames nothing", func(t *testing.T) {
		f := newFakeRetireFS()
		retireBaseWorld(f)
		// The plan holds this refusal, so Apply must return it without planning
		// again.
		delete(f.files, filepath.Join(retireRigPath, constants.DirBeads, "metadata.json"))
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		if err := plan.Apply(f); err == nil {
			t.Fatal("Apply = nil, want a refusal")
		}
		if len(f.renames) != 0 {
			t.Errorf("renames = %v, want none", f.renames)
		}
	})

	t.Run("apply reports a destination it cannot read and renames nothing", func(t *testing.T) {
		f := newFakeRetireFS()
		retireBaseWorld(f)
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		f.errs[retireTo] = errors.New("permission denied")
		if err := plan.Apply(f); err == nil {
			t.Fatal("Apply = nil, want the unreadable destination")
		}
		if len(f.renames) != 0 {
			t.Errorf("renames = %v, want none", f.renames)
		}
	})

	t.Run("apply rechecks the destination taken after the plan", func(t *testing.T) {
		f := newFakeRetireFS()
		retireBaseWorld(f)
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		f.dirs[retireTo] = true
		if err := plan.Apply(f); err == nil {
			t.Fatal("Apply = nil, want a refusal naming the taken destination")
		}
		if len(f.renames) != 0 {
			t.Errorf("renames = %v, want none", f.renames)
		}
	})

	t.Run("apply rechecks the refusals taken after the plan", func(t *testing.T) {
		f := newFakeRetireFS()
		retireBaseWorld(f)
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		// The plan was clean; a redirect written after it must still stop the rename.
		redirect := filepath.Join(retireRigPath, constants.DirBeads, "redirect")
		f.files[redirect] = "mayor/rig/.beads"
		err = plan.Apply(f)
		if err == nil || !strings.Contains(err.Error(), redirect) {
			t.Fatalf("Apply error = %v, want a refusal naming %q", err, redirect)
		}
		if len(f.renames) != 0 {
			t.Errorf("renames = %v, want none", f.renames)
		}
	})

	t.Run("apply refuses when mayor/rig is gone after the plan", func(t *testing.T) {
		f := newFakeRetireFS()
		retireBaseWorld(f)
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		delete(f.dirs, retireFrom)
		if err := plan.Apply(f); err == nil {
			t.Fatal("Apply = nil, want a stale-plan refusal")
		}
		if len(f.renames) != 0 {
			t.Errorf("renames = %v, want none", f.renames)
		}
	})

	t.Run("apply renames a dangling symlink at mayor/rig", func(t *testing.T) {
		f := newFakeRetireFS()
		retireBaseWorld(f)
		delete(f.dirs, retireFrom)
		f.links[retireFrom] = true
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		if err := plan.Apply(f); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if want := []string{retireFrom + " -> " + retireTo}; !slices.Equal(f.renames, want) {
			t.Errorf("renames = %v, want %v", f.renames, want)
		}
	})

	t.Run("apply on an already retired rig is not an error and renames nothing", func(t *testing.T) {
		f := newFakeRetireFS()
		f.dirs[retireRigPath] = true
		plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
		if err != nil {
			t.Fatalf("PlanRetireMayor: %v", err)
		}
		if err := plan.Apply(f); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(f.renames) != 0 {
			t.Errorf("renames = %v, want none", f.renames)
		}
	})
}

// TestRetireMayorOSFSExistsReportsAnUnreadablePath checks that a path that runs
// through a regular file is neither present nor missing: the real filesystem
// reports an error, and OSRetireFS must pass it up.
func TestRetireMayorOSFSExistsReportsAnUnreadablePath(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (OSRetireFS{}).Exists(filepath.Join(file, "child")); err == nil {
		t.Error("Exists under a regular file = nil error, want one")
	}
}

// TestRetireMayorOSFSLstatSeesADanglingSymlink checks that a symlink whose target
// is missing exists to Lstat but not to Exists, so a dangling mayor/rig is present.
func TestRetireMayorOSFSLstatSeesADanglingSymlink(t *testing.T) {
	t.Parallel()
	link := filepath.Join(t.TempDir(), "rig")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), link); err != nil {
		t.Fatalf("creating a dangling symlink: %v", err)
	}
	exists, isDir, err := (OSRetireFS{}).Lstat(link)
	if err != nil || !exists || isDir {
		t.Errorf("Lstat(dangling link) = exists %v, isDir %v, err %v; want exists true, isDir false", exists, isDir, err)
	}
	if got, err := (OSRetireFS{}).Exists(link); err != nil || got {
		t.Errorf("Exists(dangling link) = %v, %v; want false, nil", got, err)
	}
}

// TestRetireMayorOSFSCanonicalFallsBackToTheCleanPath checks that a path which
// does not exist resolves to its cleaned spelling, so a route to a missing
// directory is still compared as text.
func TestRetireMayorOSFSCanonicalFallsBackToTheCleanPath(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "a", "..", "gone")
	got, err := (OSRetireFS{}).Canonical(missing)
	if err != nil || got != filepath.Clean(missing) {
		t.Errorf("Canonical(%q) = %q, %v; want %q, nil", missing, got, err, filepath.Clean(missing))
	}
}

func TestRetireMayorOSFSSubDirsReportsAnUnreadableDirectory(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (OSRetireFS{}).SubDirs(file); err == nil {
		t.Error("SubDirs of a regular file = nil error, want one")
	}
}

// TestRetireMayorUndo checks that the printed mv reverses the rename.
func TestRetireMayorUndo(t *testing.T) {
	t.Parallel()
	f := newFakeRetireFS()
	retireBaseWorld(f)
	plan, err := PlanRetireMayor(f, retireTown, retireRig, retireClock)
	if err != nil {
		t.Fatalf("PlanRetireMayor: %v", err)
	}
	if want := "mv " + retireTo + " " + retireFrom; plan.Undo() != want {
		t.Errorf("Undo() = %q, want %q", plan.Undo(), want)
	}
}
