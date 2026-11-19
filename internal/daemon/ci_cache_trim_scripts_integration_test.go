//go:build integration

package daemon

// The trim's two programs run as root inside the runner's dind container: they
// are shell text, not Go, so the only proof of what they delete is running the
// text. This is the integration tier because each case starts sh, which the
// unit tier forbids; the temporary tree each case builds stands in for the
// ci-gocache volume (gt-by8lm5).

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// ciCacheScriptAgeMinutes is the age the cases pass as the script's second
	// argument: an hour, so the tree's two-hour-old files are candidates and
	// its one-minute-old file is not.
	ciCacheScriptAgeMinutes = "60"

	// ciCacheScriptOldAge dates the tree's old files well past that age.
	ciCacheScriptOldAge = 2 * time.Hour
)

// ciCacheScriptOld is every file the tree dates before the age. trim.txt is at
// the root and old on purpose: only its depth keeps it.
var ciCacheScriptOld = []string{
	"trim.txt",
	filepath.Join("00", "old-at-two.txt"),
	filepath.Join("ab", "cd", "old-at-three.txt"),
}

// ciCacheScriptFresh is the file the tree dates after the age, under a
// directory the apply keeps.
var ciCacheScriptFresh = []string{
	filepath.Join("ab", "new-at-two.txt"),
}

// ciCacheScriptRemoved is what the apply removes: the old files below the top
// level.
var ciCacheScriptRemoved = []string{
	filepath.Join("00", "old-at-two.txt"),
	filepath.Join("ab", "cd", "old-at-three.txt"),
}

// ciCacheScriptTree writes the volume tree the cases run against under root:
// the old and the fresh file. The apply empties 00, a depth-1 two-hex shard
// directory, and ab/cd at depth 2, each of which holds only an old file.
func ciCacheScriptTree(t *testing.T, root string) {
	t.Helper()
	now := time.Now()
	files := append(append([]string{}, ciCacheScriptOld...), ciCacheScriptFresh...)
	for _, path := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("making the directory of %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(path+"\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		mtime := now.Add(-time.Minute)
		if slices.Contains(ciCacheScriptOld, path) {
			mtime = now.Add(-ciCacheScriptOldAge)
		}
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatalf("dating %s: %v", path, err)
		}
	}
}

// ciCacheScriptFiles lists every regular file under root, relative to it and
// sorted.
func ciCacheScriptFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the volume tree: %v", err)
	}
	slices.Sort(files)
	return files
}

