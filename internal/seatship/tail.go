package seatship

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TailOf returns the last n bytes of the newest *.jsonl transcript under
// projectsDir, recursively, starting at a line boundary: a cut that lands
// mid-line drops the partial line, so a window with no complete line (a final
// record longer than n) yields "". It returns "" when there is no transcript,
// and an error when projectsDir cannot be walked, a *.jsonl entry is not a
// regular file, or the chosen transcript cannot be read.
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
		// Read whole so the open and read share the one error return a test can
		// drive; a seek-and-bound read adds failure branches no fixture reaches.
		b, err = os.ReadFile(newest)
	}
	if err != nil {
		return "", err
	}
	start := min(max(len(b)-n, 0), len(b))
	window := b[start:]
	if start > 0 && b[start-1] != '\n' {
		i := strings.IndexByte(string(window), '\n')
		if i < 0 {
			return "", nil // one partial record: no line boundary in the window
		}
		window = window[i+1:]
	}
	return string(window), nil
}
