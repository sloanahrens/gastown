package tiersweep

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeInfo is one path's Lstat answer in a prune test.
type fakeInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return i.modTime }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

// dirInfo is a real directory's Lstat answer.
func dirInfo(name string, size int64, modTime time.Time) fakeInfo {
	return fakeInfo{name: name, size: size, mode: fs.ModeDir | 0o755, modTime: modTime}
}

// linkToDir is the Lstat answer a symlink gets in this test, carrying the
// symlink bit and IsDir as the target would answer it: that shapes the row so
// the symlink test alone rejects it, and dropping that test turns the row red
// (the mutation gt-xk9my records for this case).
func linkToDir(name string, modTime time.Time) fakeInfo {
	return fakeInfo{name: name, mode: fs.ModeSymlink | fs.ModeDir | 0o777, modTime: modTime}
}

// pruneNow is the clock every prune test reads, and pruneOld the modification
// time a directory past PruneMaxAge takes.
var (
	pruneNow = time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	pruneOld = pruneNow.Add(-72 * time.Hour)
)

// pruneFake is the Host a prune test reads: a directory listing, the Lstat
// answer per path, and the removals it saw. No path on disk is touched.
func pruneFake(t *testing.T, entries ...fs.DirEntry) *fakeHost {
	t.Helper()
	return &fakeHost{
		env:      map[string]string{},
		listings: map[string][]fs.DirEntry{"/tmp": entries},
		infos: map[string]fs.FileInfo{
			"/tmp/tier-sweep.aaa":  dirInfo("tier-sweep.aaa", 4096, pruneOld),
			"/tmp/tier-sweep.bbb":  dirInfo("tier-sweep.bbb", 8192, pruneOld),
			"/tmp/tier-sweep.new":  dirInfo("tier-sweep.new", 4096, pruneNow.Add(-time.Hour)),
			"/tmp/tier-sweep.link": linkToDir("tier-sweep.link", pruneOld),
			"/tmp/tier-sweep.file": fakeInfo{name: "tier-sweep.file", size: 12, mode: 0o644, modTime: pruneOld},
			"/tmp/other-dir":       dirInfo("other-dir", 4096, pruneOld),
		},
	}
}

