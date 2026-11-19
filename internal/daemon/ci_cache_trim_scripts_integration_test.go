//go:build integration

package daemon

// The trim's programs run as root inside the runner's dind container: they are
// shell text, not Go, so the only proof of what they select and delete is
// running the text. This is the integration tier because each case starts sh,
// which the unit tier forbids; the temporary tree each case builds stands in
// for the ci-gocache volume (gt-by8lm5).

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

// ciCacheLRUPaths joins each tree name under root, the way the program prints
// the names it selects.
func ciCacheLRUPaths(root string, names ...string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, filepath.Join(root, filepath.FromSlash(name)))
	}
	return out
}

// TestIntegrationCICacheTrimLRUSelects: over its cap the list program selects
// the age rule's files and then the window's files oldest first, only until
// their sizes cover the excess, and removes nothing. When the floor keeps the
// volume over the cap, every window file is selected and none younger than the
// floor is, whatever the cap.
func TestIntegrationCICacheTrimLRUSelects(t *testing.T) {
	t.Parallel()

	// The volume is over its cap by the age rule's file plus what the window
	// has to cover after it, in KiB: the age rule's file is selected whatever
	// the cap and counts toward the excess, and 400 KiB is more than one 256 KiB
	// window file covers and less than two.
	const (
		ageFileKiB      = 256
		windowExcessKiB = 400
	)

	t.Run("over the cap the oldest files are selected until the excess is covered", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())
		before := ciCacheScriptFiles(t, root)
		capKiB := ciCacheLRUBefore(t, root) - ageFileKiB - windowExcessKiB

		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", strconv.FormatInt(capKiB, 10), strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 0 {
			t.Fatalf("the LRU program exited %d: %s", status, stderr)
		}
		files, reply := ciCacheLRURead(t, stdout)
		want := ciCacheLRUPaths(root, "00/old-3h", "ab/lru-100m", "00/lru-96m")
		if !slices.Equal(files, want) {
			t.Errorf("selected %v, want %v: the age rule's file (%d KiB), then the window's oldest until the %d KiB beyond it is covered, %d KiB over the cap in all", files, want, ageFileKiB, windowExcessKiB, ageFileKiB+windowExcessKiB)
		}
		if reply.removed != int64(len(want)) {
			t.Errorf("removed = %d, want %d", reply.removed, len(want))
		}
		if reply.afterKiB != reply.beforeKiB {
			t.Errorf("list reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
		}
		if after := ciCacheScriptFiles(t, root); !slices.Equal(after, before) {
			t.Errorf("the list removed files: %v, before %v", after, before)
		}
	})

	t.Run("a floor that keeps the volume over the cap selects no younger file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())
		before := ciCacheScriptFiles(t, root)

		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", "0", strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 0 {
			t.Fatalf("the LRU program exited %d: %s", status, stderr)
		}
		files, reply := ciCacheLRURead(t, stdout)
		want := ciCacheLRUPaths(root, "00/old-3h", "ab/lru-100m", "00/lru-96m", "ab/lru-90m")
		if !slices.Equal(files, want) {
			t.Errorf("selected %v, want every file past the floor, oldest first, and no other: %v", files, want)
		}
		for _, kept := range ciCacheLRUPaths(root, "ab/mid-45m", "ab/fresh-10m", "trim.txt") {
			if slices.Contains(files, kept) {
				t.Errorf("%s was selected, younger than the floor or above the top level", kept)
			}
		}
		if reply.removed != int64(len(want)) {
			t.Errorf("removed = %d, want %d", reply.removed, len(want))
		}
		if reply.afterKiB != reply.beforeKiB {
			t.Errorf("list reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
		}
		if after := ciCacheScriptFiles(t, root); !slices.Equal(after, before) {
			t.Errorf("the list removed files: %v, before %v", after, before)
		}
	})
}

