package deployerinbox_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// memFS is AnswerFS in memory, recording each call so a test sees the order
// and permissions WriteAnswer used. A create applies a umask of 077, as a real
// process would, so a missing Chmod leaves the file at 0600.
type memFS struct {
	files     map[string]memFile
	creates   []call
	chmods    []call
	links     [][2]string
	removes   []string
	createErr error
	chmodErr  error
	linkErr   error
	removeErr error
}

// call is one create or chmod, with the mode it was asked for.
type call struct {
	name string
	perm fs.FileMode
}

// memFile is one file in memFS.
type memFile struct {
	data []byte
	perm fs.FileMode
}

func newMemFS() *memFS {
	return &memFS{files: make(map[string]memFile)}
}

func (f *memFS) CreateExclusive(name string, data []byte, perm fs.FileMode) error {
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.files[name]; ok {
		return fmt.Errorf("create %s: %w", name, fs.ErrExist)
	}
	f.files[name] = memFile{data: append([]byte(nil), data...), perm: perm &^ 0o077}
	f.creates = append(f.creates, call{name: name, perm: perm})
	return nil
}

func (f *memFS) Chmod(name string, mode fs.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	file, ok := f.files[name]
	if !ok {
		return fmt.Errorf("chmod %s: %w", name, fs.ErrNotExist)
	}
	file.perm = mode.Perm()
	f.files[name] = file
	f.chmods = append(f.chmods, call{name: name, perm: mode})
	return nil
}

func (f *memFS) Link(oldpath, newpath string) error {
	if f.linkErr != nil {
		return f.linkErr
	}
	file, ok := f.files[oldpath]
	if !ok {
		return fmt.Errorf("link %s: %w", oldpath, fs.ErrNotExist)
	}
	if _, ok := f.files[newpath]; ok {
		return fmt.Errorf("link %s: %w", newpath, fs.ErrExist)
	}
	f.files[newpath] = file
	f.links = append(f.links, [2]string{oldpath, newpath})
	return nil
}

func (f *memFS) Remove(name string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	if _, ok := f.files[name]; !ok {
		return fmt.Errorf("remove %s: %w", name, fs.ErrNotExist)
	}
	delete(f.files, name)
	f.removes = append(f.removes, name)
	return nil
}