// TestPruneRemovesOnlyOldTierSweepDirs: of a listing holding old and new
// tier-sweep directories, a name that only contains the pattern, the bare
// pattern, a path separator, a regular file and a symlink, only the old real
// directories are candidates, and Prune deletes exactly them.
func TestPruneRemovesOnlyOldTierSweepDirs(t *testing.T) {
	t.Parallel()
	// x-tier-sweep.y, tier-sweep. and sub/tier-sweep.z have no Lstat answer: a
	// prune that looked at them would fail with a missing path, so the fake
	// proves the name test runs before the read.
	f := pruneFake(t,
		fs.FileInfoToDirEntry(dirInfo("tier-sweep.bbb", 8192, pruneOld)),
		fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
		fs.FileInfoToDirEntry(dirInfo("tier-sweep.new", 4096, pruneNow.Add(-time.Hour))),
		fs.FileInfoToDirEntry(linkToDir("tier-sweep.link", pruneOld)),
		fs.FileInfoToDirEntry(fakeInfo{name: "tier-sweep.file", size: 12, mode: 0o644, modTime: pruneOld}),
		fs.FileInfoToDirEntry(dirInfo("other-dir", 4096, pruneOld)),
		fs.FileInfoToDirEntry(dirInfo("x-tier-sweep.y", 4096, pruneOld)),
		fs.FileInfoToDirEntry(dirInfo("tier-sweep.", 4096, pruneOld)),
		fs.FileInfoToDirEntry(dirInfo("sub/tier-sweep.z", 4096, pruneOld)),
	)
	host := f.host()

	got, err := PruneCandidates(host, "/tmp", pruneNow, PruneMaxAge)
	if err != nil {
		t.Fatalf("PruneCandidates: %v", err)
	}
	want := []PruneEntry{
		{Path: "/tmp/tier-sweep.aaa", Age: 72 * time.Hour, Size: 4096},
		{Path: "/tmp/tier-sweep.bbb", Age: 72 * time.Hour, Size: 8192},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PruneCandidates = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(f.readDirs, []string{"/tmp"}) {
		t.Errorf("ReadDir called on %q, want just /tmp", f.readDirs)
	}

	removed, err := Prune(host, "/tmp", pruneNow, PruneMaxAge)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !reflect.DeepEqual(removed, []string{"/tmp/tier-sweep.aaa", "/tmp/tier-sweep.bbb"}) {
		t.Errorf("removed = %q, want the two candidates in path order", removed)
	}
	if !reflect.DeepEqual(f.removed, removed) {
		t.Errorf("RemoveAll saw %q, want exactly the returned candidates %q", f.removed, removed)
	}
}

// TestPruneReportsEveryFailure: a listing it cannot read stops the prune with
// that error, and a directory it cannot delete leaves the others deleted.
func TestPruneReportsEveryFailure(t *testing.T) {
	t.Parallel()
	t.Run("an unreadable directory", func(t *testing.T) {
		t.Parallel()
		f := pruneFake(t)
		f.readErr = errors.New("permission denied")
		if removed, err := Prune(f.host(), "/tmp", pruneNow, PruneMaxAge); removed != nil || err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("Prune = %q, %v, want no removals and the read error", removed, err)
		}
		if len(f.removed) != 0 {
			t.Errorf("RemoveAll ran despite the read error: %q", f.removed)
		}
	})
	t.Run("a path it cannot describe", func(t *testing.T) {
		t.Parallel()
		// Lstat fails for a reason other than a vanished path: the prune stops,
		// because it cannot tell what the entry is.
		f := pruneFake(t,
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.denied", 4096, pruneOld)),
		)
		f.infoErrs = map[string]error{"/tmp/tier-sweep.denied": errors.New("permission denied")}
		removed, err := Prune(f.host(), "/tmp", pruneNow, PruneMaxAge)
		if removed != nil || err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("Prune = %q, %v, want no removals and the Lstat error", removed, err)
		}
		if len(f.removed) != 0 {
			t.Errorf("RemoveAll ran despite the Lstat error: %q", f.removed)
		}
	})
	t.Run("an entry another sweep pruned first", func(t *testing.T) {
		t.Parallel()
		// The listing names an entry that is gone by the time it is described,
		// as a concurrent sweep's prune leaves it: it is skipped, and the
		// candidates beside it are still removed.
		f := pruneFake(t,
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.missing", 4096, pruneOld)),
		)
		removed, err := Prune(f.host(), "/tmp", pruneNow, PruneMaxAge)
		if err != nil {
			t.Fatalf("Prune: %v, want the vanished entry skipped", err)
		}
		if !reflect.DeepEqual(removed, []string{"/tmp/tier-sweep.aaa"}) {
			t.Errorf("removed = %q, want the candidate that was still there", removed)
		}
		if !reflect.DeepEqual(f.removed, []string{"/tmp/tier-sweep.aaa"}) {
			t.Errorf("RemoveAll saw %q, want only the entry that was still there", f.removed)
		}
	})
	t.Run("one directory it cannot delete", func(t *testing.T) {
		t.Parallel()
		f := pruneFake(t,
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.bbb", 8192, pruneOld)),
		)
		f.removeErr = map[string]error{"/tmp/tier-sweep.aaa": errors.New("busy")}
		removed, err := Prune(f.host(), "/tmp", pruneNow, PruneMaxAge)
		if err == nil || !strings.Contains(err.Error(), "busy") {
			t.Errorf("Prune error = %v, want the failed removal", err)
		}
		if !reflect.DeepEqual(removed, []string{"/tmp/tier-sweep.bbb"}) {
			t.Errorf("removed = %q, want the candidate that could be deleted", removed)
		}
		if !reflect.DeepEqual(f.removed, []string{"/tmp/tier-sweep.aaa", "/tmp/tier-sweep.bbb"}) {
			t.Errorf("RemoveAll saw %q, want every candidate tried", f.removed)
		}
	})
}

// TestGreenSweepRemovesItsOwnLogDir: a GREEN run that made its own directory
// prunes the old ones in TMPDIR first, then removes its directory once the last
// summary line is printed.
func TestGreenSweepRemovesItsOwnLogDir(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.ownLogDir, r.tmpDir = true, "/var/tmp"
	r.shellOut, r.shellCode = shellLine("GREEN"), 0
	code, stdout, stderr := r.run("shell")
	if code != 0 || !strings.Contains(stdout, "shell GREEN") {
		t.Fatalf("code %d, stdout %q, stderr %q, want a GREEN exit 0", code, stdout, stderr)
	}
	if !reflect.DeepEqual(r.prunes, []string{"/var/tmp"}) {
		t.Errorf("pruned %q, want the TMPDIR the run made its directory in", r.prunes)
	}
	if !reflect.DeepEqual(r.removals, []string{r.logDir}) {
		t.Errorf("removed %q, want just the run's own log directory %q", r.removals, r.logDir)
	}
	line, removed := -1, -1
	for i, e := range r.events {
		switch e {
		case "removed " + r.logDir:
			removed = i
		default:
			if strings.HasPrefix(e, "line: ") {
				line = i
			}
		}
	}
	if line < 0 || removed < line {
		t.Errorf("events = %q, want the removal after the last summary line", r.events)
	}
}