// ciCacheScriptRun runs a trim program the way the daemon does — the constant
// through sh, with the volume root, the age in minutes and any further argv data
// (the capped script's cap) as arguments — and returns its output and exit
// status. The command's environment is exactly PATH and TMPDIR: nothing is
// inherited, so neither a tool on the caller's PATH nor the shell itself can be
// swapped for the system's.
func ciCacheScriptRun(t *testing.T, script, root, minutes string, extra ...string) (string, string, int) {
	t.Helper()
	args := append([]string{"-c", script, "ci-cache-trim", root, minutes}, extra...)
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("running the trim program: %v", err)
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

// TestIntegrationCICacheTrimScripts runs the trim's two programs against a
// temporary volume tree: the dry run reports the old files and removes none;
// the apply removes exactly them, removing the empty directory the removal
// leaves below the top level while keeping the depth-1 shard directory it
// empties; both keep the fresh file and the root's own file, replying with the
// three lines the parser reads.
//
// Expected to fail under five changes to the programs, each run by hand on its
// bead: flipping the age test from older-than to newer-than (-mmin +age to
// -mmin -age, so the newest files go), dropping the depth floor (-mindepth 2 to
// -mindepth 0, so the root's own files go), and moving the missing-path exit
// away from ciCacheMissingPathExit (gt-by8lm5); restoring the depth-1
// empty-directory sweep (-mindepth 2 to -mindepth 1, so the cache's shard
// directory goes) (gt-qaqvf4); and comparing the capped script's size with -lt
// instead of -le, so a volume exactly at its cap is trimmed (gt-lbtul0).
func TestIntegrationCICacheTrimScripts(t *testing.T) {
	t.Parallel()

	t.Run("the dry run reports the old files and removes none", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheScriptTree(t, root)
		before := ciCacheScriptFiles(t, root)

		stdout, stderr, status := ciCacheScriptRun(t, ciCacheListScript, root, ciCacheScriptAgeMinutes)
		if status != 0 {
			t.Fatalf("the dry run exited %d: %s", status, stderr)
		}
		reply, err := parseCICacheTrimReply([]byte(stdout))
		if err != nil {
			t.Fatalf("the dry run's reply does not parse: %v:\n%s", err, stdout)
		}
		if want := int64(len(ciCacheScriptRemoved)); reply.removed != want {
			t.Errorf("the dry run reports %d files to remove, want %d:\n%s", reply.removed, want, stdout)
		}
		if reply.afterKiB != reply.beforeKiB {
			t.Errorf("the dry run reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
		}
		if after := ciCacheScriptFiles(t, root); !slices.Equal(after, before) {
			t.Errorf("the dry run removed files: %v, before %v", after, before)
		}
		if _, err := os.Stat(filepath.Join(root, "ab", "cd")); err != nil {
			t.Errorf("the dry run removed the directory it would empty: %v", err)
		}
	})

	t.Run("the apply removes the old files below the top level only", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheScriptTree(t, root)

		stdout, stderr, status := ciCacheScriptRun(t, ciCacheTrimScript, root, ciCacheScriptAgeMinutes)
		if status != 0 {
			t.Fatalf("the apply exited %d: %s", status, stderr)
		}
		reply, err := parseCICacheTrimReply([]byte(stdout))
		if err != nil {
			t.Fatalf("the apply's reply does not parse: %v:\n%s", err, stdout)
		}
		if want := int64(len(ciCacheScriptRemoved)); reply.removed != want {
			t.Errorf("the apply reports %d files removed, want %d:\n%s", reply.removed, want, stdout)
		}
		if reply.afterKiB > reply.beforeKiB {
			t.Errorf("the apply reports the volume growing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
		}

		want := append([]string{"trim.txt"}, ciCacheScriptFresh...)
		slices.Sort(want)
		if got := ciCacheScriptFiles(t, root); !slices.Equal(got, want) {
			t.Errorf("the volume holds %v after the apply, want %v: only the old files below the root go", got, want)
		}
		if _, err := os.Stat(filepath.Join(root, "ab", "cd")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the directory the removal emptied is still present (stat error %v)", err)
		}
	})

	t.Run("the apply keeps the depth-1 two-hex directory it empties", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheScriptTree(t, root)
		shard := filepath.Join(root, "00")

		_, stderr, status := ciCacheScriptRun(t, ciCacheTrimScript, root, ciCacheScriptAgeMinutes)
		if status != 0 {
			t.Fatalf("the apply exited %d: %s", status, stderr)
		}
		// 00's only file is older than the age, so the apply empties it: the
		// file goes, and the shard directory Go created stays for later writes.
		if _, err := os.Stat(filepath.Join(shard, "old-at-two.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the apply kept the old file in the depth-1 directory (stat error %v)", err)
		}
		if _, err := os.Stat(shard); err != nil {
			t.Errorf("the apply removed the depth-1 two-hex directory it emptied: %v", err)
		}
	})

	t.Run("the capped apply at or under the cap removes nothing", func(t *testing.T) {
		t.Parallel()
		// The cap is the tree's size as the dry run reads it, and one KiB more.
		for name, capOf := range map[string]func(before int64) int64{
			"at the cap":    func(before int64) int64 { return before },
			"under the cap": func(before int64) int64 { return before + 1 },
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				ciCacheScriptTree(t, root)
				before := ciCacheScriptFiles(t, root)

				listed, stderr, status := ciCacheScriptRun(t, ciCacheListScript, root, ciCacheScriptAgeMinutes)
				if status != 0 {
					t.Fatalf("the dry run exited %d: %s", status, stderr)
				}
				listedReply, err := parseCICacheTrimReply([]byte(listed))
				if err != nil {
					t.Fatalf("the dry run's reply does not parse: %v:\n%s", err, listed)
				}

				stdout, stderr, status := ciCacheScriptRun(t, ciCacheCappedTrimScript, root, ciCacheScriptAgeMinutes,
					strconv.FormatInt(capOf(listedReply.beforeKiB), 10))
				if status != 0 {
					t.Fatalf("the capped apply exited %d: %s", status, stderr)
				}
				reply, err := parseCICacheTrimReply([]byte(stdout))
				if err != nil {
					t.Fatalf("the capped apply's reply does not parse: %v:\n%s", err, stdout)
				}
				if reply.removed != 0 {
					t.Errorf("the capped apply reports %d files removed under its cap, want 0:\n%s", reply.removed, stdout)
				}
				if reply.afterKiB != reply.beforeKiB {
					t.Errorf("the capped apply reports the volume changing under its cap, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
				}
				if got := ciCacheScriptFiles(t, root); !slices.Equal(got, before) {
					t.Errorf("the capped apply removed files under its cap: %v, before %v", got, before)
				}
			})
		}
	})

	t.Run("the capped apply over the cap removes what the apply removes", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheScriptTree(t, root)

		// A cap of 1 KiB: the tree is over it, so the capped apply runs the age rule.
		stdout, stderr, status := ciCacheScriptRun(t, ciCacheCappedTrimScript, root, ciCacheScriptAgeMinutes, "1")
		if status != 0 {
			t.Fatalf("the capped apply exited %d: %s", status, stderr)
		}
		reply, err := parseCICacheTrimReply([]byte(stdout))
		if err != nil {
			t.Fatalf("the capped apply's reply does not parse: %v:\n%s", err, stdout)
		}
		if want := int64(len(ciCacheScriptRemoved)); reply.removed != want {
			t.Errorf("the capped apply reports %d files removed, want %d:\n%s", reply.removed, want, stdout)
		}

		want := append([]string{"trim.txt"}, ciCacheScriptFresh...)
		slices.Sort(want)
		if got := ciCacheScriptFiles(t, root); !slices.Equal(got, want) {
			t.Errorf("the volume holds %v after the capped apply, want %v", got, want)
		}
		if _, err := os.Stat(filepath.Join(root, "ab", "cd")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the directory the removal emptied is still present (stat error %v)", err)
		}
	})

	t.Run("a missing root is the missing-path status", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name   string
			script string
			extra  []string
		}{
			{name: "the dry run", script: ciCacheListScript},
			{name: "the apply", script: ciCacheTrimScript},
			{name: "the capped apply", script: ciCacheCappedTrimScript, extra: []string{"1024"}},
			{name: "the module dry run", script: ciCacheModuleListScript, extra: []string{"1024"}},
			{name: "the module apply", script: ciCacheModuleTrimScript, extra: []string{"1024"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				root := filepath.Join(t.TempDir(), "absent")
				stdout, stderr, status := ciCacheScriptRun(t, tt.script, root, ciCacheScriptAgeMinutes, tt.extra...)
				if status != ciCacheMissingPathExit {
					t.Fatalf("a missing root exited %d, want the missing-path status %d", status, ciCacheMissingPathExit)
				}
				if stdout != "" {
					t.Errorf("a missing root reported on stdout: %q", stdout)
				}
				if !strings.Contains(stderr, "volume path not found") {
					t.Errorf("a missing root said %q, want the script's own line", stderr)
				}
			})
		}
	})
}

