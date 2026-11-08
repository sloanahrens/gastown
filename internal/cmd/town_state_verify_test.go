package cmd

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/townstate"
	"github.com/steveyegge/gastown/internal/workspace"
)

// writeTownStateFile makes dirs and a file under town, so a test town holds a path
// without touching the host.
func writeTownStateFile(t *testing.T, town, rel string) string {
	t.Helper()
	p := filepath.Join(town, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeTownStateRegistry writes mayor/town.json as an empty registry. verify
// reads that file for the rig names, so it must be a JSON object.
func writeTownStateRegistry(t *testing.T, town string) string {
	t.Helper()
	p := writeTownStateFile(t, town, "mayor/town.json")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ageTownStatePath sets p's mtime outside the recent window, so an old session write
// does not read as the daemon's.
func ageTownStatePath(t *testing.T, p string, old time.Time) {
	t.Helper()
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

// townStateLineHolds reports whether the printed report holds a line that
// names p and holds reason.
func townStateLineHolds(printed, p, reason string) bool {
	for _, line := range strings.Split(printed, "\n") {
		if strings.HasPrefix(strings.TrimPrefix(line, "info: "), p+": ") && strings.Contains(line, reason) {
			return true
		}
	}
	return false
}

// TestTownStateVerifyNamesStrayPaths drives what `gt town state verify` runs:
// a path under no entry, a recently written session-classed path, and two
// entries overlapping with different owners are each named, and a clean town
// passes.
func TestTownStateVerifyNamesStrayPaths(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-48 * time.Hour)

	town := t.TempDir()
	writeTownStateRegistry(t, town)
	writeTownStateFile(t, town, "settings/config.json")
	writeTownStateFile(t, town, ".runtime/jev/run.json")
	for _, p := range []string{
		writeTownStateFile(t, town, "gastown/crew/eagle/notes.md"),
		writeTownStateFile(t, town, "logs/keep/state.json"),
		writeTownStateFile(t, town, "stray/unlisted.txt"),
		writeTownStateFile(t, town, ".runtime/wren/notes.txt"),
	} {
		ageTownStatePath(t, p, old)
	}
	for _, dir := range []string{"gastown", "gastown/crew", "gastown/crew/eagle", "logs", "logs/keep"} {
		ageTownStatePath(t, filepath.Join(town, filepath.FromSlash(dir)), old)
	}
	// A daemon subsystem directory written recently under a bird's tree: the
	// shape a daemon write takes when it lands in a session directory.
	recentWrite := ".runtime/wren/usage/report.json"
	writeTownStateFile(t, town, recentWrite)
	// The live session's own recent work, under names no daemon subsystem
	// uses. Metadata cannot tell it from a daemon write by time alone, so verify
	// must not report it.
	ownWork := []string{".runtime/wren/notes.json", "gastown/crew/eagle/work.md"}
	for _, p := range ownWork {
		writeTownStateFile(t, town, p)
	}

	m := &townstate.Manifest{Entries: []townstate.Entry{
		{Path: "mayor", Owner: townstate.OwnerDaemon, Why: "town config"},
		{Path: "settings", Owner: townstate.OwnerShared, Why: "edited by gt config daemon"},
		{Path: ".runtime/jev", Owner: townstate.OwnerDaemon, Why: "jev dataset"},
		{Path: ".runtime/usage", Owner: townstate.OwnerDaemon, Why: "usage records"},
		{Path: ".runtime/wren", Owner: townstate.OwnerSession, Why: "a bird's own directory"},
		{Path: "gastown/crew", Owner: townstate.OwnerSession, Why: "crew workspaces"},
		{Path: "logs", Owner: townstate.OwnerDaemon, Why: "daemon logs"},
		{Path: "logs/keep", Owner: townstate.OwnerSession, Why: "a second owner over the same tree"},
	}}
	var out bytes.Buffer
	err := verifyTownState(&out, os.DirFS(town), m, now)
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("verifyTownState error = %v, want an ExitCodeError with code 1", err)
	}
	printed := out.String()
	for _, want := range []struct{ path, reason, what string }{
		{"stray", "no manifest entry", "the directory under no manifest entry, named once"},
		{recentWrite, "below the session entry", "the recently written path under a daemon subsystem name, as information"},
		{"logs/keep", "overlaps logs", "the entry overlapping another with a different owner"},
	} {
		if !townStateLineHolds(printed, want.path, want.reason) {
			t.Errorf("verify printed no line naming %s (want %q); output:\n%s", want.what, want.reason, printed)
		}
	}
	if !strings.Contains(printed, "info: "+recentWrite+": ") {
		t.Errorf("the recent write must be printed as information (an info: line); output:\n%s", printed)
	}
	for _, p := range ownWork {
		if townStateLineHolds(printed, p, "below the session entry") {
			t.Errorf("verify reported the live session's own recent write %s; output:\n%s", p, printed)
		}
	}

	// A clean town passes: every path covered, no recent session write, no
	// entry overlap.
	clean := t.TempDir()
	writeTownStateRegistry(t, clean)
	writeTownStateFile(t, clean, "gastown/crew/eagle/notes.md")
	ageTownStatePath(t, filepath.Join(clean, "gastown", "crew", "eagle", "notes.md"), old)
	for _, dir := range []string{"gastown", "gastown/crew", "gastown/crew/eagle"} {
		ageTownStatePath(t, filepath.Join(clean, filepath.FromSlash(dir)), old)
	}
	cleanManifest := &townstate.Manifest{Entries: []townstate.Entry{
		{Path: "mayor", Owner: townstate.OwnerDaemon, Why: "town config"},
		{Path: "gastown/crew", Owner: townstate.OwnerSession, Why: "crew workspaces"},
	}}
	out.Reset()
	if err := verifyTownState(&out, os.DirFS(clean), cleanManifest, now); err != nil {
		t.Fatalf("a clean town failed verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "town state: ok") {
		t.Errorf("a clean town printed %q, want the ok line", out.String())
	}
}

// TestTownStateVerifyCommandFindsTheTownRoot drives the command body over an
// injected town lookup, and pins the lookup the command itself uses.
func TestTownStateVerifyCommandFindsTheTownRoot(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town := t.TempDir()
	writeTownStateRegistry(t, town)
	writeTownStateFile(t, town, "stray/unlisted.txt")

	var out bytes.Buffer
	err := runTownStateVerifyIn(townStateVerifyEnv{
		findTown: func() (string, error) { return town, nil },
		now:      func() time.Time { return now },
		out:      &out,
	})
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("runTownStateVerifyIn error = %v, want an ExitCodeError with code 1", err)
	}
	if !strings.Contains(out.String(), "stray") {
		t.Errorf("runTownStateVerifyIn printed %q, want it to name the stray path", out.String())
	}

	// A town lookup that fails stops the command before it reads anything.
	findErr := errors.New("no town here")
	err = runTownStateVerifyIn(townStateVerifyEnv{
		findTown: func() (string, error) { return "", findErr },
		now:      func() time.Time { return now },
		out:      io.Discard,
	})
	if !errors.Is(err, findErr) {
		t.Errorf("runTownStateVerifyIn error = %v, want it to wrap the lookup's error", err)
	}

	// A town that cannot be read fails the command rather than passing.
	err = verifyTownState(io.Discard, brokenTownStateFS{}, townstate.Default(), now)
	if err == nil || !strings.Contains(err.Error(), "verifying the town root") {
		t.Errorf("verifyTownState error = %v, want it to say the town root could not be verified", err)
	}

	// The command reads the town the way every cwd-based command does.
	got := reflect.ValueOf(townStateVerifyRunEnv(&cobra.Command{}).findTown).Pointer()
	want := reflect.ValueOf(workspace.FindFromCwdOrError).Pointer()
	if got != want {
		t.Error("townStateVerifyRunEnv().findTown is not workspace.FindFromCwdOrError")
	}
}

// townStateFake is an in-memory town root that records every Open, so a test can
// prove verify reads no file contents.
type townStateFake struct {
	dirs     map[string]time.Time
	files    map[string]time.Time
	contents map[string]string
	opened   []string
}

func newTownStateFake() *townStateFake {
	return &townStateFake{
		dirs:     map[string]time.Time{".": time.Now()},
		files:    map[string]time.Time{},
		contents: map[string]string{},
	}
}

func (f *townStateFake) addDir(name string, mtime time.Time) { f.dirs[name] = mtime }

func (f *townStateFake) addFile(name string, mtime time.Time) { f.files[name] = mtime }

// addFileWithContent adds a file whose bytes verify may read: the registry.
func (f *townStateFake) addFileWithContent(name string, mtime time.Time, body string) {
	f.files[name] = mtime
	f.contents[name] = body
}

func (f *townStateFake) Open(name string) (fs.File, error) {
	f.opened = append(f.opened, name)
	if mtime, ok := f.dirs[name]; ok {
		return &townStateDir{f: f, name: name, mtime: mtime}, nil
	}
	if mtime, ok := f.files[name]; ok {
		file := &townStateFile{name: name, mtime: mtime}
		if body, ok := f.contents[name]; ok {
			file.r = strings.NewReader(body)
		}
		return file, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (f *townStateFake) Stat(name string) (fs.FileInfo, error) {
	if mtime, ok := f.dirs[name]; ok {
		return townStateInfo{name: name, dir: true, mtime: mtime}, nil
	}
	if mtime, ok := f.files[name]; ok {
		return townStateInfo{name: name, mtime: mtime}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// entries lists name's children in lexical order.
func (f *townStateFake) entries(name string) []fs.DirEntry {
	prefix := name + "/"
	if name == "." {
		prefix = ""
	}
	var out []fs.DirEntry
	seen := map[string]bool{}
	for p, mtime := range f.dirs {
		if p == "." || !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if strings.Contains(rest, "/") || seen[rest] {
			continue
		}
		seen[rest] = true
		out = append(out, townStateEntry{info: townStateInfo{name: rest, dir: true, mtime: mtime}})
	}
	for p, mtime := range f.files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if strings.Contains(rest, "/") || seen[rest] {
			continue
		}
		seen[rest] = true
		out = append(out, townStateEntry{info: townStateInfo{name: rest, mtime: mtime}})
	}
	sortTownStateEntries(out)
	return out
}

type townStateInfo struct {
	name  string
	dir   bool
	mtime time.Time
}

func (i townStateInfo) Name() string { return i.name }
func (i townStateInfo) Size() int64  { return 0 }
func (i townStateInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i townStateInfo) ModTime() time.Time { return i.mtime }
func (i townStateInfo) IsDir() bool        { return i.dir }
func (i townStateInfo) Sys() any           { return nil }

type townStateEntry struct{ info townStateInfo }

func (e townStateEntry) Name() string               { return e.info.name }
func (e townStateEntry) IsDir() bool                { return e.info.dir }
func (e townStateEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e townStateEntry) Info() (fs.FileInfo, error) { return e.info, nil }

type townStateDir struct {
	f     *townStateFake
	name  string
	mtime time.Time
	done  bool
}

func (d *townStateDir) Stat() (fs.FileInfo, error) {
	return townStateInfo{name: d.name, dir: true, mtime: d.mtime}, nil
}
func (d *townStateDir) Read([]byte) (int, error) { return 0, errors.New("is a directory") }
func (d *townStateDir) Close() error             { return nil }
func (d *townStateDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.done {
		return nil, nil
	}
	d.done = true
	entries := d.f.entries(d.name)
	if n > 0 && len(entries) > n {
		entries = entries[:n]
	}
	return entries, nil
}

type townStateFile struct {
	name  string
	mtime time.Time
	// r holds the bytes of a file whose contents verify may read; nil for any
	// other file, which refuses every read.
	r *strings.Reader
}

func (f *townStateFile) Stat() (fs.FileInfo, error) {
	return townStateInfo{name: f.name, mtime: f.mtime}, nil
}
func (f *townStateFile) Read(b []byte) (int, error) {
	if f.r == nil {
		return 0, errors.New("a file has no content to read here")
	}
	return f.r.Read(b)
}
func (f *townStateFile) Close() error { return nil }

// sortTownStateEntries orders entries by name the way a real ReadDir does.
func sortTownStateEntries(entries []fs.DirEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
}

// TestTownStateVerifyReadsNoContents proves verify looks at metadata only and
// never descends into .dolt-data* or a .git tree. The one file it reads is the
// rig registry, for the rig names; the registry paths are the only ones it may
// open as a regular file.
func TestTownStateVerifyReadsNoContents(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	f := newTownStateFake()
	for _, dir := range []string{
		"mayor", "settings", "gastown", "gastown/crew", "gastown/crew/eagle",
		"gastown/mayor", "gastown/mayor/rig", ".runtime", ".runtime/jev",
		".dolt-data", ".dolt-data/gt", "gastown/.git",
	} {
		f.addDir(dir, old)
	}
	f.addFileWithContent("mayor/town.json", old, "{\"registry\":{\"version\":1,\"rigs\":{\"gastown\":{}}}}\n")
	f.addFile("settings/config.json", old)
	f.addFile("gastown/config.json", old)
	f.addFile(".runtime/jev/run.json", now)
	f.addFile("gastown/crew/eagle/notes.md", old)
	f.addFile(".dolt-data/gt/stray.bin", now)
	f.addFile("gastown/.git/config", now)
	// A Dolt data directory nested under a daemon subsystem is skipped too.
	f.addDir(".runtime/jev/.dolt-data", old)
	f.addFile(".runtime/jev/.dolt-data/stray.bin", now)

	var out bytes.Buffer
	if err := verifyTownState(&out, f, townstate.Default(), now); err != nil {
		t.Fatalf("verifyTownState error = %v\n%s", err, out.String())
	}
	registry := map[string]bool{"mayor/town.json": true, "mayor/rigs.json": true}
	for _, name := range f.opened {
		if _, ok := f.files[name]; ok && !registry[name] {
			t.Errorf("verify opened the regular file %s; it must read metadata only", name)
		}
		if name == ".dolt-data" || strings.HasPrefix(name, ".dolt-data/") || strings.Contains(name, "/.dolt-data") ||
			strings.HasSuffix(name, "/.git") || strings.Contains(name, "/.git/") {
			t.Errorf("verify descended into the skipped path %s", name)
		}
	}
}

// brokenTownStateFS is a town root whose every open fails.
type brokenTownStateFS struct{}

func (brokenTownStateFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

// TestTownStateVerifyInfoIsNotAFailure: a recent change under a session entry
// is information and never a failure, because file metadata cannot say who
// wrote it. A town whose only finding is that exits 0 and still prints the
// finding and the ok line.
func TestTownStateVerifyInfoIsNotAFailure(t *testing.T) {
	t.Parallel()
	now := time.Now()
	town := t.TempDir()
	writeTownStateFile(t, town, ".runtime/wren/usage/report.json")
	m := &townstate.Manifest{Entries: []townstate.Entry{
		{Path: ".runtime/usage", Owner: townstate.OwnerDaemon, Why: "usage records"},
		{Path: ".runtime/wren", Owner: townstate.OwnerSession, Why: "a bird's own directory"},
	}}
	var out bytes.Buffer
	if err := verifyTownState(&out, os.DirFS(town), m, now); err != nil {
		t.Fatalf("an informational finding failed verify: %v\n%s", err, out.String())
	}
	printed := out.String()
	if !strings.Contains(printed, "info: .runtime/wren/usage") || !strings.Contains(printed, "town state: ok") {
		t.Errorf("output = %q, want the info line and the ok line", printed)
	}
}