// TestRedSweepKeepsIt: a RED run keeps its own log directory, the only copy of
// its per-package logs, and still prunes the old ones.
func TestRedSweepKeepsIt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.ownLogDir = true
	r.shellOut, r.shellCode = shellLine("RED"), 1
	code, stdout, stderr := r.run("shell")
	if code != 1 || !strings.Contains(stdout, "shell RED") {
		t.Fatalf("code %d, stdout %q, stderr %q, want a RED exit 1", code, stdout, stderr)
	}
	if len(r.removals) != 0 {
		t.Errorf("removed %q, want a RED run to keep its log directory", r.removals)
	}
	if !reflect.DeepEqual(r.prunes, []string{DefaultTempDir}) {
		t.Errorf("pruned %q, want the default TMPDIR pruned either way", r.prunes)
	}
}

// TestNamedLogDirIsNeverRemoved: a run that took its directory from
// TIER_SWEEP_LOGDIR never removes it, and never prunes a directory of the
// caller's.
func TestNamedLogDirIsNeverRemoved(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.shellOut, r.shellCode = shellLine("GREEN"), 0
	if code, stdout, stderr := r.run("shell"); code != 0 || !strings.Contains(stdout, "shell GREEN") {
		t.Fatalf("code %d, stdout %q, stderr %q, want a GREEN exit 0", code, stdout, stderr)
	}
	if len(r.removals) != 0 {
		t.Errorf("removed %q, want a named log directory kept", r.removals)
	}
	if len(r.prunes) != 0 {
		t.Errorf("pruned in %q, want a named log directory's parent left alone", r.prunes)
	}
}

// TestCurrentLogDirIsNeverPruned: the run's own log directory is held back by
// name, not by its age, so it is never a prune candidate even when its mtime is
// old, and a RED run that keeps it never sees it go.
func TestCurrentLogDirIsNeverPruned(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.ownLogDir = true
	r.logDir = filepath.Join(t.TempDir(), "tier-sweep.current")
	r.tmpDir = filepath.Dir(r.logDir)
	if err := os.MkdirAll(r.logDir, 0o755); err != nil {
		t.Fatalf("make the run's log directory: %v", err)
	}
	oldSibling := filepath.Join(r.tmpDir, "tier-sweep.old")
	r.listings = map[string][]fs.DirEntry{
		r.tmpDir: {
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.current", 4096, pruneOld)),
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.old", 4096, pruneOld)),
		},
	}
	r.infos = map[string]fs.FileInfo{
		r.logDir:   dirInfo("tier-sweep.current", 4096, pruneOld),
		oldSibling: dirInfo("tier-sweep.old", 4096, pruneOld),
	}
	r.shellOut, r.shellCode = shellLine("RED"), 1
	code, stdout, stderr := r.run("shell")
	if code != 1 || !strings.Contains(stdout, "shell RED") {
		t.Fatalf("code %d, stdout %q, stderr %q, want a RED exit 1", code, stdout, stderr)
	}
	if want := []string{oldSibling}; !reflect.DeepEqual(r.removals, want) {
		t.Errorf("removed %q, want only the old sibling %q: the run's own %q is never pruned", r.removals, want, r.logDir)
	}
}