// TestIntegrationCICacheTrimSelection feeds the selection parser the real list
// program's stdout, not a constant reply: the program and the parser agree on
// the wire only if every file line it prints is a clean absolute path strictly
// under the root, one per removed file, and the three figures it prints are the
// three the parser reads. The unit tier proves the parser on constant replies;
// this proves the pair on the program's own output, so a change to either side's
// format is red here rather than in the daemon.
func TestIntegrationCICacheTrimSelection(t *testing.T) {
	t.Parallel()

	t.Run("an over-cap selection parses with one file line per removed file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())

		// The same cap the selection cases use: over the volume by the age
		// rule's file plus 400 KiB, so three files are selected and the reply
		// has file lines to parse.
		capKiB := ciCacheLRUBefore(t, root) - 256 - 400
		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", strconv.FormatInt(capKiB, 10), strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 0 {
			t.Fatalf("the LRU program exited %d: %s", status, stderr)
		}
		files, reply := ciCacheLRURead(t, stdout)
		if len(files) == 0 {
			t.Fatalf("the program selected no file, so the parser would not read a file line:\n%s", stdout)
		}

		got, err := parseCICacheTrimSelection([]byte(stdout), root)
		if err != nil {
			t.Fatalf("the parser refused the list program's own stdout: %v:\n%s", err, stdout)
		}
		if got != reply {
			t.Errorf("the parser read %+v, the program's reply is %+v", got, reply)
		}
		if int64(len(files)) != reply.removed {
			t.Errorf("the reply has %d file line(s) but reports removed=%d", len(files), reply.removed)
		}
		if got.afterKiB != got.beforeKiB {
			t.Errorf("list reports the volume changing, %d to %d KiB", got.beforeKiB, got.afterKiB)
		}
	})

	t.Run("an under-cap reply with no file line parses", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())

		// An age past every file's, so the age rule selects none, and a cap
		// above the volume, so the cut's excess is negative and it stops at the
		// first candidate: the program prints the three figures only, and the
		// parser sees no file line.
		capKiB := ciCacheLRUBefore(t, root) + 1024
		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "600", strconv.FormatInt(capKiB, 10), strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 0 {
			t.Fatalf("the LRU program exited %d: %s", status, stderr)
		}
		files, reply := ciCacheLRURead(t, stdout)
		if len(files) != 0 {
			t.Fatalf("the under-cap program printed %d file line(s), want none:\n%s", len(files), stdout)
		}

		got, err := parseCICacheTrimSelection([]byte(stdout), root)
		if err != nil {
			t.Fatalf("the parser refused the under-cap reply: %v:\n%s", err, stdout)
		}
		if got != reply {
			t.Errorf("the parser read %+v, the program's reply is %+v", got, reply)
		}
		if got.removed != 0 {
			t.Errorf("removed = %d, want 0 under the cap", got.removed)
		}
	})
}

// TestIntegrationCICacheTrimLRUStrictOrder proves the selection's order: the
// files past the floor are printed strictly oldest first by mtime, not merely
// as a set holding the right names. The cap is 0, so the excess is the volume's
// whole size, more than the past-floor files total, and the cut never stops the
// read: every past-floor file is printed and the order is the sort's alone.
// Reversing that sort (sort -n to sort -rn) or shuffling it reorders the
// selection and turns this test red.
func TestIntegrationCICacheTrimLRUStrictOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ciCacheLRUTreeWrite(t, root, time.Now())
	before := ciCacheLRUBefore(t, root)

	stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", "0", strconv.Itoa(ciCacheLRUFloorMinutes))
	if status != 0 {
		t.Fatalf("the LRU program exited %d: %s", status, stderr)
	}
	files, reply := ciCacheLRURead(t, stdout)

	// The rows whose age passes the floor, oldest first: 180, 100, 96 and 90
	// minutes. mid-45m and fresh-10m are inside the floor, and trim.txt is above
	// the top level.
	want := ciCacheLRUPaths(root, "00/old-3h", "ab/lru-100m", "00/lru-96m", "ab/lru-90m")
	if !slices.Equal(files, want) {
		t.Fatalf("selected %v, want the files past the floor in mtime order, oldest first: %v", files, want)
	}
	if reply.beforeKiB != before {
		t.Errorf("before_kib = %d, want the volume's %d KiB", reply.beforeKiB, before)
	}
	if reply.removed != int64(len(want)) {
		t.Errorf("removed = %d, want %d", reply.removed, len(want))
	}
	if reply.afterKiB != before {
		t.Errorf("after_kib = %d, want the volume's %d KiB: the list removes nothing", reply.afterKiB, before)
	}
}

