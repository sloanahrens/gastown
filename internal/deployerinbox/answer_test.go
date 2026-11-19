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

// memFS is AnswerFS in memory, recording each write and rename so a test sees
// the order and permissions WriteAnswer used.
type memFS struct {
	files     map[string]memFile
	writes    []write
	renames   [][2]string
	statErr   error
	writeErr  error
	renameErr error
}

// write is one WriteFile call.
type write struct {
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

func (f *memFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[name] = memFile{data: append([]byte(nil), data...), perm: perm}
	f.writes = append(f.writes, write{name: name, perm: perm})
	return nil
}

func (f *memFS) Rename(oldpath, newpath string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	file, ok := f.files[oldpath]
	if !ok {
		return fmt.Errorf("rename %s: %w", oldpath, fs.ErrNotExist)
	}
	delete(f.files, oldpath)
	f.files[newpath] = file
	f.renames = append(f.renames, [2]string{oldpath, newpath})
	return nil
}

func (f *memFS) Stat(name string) (fs.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	file, ok := f.files[name]
	if !ok {
		return nil, fmt.Errorf("stat %s: %w", name, fs.ErrNotExist)
	}
	return memInfo{name: filepath.Base(name), size: int64(len(file.data)), mode: file.perm}, nil
}

// memInfo is the fs.FileInfo memFS reports for a file it holds.
type memInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return i.mode }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }

func TestDeployerInboxAnswer(t *testing.T) {
	t.Parallel()

	const dir = "/state/answers"
	at := time.Date(2026, 11, 19, 18, 14, 50, 0, time.UTC)
	target := filepath.Join(dir, goodID+".json")
	good := deployerinbox.Answer{
		ID:         goodID,
		Status:     deployerinbox.StatusApplied,
		PlanSHA256: strings.Repeat("a", 64),
		Detail:     "applied 3 kinds against control",
		At:         at,
	}

	t.Run("writes by temp file and rename", func(t *testing.T) {
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
			t.Fatalf("the answer mode is %o, want 644", got)
		}
		if len(fsys.writes) != 1 {
			t.Fatalf("WriteFile was called %d times, want 1", len(fsys.writes))
		}
		if tmp := fsys.writes[0].name; tmp == target {
			t.Fatalf("the answer was written straight to %s, want a temp file renamed onto it", target)
		}
		if got := fsys.writes[0].perm.Perm(); got != 0o644 {
			t.Fatalf("the temp file mode is %o, want 644", got)
		}
		if len(fsys.renames) != 1 {
			t.Fatalf("Rename was called %d times, want 1", len(fsys.renames))
		}
		if want := [2]string{fsys.writes[0].name, target}; fsys.renames[0] != want {
			t.Fatalf("Rename%v, want %v", fsys.renames[0], want)
		}
		if _, ok := fsys.files[fsys.writes[0].name]; ok {
			t.Fatalf("the temp file %s was left behind", fsys.writes[0].name)
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

	t.Run("refuses an answer that exists", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		first := []byte("the first answer")
		fsys.files[target] = memFile{data: first, perm: 0o644}

		if err := deployerinbox.WriteAnswer(fsys, dir, good); err == nil {
			t.Fatal("WriteAnswer replaced an existing answer, want an error")
		}
		if got := string(fsys.files[target].data); got != string(first) {
			t.Fatalf("the existing answer is now %q, want %q", got, first)
		}
		if len(fsys.writes) != 0 || len(fsys.renames) != 0 {
			t.Fatalf("WriteAnswer wrote %d files and renamed %d, want none", len(fsys.writes), len(fsys.renames))
		}
	})

	t.Run("returns a Stat error that is not not-exist", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		fsys.statErr = fs.ErrPermission

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("WriteAnswer = %v, want the Stat error fs.ErrPermission", err)
		}
		if len(fsys.files) != 0 {
			t.Fatalf("WriteAnswer wrote %v, want nothing", keys(fsys.files))
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

	t.Run("returns a write failure", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		writeErr := errors.New("disk full")
		fsys.writeErr = writeErr

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, writeErr) {
			t.Fatalf("WriteAnswer = %v, want the WriteFile error %v", err, writeErr)
		}
		if _, ok := fsys.files[target]; ok {
			t.Fatalf("WriteAnswer published %s after a write failure", target)
		}
	})

	t.Run("returns a rename failure", func(t *testing.T) {
		t.Parallel()
		fsys := newMemFS()
		renameErr := errors.New("cross-device link")
		fsys.renameErr = renameErr

		err := deployerinbox.WriteAnswer(fsys, dir, good)
		if !errors.Is(err, renameErr) {
			t.Fatalf("WriteAnswer = %v, want the Rename error %v", err, renameErr)
		}
		if _, ok := fsys.files[target]; ok {
			t.Fatalf("WriteAnswer published %s after a rename failure", target)
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
