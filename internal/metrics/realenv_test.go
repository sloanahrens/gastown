package metrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sameStat reports whether two diff stats match, their floats within 1e-9.
func sameStat(a, b DiffStat) bool {
	return a.LinesAdded == b.LinesAdded && a.LinesDeleted == b.LinesDeleted && a.FilesTouched == b.FilesTouched &&
		math.Abs(a.FileEntropy-b.FileEntropy) < 1e-9 && math.Abs(a.PackageEntropy-b.PackageEntropy) < 1e-9
}

// The cases below wire the extract to a fixture town under t.TempDir() and
// answer every git and bd command from a script, so no case reads ~/gt or
// starts a process.

// realCall is one command the wiring ran.
type realCall struct {
	dir  string
	argv []string
}

// realRunner answers each command with answer and records every call.
type realRunner struct {
	calls  []realCall
	answer func(dir string, argv []string) ([]byte, error)
}

// run is the RunnerDir the wiring takes.
func (r *realRunner) run(_ context.Context, dir string, argv []string) ([]byte, error) {
	r.calls = append(r.calls, realCall{dir: dir, argv: append([]string(nil), argv...)})
	return r.answer(dir, argv)
}

// realFixture is the ported cases' town: gastown has a ledger with a blank line
// and an .om.json history of two commits, one of which git show fails on; gt-a
// has a usage file and an overrun stop; both caches hold one entry. Every other
// rig has no ledger and no history.
func realFixture(t *testing.T) (town, home string) {
	t.Helper()
	town, home = t.TempDir(), t.TempDir()
	townFile(t, town, ".runtime/landings/gastown.jsonl", `{"bead":"gt-a"}`+"\n\n"+`{"bead":"gt-b"}`+"\n")
	townFile(t, town, ".runtime/landings/reviews.jsonl", `{"bead":"gt-a"}`+"\n")
	townFile(t, town, ".runtime/usage/beads/gt-a.work.json",
		`{"models":{"m":{"calls":2}},"seat":"s","end":"2026-10-01T00:00:00Z","sessions":[{},{}]}`)
	townFile(t, town, ".runtime/usage/beads/gt-b.work.json", `{}`)
	townFile(t, town, ".runtime/usage/beads/README", "not a usage file")
	townFile(t, town, ".runtime/overrun-guard/gt-a.json", `{"stop_began_at":"2026-10-01T00:00:00Z"}`)
	townFile(t, town, ".runtime/overrun-guard/gt-b.json", `{}`)
	townFile(t, town, ".runtime/metrics/cache/handoff_bytes.json", `{"gt-a":120}`)
	townFile(t, town, ".runtime/metrics/cache/diffstats.json",
		`{"gastown:abc1234":{"lines_added":3,"lines_deleted":1,"files_touched":2,"file_entropy":1.5,"package_entropy":1}}`)
	return town, home
}

