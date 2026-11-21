package deployerinbox_test

import (
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// osfsDir returns a temporary directory set to 0700, so the test does not
// depend on the umask of the runner.
func osfsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	//testpolicy:allow no-exec-files — the target is the fixture directory: a directory needs its search bit to be entered
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod the temporary directory: %v", err)
	}
	return dir
}

func TestDeployerInboxOSFSCreate(t *testing.T) {
	t.Parallel()

	dir := osfsDir(t)
	path := filepath.Join(dir, "answer.tmp")
	fsys := deployerinbox.OSAnswerFS{}

	if err := fsys.CreateExclusive(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("CreateExclusive on a new path: %v", err)
	}

	t.Run("writes the data", func(t *testing.T) {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the created file: %v", err)
		}
		if string(got) != "first" {
			t.Errorf("created file holds %q, want %q", got, "first")
		}
	})

	t.Run("fails on an existing path and leaves it unchanged", func(t *testing.T) {
		if err := fsys.CreateExclusive(path, []byte("second"), 0o600); err == nil {
			t.Fatal("CreateExclusive on an existing path succeeded, want an error")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the file after the refused create: %v", err)
		}
		if string(got) != "first" {
			t.Errorf("refused create changed the file to %q, want %q", got, "first")
		}
	})

	t.Run("Chmod sets the mode to 0644", func(t *testing.T) {
		if err := fsys.Chmod(path, 0o644); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat the file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("mode is %o, want 644", got)
		}
	})

	t.Run("Chmod fails on a missing path", func(t *testing.T) {
		err := fsys.Chmod(filepath.Join(dir, "missing"), 0o644)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Chmod on a missing path returned %v, want fs.ErrNotExist", err)
		}
	})
}

// TestDeployerInboxOSFSCreateWriteFails drives the write failure. A zero file
// size limit makes the write fail with EFBIG. The limit is process-wide, so
// this test is not parallel: Go resumes parallel tests only after the serial
// ones finish, and Cleanup restores the limit before it returns.
//
//testpolicy:allow parallel — sets the process-wide file size limit, which a parallel test must not see mid-run
func TestDeployerInboxOSFSCreateWriteFails(t *testing.T) {
	dir := osfsDir(t)
	path := filepath.Join(dir, "answer.tmp")

	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatalf("read the file size limit: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() { signal.Reset(syscall.SIGXFSZ) })
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: old.Max}); err != nil {
		t.Fatalf("set the file size limit to zero: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Errorf("restore the file size limit: %v", err)
		}
	})

	err := deployerinbox.OSAnswerFS{}.CreateExclusive(path, []byte("answer"), 0o600)
	if !errors.Is(err, syscall.EFBIG) {
		t.Errorf("CreateExclusive with a failing write returned %v, want EFBIG", err)
	}
}

func TestDeployerInboxOSFSLink(t *testing.T) {
	t.Parallel()

	dir := osfsDir(t)
	tmp := filepath.Join(dir, "answer.json.tmp")
	target := filepath.Join(dir, "answer.json")
	fsys := deployerinbox.OSAnswerFS{}

	if err := fsys.CreateExclusive(tmp, []byte("answer"), 0o644); err != nil {
		t.Fatalf("CreateExclusive for the temp file: %v", err)
	}

	t.Run("publishes the file when the target is absent", func(t *testing.T) {
		if err := fsys.Link(tmp, target); err != nil {
			t.Fatalf("Link to a new target: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read the published file: %v", err)
		}
		if string(got) != "answer" {
			t.Errorf("published file holds %q, want %q", got, "answer")
		}
	})

	t.Run("fails when the target exists and leaves it unchanged", func(t *testing.T) {
		// A source of its own, so the refusal is for the existing target and
		// not for a source the publish step already moved.
		other := filepath.Join(dir, "other.tmp")
		if err := fsys.CreateExclusive(other, []byte("replacement"), 0o644); err != nil {
			t.Fatalf("CreateExclusive for the refused source: %v", err)
		}
		if err := fsys.Link(other, target); err == nil {
			t.Fatal("Link onto an existing target succeeded, want an error")
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read the target after the refused link: %v", err)
		}
		if string(got) != "answer" {
			t.Errorf("refused link changed the target to %q, want %q", got, "answer")
		}
	})

	t.Run("Remove deletes the temp file", func(t *testing.T) {
		if err := fsys.Remove(tmp); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("temp file still present after Remove: stat error %v", err)
		}
	})

	t.Run("Remove fails when the file is already gone", func(t *testing.T) {
		if err := fsys.Remove(tmp); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Remove on a missing file returned %v, want fs.ErrNotExist", err)
		}
	})
}

func TestDeployerInboxOSFSWriteAnswer(t *testing.T) {
	t.Parallel()

	dir := osfsDir(t)
	a := deployerinbox.Answer{
		ID:         goodID,
		Status:     deployerinbox.StatusApplied,
		PlanSHA256: strings.Repeat("a", 64),
		Detail:     "applied 3 kinds against control",
		At:         time.Date(2026, 11, 19, 18, 14, 50, 0, time.UTC),
	}

	if err := deployerinbox.WriteAnswer(deployerinbox.OSAnswerFS{}, dir, a); err != nil {
		t.Fatalf("WriteAnswer over OSAnswerFS: %v", err)
	}

	t.Run("the answer exists with mode 0644", func(t *testing.T) {
		info, err := os.Stat(filepath.Join(dir, goodFile))
		if err != nil {
			t.Fatalf("stat the answer: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("answer mode is %o, want 644", got)
		}
	})

	t.Run("no temp file remains", func(t *testing.T) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read the answer directory: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != goodFile {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("directory holds %v, want only %s", names, goodFile)
		}
	})
}