// ciCacheModuleDir is a module directory the module tree writes, relative to the
// volume root, and whether its time is dated before the age.
type ciCacheModuleDir struct {
	path string
	old  bool
}

// ciCacheModuleDirs is the module tree. The three old module directories the
// apply removes whole are the first three: two at depth 3 and one at depth 8,
// the deepest the walk reaches. The rest are kept: a fresh module; an old module
// at depth 9, past the walk; and the download cache's @v directory, which matches
// the module pattern and is old, so only the ./cache prune keeps it.
var ciCacheModuleDirs = []ciCacheModuleDir{
	{filepath.Join("pkg", "mod", "github.com", "old", "one@v1.0.0"), true},
	{filepath.Join("pkg", "mod", "golang.org", "x", "old@v0.1.0"), true},
	{filepath.Join("pkg", "mod", "a", "b", "c", "d", "e", "f", "g", "h@v1.0.0"), true},
	{filepath.Join("pkg", "mod", "github.com", "new", "two@v1.0.0"), false},
	{filepath.Join("pkg", "mod", "p", "q", "r", "s", "t", "u", "v", "w", "x@v1.0.0"), true},
	{filepath.Join("pkg", "mod", "cache", "download", "github.com", "old", "one", "@v"), true},
}

// ciCacheModuleRemoved is what the module apply removes, relative to the volume
// root: the old module directories the walk reaches.
var ciCacheModuleRemoved = ciCacheModuleDirs[:3]

