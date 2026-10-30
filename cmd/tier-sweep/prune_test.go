package main

import (
	"bytes"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
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

// pruneNow is the clock every prune test reads, and pruneOld the modification
// time a directory past PruneMaxAge takes.
var (
	pruneNow = time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	pruneOld = pruneNow.Add(-72 * time.Hour)
)

// pruneFake is the Host a prune test reads: one listing per ReadDir call, the
// Lstat answer per path, and the removals it saw. No path on disk is touched.
type pruneFake struct {
	env        map[string]string
	listings   [][]fs.DirEntry // the last answer repeats
	infos      map[string]fs.FileInfo
	readErr    error
	removeErrs map[string]error
	readDirs   []string
	removed    []string
	// goneAfter makes a path vanish once it has been described that many times:
	// 1 means the listing sees it and the recheck before deletion does not.
	goneAfter map[string]int
	lstats    map[string]int
}

func (f *pruneFake) host() tiersweep.Host {
	return tiersweep.Host{
		Env: func(name string) string { return f.env[name] },
		ReadDir: func(name string) ([]fs.DirEntry, error) {
			f.readDirs = append(f.readDirs, name)
			if f.readErr != nil {
				return nil, f.readErr
			}
			if len(f.listings) == 0 {
				return nil, nil
			}
			i := min(len(f.readDirs)-1, len(f.listings)-1)
			return f.listings[i], nil
		},
		Lstat: func(name string) (fs.FileInfo, error) {
			if f.lstats == nil {
				f.lstats = map[string]int{}
			}
			f.lstats[name]++
			if n, ok := f.goneAfter[name]; ok && f.lstats[name] > n {
				return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
			}
			info, ok := f.infos[name]
			if !ok {
				return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
			}
			return info, nil
		},
		RemoveAll: func(path string) error {
			f.removed = append(f.removed, path)
			return f.removeErrs[path]
		},
	}
}

// newPruneFake is a listing of old and new log directories, a name that only
// contains the pattern, a symlink and a regular file: everything the sweep's
// rule has to tell apart, with /tmp as the TMPDIR.
func newPruneFake(t *testing.T) *pruneFake {
	t.Helper()
	return &pruneFake{
		env: map[string]string{"TMPDIR": "/tmp"},
		listings: [][]fs.DirEntry{{
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.bbb", 8192, pruneOld)),
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.new", 4096, pruneNow.Add(-time.Hour))),
			fs.FileInfoToDirEntry(fakeInfo{name: "tier-sweep.link", mode: fs.ModeSymlink | fs.ModeDir | 0o777, modTime: pruneOld}),
			fs.FileInfoToDirEntry(fakeInfo{name: "tier-sweep.file", size: 12, mode: 0o644, modTime: pruneOld}),
			fs.FileInfoToDirEntry(dirInfo("other-dir", 4096, pruneOld)),
		}},
		infos: map[string]fs.FileInfo{
			"/tmp/tier-sweep.aaa":  dirInfo("tier-sweep.aaa", 4096, pruneOld),
			"/tmp/tier-sweep.bbb":  dirInfo("tier-sweep.bbb", 8192, pruneOld),
			"/tmp/tier-sweep.new":  dirInfo("tier-sweep.new", 4096, pruneNow.Add(-time.Hour)),
			"/tmp/tier-sweep.link": fakeInfo{name: "tier-sweep.link", mode: fs.ModeSymlink | fs.ModeDir | 0o777, modTime: pruneOld},
			"/tmp/tier-sweep.file": fakeInfo{name: "tier-sweep.file", size: 12, mode: 0o644, modTime: pruneOld},
			"/tmp/other-dir":       dirInfo("other-dir", 4096, pruneOld),
		},
	}
}

// pruneDeps is the command's seams over a fake host and the fixed clock.
func pruneDeps(f *pruneFake) tiersweep.Deps {
	return tiersweep.Deps{Host: f.host(), Now: func() time.Time { return pruneNow }}
}

// runPruneFake runs the prune subcommand over the fake and returns what it
// printed.
func runPruneFake(t *testing.T, f *pruneFake, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runPrune(args, &out, &errOut, pruneDeps(f))
	return code, out.String(), errOut.String()
}

// TestPruneDryRunListsAndRemovesNothing: the output names the old tier-sweep
// directories, with age and size and a total, and the dry run calls no
// RemoveAll. The new directory, the symlink, the regular file and the
// differently named directory are not on the list.
func TestPruneDryRunListsAndRemovesNothing(t *testing.T) {
	t.Parallel()
	f := newPruneFake(t)
	code, stdout, stderr := runPruneFake(t, f)
	if code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q, want exit 0 and no stderr", code, stderr)
	}
	want := "tier-sweep: prune /tmp/tier-sweep.aaa age 72h0m0s size 4096 bytes\n" +
		"tier-sweep: prune /tmp/tier-sweep.bbb age 72h0m0s size 8192 bytes\n" +
		"tier-sweep: prune 2 directories, 12288 bytes, older than 48h0m0s in /tmp: nothing removed (--apply deletes them)\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if len(f.removed) != 0 {
		t.Errorf("RemoveAll saw %q, want a dry run to delete nothing", f.removed)
	}
	if !reflect.DeepEqual(f.readDirs, []string{"/tmp"}) {
		t.Errorf("ReadDir read %q, want just the TMPDIR", f.readDirs)
	}
}

