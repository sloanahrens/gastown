package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the real-environment wiring. It wires the town the extract reads:
// every file it opens and every directory it lists goes through the RealFiles
// seam, and every git and bd command through one injected runner, so a unit
// test supplies a fixture town and a scripted runner instead of ~/gt, and the
// hourly smoke is the only test that reads the real town.

// Rigs is the town's rig list, in the order the ported extract reads it.
var Rigs = []string{"gastown", "devops", "fractals", "beaver", "agentlab", "om", "hm", "mango", "beads"}

// PrefixToRig maps a bead id's prefix to its rig name.
var PrefixToRig = map[string]string{
	"gt": "gastown", "dv": "devops", "fr": "fractals", "bv": "beaver", "al": "agentlab",
	"om": "om", "hm": "hm", "ma": "mango", "be": "beads", "hq": "hq", "claude": "claude",
}

// The environment variables the town and docs directories fall back to.
const (
	envTown = "TOWN_METRICS_TOWN"
	envDocs = "TOWN_METRICS_DOCS"
)

// The cache files real_env reads and save_caches writes, under the town's
// .runtime/metrics/cache.
const (
	handoffCacheName   = "handoff_bytes.json"
	diffstatsCacheName = "diffstats.json"
)

// The command budgets _run has: the ported default, and the shorter one every
// git read in real_env takes.
const (
	runTimeout = 120 * time.Second
	gitTimeout = 60 * time.Second
)

// RealFiles is the file seam every file real_env opens and every directory it
// lists goes through, so a unit test supplies a fixture filesystem and no case
// reads the host; a field left nil is the host's.
type RealFiles struct {
	// ReadFile reads a whole file.
	ReadFile func(path string) ([]byte, error)
	// ReadDir lists a directory's entries in name order. A path that holds no
	// directory lists nothing; a directory
	// that cannot be listed is an error, because a listing that failed is not
	// an empty one.
	ReadDir func(dir string) ([]DirEntry, error)
}

// withHostDefaults fills each seam the caller left unset with the host's, so the
// zero RealFiles reads the filesystem the process runs on.
func (f RealFiles) withHostDefaults() RealFiles {
	if f.ReadFile == nil {
		f.ReadFile = os.ReadFile
	}
	if f.ReadDir == nil {
		f.ReadDir = hostDir
	}
	return f
}

// hostFiles is the seam of the host's own filesystem, for a caller that reads
// what the port already wrote there.
func hostFiles() RealFiles {
	return RealFiles{}.withHostDefaults()
}

// hostDir is the host's directory listing: entries in name order, none for a
// path that holds no directory, and an error for a
// directory that cannot be listed.
func hostDir(dir string) ([]DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// ReadDir refuses a path that is a file, where the stat check
		// lists nothing.
		if info, statErr := os.Stat(dir); statErr == nil && !info.IsDir() {
			return nil, nil
		}
		return nil, err
	}
	out := make([]DirEntry, len(entries))
	for i, entry := range entries {
		out[i] = DirEntry{Name: entry.Name(), IsDir: entry.IsDir()}
	}
	return out, nil
}

// RealEnvOptions is real_env's input: the day it reports, the town and docs
// directories, the home directory the defaults join to, the filesystem the town
// is read from, and the runner every command goes through.
type RealEnvOptions struct {
	// Today names the extract's output directory under the town's runtime.
	Today string
	// Town is the town root; empty is TOWN_METRICS_TOWN, else Home/gt.
	Town string
	// DocsDir is where the digest and regime-events.json live; empty is
	// TOWN_METRICS_DOCS, else Home/.claude/docs/metrics.
	DocsDir string
	// Home is the directory the defaults and the home-level bead listing join
	// to; empty is HomeDir's.
	Home string
	// HomeDir finds the home directory Home leaves to it; nil is the process's
	// own home.
	HomeDir func() (string, error)
	// Getenv reads the environment's town and docs directories when Town and
	// DocsDir are empty; nil is the process's environment.
	Getenv func(string) string
	// Files reads the town; nil reads the host's filesystem.
	Files RealFiles
	// Run runs every git and bd command; nil is an error.
	Run RunnerDir
}

// RealTown is what real_env builds: the extract's sources, the town they were
// read from, and the docs directory the report writes its digest into.
// ExtractEnv carries no docs directory because the extract never reads one.
type RealTown struct {
	Town    string
	DocsDir string
	Extract ExtractEnv
}