// TestIntegrationCICacheTrimLRUNoOvershoot: the cut stops at the file that
// covers the excess. The window is 540 minutes wide, the floor to the age, and
// its two oldest candidates are 2 minutes apart and both inside the age: the
// older covers the excess of 256 KiB on its own, so it is the only file
// selected. A program that takes the file after the one that covers the excess
// selects the second of the pair too, and this test goes red.
func TestIntegrationCICacheTrimLRUNoOvershoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now()
	ciCacheLRUTreeWrite(t, root, now)

	// The shared tree's candidates span 90 minutes, so this case adds the pair it
	// needs: 2 minutes apart, with the older file covering the excess, and 410
	// minutes above the tree's newest candidate, so the candidates span more than
	// 240 minutes.
	for _, f := range []struct {
		path   string
		ageMin int
	}{
		{path: filepath.Join("ab", "lru-500m"), ageMin: 500},
		{path: filepath.Join("ab", "lru-498m"), ageMin: 498},
	} {
		full := filepath.Join(root, f.path)
		if err := os.WriteFile(full, bytes.Repeat([]byte{'x'}, 256*1024), 0o644); err != nil {
			t.Fatalf("writing %s: %v", f.path, err)
		}
		mtime := now.Add(-time.Duration(f.ageMin) * time.Minute)
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatalf("dating %s: %v", f.path, err)
		}
	}
	before := ciCacheScriptFiles(t, root)

	// The excess is one file's size, measured the way the program measures the
	// volume, so the older of the pair covers it exactly.
	const excessKiB = 256
	capKiB := ciCacheLRUBefore(t, root) - excessKiB

	stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "600", strconv.FormatInt(capKiB, 10), strconv.Itoa(ciCacheLRUFloorMinutes))
	if status != 0 {
		t.Fatalf("the LRU program exited %d: %s", status, stderr)
	}
	files, reply := ciCacheLRURead(t, stdout)
	want := ciCacheLRUPaths(root, "ab/lru-500m")
	if !slices.Equal(files, want) {
		t.Errorf("selected %v, want only %v: the excess of %d KiB is the file's, so the file after it is not selected", files, want, excessKiB)
	}
	if reply.removed != 1 {
		t.Errorf("removed = %d, want 1", reply.removed)
	}
	if reply.afterKiB != reply.beforeKiB {
		t.Errorf("list reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
	}
	if after := ciCacheScriptFiles(t, root); !slices.Equal(after, before) {
		t.Errorf("the list removed files: %v, before %v", after, before)
	}
}

// TestIntegrationCICacheTrimLRUFloorAtListing proves the floor bounds the
// listing, not the selection: a file younger than ciCacheLRUFloorMinutes is
// never in the list, so no cap can select one, and a listing that loses the
// floor condition selects the floor-edge files and turns this test red.
//
// The tree is the shared one plus two files dated just inside the floor, the
// oldest files inside it: the first a floorless listing would reach once the
// past-floor files are taken. It is over the cap by more than the four
// past-floor files total, so the listing without the floor would keep selecting
// past them. The selection still covers what the floor allows: the past-floor
// files, which leave the volume over its cap.
func TestIntegrationCICacheTrimLRUFloorAtListing(t *testing.T) {
	t.Parallel()

	// More than the four past-floor files' 1024 KiB, so the excess is not
	// covered by them and a floorless listing selects the floor-edge files.
	const excessKiB = 1200

	for _, tt := range []struct {
		name  string
		capOf func(before int64) int64
	}{
		{name: "the excess is the whole volume", capOf: func(int64) int64 { return 0 }},
		{name: "the excess is more than the past-floor files cover",
			capOf: func(before int64) int64 { return before - excessKiB }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			now := time.Now()
			ciCacheLRUTreeWrite(t, root, now)

			for _, f := range []struct {
				path   string
				ageMin int
			}{
				{path: filepath.Join("ab", "floor-edge-59m"), ageMin: 59},
				{path: filepath.Join("ab", "floor-edge-55m"), ageMin: 55},
			} {
				full := filepath.Join(root, f.path)
				if err := os.WriteFile(full, bytes.Repeat([]byte{'x'}, 256*1024), 0o644); err != nil {
					t.Fatalf("writing %s: %v", f.path, err)
				}
				mtime := now.Add(-time.Duration(f.ageMin) * time.Minute)
				if err := os.Chtimes(full, mtime, mtime); err != nil {
					t.Fatalf("dating %s: %v", f.path, err)
				}
			}
			before := ciCacheScriptFiles(t, root)
			capKiB := tt.capOf(ciCacheLRUBefore(t, root))

			stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", strconv.FormatInt(capKiB, 10), strconv.Itoa(ciCacheLRUFloorMinutes))
			if status != 0 {
				t.Fatalf("the LRU program exited %d: %s", status, stderr)
			}
			files, reply := ciCacheLRURead(t, stdout)
			want := ciCacheLRUPaths(root, "00/old-3h", "ab/lru-100m", "00/lru-96m", "ab/lru-90m")
			if !slices.Equal(files, want) {
				t.Errorf("selected %v, want every past-floor file and no other: %v", files, want)
			}
			for _, kept := range ciCacheLRUPaths(root, "ab/floor-edge-59m", "ab/floor-edge-55m", "ab/mid-45m", "ab/fresh-10m") {
				if slices.Contains(files, kept) {
					t.Errorf("%s was selected: its mtime is inside the %d minute floor", kept, ciCacheLRUFloorMinutes)
				}
			}
			if reply.beforeKiB <= capKiB {
				t.Errorf("before_kib = %d, want the volume over its %d KiB cap", reply.beforeKiB, capKiB)
			}
			if reply.removed != int64(len(want)) {
				t.Errorf("removed = %d, want %d", reply.removed, len(want))
			}
			if reply.afterKiB != reply.beforeKiB {
				t.Errorf("list reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
			}
			if after := ciCacheScriptFiles(t, root); !slices.Equal(after, before) {
				t.Errorf("the list removed files: %v, before %v", after, before)
			}
		})
	}
}