// ciCacheModuleTree writes the module tree under root: each module directory
// with one file, the top-level file of pkg/mod, which is old and is never a
// candidate, and each directory dated after its file is written, so the directory's
// own time is the one the walk reads.
func ciCacheModuleTree(t *testing.T, root string) {
	t.Helper()
	now := time.Now()
	for _, d := range ciCacheModuleDirs {
		full := filepath.Join(root, d.path)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("making %s: %v", d.path, err)
		}
		if err := os.WriteFile(filepath.Join(full, "file.go"), []byte(d.path+"\n"), 0o644); err != nil {
			t.Fatalf("writing into %s: %v", d.path, err)
		}
	}
	top := filepath.Join(root, "pkg", "mod", "trim.txt")
	if err := os.WriteFile(top, []byte("trim.txt\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", top, err)
	}
	if err := os.Chtimes(top, now.Add(-ciCacheScriptOldAge), now.Add(-ciCacheScriptOldAge)); err != nil {
		t.Fatalf("dating %s: %v", top, err)
	}
	for _, d := range ciCacheModuleDirs {
		mtime := now.Add(-time.Minute)
		if d.old {
			mtime = now.Add(-ciCacheScriptOldAge)
		}
		full := filepath.Join(root, d.path)
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatalf("dating %s: %v", d.path, err)
		}
	}
}

// ciCacheModuleKept is what the module apply keeps: the file of each module
// directory it does not remove, the pkg/mod top-level file, and nothing else.
func ciCacheModuleKept() []string {
	kept := []string{filepath.Join("pkg", "mod", "trim.txt")}
	for _, d := range ciCacheModuleDirs[len(ciCacheModuleRemoved):] {
		kept = append(kept, filepath.Join(d.path, "file.go"))
	}
	slices.Sort(kept)
	return kept
}

