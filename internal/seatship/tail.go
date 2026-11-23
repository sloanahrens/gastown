package seatship

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TailOf returns the last n bytes of the newest *.jsonl transcript under
// projectsDir, recursively, from a line boundary when one fits in the window,
// or "" when there is none. A non-regular entry (a symlink, a FIFO) is refused,
// never followed; an unwalkable projectsDir is an error. The newest is read whole.
func TailOf(projectsDir string, n int) (string, error) {
	var newest string
	var newestInfo os.FileInfo
	err := filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info() // lstat: a symlink is reported as one, not followed
		if err != nil {
			return nil // vanished between the listing and this lstat
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("seatship: transcript %s is not a regular file", path)
		}
		if newestInfo == nil || info.ModTime().After(newestInfo.ModTime()) {
			newest, newestInfo = path, info
		}
		return nil
	})
	var b []byte
	if err == nil && newest != "" {
		b, err = os.ReadFile(newest)
	}
	if err != nil {
		return "", err
	}
	start := min(max(len(b)-n, 0), len(b))
	window := b[start:]
	if start > 0 && b[start-1] != '\n' {
		// With no complete line in the window (a final record over n), keep it.
		if i := strings.IndexByte(string(window), '\n'); i >= 0 && i+1 < len(window) {
			window = window[i+1:]
		}
	}
	return string(window), nil
}
