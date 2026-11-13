package usageattrib

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// dedupeAcross counts a message id once across the sessions given, in the order given. A resumed
// session repeats the messages of the session it resumed, so a repeat merges into its first
// sighting with each token field taking the maximum, as ReadAssistantUsage does within one file.
// Messages with no id are never merged. The sessions are changed in place.
func dedupeAcross(sessions []SessionUsage) {
	first := map[string]*Message{}
	for i := range sessions {
		sessions[i].Main = dedupeList(sessions[i].Main, first)
		sessions[i].Subagent = dedupeList(sessions[i].Subagent, first)
	}
}

// dedupeList keeps the messages of msgs that are first sightings. The output is allocated at the
// input's capacity, so the pointers kept in first stay valid as it grows.
func dedupeList(msgs []Message, first map[string]*Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.ID == nil {
			out = append(out, m)
			continue
		}
		if p, seen := first[*m.ID]; seen {
			p.Input = max(p.Input, m.Input)
			p.CacheCreation = max(p.CacheCreation, m.CacheCreation)
			p.CacheRead = max(p.CacheRead, m.CacheRead)
			p.Output = max(p.Output, m.Output)
			continue
		}
		out = append(out, m)
		first[*m.ID] = &out[len(out)-1]
	}
	return out
}