// townFile writes one file of the fixture town, making its directory.
func townFile(t *testing.T, town, rel, content string) {
	t.Helper()
	path := filepath.Join(town, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// realGit answers the fixture's git and bd commands: gastown's history is two
// commits, the second of whose .om.json git show cannot read; every other rig
// has none, and bd lists nothing.
func realGit(town string) func(string, []string) ([]byte, error) {
	return func(_ string, argv []string) ([]byte, error) {
		if len(argv) < 4 {
			return nil, nil
		}
		if argv[3] == "bd" {
			return []byte("[]"), nil
		}
		if len(argv) < 8 || argv[3] != "git" {
			return nil, nil
		}
		switch {
		case argv[6] == "log" && argv[5] == rigRepo(town, "gastown"):
			return []byte("2026-09-01T00:00:00Z aaa1111\n2026-10-01T00:00:00Z bbb2222\n"), nil
		case argv[6] == "log":
			return nil, nil
		case argv[7] == "bbb2222:.om.json":
			return nil, errors.New("unknown revision")
		}
		return []byte(`{"threshold": 0.7}`), nil
	}
}

// realWired builds the fixture town's real environment.
func realWired(t *testing.T, today string) (real RealTown, town, home string, runner *realRunner) {
	t.Helper()
	town, home = realFixture(t)
	runner = &realRunner{answer: realGit(town)}
	real, err := RealEnv(context.Background(), RealEnvOptions{
		Today: today, Town: town, DocsDir: filepath.Join(home, "docs"), Home: home, Run: runner.run,
	})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	return real, town, home, runner
}

func TestRealEnvReadsTheTown(t *testing.T) {
	t.Parallel()
	real, town, home, runner := realWired(t, "smoke")
	if real.Town != town {
		t.Errorf("Town = %q, want %q", real.Town, town)
	}
	if got, want := real.DocsDir, filepath.Join(home, "docs"); got != want {
		t.Errorf("DocsDir = %q, want the argument's %q", got, want)
	}
	if got, want := real.Extract.OutDir, filepath.Join(town, ".runtime", "metrics", "smoke"); got != want {
		t.Errorf("OutDir = %q, want %q", got, want)
	}
	if real.Extract.Today != "smoke" {
		t.Errorf("Today = %q, want smoke", real.Extract.Today)
	}
	if got, want := real.Extract.LedgerLines["gastown"], []string{`{"bead":"gt-a"}`, `{"bead":"gt-b"}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("gastown ledger = %q, want %q", got, want)
	}
	if len(real.Extract.LedgerLines) != 1 {
		t.Errorf("ledger holds %d rigs, want only gastown: %v", len(real.Extract.LedgerLines), real.Extract.LedgerLines)
	}
	wantUnreadable := make([]string, 0, len(Rigs))
	for _, rig := range Rigs {
		if rig != "gastown" {
			wantUnreadable = append(wantUnreadable, "ledger:"+rig)
		}
	}
	if !reflect.DeepEqual(real.Extract.Unreadable, wantUnreadable) {
		t.Errorf("Unreadable = %v, want %v", real.Extract.Unreadable, wantUnreadable)
	}
	if got, want := real.Extract.ReviewLines, []string{`{"bead":"gt-a"}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReviewLines = %q, want %q", got, want)
	}
	usage, ok := real.Extract.UsageFiles["gt-a"]
	if !ok || len(real.Extract.UsageFiles) != 1 {
		t.Fatalf("UsageFiles = %v, want only gt-a: the empty file and the other suffix are not usage", real.Extract.UsageFiles)
	}
	if usage.Models["m"].Calls != 2 || len(usage.Sessions) != 2 {
		t.Errorf("gt-a usage = %+v, want two calls and two sessions", usage)
	}
	if got, want := real.Extract.OverrunStates, map[string]OverrunState{
		"gt-a": {StopBeganAt: "2026-10-01T00:00:00Z"},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("OverrunStates = %v, want only gt-a's stop", got)
	}
	if got, want := real.Extract.OMCommits["gastown"], []OMCommit{
		{At: "2026-09-01T00:00:00Z", Text: `{"threshold": 0.7}`},
		{At: "2026-10-01T00:00:00Z", Text: ""},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("gastown OMCommits = %+v, want %+v", got, want)
	}
	if len(real.Extract.HandoffCache) != 1 || real.Extract.HandoffCache["gt-a"] != 120 {
		t.Errorf("HandoffCache = %v, want the cached gt-a", real.Extract.HandoffCache)
	}
	if !reflect.DeepEqual(real.Extract.PrefixToRig, PrefixToRig) {
		t.Errorf("PrefixToRig = %v, want the town's prefixes", real.Extract.PrefixToRig)
	}
	wantDirs := make([]string, 0, len(Rigs)+2)
	for _, rig := range Rigs {
		wantDirs = append(wantDirs, filepath.Join(town, rig))
	}
	wantDirs = append(wantDirs, town, filepath.Join(home, ".claude"))
	if !reflect.DeepEqual(real.Extract.BeadDirs, wantDirs) {
		t.Errorf("BeadDirs = %q, want %q", real.Extract.BeadDirs, wantDirs)
	}

	// The cached diff is answered from the cache file, so reading it runs
	// nothing: a command here means the cache was not used.
	runner.answer = func(string, []string) ([]byte, error) {
		t.Error("a cached commit's stat ran a command")
		return nil, errors.New("unexpected command")
	}
	stat, err := real.Extract.Diffs.Stats(context.Background(), "gastown", "abc1234")
	if err != nil {
		t.Fatalf("Stats from the cache: %v", err)
	}
	if want := (DiffStat{LinesAdded: 3, LinesDeleted: 1, FilesTouched: 2, FileEntropy: 1.5, PackageEntropy: 1}); !sameStat(stat, want) {
		t.Errorf("cached stat = %+v, want %+v", stat, want)
	}
}

func TestRealEnvLivesWithNoSourceFiles(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	runner := &realRunner{answer: func(string, []string) ([]byte, error) { return nil, nil }}
	real, err := RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Town: town, DocsDir: t.TempDir(), Home: t.TempDir(), Run: runner.run,
	})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	if len(real.Extract.LedgerLines) != 0 || len(real.Extract.UsageFiles) != 0 || len(real.Extract.OverrunStates) != 0 {
		t.Errorf("a town with no sources read %d ledgers, %d usage files and %d overrun states",
			len(real.Extract.LedgerLines), len(real.Extract.UsageFiles), len(real.Extract.OverrunStates))
	}
	if want := len(Rigs) + 1; len(real.Extract.Unreadable) != want {
		t.Errorf("Unreadable = %v, want each of the %d missing ledgers and reviews", real.Extract.Unreadable, want)
	}
}