// TestIntegrationCICacheTrimLRUUnderCapAndMode: the cut is the sum beyond the
// age rule, never a whole trim. A volume at or under its cap still selects the
// files past the age and nothing else; a volume one KiB over selects the oldest
// file past the floor, whose size covers the excess; and the program has the two
// modes list and apply, so any other mode is refused with exit 2 before it looks
// at the volume.
//
// The over-cap case runs with an age past every file's, so the age rule selects
// nothing and the selection is the cut's alone: removing the cut's exit selects
// every past-floor file and turns it red, and deleting the mode guard lets each
// other mode run as a list and turns the mode case red.
func TestIntegrationCICacheTrimLRUUnderCapAndMode(t *testing.T) {
	t.Parallel()

	t.Run("at or under the cap selects only the files past the age", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name   string
			capOf  func(before int64) int64
			reason string
		}{
			{name: "at the cap", capOf: func(before int64) int64 { return before },
				reason: "the excess is zero, so the cut adds nothing to the age rule"},
			{name: "under the cap", capOf: func(before int64) int64 { return before + 4096 },
				reason: "the excess is negative, so the cut adds nothing to the age rule"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				ciCacheLRUTreeWrite(t, root, time.Now())
				before := ciCacheLRUBefore(t, root)
				beforeFiles := ciCacheScriptFiles(t, root)

				stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", strconv.FormatInt(tt.capOf(before), 10), strconv.Itoa(ciCacheLRUFloorMinutes))
				if status != 0 {
					t.Fatalf("the LRU program exited %d: %s", status, stderr)
				}
				files, reply := ciCacheLRURead(t, stdout)
				want := ciCacheLRUPaths(root, "00/old-3h")
				if !slices.Equal(files, want) {
					t.Errorf("selected %v, want only the file past the 120 minute age, %v: %s", files, want, tt.reason)
				}
				if reply.removed != int64(len(want)) {
					t.Errorf("removed = %d, want %d", reply.removed, len(want))
				}
				if reply.afterKiB != reply.beforeKiB {
					t.Errorf("list reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
				}
				if after := ciCacheScriptFiles(t, root); !slices.Equal(after, beforeFiles) {
					t.Errorf("the list removed files: %v, before %v", after, beforeFiles)
				}
			})
		}
	})

	t.Run("one KiB over the cap selects the oldest file that covers it", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())
		before := ciCacheLRUBefore(t, root)
		beforeFiles := ciCacheScriptFiles(t, root)

		// An age past every file's: no file is past the age, so the age rule
		// selects none and only the cut can select.
		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "600", strconv.FormatInt(before-1, 10), strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 0 {
			t.Fatalf("the LRU program exited %d: %s", status, stderr)
		}
		files, reply := ciCacheLRURead(t, stdout)
		want := ciCacheLRUPaths(root, "00/old-3h")
		if !slices.Equal(files, want) {
			t.Errorf("selected %v, want only %v: the oldest file past the floor, whose 256 KiB covers the 1 KiB excess, stops the cut", files, want)
		}
		if reply.removed != int64(len(want)) {
			t.Errorf("removed = %d, want %d", reply.removed, len(want))
		}
		if reply.afterKiB != reply.beforeKiB {
			t.Errorf("list reports the volume changing, %d to %d KiB", reply.beforeKiB, reply.afterKiB)
		}
		if after := ciCacheScriptFiles(t, root); !slices.Equal(after, beforeFiles) {
			t.Errorf("the list removed files: %v, before %v", after, beforeFiles)
		}
	})

	t.Run("any mode but list and apply exits 2", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())
		beforeFiles := ciCacheScriptFiles(t, root)

		for _, mode := range []string{"remove", "", "LIST", "List"} {
			stdout, _, status := ciCacheLRURun(t, ciCacheLRUTrimScript, mode, root, "120", "0", strconv.Itoa(ciCacheLRUFloorMinutes))
			if status != 2 || stdout != "" {
				t.Errorf("mode %q: exit %d with output %q, want exit 2 and none", mode, status, stdout)
			}
			if after := ciCacheScriptFiles(t, root); !slices.Equal(after, beforeFiles) {
				t.Errorf("mode %q changed the volume: %v, before %v", mode, after, beforeFiles)
			}
		}
	})
}