// TestIntegrationCICacheTrimModuleScripts runs the module programs against a
// temporary pkg/mod tree: the dry run reports the old module directories and
// removes none; the apply removes exactly those, whole, and keeps the fresh
// module, the module past the depth walk, the download cache's @v directory and
// the top-level file; and at or under the cap the apply removes nothing.
//
// Each change to the module program below turns this test red: dropping the
// ./cache prune (the old @v directory goes); moving the depth limit from 8 to 9
// (the module at depth 9 goes); flipping the age test to newer-than (the fresh
// module goes).
func TestIntegrationCICacheTrimModuleScripts(t *testing.T) {
	t.Parallel()

	t.Run("the dry run reports the old module directories and removes none", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheModuleTree(t, root)
		before := ciCacheScriptFiles(t, root)

		// A cap of 1 KiB: the tree is over it, so the walk runs.
		stdout, stderr, status := ciCacheScriptRun(t, ciCacheModuleListScript, root, ciCacheScriptAgeMinutes, "1")
		if status != 0 {
			t.Fatalf("the dry run exited %d: %s", status, stderr)
		}
		reply, err := parseCICacheTrimReply([]byte(stdout))
		if err != nil {
			t.Fatalf("the dry run's reply does not parse: %v:\n%s", err, stdout)
		}
		if want := int64(len(ciCacheModuleRemoved)); reply.removed != want {
			t.Errorf("the dry run reports %d module directories to remove, want %d:\n%s", reply.removed, want, stdout)
		}
		if reply.afterKiB != reply.beforeKiB {
			t.Errorf("the dry run reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
		}
		if after := ciCacheScriptFiles(t, root); !slices.Equal(after, before) {
			t.Errorf("the dry run removed files: %v, before %v", after, before)
		}
	})

	t.Run("the apply removes the old module directories whole and keeps the rest", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheModuleTree(t, root)

		stdout, stderr, status := ciCacheScriptRun(t, ciCacheModuleTrimScript, root, ciCacheScriptAgeMinutes, "1")
		if status != 0 {
			t.Fatalf("the apply exited %d: %s", status, stderr)
		}
		reply, err := parseCICacheTrimReply([]byte(stdout))
		if err != nil {
			t.Fatalf("the apply's reply does not parse: %v:\n%s", err, stdout)
		}
		if want := int64(len(ciCacheModuleRemoved)); reply.removed != want {
			t.Errorf("the apply reports %d module directories removed, want %d:\n%s", reply.removed, want, stdout)
		}
		if got, want := ciCacheScriptFiles(t, root), ciCacheModuleKept(); !slices.Equal(got, want) {
			t.Errorf("the volume holds %v after the apply, want %v", got, want)
		}
		for _, d := range ciCacheModuleRemoved {
			if _, err := os.Stat(filepath.Join(root, d.path)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the old module directory %s is still present (stat error %v)", d.path, err)
			}
		}
	})

	t.Run("at or under the cap the apply removes nothing", func(t *testing.T) {
		t.Parallel()
		// The cap is the tree's size as the dry run reads it, and one KiB more.
		for name, capOf := range map[string]func(before int64) int64{
			"at the cap":    func(before int64) int64 { return before },
			"under the cap": func(before int64) int64 { return before + 1 },
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				ciCacheModuleTree(t, root)
				before := ciCacheScriptFiles(t, root)

				listed, stderr, status := ciCacheScriptRun(t, ciCacheModuleListScript, root, ciCacheScriptAgeMinutes, "1")
				if status != 0 {
					t.Fatalf("the dry run exited %d: %s", status, stderr)
				}
				listedReply, err := parseCICacheTrimReply([]byte(listed))
				if err != nil {
					t.Fatalf("the dry run's reply does not parse: %v:\n%s", err, listed)
				}

				stdout, stderr, status := ciCacheScriptRun(t, ciCacheModuleTrimScript, root, ciCacheScriptAgeMinutes,
					strconv.FormatInt(capOf(listedReply.beforeKiB), 10))
				if status != 0 {
					t.Fatalf("the module apply exited %d: %s", status, stderr)
				}
				reply, err := parseCICacheTrimReply([]byte(stdout))
				if err != nil {
					t.Fatalf("the module apply's reply does not parse: %v:\n%s", err, stdout)
				}
				if reply.removed != 0 {
					t.Errorf("the module apply reports %d directories removed under its cap, want 0:\n%s", reply.removed, stdout)
				}
				if got := ciCacheScriptFiles(t, root); !slices.Equal(got, before) {
					t.Errorf("the module apply removed files under its cap: %v, before %v", got, before)
				}
			})
		}
	})

	t.Run("module names with a space or a line break are removed whole, one entry each", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		// A space or a line break in a module's name must not split it into two
		// entries: the apply would then remove neither directory and report more.
		// The fresh module is the neighbour the apply must leave alone.
		modules := filepath.Join(root, "pkg", "mod")
		now := time.Now()
		for name, mtime := range map[string]time.Time{
			"odd name@v1.0.0":  now.Add(-ciCacheScriptOldAge),
			"odd\nname@v1.0.0": now.Add(-ciCacheScriptOldAge),
			"keep@v1.0.0":      now.Add(-time.Minute),
		} {
			dir := filepath.Join(modules, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("making the module directory %q: %v", name, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "file.go"), []byte("package odd\n"), 0o644); err != nil {
				t.Fatalf("writing into the module directory %q: %v", name, err)
			}
			if err := os.Chtimes(dir, mtime, mtime); err != nil {
				t.Fatalf("dating the module directory %q: %v", name, err)
			}
		}

		stdout, stderr, status := ciCacheScriptRun(t, ciCacheModuleTrimScript, root, ciCacheScriptAgeMinutes, "1")
		if status != 0 {
			t.Fatalf("the module apply exited %d: %s", status, stderr)
		}
		reply, err := parseCICacheTrimReply([]byte(stdout))
		if err != nil {
			t.Fatalf("the module apply's reply does not parse: %v:\n%s", err, stdout)
		}
		if reply.removed != 2 {
			t.Errorf("the module apply reports %d directories removed, want 2:\n%s", reply.removed, stdout)
		}
		for _, name := range []string{"odd name@v1.0.0", "odd\nname@v1.0.0"} {
			if _, err := os.Stat(filepath.Join(modules, name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the old module directory %q is still present (stat error %v)", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(modules, "keep@v1.0.0")); err != nil {
			t.Errorf("the fresh module directory was touched: %v", err)
		}
	})
}