func TestRealEnvRunsBdAtLowPriority(t *testing.T) {
	t.Parallel()
	real, _, _, runner := realWired(t, "smoke")
	var bids []realCall
	// The ported _run is every command at low priority from the directory the
	// caller named, so bd answers here only when both hold: the check is the
	// runner's, and a call that misses either is bd's failure, not a retry.
	runner.answer = func(dir string, argv []string) ([]byte, error) {
		bids = append(bids, realCall{dir: dir, argv: argv})
		if dir != "/beads/dir" {
			return nil, fmt.Errorf("bd ran in %q, not the directory named", dir)
		}
		if len(argv) < 3 || argv[0] != "nice" || argv[1] != "-n" || argv[2] != "15" {
			return nil, errors.New("bd ran without nice -n 15")
		}
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "bd" && argv[i+1] == "list" {
				return []byte("[]"), nil
			}
		}
		return nil, errors.New("bd ran without the caller's subcommand")
	}
	out, ok := real.Extract.RunBd("/beads/dir", []string{"list", "--all", "--json"})
	if !ok || string(out) != "[]" {
		t.Fatalf("RunBd = %q, %v; want the runner's answer; bids = %+v", out, ok, bids)
	}
	if len(bids) != 1 {
		t.Errorf("RunBd made %d commands, want one", len(bids))
	}

	runner.answer = func(string, []string) ([]byte, error) { return nil, errors.New("bd failed") }
	if _, ok := real.Extract.RunBd("/beads/dir", []string{"list"}); ok {
		t.Error("RunBd reported ok for a failed bd")
	}
}

func TestRealEnvStopsOnAGitLogItCannotRead(t *testing.T) {
	t.Parallel()
	town, home := realFixture(t)
	runner := &realRunner{answer: func(string, []string) ([]byte, error) {
		return nil, errors.New("git failed")
	}}
	_, err := RealEnv(context.Background(), RealEnvOptions{Today: "smoke", Town: town, Home: home, Run: runner.run})
	if err == nil {
		t.Fatal("RealEnv wired a town whose om history it could not read")
	}
	if !strings.Contains(err.Error(), "gastown") || !strings.Contains(err.Error(), "om history") {
		t.Errorf("error %q names neither the rig nor the om history", err)
	}
}