// TestIntegrationCICacheTrimLRUApplyRemovesOldestFirst runs apply over the tree
// the list cases use, at the same cap: it removes exactly the files list prints —
// the age rule's file, then the window's oldest until the excess is covered,
// oldest first — and keeps the newest. Its reply is the three lines
// parseCICacheTrimReply reads and no file line, and its after_kib is the volume
// read again after the removal, at or under the cap.
//
// Removing the mode's removal branch leaves every file in place and turns this
// test red; printing the selection as list does makes the reply more than three
// lines and turns it red; keeping the list's after_kib equal to before_kib turns
// the re-read assertion red.
func TestIntegrationCICacheTrimLRUApplyRemovesOldestFirst(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ciCacheLRUTreeWrite(t, root, time.Now())
	before := ciCacheLRUBefore(t, root)

	// The excess is the age rule's file and 400 KiB beyond it, more than one
	// 256 KiB window file covers and less than two.
	const (
		ageFileKiB      = 256
		windowExcessKiB = 400
	)
	capKiB := before - ageFileKiB - windowExcessKiB
	capArg := strconv.FormatInt(capKiB, 10)
	floor := strconv.Itoa(ciCacheLRUFloorMinutes)

	// The selection list prints on this tree is the set apply must remove.
	listed, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", capArg, floor)
	if status != 0 {
		t.Fatalf("the LRU list exited %d: %s", status, stderr)
	}
	selected, _ := ciCacheLRURead(t, listed)
	wantRemoved := ciCacheLRUPaths(root, "00/old-3h", "ab/lru-100m", "00/lru-96m")
	if !slices.Equal(selected, wantRemoved) {
		t.Fatalf("list selected %v, want the age rule's file and the window's oldest until the excess is covered: %v", selected, wantRemoved)
	}

	stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "apply", root, "120", capArg, floor)
	if status != 0 {
		t.Fatalf("the LRU apply exited %d: %s", status, stderr)
	}
	// apply prints the reply only: a file line would make it unreadable to the
	// daemon's parser.
	reply, err := parseCICacheTrimReply([]byte(stdout))
	if err != nil {
		t.Fatalf("the apply's reply does not parse: %v:\n%s", err, stdout)
	}
	if want := int64(len(selected)); reply.removed != want {
		t.Errorf("removed = %d, want the %d files list selected", reply.removed, want)
	}
	if reply.beforeKiB != before {
		t.Errorf("before_kib = %d, want the volume's %d KiB", reply.beforeKiB, before)
	}

	// The files list did not select stay: the window file after the cut and the
	// two files inside the floor.
	kept := []string{filepath.Join("ab", "lru-90m"), filepath.Join("ab", "mid-45m"), filepath.Join("ab", "fresh-10m"), "trim.txt"}
	slices.Sort(kept)
	if got := ciCacheScriptFiles(t, root); !slices.Equal(got, kept) {
		t.Errorf("the volume holds %v after the apply, want %v: the selected files are the ones removed", got, kept)
	}
	for _, gone := range wantRemoved {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a selected file is still present after the apply: %s (stat error %v)", gone, err)
		}
	}

	if after := ciCacheLRUBefore(t, root); reply.afterKiB != after {
		t.Errorf("after_kib = %d, want the volume read again after the removal, %d KiB", reply.afterKiB, after)
	}
	if reply.afterKiB >= reply.beforeKiB {
		t.Errorf("after_kib = %d, want less than before_kib, %d: the apply removed files", reply.afterKiB, reply.beforeKiB)
	}
	if reply.afterKiB > capKiB {
		t.Errorf("after_kib = %d, want at or under the %d KiB cap", reply.afterKiB, capKiB)
	}
}

