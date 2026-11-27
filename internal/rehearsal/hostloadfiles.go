package rehearsal

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// HostLoadReader returns the host load day files a run window spans, from
// from's local date through now()'s date there, concatenated in date order.
// It opens nothing until the first Read, so the text includes the samples the
// run wrote; a file that is missing or unreadable fails that Read and every
// later one with an error naming it, and serves no bytes.
func HostLoadReader(dir string, from time.Time, now func() time.Time) io.Reader {
	return &hostLoadFiles{load: func() ([]byte, error) {
		loc := from.Location()
		day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
		end := now().In(loc)
		endDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, loc)
		var text []byte
		for d := day; ; d = d.AddDate(0, 0, 1) {
			path := filepath.Join(dir, d.Format("2006-01-02")+".log")
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("reading host load file %s: %w", path, err)
			}
			text = append(text, data...)
			if !d.Before(endDay) {
				return text, nil
			}
		}
	}}
}

// hostLoadFiles reads the window once, on the first Read, after the run wrote.
type hostLoadFiles struct {
	load func() ([]byte, error)
	r    *bytes.Reader
	err  error
}

func (r *hostLoadFiles) Read(p []byte) (int, error) {
	if r.r == nil && r.err == nil {
		var text []byte
		text, r.err = r.load()
		r.r = bytes.NewReader(text)
	}
	if r.err != nil {
		return 0, r.err
	}
	return r.r.Read(p)
}