func TestRealEnvStopsOnAMalformedHistoryLine(t *testing.T) {
	t.Parallel()
	town, home := realFixture(t)
	runner := &realRunner{answer: func(_ string, argv []string) ([]byte, error) {
		if len(argv) > 6 && argv[6] == "log" && argv[5] == rigRepo(town, "gastown") {
			return []byte("just-a-time\n"), nil
		}
		return nil, nil
	}}
	_, err := RealEnv(context.Background(), RealEnvOptions{Today: "smoke", Town: town, Home: home, Run: runner.run})
	if err == nil || !strings.Contains(err.Error(), "not a time and a commit") {
		t.Errorf("RealEnv error = %v, want the malformed line refused", err)
	}
}

func TestRealEnvNeedsARunner(t *testing.T) {
	t.Parallel()
	if _, err := RealEnv(context.Background(), RealEnvOptions{Town: t.TempDir()}); err == nil {
		t.Fatal("RealEnv wired a town with no runner")
	}
}

func TestRealEnvStopsWhenTheHomeCannotBeFound(t *testing.T) {
	t.Parallel()
	runner := &realRunner{answer: func(string, []string) ([]byte, error) { return nil, nil }}
	_, err := RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Town: t.TempDir(), DocsDir: t.TempDir(), Run: runner.run,
		HomeDir: func() (string, error) { return "", errors.New("no home directory") },
	})
	if err == nil || !strings.Contains(err.Error(), "no home directory") {
		t.Errorf("RealEnv error = %v, want the home lookup refused", err)
	}
}

func TestRealEnvDoesNotWriteTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	runner := &realRunner{answer: func(string, []string) ([]byte, error) { return nil, nil }}
	if _, err := RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Town: town, DocsDir: t.TempDir(), Home: t.TempDir(), Run: runner.run,
	}); err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	// The wiring reads the town; the extract's writes and the caches' are its
	// callers', so wiring a town leaves it exactly as it was. This is what lets
	// the hourly smoke run over the live town.
	entries, err := os.ReadDir(town)
	if err != nil {
		t.Fatalf("list %s: %v", town, err)
	}
	if len(entries) != 0 {
		t.Errorf("wiring the town made %d entries in it: %v", len(entries), entries)
	}
}

func TestRealEnvStopsOnASourceDirectoryItCannotList(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, rel, want string }{
		{"usage", ".runtime/usage/beads", "usage"},
		{"overrun", ".runtime/overrun-guard", "overrun"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			path := filepath.Join(town, tc.rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("make %s: %v", filepath.Dir(path), err)
			}
			// A link to itself: every read of it fails, and not as a missing path.
			if err := os.Symlink(path, path); err != nil {
				t.Fatalf("symlink %s: %v", path, err)
			}
			runner := &realRunner{answer: func(string, []string) ([]byte, error) { return nil, nil }}
			_, err := RealEnv(context.Background(), RealEnvOptions{
				Today: "smoke", Town: town, DocsDir: t.TempDir(), Home: t.TempDir(), Run: runner.run,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("RealEnv error = %v, want the %s directory refused", err, tc.want)
			}
		})
	}
}

func TestRealEnvListsNothingWhereAFileSits(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	townFile(t, town, ".runtime/usage/beads", "not a directory")
	townFile(t, town, ".runtime/overrun-guard", "not a directory")
	runner := &realRunner{answer: func(string, []string) ([]byte, error) { return nil, nil }}
	real, err := RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Town: town, DocsDir: t.TempDir(), Home: t.TempDir(), Run: runner.run,
	})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	if len(real.Extract.UsageFiles) != 0 || len(real.Extract.OverrunStates) != 0 {
		t.Errorf("a source path holding a file listed %d usage files and %d overrun states",
			len(real.Extract.UsageFiles), len(real.Extract.OverrunStates))
	}
}

// seamFiles is a RealFiles over a town that exists only in these maps: files by
// path, and the entries each directory lists. A path in neither map reads as
// absent, except a file's own path, which lists no entries as a file does.
type seamFiles struct {
	files map[string]string
	dirs  map[string][]DirEntry
	errs  map[string]error
}

