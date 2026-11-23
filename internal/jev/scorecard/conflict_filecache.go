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

// landedFilesCache is a file-backed cache of a landing's changed files by its
// base and head, so a later run reuses the list instead of running git.
type landedFilesCache struct {
	path   string
	byPair map[landingPair][]string
}

// openLandedFilesCache reads the JSON lines at path. A missing file is empty;
// a last line without its newline is a torn append and is dropped; a complete
// line that does not decode is an error naming the path and line number.
func openLandedFilesCache(path string) (*landedFilesCache, error) {
	c := &landedFilesCache{path: path, byPair: make(map[landingPair][]string)}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the landed files cache %s: %w", path, err)
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

// Get returns the pair's cached files and whether the cache holds it.
func (c *landedFilesCache) Get(base, head string) ([]string, bool) {
	files, ok := c.byPair[landingPair{base, head}]
	return files, ok
}

// Put appends the pair's files as one O_APPEND write and remembers them. A
// later line for the same pair replaces the earlier one.
func (c *landedFilesCache) Put(base, head string, files []string) error {
	// The line holds only strings and a string slice, so its JSON cannot fail
	// to encode.
	line, _ := json.Marshal(landedFilesLine{Base: base, Head: head, Files: files})
	if err := appendBytes(c.path, append(line, '\n')); err != nil {
		return fmt.Errorf("appending to the landed files cache %s: %w", c.path, err)
	}
	c.byPair[landingPair{base, head}] = files
	return nil
}