// RealEnv is real_env: it wires the extract to the town at opts.Town, reading
// the ledgers, the usage and overrun files, each rig's .om.json history and the
// caches under .runtime/metrics/cache, and runs bd per bead directory. It only
// reads the town: the extract writes under OutDir and SaveCaches writes the
// caches, so wiring a town, or running the smoke over it, changes nothing there.
func RealEnv(ctx context.Context, opts RealEnvOptions) (RealTown, error) {
	var real RealTown
	if opts.Run == nil {
		return real, errors.New("real env: RealEnvOptions.Run is unset")
	}
	home, err := homeDir(opts.Home, opts.HomeDir)
	if err != nil {
		return real, fmt.Errorf("real env: %w", err)
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	files := opts.Files.withHostDefaults()
	town := townRoot(opts.Town, getenv(envTown), home)
	real.Town = town
	real.DocsDir = docsDir(opts.DocsDir, getenv(envDocs), home)
	runtime := filepath.Join(town, ".runtime")
	cacheDir := filepath.Join(runtime, "metrics", "cache")

	var unreadable []string
	ledger := make(map[string][]string, len(Rigs))
	for _, rig := range Rigs {
		lines, err := readLines(files, filepath.Join(runtime, "landings", rig+".jsonl"))
		if err != nil {
			// The extract counts each of these in the footer's
			// sources_unreadable, so a rig with no ledger is named there.
			unreadable = append(unreadable, "ledger:"+rig)
			continue
		}
		ledger[rig] = lines
	}
	reviews, err := readLines(files, filepath.Join(runtime, "landings", "reviews.jsonl"))
	if err != nil {
		reviews = nil
		unreadable = append(unreadable, "reviews")
	}
	usage, err := realUsageFiles(files, filepath.Join(runtime, "usage", "beads"))
	if err != nil {
		return real, fmt.Errorf("real env: usage: %w", err)
	}
	overrun, err := realOverrunStates(files, filepath.Join(runtime, "overrun-guard"))
	if err != nil {
		return real, fmt.Errorf("real env: overrun: %w", err)
	}
	omCommits, err := realOMHistory(ctx, opts.Run, town)
	if err != nil {
		return real, fmt.Errorf("real env: %w", err)
	}
	diffDocs, err := readCache[map[string]diffStatDoc](files, filepath.Join(cacheDir, diffstatsCacheName))
	if err != nil {
		return real, fmt.Errorf("real env: diff cache: %w", err)
	}
	diffs := &GitDiffSource{
		BareRepoFor: func(rig string) string { return rigRepo(town, rig) },
		Run: func(ctx context.Context, argv []string) ([]byte, error) {
			return run(ctx, opts.Run, "", argv, gitTimeout)
		},
		Cache: diffCacheFromDocs(diffDocs),
	}
	// Both caches stay live: the extract fills the handoff map and the diff
	// source fills its own, so SaveCaches writes the sizes this run fetched.
	handoff, err := readCache[map[string]int](files, filepath.Join(cacheDir, handoffCacheName))
	if err != nil {
		return real, fmt.Errorf("real env: handoff cache: %w", err)
	}
	if handoff == nil {
		handoff = map[string]int{}
	}
	beadDirs := make([]string, 0, len(Rigs)+2)
	for _, rig := range Rigs {
		beadDirs = append(beadDirs, filepath.Join(town, rig))
	}
	beadDirs = append(beadDirs, town, filepath.Join(home, ".claude"))

	real.Extract = ExtractEnv{
		LedgerLines:   ledger,
		ReviewLines:   reviews,
		Diffs:         diffs,
		RunBd:         realBd(ctx, opts.Run),
		BeadDirs:      beadDirs,
		PrefixToRig:   PrefixToRig,
		UsageFiles:    usage,
		OverrunStates: overrun,
		OMCommits:     omCommits,
		OutDir:        filepath.Join(runtime, "metrics", opts.Today),
		Today:         opts.Today,
		Unreadable:    unreadable,
		HandoffCache:  handoff,
		BdShowCap:     defaultBdShowCap,
	}
	return real, nil
}

// realBd is the extract's bd seam over the runner: bd in dir, its stdout, and
// ok false for any failure.
func realBd(ctx context.Context, r RunnerDir) BdRun {
	return func(dir string, args []string) ([]byte, bool) {
		out, err := run(ctx, r, dir, append([]string{"bd"}, args...), runTimeout)
		return out, err == nil
	}
}

// rigRepo is the path of a rig's bare repository.
func rigRepo(town, rig string) string {
	return filepath.Join(town, rig, ".repo.git")
}

// realOMHistory is each rig's .om.json history, oldest first. A rig whose log
// cannot be read stops the wiring, so a rig with no history never passes for one
// whose threshold never moved.
func realOMHistory(ctx context.Context, r RunnerDir, town string) (map[string][]OMCommit, error) {
	out := make(map[string][]OMCommit, len(Rigs))
	for _, rig := range Rigs {
		gitDir := rigRepo(town, rig)
		log, err := run(ctx, r, "", []string{"git", "--git-dir", gitDir, "log", "--reverse",
			"--format=%cI %H", "origin/main", "--", ".om.json"}, gitTimeout)
		if err != nil {
			return nil, fmt.Errorf("%s: om history: %w", rig, err)
		}
		commits := make([]OMCommit, 0)
		for _, line := range pySplitLines(string(log)) {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				return nil, fmt.Errorf("%s: om history line %q is not a time and a commit", rig, line)
			}
			text, err := run(ctx, r, "", []string{"git", "--git-dir", gitDir, "show", fields[1] + ":.om.json"}, gitTimeout)
			if err != nil {
				// A show that fails contributes an empty read, which
				// means the commit names no threshold, and OMEras skips it.
				text = nil
			}
			commits = append(commits, OMCommit{At: fields[0], Text: string(text)})
		}
		out[rig] = commits
	}
	return out, nil
}

