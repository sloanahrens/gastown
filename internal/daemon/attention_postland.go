package daemon

import (
	"context"
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

// postLandUnknownStore holds the landed commits whose post-land status context
// never reached a verdict, keyed by landworker.PostLandUnknown.Key. It is in
// memory: a daemon restart reruns the wait at the untested tip (gt-gb4ij), so
// the same key comes back and the attention state keeps its first_seen.
type postLandUnknownStore struct {
	mu   sync.Mutex
	recs map[string]landworker.PostLandUnknown
}

// record stores u, replacing a record with the same key so a rerun at the same
// commit keeps one, and drops records older than window before u.At.
func (s *postLandUnknownStore) record(u landworker.PostLandUnknown, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recs == nil {
		s.recs = map[string]landworker.PostLandUnknown{}
	}
	cutoff := u.At.Add(-window)
	for key, r := range s.recs {
		if r.At.Before(cutoff) {
			delete(s.recs, key)
		}
	}
	s.recs[u.Key()] = u
}

// snapshot returns the records at or after since, ordered by key so the queue
// is the same list every tick.
func (s *postLandUnknownStore) snapshot(since time.Time) []landworker.PostLandUnknown {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// collectPostLandUnknown raises one item per landed commit whose post-land
// status context never reached a verdict before its timeout: CI stopped
// reporting, so the post-land check is silent and the 24 hour townhealth
// reading is the backstop behind it (gt-gg9hv.7.1, gt-gg9hv.7.2). Its low
// severity is the starved tier's: the check is unmeasured, not known bad. The
// key is the rig and landed commit, so a rerun at the same commit raises no
// second item, and a record stops raising after postLandUnknownWindow.
func (s *attentionSources) collectPostLandUnknown(context.Context) ([]attention.Item, error) {
	since := s.now.Add(-postLandUnknownWindow)
	recs := s.d.postLandUnknowns.snapshot(since)
	out := make([]attention.Item, 0, len(recs))
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
	return out, nil
}