// TestPruneApplyRemovesExactlyTheListedDirs: --apply prints the same candidate
// lines the dry run printed and hands RemoveAll exactly those paths, in that
// order. A prune that deleted everything under the directory, the new one and
// the link included, fails here.
func TestPruneApplyRemovesExactlyTheListedDirs(t *testing.T) {
	t.Parallel()
	_, dry, _ := runPruneFake(t, newPruneFake(t))
	f := newPruneFake(t)
	code, stdout, stderr := runPruneFake(t, f, "--apply")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q, want exit 0 and no stderr", code, stderr)
	}
	wantRemoved := []string{"/tmp/tier-sweep.aaa", "/tmp/tier-sweep.bbb"}
	if !reflect.DeepEqual(f.removed, wantRemoved) {
		t.Errorf("RemoveAll saw %q, want exactly the listed directories %q", f.removed, wantRemoved)
	}
	dryLines, applyLines := strings.Split(dry, "\n"), strings.Split(stdout, "\n")
	if len(applyLines) != len(dryLines) {
		t.Fatalf("apply printed %d lines, the dry run %d: %q", len(applyLines)-1, len(dryLines)-1, stdout)
	}
	for i := range applyLines {
		if i == len(applyLines)-2 {
			continue // the total line, which differs by design
		}
		if applyLines[i] != dryLines[i] {
			t.Errorf("line %d = %q, the dry run's %q", i+1, applyLines[i], dryLines[i])
		}
	}
	wantTotal := "tier-sweep: prune 2 directories, 12288 bytes, older than 48h0m0s in /tmp: removed 2 of 2"
	if got := applyLines[len(applyLines)-2]; got != wantTotal {
		t.Errorf("total line = %q, want %q", got, wantTotal)
	}
}

// TestPruneRefusesBadFlags: a command line it cannot read exits 2 and reads and
// deletes nothing, so a typo cannot reach the directories.
func TestPruneRefusesBadFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an unknown flag", []string{"--force"}, "unknown argument --force"},
		{"a zero age", []string{"--older-than", "0"}, "--older-than 0: want a positive age"},
		{"an age that is not a duration", []string{"--older-than", "abc"}, "--older-than abc: want a duration such as 48h"},
		{"a flag with no value", []string{"--dir"}, "--dir needs a value"},
		{"an empty directory", []string{"--dir="}, "--dir needs a directory"},
		{"--apply with a value", []string{"--apply=yes"}, "unknown argument --apply=yes"},
		{"a tier name", []string{"shell"}, "unknown argument shell"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPruneFake(t)
			code, stdout, stderr := runPruneFake(t, f, tc.args...)
			if code != tiersweep.UsageExit {
				t.Errorf("exit = %d, want the usage exit %d", code, tiersweep.UsageExit)
			}
			if !strings.Contains(stderr, tc.want) || !strings.Contains(stderr, "usage: tier-sweep prune") {
				t.Errorf("stderr = %q, want it to name %q and the usage", stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing listed for a refused command line", stdout)
			}
			if len(f.readDirs) != 0 || len(f.removed) != 0 {
				t.Errorf("read %q and removed %q, want nothing touched", f.readDirs, f.removed)
			}
		})
	}
}