// ciCacheLRUTree is the volume the LRU cases run over, relative to its root:
// each file with its age in minutes and its size in KiB. The ages sit either
// side of the floor and the age a case passes, so one find bounds the list
// below the floor and another above the age; trim.txt is at the root and old on
// purpose, so only its depth keeps it out of both. Every size is a whole number
// of KiB, so du's figures are the same on the host and in the runner.
var ciCacheLRUTree = []struct {
	path    string
	ageMin  int
	sizeKiB int
}{
	{path: filepath.Join("00", "old-3h"), ageMin: 180, sizeKiB: 256},
	{path: filepath.Join("ab", "lru-100m"), ageMin: 100, sizeKiB: 256},
	{path: filepath.Join("00", "lru-96m"), ageMin: 96, sizeKiB: 256},
	{path: filepath.Join("ab", "lru-90m"), ageMin: 90, sizeKiB: 256},
	{path: filepath.Join("ab", "mid-45m"), ageMin: 45, sizeKiB: 128},
	{path: filepath.Join("ab", "fresh-10m"), ageMin: 10, sizeKiB: 64},
	{path: "trim.txt", ageMin: 180, sizeKiB: 32},
}

// ciCacheLRUTreeWrite writes the table under root, each file dated its age
// before now, so every case reads the same ages whenever it runs. root is
// chmod 0700 first: the runner's umask 000 would leave it world-writable.
func ciCacheLRUTreeWrite(t *testing.T, root string, now time.Time) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod %s: %v", root, err)
	}
	for _, f := range ciCacheLRUTree {
		full := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("making the directory of %s: %v", f.path, err)
		}
		if err := os.WriteFile(full, bytes.Repeat([]byte{'x'}, f.sizeKiB*1024), 0o644); err != nil {
			t.Fatalf("writing %s: %v", f.path, err)
		}
		mtime := now.Add(-time.Duration(f.ageMin) * time.Minute)
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatalf("dating %s: %v", f.path, err)
		}
	}
}

// ciCacheLRURun runs an LRU program the way the daemon does — the script text
// through sh, with the mode, the volume root, the age in minutes, the cap in
// KiB and the floor in minutes as argv data — and returns its output and exit
// status. The script is a parameter so the helpers do not depend on the program
// existing. The environment is exactly PATH and TMPDIR, so neither a tool on
// the caller's PATH nor the shell itself can be swapped for the system's.
func ciCacheLRURun(t *testing.T, script, mode, root, age, capKiB, floor string) (string, string, int) {
	t.Helper()
	args := []string{"-c", script, "ci-cache-trim", mode, root, age, capKiB, floor}
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("running the LRU program: %v", err)
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

// ciCacheLRURead splits an LRU program's output: the file lines it selected, in
// the order it printed them, and the three-line reply that follows them. The
// reply must be there: a run read as zero figures would read as a trim that
// removed nothing.
func ciCacheLRURead(t *testing.T, stdout string) ([]string, ciCacheTrimReply) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("the LRU program printed %d line(s), want three at least:\n%s", len(lines), stdout)
	}
	reply, err := parseCICacheTrimReply([]byte(strings.Join(lines[len(lines)-3:], "\n") + "\n"))
	if err != nil {
		t.Fatalf("the LRU program's reply does not parse: %v:\n%s", err, stdout)
	}
	return lines[:len(lines)-3], reply
}

// ciCacheLRUBefore returns the volume tree's size in KiB, measured with du -sk:
// the figure an LRU program reports as before_kib.
func ciCacheLRUBefore(t *testing.T, root string) int64 {
	t.Helper()
	cmd := exec.Command("du", "-sk", root)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("du -sk over the volume tree failed: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("du -sk printed no size for the volume tree")
	}
	kiB, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatalf("du -sk's size field is not a whole number (parse error %v)", err)
	}
	return kiB
}