// TestIntegrationCICacheTrimLRUApplyEmptySelection removes nothing when the
// selection is empty: with an age past every file's and a cap the volume is
// under, no file is selected, so nothing is removed, the volume is unchanged and
// the reply reads removed=0 with after_kib equal to before_kib. The removal's
// `removed -gt 0` guard keeps rm from being started with no operand, which a
// shell that refuses it would end non-zero.
func TestIntegrationCICacheTrimLRUApplyEmptySelection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ciCacheLRUTreeWrite(t, root, time.Now())
	before := ciCacheLRUBefore(t, root)
	beforeFiles := ciCacheScriptFiles(t, root)

	stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "apply", root, "600", strconv.FormatInt(before+4096, 10), strconv.Itoa(ciCacheLRUFloorMinutes))
	if status != 0 {
		t.Fatalf("the LRU apply exited %d: %s", status, stderr)
	}
	reply, err := parseCICacheTrimReply([]byte(stdout))
	if err != nil {
		t.Fatalf("the apply's reply does not parse: %v:\n%s", err, stdout)
	}
	if reply.removed != 0 {
		t.Errorf("removed = %d, want 0: no file is past the age or over the cap", reply.removed)
	}
	if reply.afterKiB != reply.beforeKiB {
		t.Errorf("after_kib = %d, want before_kib, %d: an empty selection removes nothing", reply.afterKiB, reply.beforeKiB)
	}
	if got := ciCacheScriptFiles(t, root); !slices.Equal(got, beforeFiles) {
		t.Errorf("the apply removed files from an empty selection: %v, before %v", got, beforeFiles)
	}
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

// TestIntegrationCICacheTrimLRUArguments: the program computes with its three
// numbers in shell arithmetic, so a value that is not digits is refused with exit
// 2 before anything is computed or printed. A path that holds a newline is left
// out of the listing: it is never selected, and never read as a fragment of a path.
func TestIntegrationCICacheTrimLRUArguments(t *testing.T) {
	t.Parallel()

	t.Run("a number that is not digits is refused", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())
		good := map[string]string{"age": "120", "cap": "0", "floor": "60"}
		for _, field := range []string{"age", "cap", "floor"} {
			for _, bad := range []string{"", "-1", "1+1", "$(id)", "1 2", "0x10"} {
				args := map[string]string{"age": good["age"], "cap": good["cap"], "floor": good["floor"]}
				args[field] = bad
				stdout, _, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, args["age"], args["cap"], args["floor"])
				if status != 2 || stdout != "" {
					t.Errorf("%s %q: exit %d with output %q, want exit 2 and none", field, bad, status, stdout)
				}
			}
		}
	})

	t.Run("a path with a newline is never selected", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		now := time.Now()
		ciCacheLRUTreeWrite(t, root, now)
		odd := filepath.Join(root, "ab", "odd\nname")
		if err := os.WriteFile(odd, bytes.Repeat([]byte{'x'}, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(odd, now.Add(-95*time.Minute), now.Add(-95*time.Minute)); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "120", "0", strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 0 {
			t.Fatalf("the LRU program exited %d: %s", status, stderr)
		}
		files, reply := ciCacheLRURead(t, stdout)
		want := ciCacheLRUPaths(root, "00/old-3h", "ab/lru-100m", "00/lru-96m", "ab/lru-90m")
		if !slices.Equal(files, want) {
			t.Errorf("selected %v, want %v and no path with a newline in it", files, want)
		}
		if strings.Contains(stdout, "odd") || strings.Contains(stdout, "name") {
			t.Errorf("the newline path, or a fragment of it, was printed:\n%s", stdout)
		}
		if reply.removed != int64(len(want)) {
			t.Errorf("removed = %d, want %d", reply.removed, len(want))
		}
	})
}