// TestPruneListsTheDirectoryAndAgeItWasGiven: the listing is TMPDIR's unless
// --dir names another, and --older-than replaces the sweep's 48h.
func TestPruneListsTheDirectoryAndAgeItWasGiven(t *testing.T) {
	t.Parallel()
	young := pruneNow.Add(-time.Hour)
	cases := []struct {
		name      string
		env       map[string]string
		args      []string
		wantDir   string
		wantLine  []string
		wantTotal string
	}{
		{
			name:      "TMPDIR when the command line names no directory",
			env:       map[string]string{"TMPDIR": "/var/tmp"},
			wantDir:   "/var/tmp",
			wantLine:  []string{"tier-sweep: prune /var/tmp/tier-sweep.older age 72h0m0s size 4096 bytes"},
			wantTotal: "tier-sweep: prune 1 directory, 4096 bytes, older than 48h0m0s in /var/tmp:",
		},
		{
			name:      "the default temp directory when TMPDIR is unset",
			env:       map[string]string{},
			wantDir:   tiersweep.DefaultTempDir,
			wantLine:  []string{"tier-sweep: prune /tmp/tier-sweep.older age 72h0m0s size 4096 bytes"},
			wantTotal: "tier-sweep: prune 1 directory, 4096 bytes, older than 48h0m0s in /tmp:",
		},
		{
			name:      "--dir and --older-than",
			env:       map[string]string{"TMPDIR": "/tmp"},
			args:      []string{"--dir", "/var/tmp", "--older-than", "2h"},
			wantDir:   "/var/tmp",
			wantLine:  []string{"tier-sweep: prune /var/tmp/tier-sweep.older age 72h0m0s size 4096 bytes"},
			wantTotal: "tier-sweep: prune 1 directory, 4096 bytes, older than 2h0m0s in /var/tmp:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := tc.wantDir
			f := &pruneFake{
				env: tc.env,
				listings: [][]fs.DirEntry{{
					fs.FileInfoToDirEntry(dirInfo("tier-sweep.older", 4096, pruneOld)),
					fs.FileInfoToDirEntry(dirInfo("tier-sweep.young", 4096, young)),
				}},
				infos: map[string]fs.FileInfo{
					dir + "/tier-sweep.older": dirInfo("tier-sweep.older", 4096, pruneOld),
					dir + "/tier-sweep.young": dirInfo("tier-sweep.young", 4096, young),
				},
			}
			code, stdout, stderr := runPruneFake(t, f, tc.args...)
			if code != 0 || stderr != "" {
				t.Fatalf("code %d, stderr %q, want exit 0 and no stderr", code, stderr)
			}
			if !reflect.DeepEqual(f.readDirs, []string{dir}) {
				t.Errorf("ReadDir read %q, want %q", f.readDirs, dir)
			}
			if !strings.Contains(stdout, strings.Join(tc.wantLine, "\n")) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tc.wantLine)
			}
			if strings.Contains(stdout, "tier-sweep.young") {
				t.Errorf("stdout = %q, want the directory under --older-than left out", stdout)
			}
			if !strings.Contains(stdout, tc.wantTotal) {
				t.Errorf("stdout = %q, want a total line %q", stdout, tc.wantTotal)
			}
		})
	}
}

