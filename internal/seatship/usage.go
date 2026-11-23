package seatship

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// UsageFrom builds a seat's usage record from every *.jsonl under projectsDir,
// recursively, and returns it with the session ids it counted. A partial transcript
// marks the record incomplete, as usageattrib.Scan does, because the seat still
// posts what it read; only an unwalkable directory is an error. An empty directory
// stays incomplete, and zero f.Start and f.End take the transcript write times.
func UsageFrom(projectsDir string, f usageattrib.Facts) (dashboard.UsageRecord, []string, error) {
	var sessions []usageattrib.SessionUsage
	var ids []string
	var first, last time.Time
	err := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		s, mtime, rerr := usageattrib.ReadSession(path)
		// As usageattrib.Scan does, an error leaves the session incomplete, never whole.
		s.Complete = s.Complete && rerr == nil
		sessions = append(sessions, s)
		if !slices.Contains(ids, s.ID) {
			ids = append(ids, s.ID)
		}
		if !mtime.IsZero() {
			if first.IsZero() || mtime.Before(first) {
				first = mtime
			}
			if last.IsZero() || mtime.After(last) {
				last = mtime
			}
		}
		return nil
	})
	if err != nil {
		return dashboard.UsageRecord{}, nil, fmt.Errorf("seatship: reading %s: %w", projectsDir, err)
	}
	if f.Start.IsZero() {
		f.Start = first
	}
	if f.End.IsZero() {
		f.End = last
	}
	return usageattrib.Build(f, sessions), ids, nil
}
