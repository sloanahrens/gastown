package usageattrib

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ReadSession reads one session's transcript and the subagent files it started, and returns the
// session with the transcript's last-write time, which decides which run the session belongs to.
// A file that cannot be read, or has a line that does not parse, makes the session incomplete and
// is returned as err; the messages that were read are kept.
func ReadSession(path string) (SessionUsage, time.Time, error) {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if base == "" {
		return SessionUsage{}, time.Time{}, fmt.Errorf("transcript %s has no session id", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return SessionUsage{ID: base}, time.Time{}, fmt.Errorf("statting its transcript: %w", err)
	}
	s := SessionUsage{ID: base, Complete: true}
	var errs []error
	own, err := readMessages(path)
	s.Main = own
	if err != nil {
		s.Complete = false
		errs = append(errs, err)
	}
	subDir := filepath.Join(filepath.Dir(path), base, "subagents")
	entries, err := os.ReadDir(subDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.Complete = false
		errs = append(errs, fmt.Errorf("listing its subagents: %w", err))
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "agent-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		sub, err := readMessages(filepath.Join(subDir, e.Name()))
		s.Subagent = append(s.Subagent, sub...)
		if err != nil {
			s.Complete = false
			errs = append(errs, err)
		}
	}
	return s, info.ModTime(), errors.Join(errs...)
}

// readMessages reads the assistant messages of one transcript file. A file that cannot be read,
// or has a line that does not parse, is an error; the messages read so far are returned.
func readMessages(path string) ([]Message, error) {
	msgs, bad, err := IterAssistantUsage(path)
	if err != nil {
		return msgs, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	if bad > 0 {
		return msgs, fmt.Errorf("%s has %d lines that do not parse", filepath.Base(path), bad)
	}
	return msgs, nil
}

// copyOrder places one session's copy of a repeated message id against the other sessions' copies.
// The copy that sorts first owns the message, whatever order the sessions were read in: first the
// copy's own timestamp as an instant, then the session's start (its earliest parseable message
// timestamp), then the session id, then the transcript path, which is unique on disk. A copy whose
// timestamp does not parse sorts after every copy whose timestamp does; among copies with equal
// timestamps, a session with no parseable start sorts after one with a start.
type copyOrder struct {
	at, start     time.Time
	atOK, startOK bool
	session, path string
}

// before reports whether copy a owns the message ahead of copy b.
func (a copyOrder) before(b copyOrder) bool {
	if a.atOK != b.atOK {
		return a.atOK
	}
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	if a.startOK != b.startOK {
		return a.startOK
	}
	if !a.start.Equal(b.start) {
		return a.start.Before(b.start)
	}
	if a.session != b.session {
		return a.session < b.session
	}
	return a.path < b.path
}

// sessionStart is the earliest timestamp of the session's messages that parses, and whether one does.
func sessionStart(s SessionUsage) (time.Time, bool) {
	var start time.Time
	found := false
	for _, msgs := range [][]Message{s.Main, s.Subagent} {
		for _, m := range msgs {
			t, err := ParseTS(m.TS)
			if err == nil && (!found || t.Before(start)) {
				start, found = t, true
			}
		}
	}
	return start, found
}

// dedupeAcross decides, over all the sessions before any total is summed, which session counts each
// repeated message id, so the answer does not depend on the order the sessions were read in. A
// resumed session repeats the messages of the session it resumed. The owner of an id is the session
// whose copy sorts first (copyOrder), and the copy that counts is the owner's first sighting of the
// id, with each token field taking the maximum over every copy, as ReadAssistantUsage does within one
// file. The other sessions drop their copies. Messages with no id are never merged. paths[i] is the
// transcript sessions[i] was read from. The sessions are changed in place.
func dedupeAcross(sessions []SessionUsage, paths []string) {
	type claim struct {
		order copyOrder
		index int
	}
	owners := map[string]claim{}
	widest := map[string]Message{}
	for i, s := range sessions {
		start, startOK := sessionStart(s)
		seen := map[string]bool{}
		for _, m := range slices.Concat(s.Main, s.Subagent) {
			if m.ID == nil {
				continue
			}
			if w, ok := widest[*m.ID]; ok {
				w.Input = max(w.Input, m.Input)
				w.CacheCreation = max(w.CacheCreation, m.CacheCreation)
				w.CacheRead = max(w.CacheRead, m.CacheRead)
				w.Output = max(w.Output, m.Output)
				widest[*m.ID] = w
			} else {
				widest[*m.ID] = m
			}
			if seen[*m.ID] {
				continue
			}
			seen[*m.ID] = true
			at, err := ParseTS(m.TS)
			o := copyOrder{at: at, atOK: err == nil, start: start, startOK: startOK, session: s.ID, path: paths[i]}
			if c, ok := owners[*m.ID]; !ok || o.before(c.order) {
				owners[*m.ID] = claim{order: o, index: i}
			}
		}
	}
	for i := range sessions {
		counted := map[string]bool{}
		keep := func(msgs []Message) []Message {
			out := make([]Message, 0, len(msgs))
			for _, m := range msgs {
				if m.ID == nil {
					out = append(out, m)
					continue
				}
				if owners[*m.ID].index != i || counted[*m.ID] {
					continue
				}
				counted[*m.ID] = true
				w := widest[*m.ID]
				m.Input, m.CacheCreation, m.CacheRead, m.Output = w.Input, w.CacheCreation, w.CacheRead, w.Output
				out = append(out, m)
			}
			return out
		}
		sessions[i].Main = keep(sessions[i].Main)
		sessions[i].Subagent = keep(sessions[i].Subagent)
	}
}