// realUsageFiles is real_env's usage map: each bead's *.work.json, named by the
// file minus its suffix. A file that does not decode, or whose value is
// falsy (see pyTruthy), is left out, so the footer does not count it.
func realUsageFiles(files RealFiles, dir string) (map[string]UsageFile, error) {
	const suffix = ".work.json"
	out := map[string]UsageFile{}
	names, err := listDirNames(files, dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		file, ok := readJSONTruthy[UsageFile](files, filepath.Join(dir, name))
		if !ok {
			continue
		}
		out[strings.TrimSuffix(name, suffix)] = file
	}
	return out, nil
}

// realOverrunStates is real_env's overrun map: each bead's guard state file,
// named by the file minus its suffix.
func realOverrunStates(files RealFiles, dir string) (map[string]OverrunState, error) {
	const suffix = ".json"
	out := map[string]OverrunState{}
	names, err := listDirNames(files, dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		state, ok := readJSONTruthy[OverrunState](files, filepath.Join(dir, name))
		if !ok {
			continue
		}
		out[strings.TrimSuffix(name, suffix)] = state
	}
	return out, nil
}

// run is _run: argv at low priority, its stdout, or an error on any failure or
// on the timeout.
func run(ctx context.Context, r RunnerDir, dir string, argv []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r(ctx, dir, append([]string{"nice", "-n", "15"}, argv...))
}

// townRoot is _town: the town named, else the environment's, else home/gt.
func townRoot(town, fromEnv, home string) string {
	if town != "" {
		return town
	}
	if fromEnv != "" {
		return fromEnv
	}
	return filepath.Join(home, "gt")
}

// docsDir is _docs_dir: the directory named, else the environment's, else
// home/.claude/docs/metrics.
func docsDir(docs, fromEnv, home string) string {
	if docs != "" {
		return docs
	}
	if fromEnv != "" {
		return fromEnv
	}
	return filepath.Join(home, ".claude", "docs", "metrics")
}

// homeDir is the home directory townRoot, docsDir and the bead listing join
// to: the one named, else find's.
func homeDir(home string, find func() (string, error)) (string, error) {
	if home != "" {
		return home, nil
	}
	if find == nil {
		find = os.UserHomeDir
	}
	return find()
}

// readLines reads a file as its non-empty lines.
func readLines(files RealFiles, path string) ([]string, error) {
	data, err := files.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return pySplitLines(string(data)), nil
}

// listDirNames lists a directory's entry names in name order, which is the
// order os.ReadDir returns. The seam reports a path that holds no directory as
// no entries, and a directory that cannot be listed is an error rather than an
// empty one.
func listDirNames(files RealFiles, dir string) ([]string, error) {
	entries, err := files.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names, nil
}

