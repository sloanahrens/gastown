package seatship

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TailOf returns the last n bytes of the newest *.jsonl transcript under
// projectsDir, starting at a line boundary, and "" when there is none. A
// projectsDir the walk cannot enter is an error, never "". The newest
// transcript is read whole and only its tail is returned.
func TailOf(projectsDir string, n int) (string, error) {
	newest := ""
	var newestTime time.Time
	err := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.ModTime().After(newestTime) {
			newest, newestTime = path, info.ModTime()
		}
		return nil
	})
	if err != nil || newest == "" {
		return "", err
	}
	b, err := os.ReadFile(newest)
	if err != nil {
		return "", err
	}
	if start := min(max(len(b)-n, 0), len(b)); start > 0 {
		if b[start-1] != '\n' { // the window starts mid-line: drop its remains
			i := bytes.IndexByte(b[start:], '\n')
			if i < 0 {
				return "", nil
			}
			start += i + 1
		}
		b = b[start:]
	}
	return string(b), nil
}
