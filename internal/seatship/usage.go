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

// UsageFrom builds a seat's usage record from every session transcript (*.jsonl) under
// projectsDir, recursively, and returns it with the session ids it counted. Each transcript is read
// by usageattrib.ReadSession, which folds in that session's subagents/agent-*.jsonl files, so the
// walk skips a session's subagents directory rather than counting those messages twice. Repeated
// messages across sessions are deduplicated as usageattrib.Scan does, so a resumed session's copies
// count once. A partial transcript leaves the record incomplete, as Scan does, because the seat
// still posts what it read; only an unwalkable directory is an error. An empty directory stays
// incomplete.
//
// A zero f.Start or f.End takes the transcripts' earliest and latest last-write time. Scan bounds
// the same fields with its run's start and end events; a seat has no runs of its own, so the
// transcript write times are the nearest bound it can compute for itself.
func UsageFrom(projectsDir string, f usageattrib.Facts) (dashboard.UsageRecord, []string, error) {
	var sessions []usageattrib.SessionUsage
	var paths []string
	var ids []string
	var first, last time.Time
	err := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// A session's subagent transcripts live in <session>/subagents; ReadSession reads them
			// with their session, so visiting them again would count their messages twice.
			if path != projectsDir && d.Name() == "subagents" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		// ReadSession keeps the messages it read and marks the session incomplete on any read
		// error, so an unreadable or partly malformed transcript is recorded as incomplete rather
		// than losing the tokens; there is no further action for that error here.
		s, mtime, _ := usageattrib.ReadSession(path)
		sessions = append(sessions, s)
		paths = append(paths, path)
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
	usageattrib.DedupeSessions(sessions, paths)
	if f.Start.IsZero() {
		f.Start = first
	}
	if f.End.IsZero() {
		f.End = last
	}
	return usageattrib.Build(f, sessions), ids, nil
}
