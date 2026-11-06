package daemon

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/landworker"
)

// postLandRunnerSet holds the post-land runner of each rig, so the attention
// collector can ask each for the alarms its durable pending entries imply. The
// collector reads the rigs' own state; there is no store of alarms apart from it
// (gt-gg9hv.7.1).
type postLandRunnerSet struct {
	mu      sync.Mutex
	runners map[string]*landworker.PostLandRunner
	// paths is each rig's checkout, where its own settings file lives.
	paths map[string]string
}

// setSettingsPath records where the rig's settings file is, so the collector can
// say when it cannot be read.
func (s *postLandRunnerSet) setSettingsPath(rig, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paths == nil {
		s.paths = map[string]string{}
	}
	s.paths[rig] = path
}

func (s *postLandRunnerSet) settingsPath(rig string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paths[rig]
}

// set registers the rig's runner, replacing an earlier one of the same name.
func (s *postLandRunnerSet) set(rig string, r *landworker.PostLandRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runners == nil {
		s.runners = map[string]*landworker.PostLandRunner{}
	}
	s.runners[rig] = r
}

// sorted returns the rigs' names and runners in name order, so the queue is the
// same list every tick.
func (s *postLandRunnerSet) sorted() ([]string, []*landworker.PostLandRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.runners))
	for name := range s.runners {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*landworker.PostLandRunner, len(names))
	for i, name := range names {
		out[i] = s.runners[name]
	}
	return names, out
}

// collectPostLandUnknown derives the post-land alarms from each rig's durable
// pending entries: one item per landed commit whose status context never reached
// a verdict before the configured timeout (CI stopped reporting, so the check is
// silent), one per rig whose pending store cannot be read or written, and one per
// rig whose bound dropped entries. Nothing is stored apart from the entries
// themselves, so an item cannot be lost between a callback and a save, and a
// restarted daemon derives the same items from the same entries. An unreadable
// store is an item, never zero alarms. Low severity is the starved tier's: the
// check is unmeasured, not known bad. The key is the rig and landed commit, or the
// rig alone for a store problem, so a rerun raises no second item (gt-gg9hv.7.1,
// gt-gg9hv.7.2; the 24 hour townhealth reading is the backstop).
func (s *attentionSources) collectPostLandUnknown(context.Context) ([]attention.Item, error) {
	rigs, runners := s.d.postLandRunners.sorted()
	var out []attention.Item
	for i, runner := range runners {
		rig := rigs[i]
		// A settings file that cannot be read turns status mode off: a landing
		// then records no pending verdict and none is ever reached. That is an
		// item at once, not only a log line (gt-gg9hv.7.1).
		if path := s.d.postLandRunners.settingsPath(rig); path != "" {
			if _, serr := rigPostLandStatusContextErr(path); serr != nil {
				out = append(out, attention.Item{
					Key: "post-land-settings-unreadable:" + rig, Kind: attention.KindPostLandUnknown, Severity: attention.SeverityLow,
					Rig: rig,
					Summary: fmt.Sprintf("the settings file of %s cannot be read, so post-land status mode is off and landings record no verdict until it can be: %s",
						rig, landworker.SanitizeStatusText(serr.Error())),
				})
			}
		}
		alarms, err := runner.Alarms(s.now)
		if err != nil {
			// An unreadable store is an item at once, never zero alarms; it is
			// the same condition the runner's own failure count reports, so one
			// item (keyed by rig) stands for both.
			f := landworker.PostLandStoreFailure{Rig: rig, Error: err.Error()}
			out = append(out, attention.Item{
				Key: f.Key(), Kind: attention.KindPostLandUnknown, Severity: attention.SeverityLow,
				Rig: rig, Summary: f.Summary(),
			})
			continue
		}
		// A store that reads but cannot be written fails the runner's passes: after
		// PostLandStatusStoreFailureAfter of them the safety net is blind.
		if f := runner.StoreFailure(); f != nil {
			out = append(out, attention.Item{
				Key: f.Key(), Kind: attention.KindPostLandUnknown, Severity: attention.SeverityLow,
				Rig: f.Rig, Summary: f.Summary(),
			})
		}
		for _, u := range alarms.Unknown {
			out = append(out, attention.Item{
				Key: u.Key(), Kind: attention.KindPostLandUnknown, Severity: attention.SeverityLow,
				Rig: u.Rig, SHA: u.SHA, Summary: u.Summary(),
			})
		}
		if d := alarms.Dropped; d != nil {
			out = append(out, attention.Item{
				Key: "post-land-dropped:" + rig, Kind: attention.KindPostLandUnknown, Severity: attention.SeverityLow,
				Rig: rig, SHA: d.Oldest,
				Summary: fmt.Sprintf("%d pending post-land verdicts for %s were dropped at the bound (oldest %s): their checks never reached a verdict", d.Count, rig, short12(d.Oldest)),
			})
		}
	}
	return out, nil
}

// short12 is the first twelve characters of a commit.
func short12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
