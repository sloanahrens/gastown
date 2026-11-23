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
// projectsDir the walk cannot enter is an error, never "".
func TailOf(projectsDir string, n int) (string, error) {
	newest, newestTime := "", time.Time{}
	err := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		info, err := d.Info()
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
	f, err := os.Open(newest)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := min(max(info.Size()-int64(n), 0), info.Size())
	cut := start > 0
	if cut { // read one byte back, so a boundary right at start is seen
		start--
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return "", err
	}
	if cut { // drop the partial first line
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return "", nil
		}
		buf = buf[i+1:]
	}
	return string(buf), nil
}
