package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/landworker"
)

// postLandUnknownWindow is how long an unanswered post-land verdict keeps
// raising an attention item: long enough for the 24 hour townhealth reading to
// be the backstop behind it (gt-gg9hv.7.2), and bounded so an old silence does
// not stay in the queue forever.
const postLandUnknownWindow = 7 * 24 * time.Hour

// postLandUnknownFile is the durable copy of the store, under the town's
// daemon directory.
const postLandUnknownFile = "post_land_unknowns.json"

// postLandUnknownStore holds the landed commits whose post-land status context
// never reached a verdict, keyed by landworker.PostLandUnknown.Key. With a path
// it is durable: record writes the file before it returns nil, and the runner
// removes the timed-out pending entry only after that nil, so a restart finds
// either the entry (and raises the same key again) or the record (gt-gg9hv.7.1).
// A store with no path lives in memory, as the tests build it.
type postLandUnknownStore struct {
	path   string
	mu     sync.Mutex
	loaded bool
	// loadErr is set while the file exists but cannot be read or decoded: the
	// store then answers record with this error and never writes, so the file
	// is not overwritten, the runner keeps its pending entries and the problem
	// shows in the log every pass until the file is fixed or removed.
	loadErr error
	recs    map[string]landworker.PostLandUnknown
}

// load reads the file once; a missing file starts empty, and the pending
// entries still in the rig's store raise their records again. A file that
// exists but cannot be read or decoded sets loadErr and is read again on the
// next call, so a fixed file is picked up without a restart.
func (s *postLandUnknownStore) load() {
	if s.loaded && s.loadErr == nil {
		return
	}
	s.loaded = true
	s.loadErr = nil
	if s.recs == nil {
		s.recs = map[string]landworker.PostLandUnknown{}
	}
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.loadErr = fmt.Errorf("reading the post-land alarm store %s: %w", s.path, err)
		}
		return
	}
	var stored map[string]landworker.PostLandUnknown
	if err := json.Unmarshal(data, &stored); err != nil {
		s.loadErr = fmt.Errorf("decoding the post-land alarm store %s: %w", s.path, err)
		return
	}
	maps.Copy(s.recs, stored)
}

// save writes the records through a temporary file and a rename, so a crash
// mid-write leaves the previous file rather than a torn one.
func (s *postLandUnknownStore) save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.recs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// record stores u, replacing a record with the same key so a rerun at the same
// commit keeps one, and drops records older than window before u.At. It returns
// nil only when the record is stored durably.
func (s *postLandUnknownStore) record(u landworker.PostLandUnknown, window time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if s.loadErr != nil {
		return s.loadErr
	}
	cutoff := u.At.Add(-window)
	for key, r := range s.recs {
		if r.At.Before(cutoff) {
			delete(s.recs, key)
		}
	}
	s.recs[u.Key()] = u
	return s.save()
}

// snapshot returns the records at or after since, ordered by key so the queue
// is the same list every tick.
func (s *postLandUnknownStore) snapshot(since time.Time) []landworker.PostLandUnknown {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	out := make([]landworker.PostLandUnknown, 0, len(s.recs))
	for _, r := range s.recs {
		if r.At.Before(since) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// postLandStoreFailureStore holds the rigs whose pending-verdict store could
// not be read or written for three passes running, keyed by
// landworker.PostLandStoreFailure.Key. One per rig: the pass that reaches the
// store again forgets it, and the item stops raising.
type postLandStoreFailureStore struct {
	mu   sync.Mutex
	recs map[string]landworker.PostLandStoreFailure
}

// record stores f, replacing a record with the same key so one rig keeps one.
func (s *postLandStoreFailureStore) record(f landworker.PostLandStoreFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recs == nil {
		s.recs = map[string]landworker.PostLandStoreFailure{}
	}
	s.recs[f.Key()] = f
}

// forget drops the record for key: a pass reached the store again.
func (s *postLandStoreFailureStore) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, key)
}

// snapshot returns the records ordered by key, so the queue is the same list
// every tick.
func (s *postLandStoreFailureStore) snapshot() []landworker.PostLandStoreFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]landworker.PostLandStoreFailure, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// collectPostLandUnknown raises one item per landed commit whose post-land
// status context never reached a verdict before its timeout, and one per rig
// whose pending-verdict store cannot be read or written: CI stopped reporting,
// or the verdicts are not being stored, so the post-land check is silent and
// the 24 hour townhealth reading is the backstop behind it (gt-gg9hv.7.1,
// gt-gg9hv.7.2). Its low severity is the starved tier's: the check is
// unmeasured, not known bad. The key is the rig and landed commit, or the rig
// alone for a store failure, so a rerun raises no second item; a record stops
// raising after postLandUnknownWindow, and a store failure stops when the store
// is reached again.
func (s *attentionSources) collectPostLandUnknown(context.Context) ([]attention.Item, error) {
	since := s.now.Add(-postLandUnknownWindow)
	recs := s.d.postLandUnknowns.snapshot(since)
	failures := s.d.postLandStoreFailures.snapshot()
	out := make([]attention.Item, 0, len(recs)+len(failures))
	for _, u := range recs {
		out = append(out, attention.Item{
			Key:      u.Key(),
			Kind:     attention.KindPostLandUnknown,
			Severity: attention.SeverityLow,
			Rig:      u.Rig,
			SHA:      u.SHA,
			Summary:  u.Summary(),
		})
	}
	for _, f := range failures {
		out = append(out, attention.Item{
			Key:      f.Key(),
			Kind:     attention.KindPostLandUnknown,
			Severity: attention.SeverityLow,
			Rig:      f.Rig,
			Summary:  f.Summary(),
		})
	}
	return out, nil
}
