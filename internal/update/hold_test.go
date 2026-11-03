package update

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

const testTownRoot = "/town"

// fakeEntry is one file the fake FileOps holds.
type fakeEntry struct {
	data []byte
	mode os.FileMode
}

// fakeFiles is an in-memory FileOps. CreateExclusive holds the lock across the
// existence check and the write, so two Holds sharing one fake race the way two
// processes race on one path.
type fakeFiles struct {
	mu        sync.Mutex
	files     map[string]fakeEntry
	statErr   map[string]error
	readErr   map[string]error
	createErr map[string]error
	writeErr  map[string]error
	removeErr map[string]error

	// afterStat runs after a Stat that found no file, so a test can put a hold
	// in place between Acquire's stat and its exclusive create.
	afterStat func()
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{
		files:     map[string]fakeEntry{},
		statErr:   map[string]error{},
		readErr:   map[string]error{},
		createErr: map[string]error{},
		writeErr:  map[string]error{},
		removeErr: map[string]error{},
	}
}

func pathError(op, path string, err error) error {
	return &fs.PathError{Op: op, Path: path, Err: err}
}

func (f *fakeFiles) Stat(path string) error {
	f.mu.Lock()
	injected := f.statErr[path]
	_, exists := f.files[path]
	f.mu.Unlock()
	if injected != nil {
		return injected
	}
	if exists {
		return nil
	}
	if f.afterStat != nil {
		f.afterStat()
	}
	return pathError("stat", path, fs.ErrNotExist)
}

func (f *fakeFiles) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.readErr[path]; err != nil {
		return nil, err
	}
	entry, ok := f.files[path]
	if !ok {
		return nil, pathError("open", path, fs.ErrNotExist)
	}
	return append([]byte(nil), entry.data...), nil
}

func (f *fakeFiles) CreateExclusive(path string, data []byte, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createErr[path]; err != nil {
		return err
	}
	if _, ok := f.files[path]; ok {
		return pathError("open", path, fs.ErrExist)
	}
	f.files[path] = fakeEntry{data: append([]byte(nil), data...), mode: mode}
	return nil
}

func (f *fakeFiles) WriteFile(path string, data []byte, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writeErr[path]; err != nil {
		return err
	}
	f.files[path] = fakeEntry{data: append([]byte(nil), data...), mode: mode}
	return nil
}

func (f *fakeFiles) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.removeErr[path]; err != nil {
		return err
	}
	if _, ok := f.files[path]; !ok {
		return pathError("remove", path, fs.ErrNotExist)
	}
	delete(f.files, path)
	return nil
}

// put seeds a file a person set before the run.
func (f *fakeFiles) put(path, content string, mode os.FileMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = fakeEntry{data: []byte(content), mode: mode}
}

// entry returns a copy of the file at path, and whether it exists.
func (f *fakeFiles) entry(path string) (fakeEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.files[path]
	return fakeEntry{data: append([]byte(nil), entry.data...), mode: entry.mode}, ok
}

func TestNewHoldUsesTheDispatchHoldPath(t *testing.T) {
	t.Parallel()
	hold := NewHold(testTownRoot, newFakeFiles())
	if want := dispatch.HoldFilePath(testTownRoot); hold.Path != want {
		t.Errorf("Path = %q, want %q", hold.Path, want)
	}
}

func TestAcquireWritesNoteAndOwns(t *testing.T) {
	t.Parallel()
	const note = "gt update 20261103T102200Z-furiosa: draining"
	files := newFakeFiles()
	hold := NewHold(testTownRoot, files)

	rec, err := hold.Acquire(note)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !rec.Owned {
		t.Error("Owned = false, want true")
	}
	if rec.Note != note {
		t.Errorf("Note = %q, want %q", rec.Note, note)
	}
	entry, ok := files.entry(hold.Path)
	if !ok {
		t.Fatalf("hold file %s was not created", hold.Path)
	}
	if string(entry.data) != note {
		t.Errorf("hold file content = %q, want %q", entry.data, note)
	}
	if entry.mode != 0o644 {
		t.Errorf("hold file mode = %o, want 644", entry.mode)
	}
}

func TestAcquireRespectsAForeignHold(t *testing.T) {
	t.Parallel()
	const foreign = "operator hold: maintenance window"
	files := newFakeFiles()
	hold := NewHold(testTownRoot, files)
	files.put(hold.Path, foreign, 0o600)

	rec, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if rec.Owned {
		t.Error("Owned = true, want false for a hold the run did not create")
	}
	if rec.Note != foreign {
		t.Errorf("Note = %q, want the file's content %q", rec.Note, foreign)
	}
	entry, ok := files.entry(hold.Path)
	if !ok {
		t.Fatalf("foreign hold file %s was removed by Acquire", hold.Path)
	}
	if string(entry.data) != foreign || entry.mode != 0o600 {
		t.Errorf("foreign hold = (%q, %o), want (%q, 600) untouched", entry.data, entry.mode, foreign)
	}
	if err := hold.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := files.entry(hold.Path); !ok {
		t.Errorf("Release removed the foreign hold %s", hold.Path)
	}
}

