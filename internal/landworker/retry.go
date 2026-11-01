package landworker

import (
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// ErrNoShowMultiple reports an inner client that cannot read several beads in
// one call; a caller with a per-record read to fall back on takes it as "read
// them one at a time", not as a failed read.
var ErrNoShowMultiple = errors.New("landworker: the beads client has no ShowMultiple")

// RetryBeads retries bd calls that Dolt refused with a serialization
// failure (Error 1213) under load. Such a transaction was rolled back, so the
// retry cannot apply a write twice. Every other error returns at once.
type RetryBeads struct {
	Inner RetryInner
	// Tries is the total number of attempts; 0 means 4.
	Tries int
	// Sleep waits between attempts; nil means time.Sleep.
	Sleep func(time.Duration)
}

// RetryInner is the beads client RetryBeads wraps: the worker's surface and
// the red-main owner's.
type RetryInner interface {
	Beads
	RedMainBeads
}

var (
	_ Beads        = RetryBeads{}
	_ RedMainBeads = RetryBeads{}
	// The daemon hands the worker a RetryBeads, so the worker's batch read
	// reaches the store through this method.
	_ interface {
		ShowMultiple([]string) (map[string]*beads.Issue, error)
	} = RetryBeads{}
)

// IsSerializationFailure reports whether err is Dolt's retryable
// transaction conflict.
func IsSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "error 1213") ||
		strings.Contains(msg, "serialization failure") ||
		strings.Contains(msg, "deadlock found") ||
		strings.Contains(msg, "try restarting transaction")
}

// retryDelay is a jittered delay in [100ms, 2s] growing with the attempt.
func retryDelay(attempt int) time.Duration {
	upper := 250 * time.Millisecond << attempt
	if upper > 2*time.Second {
		upper = 2 * time.Second
	}
	lower := 100 * time.Millisecond
	return lower + time.Duration(rand.Int64N(int64(upper-lower)+1)) //nolint:gosec // G404: jitter, not security
}

func (r RetryBeads) do(fn func() error) error {
	tries := r.Tries
	if tries <= 0 {
		tries = 4
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	var err error
	for attempt := 0; attempt < tries; attempt++ {
		if err = fn(); !IsSerializationFailure(err) {
			return err
		}
		if attempt < tries-1 {
			sleep(retryDelay(attempt))
		}
	}
	return err
}

func (r RetryBeads) Show(id string) (is *beads.Issue, err error) {
	err = r.do(func() error { is, err = r.Inner.Show(id); return err })
	return is, err
}

// ShowMultiple retries a batch read as Show does; an inner client without it
// reports ErrNoShowMultiple unretried.
func (r RetryBeads) ShowMultiple(ids []string) (out map[string]*beads.Issue, err error) {
	inner, ok := r.Inner.(interface {
		ShowMultiple([]string) (map[string]*beads.Issue, error)
	})
	if !ok {
		return nil, ErrNoShowMultiple
	}
	err = r.do(func() error { out, err = inner.ShowMultiple(ids); return err })
	return out, err
}

func (r RetryBeads) Update(id string, opts beads.UpdateOptions) error {
	return r.do(func() error { return r.Inner.Update(id, opts) })
}

func (r RetryBeads) ForceCloseWithReason(reason string, ids ...string) error {
	return r.do(func() error { return r.Inner.ForceCloseWithReason(reason, ids...) })
}

func (r RetryBeads) AppendNotes(id, note string) error {
	return r.do(func() error { return r.Inner.AppendNotes(id, note) })
}

func (r RetryBeads) List(opts beads.ListOptions) (out []*beads.Issue, err error) {
	err = r.do(func() error { out, err = r.Inner.List(opts); return err })
	return out, err
}

func (r RetryBeads) Comments(id string) (out []beads.Comment, err error) {
	err = r.do(func() error { out, err = r.Inner.Comments(id); return err })
	return out, err
}

func (r RetryBeads) AddComment(id, text string) error {
	return r.do(func() error { return r.Inner.AddComment(id, text) })
}

func (r RetryBeads) Create(opts beads.CreateOptions) (is *beads.Issue, err error) {
	err = r.do(func() error { is, err = r.Inner.Create(opts); return err })
	return is, err
}

func (r RetryBeads) CloseWithReason(reason string, ids ...string) error {
	return r.do(func() error { return r.Inner.CloseWithReason(reason, ids...) })
}

func (r RetryBeads) Children(parentID string) (out []*beads.Issue, err error) {
	err = r.do(func() error { out, err = r.Inner.Children(parentID); return err })
	return out, err
}