// readCache decodes one cache file into T. A town with no cache yet reads as an
// empty one, which is the default, so a first run costs
// what it costs; a cache file that is there and does not decode is an error,
// because a corrupt cache read as empty is a first run again on every later
// run, silently.
func readCache[T any](files RealFiles, path string) (T, error) {
	var value T
	data, err := files.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return value, fmt.Errorf("%s: %w", path, err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("%s: %w", path, err)
	}
	return value, nil
}

// readJSONTruthy decodes path into T unless the file's value is falsy (see
// pyTruthy), and the result is not ok.
func readJSONTruthy[T any](files RealFiles, path string) (T, bool) {
	var value T
	data, err := files.ReadFile(path)
	if err != nil {
		return value, false
	}
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return value, false
	}
	if !pyTruthy(probe) {
		return value, false
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, false
	}
	return value, true
}

// pyTruthy is the truth of a decoded JSON value, where null,
// false, zero, the empty string and an empty list or object are all false.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// diffStatDoc is one diffstats.json entry, in the cache file's field names.
type diffStatDoc struct {
	LinesAdded     int     `json:"lines_added"`
	LinesDeleted   int     `json:"lines_deleted"`
	FilesTouched   int     `json:"files_touched"`
	FileEntropy    float64 `json:"file_entropy"`
	PackageEntropy float64 `json:"package_entropy"`
}

// diffCacheFromDocs keys each cached stat by the rig and commit its
// "rig:commit" key names. A key with no colon names no commit and is dropped.
func diffCacheFromDocs(docs map[string]diffStatDoc) DiffCache {
	cache := make(DiffCache, len(docs))
	for key, doc := range docs {
		rig, commit, ok := strings.Cut(key, ":")
		if !ok {
			continue
		}
		cache[DiffKey{Rig: rig, Commit: commit}] = DiffStat{
			LinesAdded:     doc.LinesAdded,
			LinesDeleted:   doc.LinesDeleted,
			FilesTouched:   doc.FilesTouched,
			FileEntropy:    doc.FileEntropy,
			PackageEntropy: doc.PackageEntropy,
		}
	}
	return cache
}

// diffDocsFromCache is diffstats.json's shape for a cache, in the key form
// diffCacheFromDocs reads back.
func diffDocsFromCache(cache DiffCache) map[string]diffStatDoc {
	docs := make(map[string]diffStatDoc, len(cache))
	for key, stat := range cache {
		docs[key.Rig+":"+key.Commit] = diffStatDoc{
			LinesAdded:     stat.LinesAdded,
			LinesDeleted:   stat.LinesDeleted,
			FilesTouched:   stat.FilesTouched,
			FileEntropy:    stat.FileEntropy,
			PackageEntropy: stat.PackageEntropy,
		}
	}
	return docs
}

// SaveCaches is save_caches: it writes the handoff and diff caches into the
// town's runtime so the next run reads each bead and commit once, making the
// cache directory RealEnv now only reads from. town is the root RealEnv
// resolved; an empty one is an error, because a cache written under the wrong
// town is invisible to every later run.
func SaveCaches(env ExtractEnv, town string) error {
	if town == "" {
		return errors.New("save caches: town is unset")
	}
	diffs, ok := env.Diffs.(*GitDiffSource)
	if !ok || diffs == nil {
		return errors.New("save caches: the env's diff source keeps no cache to save")
	}
	handoff := env.HandoffCache
	if handoff == nil {
		handoff = map[string]int{}
	}
	cacheDir := filepath.Join(town, ".runtime", "metrics", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("save caches: %w", err)
	}
	for _, file := range []struct {
		name  string
		value any
	}{
		{handoffCacheName, handoff},
		{diffstatsCacheName, diffDocsFromCache(diffs.Cache)},
	} {
		if err := saveCacheFile(cacheDir, file.name, file.value); err != nil {
			return err
		}
	}
	return nil
}

// saveCacheFile writes one cache into dir through a temporary file, so a run
// interrupted mid-write leaves the previous cache whole.
func saveCacheFile(dir, name string, value any) error {
	if err := writeAtomic(dir, name, func(w io.Writer) error { return writeJSON(w, name, value) }); err != nil {
		return fmt.Errorf("save caches: %s: %w", name, err)
	}
	return nil
}