func TestAcquireRacesResolveToOneOwner(t *testing.T) {
	t.Parallel()
	files := newFakeFiles()
	notes := []string{
		"gt update 20261103T102200Z-furiosa: draining",
		"gt update 20261103T102300Z-immortan: draining",
	}
	holds := []*Hold{NewHold(testTownRoot, files), NewHold(testTownRoot, files)}

	type result struct {
		rec HoldRecord
		err error
	}
	results := make([]result, len(holds))
	var wg sync.WaitGroup
	for i, hold := range holds {
		wg.Add(1)
		go func(i int, hold *Hold) {
			defer wg.Done()
			results[i].rec, results[i].err = hold.Acquire(notes[i])
		}(i, hold)
	}
	wg.Wait()

	owner := -1
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("Acquire %d: %v", i, res.err)
		}
		if res.rec.Owned {
			if owner >= 0 {
				t.Fatalf("both holds report Owned = true, want exactly one owner")
			}
			owner = i
		}
	}
	if owner < 0 {
		t.Fatal("no hold reports Owned = true, want exactly one owner")
	}
	loser := 1 - owner
	if results[loser].rec.Note != notes[owner] {
		t.Errorf("loser's Note = %q, want the owner's note %q", results[loser].rec.Note, notes[owner])
	}
	entry, ok := files.entry(holds[owner].Path)
	if !ok {
		t.Fatalf("hold file %s was not created", holds[owner].Path)
	}
	if string(entry.data) != notes[owner] {
		t.Errorf("hold file content = %q, want the owner's note %q", entry.data, notes[owner])
	}
}

func TestAcquireAdoptsAHoldSetBetweenStatAndCreate(t *testing.T) {
	t.Parallel()
	const foreign = "operator hold: maintenance window"
	files := newFakeFiles()
	hold := NewHold(testTownRoot, files)
	files.afterStat = func() { files.put(hold.Path, foreign, 0o644) }

	rec, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if rec.Owned {
		t.Error("Owned = true, want false: the hold appeared before the exclusive create")
	}
	if rec.Note != foreign {
		t.Errorf("Note = %q, want the content the other setter wrote %q", rec.Note, foreign)
	}
	entry, ok := files.entry(hold.Path)
	if !ok {
		t.Fatalf("hold file %s was removed", hold.Path)
	}
	if string(entry.data) != foreign {
		t.Errorf("hold file content = %q, want %q untouched", entry.data, foreign)
	}
}

func TestWithHoldReleasesOnErrorPanicAndCancel(t *testing.T) {
	t.Parallel()
	const note = "gt update 20261103T102200Z-furiosa: draining"

	t.Run("error", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		sentinel := errors.New("drain blocked")
		err := WithHold(context.Background(), hold, note, func(HoldRecord) error {
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("WithHold error = %v, want it to wrap %v", err, sentinel)
		}
		if _, ok := files.entry(hold.Path); ok {
			t.Errorf("owned hold %s was left behind after fn failed", hold.Path)
		}
	})

	t.Run("panic", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = WithHold(context.Background(), hold, note, func(HoldRecord) error {
				panic("boom")
			})
		}()
		if recovered == nil {
			t.Fatal("WithHold swallowed the panic")
		}
		if _, ok := files.entry(hold.Path); ok {
			t.Errorf("owned hold %s was left behind after fn panicked", hold.Path)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		ctx, cancel := context.WithCancel(context.Background())
		err := WithHold(ctx, hold, note, func(HoldRecord) error {
			cancel()
			return ctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("WithHold error = %v, want %v", err, context.Canceled)
		}
		if _, ok := files.entry(hold.Path); ok {
			t.Errorf("owned hold %s was left behind after ctx was canceled", hold.Path)
		}
	})
}

func TestWithHoldRunsFnUnderAForeignHold(t *testing.T) {
	t.Parallel()
	const foreign = "operator hold: maintenance window"
	files := newFakeFiles()
	hold := NewHold(testTownRoot, files)
	files.put(hold.Path, foreign, 0o644)

	var got HoldRecord
	err := WithHold(context.Background(), hold, "gt update 20261103T102200Z-furiosa: draining", func(rec HoldRecord) error {
		got = rec
		return nil
	})
	if err != nil {
		t.Fatalf("WithHold: %v", err)
	}
	if got.Owned {
		t.Error("fn got Owned = true under a foreign hold")
	}
	if got.Note != foreign {
		t.Errorf("fn got Note = %q, want %q", got.Note, foreign)
	}
	if _, ok := files.entry(hold.Path); !ok {
		t.Errorf("WithHold removed the foreign hold %s", hold.Path)
	}
}