// TestPruneRefusesAHostMissingACall: a host without the call a command line
// needs exits 1 with the gap named, rather than falling back to os. A host with
// no RemoveAll still lists in a dry run, which deletes nothing, and a host with
// no Env still lists the directory --dir named.
func TestPruneRefusesAHostMissingACall(t *testing.T) {
	t.Parallel()
	t.Run("no RemoveAll under --apply", func(t *testing.T) {
		t.Parallel()
		f := newPruneFake(t)
		host := f.host()
		host.RemoveAll = nil
		var stdout, stderr bytes.Buffer
		if code := runPrune(nil, &stdout, &stderr, tiersweep.Deps{Host: host, Now: func() time.Time { return pruneNow }}); code != 0 {
			t.Errorf("dry run exit = %d, want 0: it deletes nothing (stderr %q)", code, stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		code := runPrune([]string{"--apply"}, &stdout, &stderr, tiersweep.Deps{Host: host, Now: func() time.Time { return pruneNow }})
		if code != pruneErrorExit {
			t.Errorf("--apply exit = %d, want %d for a host that cannot delete", code, pruneErrorExit)
		}
		if stdout.String() != "" {
			t.Errorf("stdout = %q, want no list and no total for a refused --apply", stdout.String())
		}
		if !strings.Contains(stderr.String(), "missing a call it needs to list, describe and delete") {
			t.Errorf("stderr = %q, want it to name what the host cannot do", stderr.String())
		}
	})
	t.Run("no ReadDir and no Lstat", func(t *testing.T) {
		t.Parallel()
		for _, drop := range []string{"ReadDir", "Lstat"} {
			f := newPruneFake(t)
			host := f.host()
			if drop == "ReadDir" {
				host.ReadDir = nil
			} else {
				host.Lstat = nil
			}
			var stdout, stderr bytes.Buffer
			code := runPrune(nil, &stdout, &stderr, tiersweep.Deps{Host: host, Now: func() time.Time { return pruneNow }})
			if code != pruneErrorExit {
				t.Errorf("without %s: exit = %d, want %d", drop, code, pruneErrorExit)
			}
			if len(f.readDirs) != 0 || len(f.removed) != 0 {
				t.Errorf("without %s: read %q and removed %q, want nothing touched", drop, f.readDirs, f.removed)
			}
		}
	})
	t.Run("no Env and no --dir", func(t *testing.T) {
		t.Parallel()
		f := newPruneFake(t)
		host := f.host()
		host.Env = nil
		var stdout, stderr bytes.Buffer
		code := runPrune(nil, &stdout, &stderr, tiersweep.Deps{Host: host, Now: func() time.Time { return pruneNow }})
		if code != pruneErrorExit {
			t.Errorf("exit = %d, want %d for a host that cannot read TMPDIR", code, pruneErrorExit)
		}
		if len(f.readDirs) != 0 || len(f.removed) != 0 {
			t.Errorf("read %q and removed %q, want nothing touched", f.readDirs, f.removed)
		}
	})
	t.Run("no Env but --dir names the directory", func(t *testing.T) {
		t.Parallel()
		f := newPruneFake(t)
		host := f.host()
		host.Env = nil
		var stdout, stderr bytes.Buffer
		code := runPrune([]string{"--dir", "/tmp"}, &stdout, &stderr, tiersweep.Deps{Host: host, Now: func() time.Time { return pruneNow }})
		if code != 0 {
			t.Errorf("exit = %d, want 0: --dir needs no TMPDIR (stderr %q)", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "tier-sweep: prune /tmp/tier-sweep.aaa age ") {
			t.Errorf("stdout = %q, want the named directory's list", stdout.String())
		}
	})
}

// TestPruneReportsFailure: a listing it cannot read and a removal that fails
// exit 1 with the failure named, and the removal that worked is still in the
// count. A directory removed though the list did not name it is a failure too:
// the list the operator approved is the whole of what a prune may delete.
func TestPruneReportsFailure(t *testing.T) {
	t.Parallel()
	t.Run("the listing fails", func(t *testing.T) {
		t.Parallel()
		f := newPruneFake(t)
		f.readErr = errors.New("permission denied")
		code, stdout, stderr := runPruneFake(t, f)
		if code != pruneErrorExit {
			t.Errorf("exit = %d, want %d", code, pruneErrorExit)
		}
		if !strings.Contains(stderr, "tier-sweep: prune /tmp: permission denied") {
			t.Errorf("stderr = %q, want the read failure", stderr)
		}
		if stdout != "" || len(f.removed) != 0 {
			t.Errorf("stdout %q, removed %q, want nothing listed and nothing deleted", stdout, f.removed)
		}
	})
	t.Run("one removal fails", func(t *testing.T) {
		t.Parallel()
		f := newPruneFake(t)
		f.removeErrs = map[string]error{"/tmp/tier-sweep.aaa": errors.New("busy")}
		code, stdout, stderr := runPruneFake(t, f, "--apply")
		if code != pruneErrorExit {
			t.Errorf("exit = %d, want %d", code, pruneErrorExit)
		}
		for _, want := range []string{
			"tier-sweep: prune /tmp: busy",
			"tier-sweep: prune /tmp/tier-sweep.aaa: not removed",
			"tier-sweep: prune 2 directories, 12288 bytes, older than 48h0m0s in /tmp: removed 1 of 2",
		} {
			if !strings.Contains(stdout, want) && !strings.Contains(stderr, want) {
				t.Errorf("output %q / %q, want it to contain %q", stdout, stderr, want)
			}
		}
		if want := []string{"/tmp/tier-sweep.aaa", "/tmp/tier-sweep.bbb"}; !reflect.DeepEqual(f.removed, want) {
			t.Errorf("RemoveAll saw %q, want every candidate tried %q", f.removed, want)
		}
	})
	t.Run("a listed directory another sweep removed first", func(t *testing.T) {
		t.Parallel()
		// bbb is on the list the operator read and is gone when the deletion
		// checks it again: no error is raised, and the list the operator read is
		// still not what happened, so the exit is 1 and bbb is named.
		f := &pruneFake{
			env: map[string]string{"TMPDIR": "/tmp"},
			listings: [][]fs.DirEntry{{
				fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
				fs.FileInfoToDirEntry(dirInfo("tier-sweep.bbb", 4096, pruneOld)),
			}},
			infos: map[string]fs.FileInfo{
				"/tmp/tier-sweep.aaa": dirInfo("tier-sweep.aaa", 4096, pruneOld),
				"/tmp/tier-sweep.bbb": dirInfo("tier-sweep.bbb", 4096, pruneOld),
			},
			goneAfter: map[string]int{"/tmp/tier-sweep.bbb": 1},
		}
		code, stdout, stderr := runPruneFake(t, f, "--apply")
		if code != pruneErrorExit {
			t.Errorf("exit = %d, want %d when a listed directory was not removed", code, pruneErrorExit)
		}
		want := "tier-sweep: prune /tmp/tier-sweep.bbb: not removed"
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
		if !strings.Contains(stdout, "removed 1 of 2") {
			t.Errorf("stdout = %q, want the count of what was removed", stdout)
		}
		if want := []string{"/tmp/tier-sweep.aaa"}; !reflect.DeepEqual(f.removed, want) {
			t.Errorf("RemoveAll saw %q, want only %q", f.removed, want)
		}
	})
}

// TestPruneApplyNeverListsAgain: --apply deletes the paths the run printed and
// reads the directory once. A directory that appears after the listing is not
// on the list the operator read, and it survives. The fake would show a second
// listing with tier-sweep.bbb in it, and the apply never asks for it.
// Mutation: list again inside the apply (the old tiersweep.Prune call) and
// RemoveAll sees bbb and the read count is 2.
func TestPruneApplyNeverListsAgain(t *testing.T) {
	t.Parallel()
	f := &pruneFake{
		env: map[string]string{"TMPDIR": "/tmp"},
		listings: [][]fs.DirEntry{
			{fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld))},
			{
				fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
				fs.FileInfoToDirEntry(dirInfo("tier-sweep.bbb", 4096, pruneOld)),
			},
		},
		infos: map[string]fs.FileInfo{
			"/tmp/tier-sweep.aaa": dirInfo("tier-sweep.aaa", 4096, pruneOld),
			"/tmp/tier-sweep.bbb": dirInfo("tier-sweep.bbb", 4096, pruneOld),
		},
	}
	code, stdout, stderr := runPruneFake(t, f, "--apply")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if want := []string{"/tmp/tier-sweep.aaa"}; !reflect.DeepEqual(f.removed, want) {
		t.Errorf("RemoveAll saw %q, want only the listed %q", f.removed, want)
	}
	if len(f.readDirs) != 1 {
		t.Errorf("the directory was read %d times, want once (the listing the operator read)", len(f.readDirs))
	}
	if !strings.Contains(stdout, "removed 1 of 1") {
		t.Errorf("stdout = %q, want removed 1 of 1", stdout)
	}
}

// A listed directory swapped for a symlink between the listing and the apply is
// not deleted through the link: the recheck sees the link and leaves it.
func TestPruneApplySkipsAListedDirectoryThatBecameALink(t *testing.T) {
	t.Parallel()
	f := &pruneFake{
		env: map[string]string{"TMPDIR": "/tmp"},
		listings: [][]fs.DirEntry{{
			fs.FileInfoToDirEntry(dirInfo("tier-sweep.aaa", 4096, pruneOld)),
		}},
		infos: map[string]fs.FileInfo{
			"/tmp/tier-sweep.aaa": dirInfo("tier-sweep.aaa", 4096, pruneOld),
		},
	}
	// The listing's own Lstat sees a directory; the second description sees a link.
	calls := 0
	h := f.host()
	base := h.Lstat
	h.Lstat = func(name string) (fs.FileInfo, error) {
		calls++
		if calls > 1 {
			return fakeInfo{name: "tier-sweep.aaa", mode: fs.ModeSymlink | fs.ModeDir | 0o777, modTime: pruneOld}, nil
		}
		return base(name)
	}
	var out, errOut bytes.Buffer
	code := runPrune([]string{"--apply"}, &out, &errOut, tiersweep.Deps{Host: h, Now: func() time.Time { return pruneNow }})
	if code != pruneErrorExit {
		t.Errorf("exit = %d, want %d for a listed directory that became a link", code, pruneErrorExit)
	}
	if len(f.removed) != 0 {
		t.Errorf("RemoveAll saw %q, want nothing deleted through a link", f.removed)
	}
	if !strings.Contains(errOut.String(), "no longer matches the list") {
		t.Errorf("stderr = %q, want the changed directory named", errOut.String())
	}
}

// TestRunDispatchesPruneAndSweep: "prune" as the first argument reaches the
// prune subcommand and every other command line reaches the sweep unchanged.
func TestRunDispatchesPruneAndSweep(t *testing.T) {
	t.Parallel()
	f := newPruneFake(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"prune"}, &stdout, &stderr, pruneDeps(f)); code != 0 {
		t.Fatalf("prune exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "tier-sweep: prune /tmp/tier-sweep.aaa age ") {
		t.Errorf("stdout = %q, want the prune's list", stdout.String())
	}
	if strings.Contains(stdout.String(), "GREEN") {
		t.Errorf("stdout = %q, want a prune to run no tier", stdout.String())
	}

	f = newPruneFake(t)
	logDir := t.TempDir()
	sweep := tiersweep.Deps{
		Host: tiersweep.Host{
			Env:      func(name string) string { return map[string]string{tiersweep.EnvLogDir: logDir}[name] },
			MkdirAll: func(string, fs.FileMode) error { return nil },
		},
		Now: func() time.Time { return pruneNow },
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"bogus"}, &stdout, &stderr, sweep); code != tiersweep.UsageExit {
		t.Fatalf("sweep exit = %d, want the usage exit %d", code, tiersweep.UsageExit)
	}
	if !strings.Contains(stderr.String(), "unknown tier bogus") {
		t.Errorf("stderr = %q, want the sweep's own refusal", stderr.String())
	}
	if len(f.readDirs) != 0 {
		t.Errorf("the sweep read %q, want the prune reached only for the prune command line", f.readDirs)
	}
}