// TestIntegrationCICacheTrimLRUContract pins the list program's contract by
// running it, not by reading its text: a volume root that is not there is
// refused with exit 3 and a message on stderr; an argument count other than the
// five the daemon passes is refused with exit 2 before the program reads an
// argument, so the status does not depend on the shell's unset-variable
// behavior; and a successful run ends with the three reply lines in the order
// the parser reads them: before_kib, removed, after_kib.
func TestIntegrationCICacheTrimLRUContract(t *testing.T) {
	t.Parallel()

	t.Run("a missing volume root exits 3 with a message", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "absent")
		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", missing, "120", "0", strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 3 {
			t.Errorf("exit %d, want 3 for a volume root that is not there", status)
		}
		if stderr == "" {
			t.Error("stderr is empty, want a message that the volume path was not found")
		}
		if stdout != "" {
			t.Errorf("stdout is %q, want none", stdout)
		}
	})

	t.Run("an argument count other than five exits 2", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())
		five := []string{"list", root, "120", "0", strconv.Itoa(ciCacheLRUFloorMinutes)}
		for _, tt := range []struct {
			name string
			args []string
		}{
			{name: "no argument", args: nil},
			{name: "four arguments, without the floor", args: five[:4]},
			{name: "six arguments, one past the floor", args: append(slices.Clone(five), "extra")},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				// ciCacheLRURun always passes the program's five arguments, so run
				// the script through sh directly with the wrong count.
				argv := append([]string{"-c", ciCacheLRUTrimScript, "ci-cache-trim"}, tt.args...)
				cmd := exec.Command("/bin/sh", argv...)
				cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				err := cmd.Run()
				status := 0
				if err != nil {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) {
						t.Fatalf("running the LRU program: %v", err)
					}
					status = exitErr.ExitCode()
				}
				if status != 2 {
					t.Errorf("exit %d with %d argument(s), want 2", status, len(tt.args))
				}
				if stdout.Len() != 0 {
					t.Errorf("stdout is %q, want none", stdout.String())
				}
			})
		}
	})

	t.Run("a successful reply ends with the three figures in order", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		ciCacheLRUTreeWrite(t, root, time.Now())
		before := ciCacheLRUBefore(t, root)

		// An age past every file's, so the age rule selects nothing and only the
		// cut selects a known set; cap 0 makes the excess the whole volume.
		stdout, stderr, status := ciCacheLRURun(t, ciCacheLRUTrimScript, "list", root, "600", "0", strconv.Itoa(ciCacheLRUFloorMinutes))
		if status != 0 {
			t.Fatalf("the LRU program exited %d: %s", status, stderr)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) < 3 {
			t.Fatalf("the LRU program printed %d line(s), want a reply at least:\n%s", len(lines), stdout)
		}
		for i, key := range []string{"before_kib", "removed", "after_kib"} {
			if line := lines[len(lines)-3+i]; !strings.HasPrefix(line, key+"=") {
				t.Errorf("the reply's line %d is %q, want %s=N", i+1, line, key)
			}
		}
		files, reply := ciCacheLRURead(t, stdout)
		if reply.beforeKiB != before {
			t.Errorf("before_kib = %d, want the volume's %d KiB", reply.beforeKiB, before)
		}
		if reply.removed != int64(len(files)) {
			t.Errorf("removed = %d, want one per file line, %d", reply.removed, len(files))
		}
		if reply.afterKiB != reply.beforeKiB {
			t.Errorf("after_kib = %d, want before_kib, %d: the list removes nothing", reply.afterKiB, reply.beforeKiB)
		}
	})
}
