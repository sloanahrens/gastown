package cmd

import (
	"errors"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// birdActivityTTL is how long an answer about one bird is reused before the
// handoff bead is read again.
const birdActivityTTL = 60 * time.Second

// birdActivity answers when each bird last updated its handoff bead (gt-<bird>),
// the bead every bird keeps current. The age of a bird's daily report says
// nothing about whether it is working; the handoff bead's update time does.
type birdActivity struct {
	show func(id string) (*beads.Issue, error)
	now  func() time.Time
	logf func(string, ...any)

	mu    sync.Mutex
	cache map[string]birdActivityAnswer
}

type birdActivityAnswer struct {
	at   time.Time
	last time.Time
	ok   bool
}

func newBirdActivity(show func(id string) (*beads.Issue, error), now func() time.Time, logf func(string, ...any)) *birdActivity {
	return &birdActivity{show: show, now: now, logf: logf, cache: map[string]birdActivityAnswer{}}
}

// Last answers when the bird's handoff bead was last updated. It answers false
// when that time is not known: the bead is missing, the read failed, or the
// time does not parse. Every answer, true or false, is reused for 60 seconds.
func (a *birdActivity) Last(bird string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if got, ok := a.cache[bird]; ok && now.Sub(got.at) < birdActivityTTL {
		return got.last, got.ok
	}
	last, ok := a.read(bird)
	a.cache[bird] = birdActivityAnswer{at: now, last: last, ok: ok}
	return last, ok
}

// read asks the store once. A missing bead is a normal answer and is not
// logged; any other failure is logged, so a dead store is visible rather than
// read as a quiet bird.
func (a *birdActivity) read(bird string) (time.Time, bool) {
	id := "gt-" + bird
	issue, err := a.show(id)
	if errors.Is(err, beads.ErrNotFound) {
		return time.Time{}, false
	}
	if err != nil {
		a.logf("dashboard: handoff bead %s read failed: %v", id, err)
		return time.Time{}, false
	}
	if issue == nil {
		a.logf("dashboard: handoff bead %s read returned no bead", id)
		return time.Time{}, false
	}
	last := beads.ParseIssueTime(issue.UpdatedAt)
	if last.IsZero() {
		a.logf("dashboard: handoff bead %s updated_at %q does not parse", id, issue.UpdatedAt)
		return time.Time{}, false
	}
	return last, true
}