func TestDeployerInboxAnswer(t *testing.T) {
	t.Parallel()

	const dir = "/state/answers"
	at := time.Date(2026, 11, 19, 18, 14, 50, 0, time.UTC)
	target := filepath.Join(dir, goodID+".json")
	tmp := target + ".tmp"
	good := deployerinbox.Answer{
		ID:         goodID,
		Status:     deployerinbox.StatusApplied,
		PlanSHA256: strings.Repeat("a", 64),
		Detail:     "applied 3 kinds against control",
		At:         at,
	}

	t.Run("writes a temp file at the answer mode", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		if err := deployerinbox.WriteAnswer(fsys, dir, good); err != nil {
			t.Fatalf("WriteAnswer = %v, want no error", err)
		}

		file, ok := fsys.files[target]
		if !ok {
			t.Fatalf("no answer file at %s; files are %v", target, keys(fsys.files))
		}
		if got := file.perm.Perm(); got != 0o644 {
			t.Fatalf("the answer mode is %o, want 644 despite the create's umask", got)
		}
		if len(fsys.creates) != 1 || fsys.creates[0].name != tmp {
			t.Fatalf("CreateExclusive calls are %v, want one for %s", fsys.creates, tmp)
		}
		if len(fsys.chmods) != 1 || fsys.chmods[0].name != tmp {
			t.Fatalf("Chmod calls are %v, want one for %s", fsys.chmods, tmp)
		}
		if got := fsys.chmods[0].perm.Perm(); got != 0o644 {
			t.Fatalf("the chmod mode is %o, want 644", got)
		}
		if len(fsys.links) != 1 || fsys.links[0] != [2]string{tmp, target} {
			t.Fatalf("Link calls are %v, want %s -> %s", fsys.links, tmp, target)
		}
		if len(fsys.removes) != 1 || fsys.removes[0] != tmp {
			t.Fatalf("Remove calls are %v, want one for %s", fsys.removes, tmp)
		}
		if _, ok := fsys.files[tmp]; ok {
			t.Fatalf("the temp file %s was left behind", tmp)
		}

		var got deployerinbox.Answer
		if err := json.Unmarshal(file.data, &got); err != nil {
			t.Fatalf("decode the written answer: %v", err)
		}
		if got.ID != good.ID || got.Status != good.Status || got.PlanSHA256 != good.PlanSHA256 || got.Detail != good.Detail {
			t.Fatalf("the written answer is %+v, want %+v", got, good)
		}
		if !got.At.Equal(good.At) {
			t.Fatalf("the written answer time is %s, want %s", got.At, good.At)
		}
	})

	for _, status := range []string{deployerinbox.StatusRefused, deployerinbox.StatusFailed} {
		status := status
		t.Run("accepts a "+status+" answer with no plan sha", func(t *testing.T) {
			t.Parallel()
			fsys := newMemFS()
			answer := good
			answer.Status = status
			answer.PlanSHA256 = ""
			if err := deployerinbox.WriteAnswer(fsys, dir, answer); err != nil {
				t.Fatalf("WriteAnswer = %v, want no error", err)
			}
			if _, ok := fsys.files[target]; !ok {
				t.Fatalf("no answer file at %s", target)
			}
		})
	}

	refuseSHA := []struct {
		name   string
		status string
		digest string
	}{
		{name: "an empty digest on a proposed answer", status: deployerinbox.StatusProposed, digest: ""},
		{name: "an empty digest on an applied answer", status: deployerinbox.StatusApplied, digest: ""},
		{name: "an upper-case digest", status: deployerinbox.StatusApplied, digest: strings.Repeat("A", 64)},
		{name: "a short digest", status: deployerinbox.StatusApplied, digest: strings.Repeat("a", 63)},
		{name: "a non-hex digest", status: deployerinbox.StatusApplied, digest: strings.Repeat("g", 64)},
		{name: "a bad digest on a refused answer", status: deployerinbox.StatusRefused, digest: strings.Repeat("a", 63)},
	}
	for _, tc := range refuseSHA {
		tc := tc
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newMemFS()
			answer := good
			answer.Status = tc.status
			answer.PlanSHA256 = tc.digest
			if err := deployerinbox.WriteAnswer(fsys, dir, answer); err == nil {
				t.Fatal("WriteAnswer accepted the plan_sha256, want an error")
			}
			if len(fsys.files) != 0 {
				t.Fatalf("WriteAnswer wrote %v, want nothing", keys(fsys.files))
			}
		})
	}

	t.Run("refuses an answer that exists", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		first := []byte("the first answer")
		fsys.files[target] = memFile{data: first, perm: 0o644}

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("WriteAnswer = %v, want an error wrapping fs.ErrExist", err)
		}
		if got := string(fsys.files[target].data); got != string(first) {
			t.Fatalf("the existing answer is now %q, want %q", got, first)
		}
		if _, ok := fsys.files[tmp]; ok {
			t.Fatalf("the temp file %s was left behind", tmp)
		}
		if len(fsys.links) != 0 {
			t.Fatalf("Link calls are %v, want none to publish", fsys.links)
		}
	})

	t.Run("refuses a detail over 4096 bytes", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		answer := good
		answer.Detail = strings.Repeat("x", deployerinbox.MaxDetailBytes+1)

		if err := deployerinbox.WriteAnswer(fsys, dir, answer); err == nil {
			t.Fatal("WriteAnswer truncated an oversize detail, want an error")
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer wrote %v, want nothing", keys(fsys.files))
		}
	})

	t.Run("refuses an id that is not a request id", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		answer := good
		answer.ID = "../escape"

		if err := deployerinbox.WriteAnswer(fsys, dir, answer); err == nil {
			t.Fatal("WriteAnswer accepted an id that is a path, want an error")
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer wrote %v, want nothing", keys(fsys.files))
		}
	})

	t.Run("refuses an unknown status", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		answer := good
		answer.Status = "done"

		if err := deployerinbox.WriteAnswer(fsys, dir, answer); err == nil {
			t.Fatal("WriteAnswer accepted the status done, want an error")
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer wrote %v, want nothing", keys(fsys.files))
		}
	})

	t.Run("returns an encode failure", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		answer := good
		answer.At = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

		if err := deployerinbox.WriteAnswer(fsys, dir, answer); err == nil {
			t.Fatal("WriteAnswer encoded a year outside 0-9999, want an error")
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer wrote %v, want nothing", keys(fsys.files))
		}
	})

	t.Run("returns a create failure", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		createErr := errors.New("no space left")
		fsys.createErr = createErr

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, createErr) {
			t.Fatalf("WriteAnswer = %v, want the CreateExclusive error %v", err, createErr)
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer wrote %v, want nothing", keys(fsys.files))
		}
	})

	t.Run("returns a remove failure after publishing", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		removeErr := errors.New("permission denied")
		fsys.removeErr = removeErr

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, removeErr) {
			t.Fatalf("WriteAnswer = %v, want the Remove error %v", err, removeErr)
		}
		if _, ok := fsys.files[target]; !ok {
			t.Fatalf("the answer at %s is missing after a failed temp removal", target)
		}
	})
}