// seam is the file seam the wiring takes.
func (f seamFiles) seam() RealFiles {
	return RealFiles{
		ReadFile: func(path string) ([]byte, error) {
			if err, ok := f.errs[path]; ok {
				return nil, err
			}
			data, ok := f.files[path]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(data), nil
		},
		ReadDir: func(dir string) ([]DirEntry, error) {
			if err, ok := f.errs[dir]; ok {
				return nil, err
			}
			if entries, ok := f.dirs[dir]; ok {
				return entries, nil
			}
			if _, isFile := f.files[dir]; isFile {
				return nil, nil
			}
			return nil, fs.ErrNotExist
		},
	}
}

// seamWired builds the wiring over files with one rig's ledger added, so every
// case has a town to read and a case that reads the host's filesystem instead
// finds nothing.
func seamWired(t *testing.T, files seamFiles) (RealTown, error) {
	t.Helper()
	if files.files == nil {
		files.files = map[string]string{}
	}
	if files.dirs == nil {
		files.dirs = map[string][]DirEntry{}
	}
	files.files[filepath.Join(townOfSeam, ".runtime", "landings", "gastown.jsonl")] = `{"bead":"gt-a"}` + "\n"
	runner := &realRunner{answer: realGit(townOfSeam)}
	return RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Town: townOfSeam, DocsDir: "/docs", Home: "/home", Files: files.seam(), Run: runner.run,
	})
}

// townOfSeam is the town of the file-seam cases: a path no host has.
const townOfSeam = "/seam-town"

