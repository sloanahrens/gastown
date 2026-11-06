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
// through sh, with the volume root and the age in minutes as argv data — and
// returns its output and exit status. The command's environment is exactly
// PATH and TMPDIR: nothing is inherited, so neither a tool on the caller's
// PATH nor the shell itself can be swapped for the system's.
func ciCacheScriptRun(t *testing.T, script, root, minutes string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script, "ci-cache-trim", root, minutes)
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
// Expected to fail under four changes to the programs, each run by hand on its
// bead: flipping the age test from older-than to newer-than (-mmin +age to
// -mmin -age, so the newest files go), dropping the depth floor (-mindepth 2 to
// -mindepth 0, so the root's own files go), and moving the missing-path exit
// away from ciCacheMissingPathExit (gt-by8lm5); and restoring the depth-1
// empty-directory sweep (-mindepth 2 to -mindepth 1, so the cache's shard
// directory goes) (gt-qaqvf4).
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

	t.Run("a missing root is the missing-path status", func(t *testing.T) {
		t.Parallel()
		for name, script := range map[string]string{
			"the dry run": ciCacheListScript,
			"the apply":   ciCacheTrimScript,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := filepath.Join(t.TempDir(), "absent")
				stdout, stderr, status := ciCacheScriptRun(t, script, root, ciCacheScriptAgeMinutes)
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