func TestWithHoldSurfacesAReleaseFailure(t *testing.T) {
	t.Parallel()
	files := newFakeFiles()
	hold := NewHold(testTownRoot, files)
	removeErr := errors.New("remove failed")
	files.removeErr[hold.Path] = removeErr
	fnErr := errors.New("drain blocked")

	err := WithHold(context.Background(), hold, "gt update 20261103T102200Z-furiosa: draining", func(HoldRecord) error {
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Errorf("WithHold error = %v, want it to wrap fn's error %v", err, fnErr)
	}
	if !errors.Is(err, removeErr) {
		t.Errorf("WithHold error = %v, want it to wrap the release error %v", err, removeErr)
	}
}

func TestStatAndCreateErrorsFailClosed(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("permission denied")
	cases := []struct {
		name   string
		inject func(*fakeFiles, string)
	}{
		{"stat", func(f *fakeFiles, path string) { f.statErr[path] = sentinel }},
		{"create", func(f *fakeFiles, path string) { f.createErr[path] = sentinel }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := newFakeFiles()
			hold := NewHold(testTownRoot, files)
			tc.inject(files, hold.Path)

			rec, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining")
			if !errors.Is(err, sentinel) {
				t.Fatalf("Acquire error = %v, want it to wrap %v", err, sentinel)
			}
			if rec.Owned {
				t.Error("Acquire reported Owned = true after an error")
			}
		})
	}

	t.Run("WithHold does not run fn", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		files.statErr[hold.Path] = sentinel

		called := false
		err := WithHold(context.Background(), hold, "note", func(HoldRecord) error {
			called = true
			return nil
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("WithHold error = %v, want it to wrap %v", err, sentinel)
		}
		if called {
			t.Error("fn ran although Acquire could not tell whether a hold exists")
		}
	})
}

func TestAcquireReadErrorFailsClosed(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("permission denied")
	files := newFakeFiles()
	hold := NewHold(testTownRoot, files)
	files.put(hold.Path, "operator hold", 0o644)
	files.readErr[hold.Path] = sentinel

	rec, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Acquire error = %v, want it to wrap %v", err, sentinel)
	}
	if rec.Owned {
		t.Error("Acquire reported Owned = true after an error")
	}
}

func TestSetNoteWritesOnlyWhenOwned(t *testing.T) {
	t.Parallel()
	t.Run("owned", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		if _, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := hold.SetNote("gt update 20261103T102200Z-furiosa: installing"); err != nil {
			t.Fatalf("SetNote: %v", err)
		}
		entry, ok := files.entry(hold.Path)
		if !ok {
			t.Fatalf("hold file %s is missing", hold.Path)
		}
		if want := "gt update 20261103T102200Z-furiosa: installing"; string(entry.data) != want {
			t.Errorf("hold file content = %q, want %q", entry.data, want)
		}
	})

	t.Run("foreign", func(t *testing.T) {
		const foreign = "operator hold: maintenance window"
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		files.put(hold.Path, foreign, 0o644)
		if _, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := hold.SetNote("gt update 20261103T102200Z-furiosa: installing"); err != nil {
			t.Fatalf("SetNote on a foreign hold: %v", err)
		}
		entry, ok := files.entry(hold.Path)
		if !ok {
			t.Fatalf("foreign hold file %s is missing", hold.Path)
		}
		if string(entry.data) != foreign {
			t.Errorf("foreign hold content = %q, want it unchanged at %q", entry.data, foreign)
		}
	})

	t.Run("write error", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		if _, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		sentinel := errors.New("disk full")
		files.writeErr[hold.Path] = sentinel
		if err := hold.SetNote("gt update 20261103T102200Z-furiosa: installing"); !errors.Is(err, sentinel) {
			t.Errorf("SetNote error = %v, want it to wrap %v", err, sentinel)
		}
	})
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	t.Run("second release", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		if _, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := hold.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if _, ok := files.entry(hold.Path); ok {
			t.Fatalf("owned hold %s was not removed", hold.Path)
		}
		if err := hold.Release(); err != nil {
			t.Fatalf("second Release: %v", err)
		}
	})

	t.Run("file already gone", func(t *testing.T) {
		files := newFakeFiles()
		hold := NewHold(testTownRoot, files)
		if _, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := files.Remove(hold.Path); err != nil {
			t.Fatalf("remove the hold behind the Hold: %v", err)
		}
		if err := hold.Release(); err != nil {
			t.Errorf("Release after the file was removed: %v", err)
		}
	})
}

func TestReleaseKeepsOwnershipOnFailure(t *testing.T) {
	t.Parallel()
	files := newFakeFiles()
	hold := NewHold(testTownRoot, files)
	if _, err := hold.Acquire("gt update 20261103T102200Z-furiosa: draining"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sentinel := errors.New("remove failed")
	files.removeErr[hold.Path] = sentinel
	if err := hold.Release(); !errors.Is(err, sentinel) {
		t.Fatalf("Release error = %v, want it to wrap %v", err, sentinel)
	}
	if _, ok := files.entry(hold.Path); !ok {
		t.Fatalf("hold %s was removed by a failed Release", hold.Path)
	}

	files.removeErr[hold.Path] = nil
	if err := hold.Release(); err != nil {
		t.Fatalf("Release after the failure cleared: %v", err)
	}
	if _, ok := files.entry(hold.Path); ok {
		t.Errorf("owned hold %s survived the retried Release", hold.Path)
	}
}