// TestHostWithoutCleanupCallsIsLeftAlone: a host that wires no ReadDir, Lstat or
// RemoveAll gets no prune and no removal, because Run fills no os default for
// them; one stderr line says the cleanup was skipped and changes no verdict.
func TestHostWithoutCleanupCallsIsLeftAlone(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.ownLogDir = true
	r.noCleanupSlots = true
	// A directory of this test's own, so a future default to the real os calls
	// would see an empty listing instead of the host's TMPDIR.
	r.tmpDir = t.TempDir()
	r.shellOut, r.shellCode = shellLine("GREEN"), 0
	code, stdout, stderr := r.run("shell")
	if code != 0 || stdout != shellLine("GREEN") {
		t.Fatalf("code %d, stdout %q, stderr %q, want the GREEN line and exit 0", code, stdout, stderr)
	}
	if len(r.prunes) != 0 || len(r.removals) != 0 {
		t.Errorf("pruned %q and removed %q, want a host with no cleanup calls left alone", r.prunes, r.removals)
	}
	if got := strings.Count(stderr, "\n"); got != 1 {
		t.Fatalf("stderr = %q, want exactly one line", stderr)
	}
	if !strings.Contains(stderr, "cleanup skipped") || !strings.Contains(stderr, sweepGoesOn) {
		t.Errorf("stderr = %q, want it to name the skipped cleanup and say the sweep goes on", stderr)
	}
}

// TestPruneFailureIsNotAVerdict: a prune or removal failure leaves the verdicts
// and the exit code unchanged and prints one stderr line for the failure, beside
// the line of each directory the prune did remove.
func TestPruneFailureIsNotAVerdict(t *testing.T) {
	t.Parallel()
	greenLine := shellLine("GREEN")
	cases := []struct {
		name  string
		setup func(r *rig)
		// wantErr is a fragment of the one failure line.
		wantErr string
		// wantPruned is a directory the prune removed before the failure; it
		// keeps its own line, so the stderr has one more line than wantErr's.
		wantPruned string
	}{
		{
			name: "the directory listing fails",
			setup: func(r *rig) {
				r.readErr = errors.New("permission denied")
			},
			wantErr: "prune /tmp: permission denied",
		},
		{
			name: "an old directory cannot be deleted",
			setup: func(r *rig) {
				r.listings = map[string][]fs.DirEntry{
					"/tmp": {fs.FileInfoToDirEntry(dirInfo("tier-sweep.old", 4096, pruneOld))},
				}
				r.infos = map[string]fs.FileInfo{"/tmp/tier-sweep.old": dirInfo("tier-sweep.old", 4096, pruneOld)}
				r.removeErrs = map[string]error{"/tmp/tier-sweep.old": errors.New("busy")}
			},
			wantErr: "prune /tmp: busy",
		},
		{
			name: "one old directory fails beside one that does not",
			setup: func(r *rig) {
				r.listings = map[string][]fs.DirEntry{
					"/tmp": {
						fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
						fs.FileInfoToDirEntry(dirInfo("tier-sweep.bbb", 4096, pruneOld)),
					},
				}
				r.infos = map[string]fs.FileInfo{
					"/tmp/tier-sweep.aaa": dirInfo("tier-sweep.aaa", 4096, pruneOld),
					"/tmp/tier-sweep.bbb": dirInfo("tier-sweep.bbb", 4096, pruneOld),
				}
				r.removeErrs = map[string]error{"/tmp/tier-sweep.aaa": errors.New("busy")}
			},
			wantErr:    "prune /tmp: busy",
			wantPruned: "tier-sweep: pruned /tmp/tier-sweep.bbb",
		},
		{
			name: "the run's own directory cannot be deleted",
			setup: func(r *rig) {
				r.removeErrs = map[string]error{r.logDir: errors.New("busy")}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.ownLogDir = true
			r.shellOut, r.shellCode = greenLine, 0
			if tc.setup != nil {
				tc.setup(r)
			}
			wantErr := tc.wantErr
			if wantErr == "" {
				wantErr = "remove " + r.logDir
			}
			code, stdout, stderr := r.run("shell")
			if code != 0 || stdout != greenLine {
				t.Errorf("code %d, stdout %q, want the GREEN line and exit 0 unchanged", code, stdout)
			}
			wantLines := 1
			if tc.wantPruned != "" {
				wantLines = 2
				if !strings.Contains(stderr, tc.wantPruned) {
					t.Errorf("stderr = %q, want it to name the directory that was pruned %q", stderr, tc.wantPruned)
				}
			}
			if got := strings.Count(stderr, "\n"); got != wantLines {
				t.Fatalf("stderr = %q, want exactly %d line(s)", stderr, wantLines)
			}
			if !strings.Contains(stderr, wantErr) || !strings.Contains(stderr, sweepGoesOn) {
				t.Errorf("stderr = %q, want it to name %q and say the sweep goes on", stderr, wantErr)
			}
		})
	}
}