func TestDeployerInboxAnswerCleanup(t *testing.T) {
	t.Parallel()

	const dir = "/state/answers"
	target := filepath.Join(dir, goodID+".json")
	tmp := target + ".tmp"
	good := deployerinbox.Answer{
		ID:         goodID,
		Status:     deployerinbox.StatusApplied,
		PlanSHA256: strings.Repeat("a", 64),
		Detail:     "applied 3 kinds against control",
		At:         time.Date(2026, 11, 19, 18, 14, 50, 0, time.UTC),
	}

	t.Run("removes the temp file when Chmod fails", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		chmodErr := errors.New("chmod refused")
		fsys.chmodErr = chmodErr

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, chmodErr) {
			t.Fatalf("WriteAnswer = %v, want the Chmod error %v", err, chmodErr)
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer left %v behind, want no temp file and no answer", keys(fsys.files))
		}
		if len(fsys.removes) != 1 || fsys.removes[0] != tmp {
			t.Fatalf("Remove calls are %v, want one for %s", fsys.removes, tmp)
		}
	})

	t.Run("removes the temp file when Link fails", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		linkErr := errors.New("link refused")
		fsys.linkErr = linkErr

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, linkErr) {
			t.Fatalf("WriteAnswer = %v, want the Link error %v", err, linkErr)
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer left %v behind, want no temp file and no answer", keys(fsys.files))
		}
		if len(fsys.removes) != 1 || fsys.removes[0] != tmp {
			t.Fatalf("Remove calls are %v, want one for %s", fsys.removes, tmp)
		}
	})

	t.Run("reports a temp removal that fails after Chmod fails", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		chmodErr := errors.New("chmod refused")
		removeErr := errors.New("remove refused")
		fsys.chmodErr = chmodErr
		fsys.removeErr = removeErr

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, chmodErr) {
			t.Fatalf("WriteAnswer = %v, want the Chmod error %v", err, chmodErr)
		}
		if !errors.Is(err, removeErr) {
			t.Fatalf("WriteAnswer = %v, want the Remove error %v", err, removeErr)
		}
		if _, ok := fsys.files[tmp]; !ok {
			t.Fatalf("the fake refused the removal, so the temp file %s should still be there", tmp)
		}
	})
}

// keys is the file names in files, for a failure message.
func keys(files map[string]memFile) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}
