package scorecard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// landedFilesLine is one cache entry: a landing's base and head and the files
// it changed between them.
type landedFilesLine struct {
	Base  string   `json:"base"`
	Head  string   `json:"head"`
	Files []string `json:"files"`
}

// landingPair keys a landing by the two full commit ids its files are read
// between.
type landingPair struct{ base, head string }

// landedFilesFS is the file work the cache does under its lock. A test swaps a
// member to drive an error that no file mode can cause for the root user.
type landedFilesFS struct {
	lock     func(path string) (func() error, error)
	truncate func(path string, size int64) error
}

// osLandedFilesFS is the real file work; nothing changes it.
var osLandedFilesFS = landedFilesFS{lock: flockFile, truncate: os.Truncate}

// landedFilesCache is a file-backed cache of a landing's changed files by its
// base and head, so a later run reuses the list instead of running git.
type landedFilesCache struct {
	path   string
	fs     landedFilesFS
	byPair map[landingPair][]string
}

// openLandedFilesCache reads the JSON lines at path. A missing file is empty;
// a last line without its newline is a torn append, cut from the file so the
// next Put appends a line of its own rather than completing the fragment; a
// complete line that does not decode is an error naming the path and line
// number.
func openLandedFilesCache(path string) (*landedFilesCache, error) {
	return openLandedFilesCacheOn(path, osLandedFilesFS)
}

func openLandedFilesCacheOn(path string, fsys landedFilesFS) (*landedFilesCache, error) {
	c := &landedFilesCache{path: path, fs: fsys, byPair: make(map[landingPair][]string)}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the landed files cache %s: %w", path, err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if data, err = c.cutTornTail(); err != nil {
			return nil, err
		}
	}
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines[:len(lines)-1] {
		var e landedFilesLine
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("decoding line %d of %s: %w", i+1, path, err)
		}
		c.byPair[landingPair{e.Base, e.Head}] = e.Files
	}
	return c, nil
}

// cutTornTail drops a last line that lacks its newline and returns the file's
// lines up to it. It holds the lock Put appends under and reads the file again
// inside it, so a line another process appended since the caller's read is kept
// and a tail that was completed meanwhile is not cut.
func (c *landedFilesCache) cutTornTail() ([]byte, error) {
	unlock, err := c.fs.lock(c.path)
	if err != nil {
		return nil, fmt.Errorf("locking the landed files cache %s: %w", c.path, err)
	}
	defer func() { _ = unlock() }()
	data, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("reading the landed files cache %s: %w", c.path, err)
	}
	keep := bytes.LastIndexByte(data, '\n') + 1
	if keep < len(data) {
		if err := c.fs.truncate(c.path, int64(keep)); err != nil {
			return nil, fmt.Errorf("dropping the torn last line of %s: %w", c.path, err)
		}
	}
	return data[:keep], nil
}

// Get returns a copy of the pair's cached files and whether the cache holds
// it, so a caller cannot change what the cache remembers.
func (c *landedFilesCache) Get(base, head string) ([]string, bool) {
	files, ok := c.byPair[landingPair{base, head}]
	if ok {
		files = append([]string(nil), files...)
	}
	return files, ok
}

// Put appends the pair's files as one O_APPEND write, under the lock the torn
// tail cut holds, and remembers them. A later line for the same pair replaces
// the earlier one.
func (c *landedFilesCache) Put(base, head string, files []string) error {
	owned := append([]string(nil), files...)
	// The line holds only strings and a string slice, so its JSON cannot fail
	// to encode.
	line, _ := json.Marshal(landedFilesLine{Base: base, Head: head, Files: owned})
	unlock, err := c.fs.lock(c.path)
	if err != nil {
		return fmt.Errorf("locking the landed files cache %s: %w", c.path, err)
	}
	defer func() { _ = unlock() }()
	if err := appendBytes(c.path, append(line, '\n')); err != nil {
		return fmt.Errorf("appending to the landed files cache %s: %w", c.path, err)
	}
	c.byPair[landingPair{base, head}] = owned
	return nil
}
