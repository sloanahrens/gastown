package beadversions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// FileStore is a Store persisted as one append-only JSONL file, one Version per
// line, so a bead's history survives a restart and a Dolt flatten; it assumes a
// single writer per file and takes no lock across processes.
type FileStore struct {
	path  string
	files fileOps

	mu       sync.Mutex
	versions map[string][]Version
	skipped  int
	loaded   bool
}

// NewFileStore returns a store backed by path, opening nothing until the first
// call that needs the file.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, files: hostFileOps()}
}

// Load rereads the file and returns the number of lines it skipped, so a caller
// can report a damaged record; a skipped line is left out rather than failing
// the load. A file that does not exist yet loads as empty.
func (s *FileStore) Load() (skipped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Put implements Store.
func (s *FileStore) Put(bead, text, actor string, at time.Time) (bool, error) {
	if bead == "" {
		return false, ErrEmptyBead
	}
	if at.IsZero() {
		return false, ErrZeroTime
	}
	hash := hashText(text)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return false, err
	}
	existing := s.versions[bead]
	if len(existing) > 0 {
		latest := existing[len(existing)-1]
		if !at.After(latest.At) {
			return false, ErrOutOfOrder
		}
		if latest.Hash == hash {
			return false, nil
		}
	}
	v := Version{Bead: bead, Text: text, Hash: hash, Actor: actor, At: at}
	if err := s.appendVersion(v); err != nil {
		return false, err
	}
	s.versions[bead] = append(existing, v)
	return true, nil
}

// Versions implements Store.
func (s *FileStore) Versions(bead string) ([]Version, error) {
	if bead == "" {
		return nil, ErrEmptyBead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return nil, err
	}
	return append([]Version(nil), s.versions[bead]...), nil
}

// AsOf implements Store.
func (s *FileStore) AsOf(bead string, t time.Time) (Version, error) {
	if bead == "" {
		return Version{}, ErrEmptyBead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return Version{}, err
	}
	versions := s.versions[bead]
	i := sort.Search(len(versions), func(i int) bool {
		return versions[i].At.After(t)
	})
	if i == 0 {
		return Version{}, ErrNotFound
	}
	return versions[i-1], nil
}

// ensureLoaded reads the file once; a failed read is not cached, so the next
// call retries rather than serving an empty history.
func (s *FileStore) ensureLoaded() error {
	if s.loaded {
		return nil
	}
	_, err := s.loadLocked()
	return err
}

// loadLocked replaces the in-memory state with the file's contents. The new
// state is built aside and assigned only on success, so a failed read leaves
// the previous state untouched.
func (s *FileStore) loadLocked() (int, error) {
	versions, skipped, err := s.readFile()
	if err != nil {
		return 0, err
	}
	s.versions = versions
	s.skipped = skipped
	s.loaded = true
	return skipped, nil
}

// readFile reads every line of the file. A missing file is an empty history
// rather than an error, so the first Put creates it.
func (s *FileStore) readFile() (map[string][]Version, int, error) {
	f, err := s.files.open(s.path, os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return make(map[string][]Version), 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("beadversions: opening %s: %w", s.path, err)
	}
	data, rerr := io.ReadAll(f)
	cerr := f.Close()
	if rerr != nil {
		return nil, 0, fmt.Errorf("beadversions: reading %s: %w", s.path, rerr)
	}
	if cerr != nil {
		return nil, 0, fmt.Errorf("beadversions: closing %s: %w", s.path, cerr)
	}
	versions, skipped := parseVersions(data)
	return versions, skipped, nil
}

// parseVersions reads one Version per line, each bead's history oldest first.
// A line this package did not write is counted, not loaded: a torn line from a
// crash mid-append, an unparsable one, one with no bead or zero time, one whose
// hash is not its text's, or one whose time does not advance its bead's latest.
// Keeping such a line out stops a damaged record from answering AsOf with text
// it never recorded.
func parseVersions(data []byte) (map[string][]Version, int) {
	versions := make(map[string][]Version)
	latest := make(map[string]time.Time)
	skipped := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var v Version
		if err := json.Unmarshal(line, &v); err != nil {
			skipped++
			continue
		}
		if v.Bead == "" || v.At.IsZero() || v.Hash != hashText(v.Text) {
			skipped++
			continue
		}
		if last, ok := latest[v.Bead]; ok && !v.At.After(last) {
			skipped++
			continue
		}
		latest[v.Bead] = v.At
		versions[v.Bead] = append(versions[v.Bead], v)
	}
	return versions, skipped
}

// appendVersion writes v as one line in a single O_APPEND write, creating the
// file 0600 and its directory 0700 when they are missing. The caller adds v to
// memory only after this returns nil.
func (s *FileStore) appendVersion(v Version) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("beadversions: encoding a version: %w", err)
	}
	line = append(line, '\n')
	dir := filepath.Dir(s.path)
	if err := s.files.mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("beadversions: creating %s: %w", dir, err)
	}
	f, err := s.files.open(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("beadversions: opening %s: %w", s.path, err)
	}
	n, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("beadversions: appending to %s: %w", s.path, werr)
	}
	if n != len(line) {
		return fmt.Errorf("beadversions: appending to %s: %w", s.path, io.ErrShortWrite)
	}
	if cerr != nil {
		return fmt.Errorf("beadversions: closing %s: %w", s.path, cerr)
	}
	return nil
}

// versionFile is what the store needs of an open file: read for a load, write
// and close for an append.
type versionFile interface {
	io.Reader
	io.Writer
	Close() error
}

// fileOps is the store's way to the disk. The production values are
// os.OpenFile and os.MkdirAll; the struct lets a test drive the failure
// branches a healthy host cannot produce.
type fileOps struct {
	open  func(name string, flag int, perm os.FileMode) (versionFile, error)
	mkdir func(name string, perm os.FileMode) error
}

// hostFileOps is the process's own filesystem.
func hostFileOps() fileOps {
	return fileOps{open: openVersionFile, mkdir: os.MkdirAll}
}

func openVersionFile(name string, flag int, perm os.FileMode) (versionFile, error) {
	return os.OpenFile(name, flag, perm)
}