// TestIntegrationCICacheTrimLRUHelpers pins the helpers the LRU cases share, so
// a case that reads the wrong figure fails here rather than inside its own
// assertions: the tree builder writes the table's files at the sizes and ages
// it names, the runner hands the script text the program's arguments and
// returns its output and status, the reader splits the selected files from the
// reply, and the size helper measures the volume the program will measure.
func TestIntegrationCICacheTrimLRUHelpers(t *testing.T) {
	t.Parallel()

	// A fixed instant: every file's mtime is this minus its row's age, never a
	// clock read during the test, so the assertion needs no tolerance.
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	t.Run("the tree builder writes the table's files at its sizes and times", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, now)

		want := make([]string, 0, len(ciCacheLRUTree))
		for _, f := range ciCacheLRUTree {
			want = append(want, f.path)
			info, err := os.Stat(filepath.Join(root, f.path))
			if err != nil {
				t.Fatalf("the builder did not write %s: %v", f.path, err)
			}
			if size := int64(f.sizeKiB) * 1024; info.Size() != size {
				t.Errorf("%s holds %d bytes, want the row's %d", f.path, info.Size(), size)
			}
			if dated := now.Add(-time.Duration(f.ageMin) * time.Minute); !info.ModTime().Equal(dated) {
				t.Errorf("%s is dated %s, want the row's %s", f.path, info.ModTime(), dated)
			}
		}
		slices.Sort(want)
		if got := ciCacheScriptFiles(t, root); !slices.Equal(got, want) {
			t.Errorf("the tree holds %v, want the table's %v", got, want)
		}
	})

	t.Run("the runner returns the script's output and status", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		script := `printf '%s\n' "$@"; printf 'refused\n' >&2; exit 7`
		stdout, stderr, status := ciCacheLRURun(t, script, "list", root, "120", "10", "60")
		if want := strings.Join([]string{"list", root, "120", "10", "60"}, "\n") + "\n"; stdout != want {
			t.Errorf("the script read %q, want its five arguments %q", stdout, want)
		}
		if want := "refused\n"; stderr != want {
			t.Errorf("the script's stderr is %q, want %q", stderr, want)
		}
		if status != 7 {
			t.Errorf("the script exited %d, want its own 7", status)
		}
	})

	t.Run("the reader splits the selected files from the reply", func(t *testing.T) {
		t.Parallel()
		selected := []string{
			filepath.Join("/vol", "00", "old-3h"),
			filepath.Join("/vol", "ab", "lru-100m"),
		}
		stdout := strings.Join(append(selected, "before_kib=768", "removed=2", "after_kib=512"), "\n") + "\n"
		files, reply := ciCacheLRURead(t, stdout)
		if !slices.Equal(files, selected) {
			t.Errorf("the reader split %v, want the file lines %v", files, selected)
		}
		if want := (ciCacheTrimReply{beforeKiB: 768, removed: 2, afterKiB: 512}); reply != want {
			t.Errorf("the reader read %+v, want the reply %+v", reply, want)
		}
	})

	t.Run("the reader takes a reply with no file line as an empty selection", func(t *testing.T) {
		t.Parallel()
		files, reply := ciCacheLRURead(t, "before_kib=0\nremoved=0\nafter_kib=0\n")
		if len(files) != 0 {
			t.Errorf("the reader split %v from output with no file line", files)
		}
		if want := (ciCacheTrimReply{}); reply != want {
			t.Errorf("the reader read %+v, want the zero reply", reply)
		}
	})

	t.Run("the size helper measures the tree in KiB", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, now)
		before := ciCacheLRUBefore(t, root)

		// A file of a whole number of KiB grows the measured volume by exactly
		// its size: du counts allocation, and every block size divides it.
		const grownKiB = 1024
		if err := os.WriteFile(filepath.Join(root, "grown"), bytes.Repeat([]byte{'y'}, grownKiB*1024), 0o644); err != nil {
			t.Fatalf("writing the grown file: %v", err)
		}
		if got := ciCacheLRUBefore(t, root) - before; got != grownKiB {
			t.Errorf("a %d KiB file grew the measured volume by %d KiB", grownKiB, got)
		}
	})
}
