package metrics

import (
	"context"
	"errors"
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
	if _, err := os.Stat(filepath.Join(town, ".runtime", "metrics", "cache")); err != nil {
		t.Errorf("the cache directory was not made: %v", err)
	}

	// The cached diff is answered from the cache file: no git show for it.
	before := len(runner.calls)
	stat, err := real.Extract.Diffs.Stats(context.Background(), "gastown", "abc1234")
	if err != nil {
		t.Fatalf("Stats from the cache: %v", err)
	}
	if len(runner.calls) != before {
		t.Errorf("Stats ran %d commands for a cached commit", len(runner.calls)-before)
	}
	if want := (DiffStat{LinesAdded: 3, LinesDeleted: 1, FilesTouched: 2, FileEntropy: 1.5, PackageEntropy: 1}); !sameStat(stat, want) {
		t.Errorf("cached stat = %+v, want %+v", stat, want)
	}

	// One log per rig, then two commands for gastown's two commits.
	if got, want := len(runner.calls), len(Rigs)+2; got != want {
		t.Errorf("ran %d commands, want %d", got, want)
	}
	wantLog := []string{"nice", "-n", "15", "git", "--git-dir", rigRepo(town, "gastown"), "log",
		"--reverse", "--format=%cI %H", "origin/main", "--", ".om.json"}
	if got := runner.calls[0].argv; !reflect.DeepEqual(got, wantLog) {
		t.Errorf("first command = %q, want %q", got, wantLog)
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
	runner.calls = nil
	out, ok := real.Extract.RunBd("/beads/dir", []string{"list", "--all", "--json"})
	if !ok || string(out) != "[]" {
		t.Fatalf("RunBd = %q, %v; want the runner's answer", out, ok)
	}
	want := realCall{dir: "/beads/dir", argv: []string{"nice", "-n", "15", "bd", "list", "--all", "--json"}}
	if !reflect.DeepEqual(runner.calls, []realCall{want}) {
		t.Errorf("bd calls = %+v, want %+v", runner.calls, want)
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

func TestRealEnvStopsOnATownItCannotWriteItsCacheInto(t *testing.T) {
	t.Parallel()
	town := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(town, nil, 0o644); err != nil {
		t.Fatalf("make %s: %v", town, err)
	}
	runner := &realRunner{answer: func(string, []string) ([]byte, error) { return nil, nil }}
	_, err := RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Town: town, DocsDir: t.TempDir(), Home: t.TempDir(), Run: runner.run,
	})
	if err == nil || !strings.Contains(err.Error(), "real env") {
		t.Errorf("RealEnv error = %v, want the cache directory refused under a file's path", err)
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
	handoff, ok := readJSONFile[map[string]int](filepath.Join(cacheDir, handoffCacheName))
	if !ok || !reflect.DeepEqual(handoff, map[string]int{"gt-a": 55}) {
		t.Errorf("%s = %v, %v; want the bead's bytes", handoffCacheName, handoff, ok)
	}
	docs, ok := readJSONFile[map[string]diffStatDoc](filepath.Join(cacheDir, diffstatsCacheName))
	want := map[string]diffStatDoc{"gastown:abc1234": {
		LinesAdded: 9, LinesDeleted: 1, FilesTouched: 2, FileEntropy: 2.5, PackageEntropy: 1,
	}}
	if !ok || !reflect.DeepEqual(docs, want) {
		t.Errorf("%s = %v, %v; want %v", diffstatsCacheName, docs, ok, want)
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
