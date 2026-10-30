// Package shelltiertest holds the fakes the shell tier's tests share, so the
// package tests and the command tests read the same state files and commits.
package shelltiertest

import "io/fs"

// HeadCommit is the commit a fake checkout's HEAD names.
const HeadCommit = "2222222222222222222222222222222222222222"

// MemFiles is tiertrigger.Files over a map, so no test touches the disk.
type MemFiles map[string][]byte

// ReadFile returns the file at path, or fs.ErrNotExist when it is absent.
func (m MemFiles) ReadFile(path string) ([]byte, error) {
	data, ok := m[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

// WriteFile replaces the file at path with data.
func (m MemFiles) WriteFile(path string, data []byte) error {
	m[path] = data
	return nil
}

// RemoveFile deletes the file at path.
func (m MemFiles) RemoveFile(path string) error {
	delete(m, path)
	return nil
}