func TestRealEnvReadsThroughItsFileSeam(t *testing.T) {
	t.Parallel()
	files := seamFiles{
		files: map[string]string{
			filepath.Join(townOfSeam, ".runtime", "landings", "reviews.jsonl"):          `{"bead":"gt-a"}` + "\n",
			filepath.Join(townOfSeam, ".runtime", "usage", "beads", "gt-a.work.json"):   `{"models":{"m":{"calls":2}}}`,
			filepath.Join(townOfSeam, ".runtime", "overrun-guard", "gt-a.json"):         `{"stop_began_at":"2026-10-01T00:00:00Z"}`,
			filepath.Join(townOfSeam, ".runtime", "metrics", "cache", handoffCacheName): `{"gt-a":120}`,
		},
		dirs: map[string][]DirEntry{
			filepath.Join(townOfSeam, ".runtime", "usage", "beads"): {{Name: "gt-a.work.json"}},
			filepath.Join(townOfSeam, ".runtime", "overrun-guard"):  {{Name: "gt-a.json"}},
		},
	}
	real, err := seamWired(t, files)
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	if got, want := real.Extract.LedgerLines["gastown"], []string{`{"bead":"gt-a"}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("gastown ledger = %q, want the seam's %q", got, want)
	}
	if len(real.Extract.UsageFiles) != 1 || len(real.Extract.OverrunStates) != 1 {
		t.Errorf("read %d usage files and %d overrun states through the seam, want one of each",
			len(real.Extract.UsageFiles), len(real.Extract.OverrunStates))
	}
	if real.Extract.HandoffCache["gt-a"] != 120 {
		t.Errorf("HandoffCache = %v, want the seam's gt-a", real.Extract.HandoffCache)
	}
}

func TestRealEnvSkipsSourceFilesItCannotUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		usage, overrun string
	}{
		{"malformed", `{"models":`, `{"stop_began_at":`},
		{"wrong shape", `{"models":"not an object"}`, `{"stop_began_at":[1]}`},
		{"empty object", `{}`, `{}`},
		{"null", `null`, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			usagePath := filepath.Join(townOfSeam, ".runtime", "usage", "beads", "gt-a.work.json")
			overrunPath := filepath.Join(townOfSeam, ".runtime", "overrun-guard", "gt-a.json")
			files := seamFiles{
				files: map[string]string{usagePath: tc.usage, overrunPath: tc.overrun},
				dirs: map[string][]DirEntry{
					filepath.Dir(usagePath):   {{Name: "gt-a.work.json"}},
					filepath.Dir(overrunPath): {{Name: "gt-a.json"}},
				},
			}
			real, err := seamWired(t, files)
			if err != nil {
				t.Fatalf("RealEnv: %v", err)
			}
			// The ported real_env leaves such a file out rather than reading it
			// as an empty record: the footer counts what it read, and a file it
			// could not use is not a bead.
			if len(real.Extract.UsageFiles) != 0 {
				t.Errorf("UsageFiles = %v, want the %s file left out", real.Extract.UsageFiles, tc.name)
			}
			if len(real.Extract.OverrunStates) != 0 {
				t.Errorf("OverrunStates = %v, want the %s file left out", real.Extract.OverrunStates, tc.name)
			}
		})
	}
}

func TestRealEnvReadsAMissingCacheAsEmpty(t *testing.T) {
	t.Parallel()
	real, err := seamWired(t, seamFiles{})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	if real.Extract.HandoffCache == nil || len(real.Extract.HandoffCache) != 0 {
		t.Errorf("HandoffCache = %v, want an empty map for a town with no cache yet", real.Extract.HandoffCache)
	}
	if cache := real.Extract.Diffs.(*GitDiffSource).Cache; len(cache) != 0 {
		t.Errorf("diff cache = %v, want empty for a town with no cache yet", cache)
	}
}

func TestRealEnvRefusesACacheItCannotDecode(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, cache string }{
		{"handoff", handoffCacheName},
		{"diffstats", diffstatsCacheName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(townOfSeam, ".runtime", "metrics", "cache", tc.cache)
			files := seamFiles{files: map[string]string{path: `{"gt-a":`}}
			_, err := seamWired(t, files)
			// A cache that is there and cannot be decoded is corruption, not a
			// first run: read as empty it would silently cost every commit's
			// git show again on every later run.
			if err == nil || !strings.Contains(err.Error(), tc.cache) {
				t.Errorf("RealEnv error = %v, want %s named", err, tc.cache)
			}
		})
	}
}

func TestRealEnvTownDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, town, fromEnv, home, want string
	}{
		{"the argument wins", "/t", "/e", "/h", "/t"},
		{"the environment next", "", "/e", "/h", "/e"},
		{"the home's gt last", "", "", "/h", "/h/gt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := townRoot(tc.town, tc.fromEnv, tc.home); got != tc.want {
				t.Errorf("townRoot(%q, %q, %q) = %q, want %q", tc.town, tc.fromEnv, tc.home, got, tc.want)
			}
		})
	}
}

func TestRealEnvDocsDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, docs, fromEnv, home, want string
	}{
		{"the argument wins", "/d", "/e", "/h", "/d"},
		{"the environment next", "", "/e", "/h", "/e"},
		{"the home's docs last", "", "", "/h", "/h/.claude/docs/metrics"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := docsDir(tc.docs, tc.fromEnv, tc.home); got != tc.want {
				t.Errorf("docsDir(%q, %q, %q) = %q, want %q", tc.docs, tc.fromEnv, tc.home, got, tc.want)
			}
		})
	}
}

func TestRealEnvDefaultsFromTheEnvironment(t *testing.T) {
	t.Parallel()
	town, home := realFixture(t)
	docs := filepath.Join(home, "docs-env")
	fromEnv := map[string]string{envTown: town, envDocs: docs}
	runner := &realRunner{answer: realGit(town)}
	real, err := RealEnv(context.Background(), RealEnvOptions{Today: "smoke", Home: home, Run: runner.run,
		Getenv: func(name string) string { return fromEnv[name] }})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	if real.Town != town {
		t.Errorf("Town = %q, want %s's %s", real.Town, envTown, town)
	}
	if real.DocsDir != docs {
		t.Errorf("DocsDir = %q, want %s's %s", real.DocsDir, envDocs, docs)
	}
	if want := filepath.Join(home, ".claude"); real.Extract.BeadDirs[len(real.Extract.BeadDirs)-1] != want {
		t.Errorf("last bead dir = %q, want the home's %q", real.Extract.BeadDirs[len(real.Extract.BeadDirs)-1], want)
	}
}

// realCacheEnv is an extract env whose diff source holds one cached commit and
// whose handoff cache holds one bead.
func realCacheEnv() ExtractEnv {
	return ExtractEnv{
		Diffs: &GitDiffSource{Cache: DiffCache{
			{Rig: "gastown", Commit: "abc1234"}: {
				LinesAdded: 9, LinesDeleted: 1, FilesTouched: 2, FileEntropy: 2.5, PackageEntropy: 1,
			},
		}},
		HandoffCache: map[string]int{"gt-a": 55},
	}
}

func TestSaveCachesWritesBothCaches(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := SaveCaches(realCacheEnv(), town); err != nil {
		t.Fatalf("SaveCaches: %v", err)
	}
	cacheDir := filepath.Join(town, ".runtime", "metrics", "cache")
	handoff, err := readCache[map[string]int](hostFiles(), filepath.Join(cacheDir, handoffCacheName))
	if err != nil || !reflect.DeepEqual(handoff, map[string]int{"gt-a": 55}) {
		t.Errorf("%s = %v, %v; want the bead's bytes", handoffCacheName, handoff, err)
	}
	docs, err := readCache[map[string]diffStatDoc](hostFiles(), filepath.Join(cacheDir, diffstatsCacheName))
	want := map[string]diffStatDoc{"gastown:abc1234": {
		LinesAdded: 9, LinesDeleted: 1, FilesTouched: 2, FileEntropy: 2.5, PackageEntropy: 1,
	}}
	if err != nil || !reflect.DeepEqual(docs, want) {
		t.Errorf("%s = %v, %v; want %v", diffstatsCacheName, docs, err, want)
	}
}

func TestRealEnvReadsCachesThatSaveCachesWrote(t *testing.T) {
	t.Parallel()
	town, home := realFixture(t)
	if err := SaveCaches(realCacheEnv(), town); err != nil {
		t.Fatalf("SaveCaches: %v", err)
	}
	runner := &realRunner{answer: realGit(town)}
	real, err := RealEnv(context.Background(), RealEnvOptions{Today: "smoke", Town: town, Home: home, Run: runner.run})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	if !reflect.DeepEqual(real.Extract.HandoffCache, map[string]int{"gt-a": 55}) {
		t.Errorf("HandoffCache = %v, want the saved cache", real.Extract.HandoffCache)
	}
	cache := real.Extract.Diffs.(*GitDiffSource).Cache
	want := DiffStat{LinesAdded: 9, LinesDeleted: 1, FilesTouched: 2, FileEntropy: 2.5, PackageEntropy: 1}
	if got := cache[DiffKey{Rig: "gastown", Commit: "abc1234"}]; !sameStat(got, want) {
		t.Errorf("diff cache entry = %+v, want the saved %+v", got, want)
	}
}

func TestSaveCachesRefusesAnEmptyTown(t *testing.T) {
	t.Parallel()
	if err := SaveCaches(realCacheEnv(), ""); err == nil {
		t.Fatal("SaveCaches wrote a cache with no town named")
	}
}

func TestSaveCachesStopsOnATownItCannotWriteInto(t *testing.T) {
	t.Parallel()
	town := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(town, nil, 0o644); err != nil {
		t.Fatalf("make %s: %v", town, err)
	}
	if err := SaveCaches(realCacheEnv(), town); err == nil {
		t.Fatal("SaveCaches wrote under a town that is a file")
	}
}

func TestSaveCachesStopsWhenACacheFileCannotBeReplaced(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory in the cache file's place: the rename of the written file
	// onto it is refused, so the save stops rather than dropping the cache.
	if err := os.MkdirAll(filepath.Join(town, ".runtime", "metrics", "cache", handoffCacheName), 0o755); err != nil {
		t.Fatalf("make the blocking directory: %v", err)
	}
	err := SaveCaches(realCacheEnv(), town)
	if err == nil || !strings.Contains(err.Error(), handoffCacheName) {
		t.Errorf("SaveCaches error = %v, want %s named", err, handoffCacheName)
	}
}

func TestSaveCachesRefusesADiffSourceWithoutACache(t *testing.T) {
	t.Parallel()
	env := realCacheEnv()
	env.Diffs = extractFakeDiffs{}
	if err := SaveCaches(env, t.TempDir()); err == nil {
		t.Fatal("SaveCaches saved a diff source that keeps no cache")
	}
}
