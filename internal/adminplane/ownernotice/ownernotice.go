// Package ownernotice composes the platform's notices to a workspace's owner:
// what happened, what is still safe, and the one thing the owner can do
// (gt-xu56t).
//
// Compose is pure: it reads no clock, no environment and no store, so the UTC
// day a DedupeKey names is the day of the Event's time. Nothing here sends a
// notice or chooses a channel; delivery is a later bead, and the caller asks a
// Deduper whether the notice's workspace, cause and day were already claimed
// before it sends it.
//
// No notice carries a key, a key revision, a provider's error text or any other
// text from outside this package. An Event holds an id, a cause, a time and
// numbers, and a notice is fixed prose plus those.
package ownernotice

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"unicode"
)

// dayFormat is the date form in a DedupeKey: one UTC calendar day.
const dayFormat = "2006-01-02"

// Cause is what happened to a workspace, which decides the notice's text.
type Cause string

// The causes Compose writes a notice for.
const (
	// CauseKeyRefused is a model provider refusing the workspace's key, which
	// pauses the workspace (plan row K3).
	CauseKeyRefused Cause = "key-refused"
	// CauseSpendCap is the workspace reaching its daily spend cap (plan row K4).
	CauseSpendCap Cause = "spend-cap"
	// CauseRotationFailed is a replacement key failing its check, which keeps
	// the previous key in use (plan row K5).
	CauseRotationFailed Cause = "rotation-failed"
	// CauseSuspended is the operator suspending the workspace (plan row V4).
	CauseSuspended Cause = "suspended"
	// CauseClosed is the workspace closing, after which its export window runs.
	CauseClosed Cause = "closed"
)

var (
	// ErrUnknownCause is Compose's refusal of a cause this package does not
	// write notices for.
	ErrUnknownCause = errors.New("ownernotice: unknown cause")
	// ErrNoWorkspace is Compose's refusal of an event with no workspace id,
	// which would leave a notice naming nothing.
	ErrNoWorkspace = errors.New("ownernotice: an event needs a workspace id")
	// ErrBadWorkspace is Compose's refusal of a workspace id holding whitespace,
	// a control character or a colon: the id is printed in the notice and is
	// part of the colon-joined DedupeKey.
	ErrBadWorkspace = errors.New("ownernotice: the workspace id holds whitespace, a control character or a colon")
	// ErrNoTime is Compose's refusal of an event with no time, whose DedupeKey
	// would name the zero day.
	ErrNoTime = errors.New("ownernotice: an event needs a time")
	// ErrExportWindow is Compose's refusal of a closed event with no export
	// window: the notice would ask the owner to export what it just said was
	// not kept.
	ErrExportWindow = errors.New("ownernotice: a closed event needs an export window of at least one day")
	// ErrSpend is Compose's refusal of a spend cap event whose spend is
	// negative or not a number, or whose cap is not positive or not a number.
	ErrSpend = errors.New("ownernotice: a spend cap event needs a finite non-negative spend and a finite positive cap")
)

// Event is what happened to one workspace, in the form Compose reads. It
// carries no free text, so nothing a provider or a caller wrote can reach a
// notice.
type Event struct {
	// Workspace is the workspace's id. Every notice names it.
	Workspace string
	// Cause is what happened.
	Cause Cause
	// At is when it happened. Its UTC date is the DedupeKey's day.
	At time.Time
	// Spent and Cap are CauseSpendCap's day's spend and daily cap in US
	// dollars. Compose ignores them for the other causes.
	Spent float64
	Cap   float64
	// Days is CauseClosed's remaining export window in days. Compose ignores it
	// for the other causes.
	Days int
}

// Notice is one owner-facing message, ready for a delivery channel to send.
type Notice struct {
	// Subject is the message's subject line.
	Subject string
	// Body is the message's body.
	Body string
	// DedupeKey is "<workspace>:<cause>:<UTC date>", naming what the notice is
	// about so a caller stores or logs it by one stable identity.
	DedupeKey string
}

// Compose writes the notice for ev. An unknown cause, an event with no usable
// workspace id or time, and a cause missing the number its text needs are
// errors; no other event is.
func Compose(ev Event) (Notice, error) {
	if ev.Workspace == "" {
		return Notice{}, ErrNoWorkspace
	}
	if badWorkspaceID(ev.Workspace) {
		return Notice{}, ErrBadWorkspace
	}
	if ev.At.IsZero() {
		return Notice{}, ErrNoTime
	}

	var subject, body string
	switch ev.Cause {
	case CauseKeyRefused:
		subject = fmt.Sprintf("Your workspace %s is paused: its key was refused", ev.Workspace)
		body = fmt.Sprintf("The model provider refused the key for %s, so the workspace is paused. Work and history are kept while the workspace is paused. Replace the key to resume the workspace.", ev.Workspace)
	case CauseSpendCap:
		if !validSpend(ev.Spent, ev.Cap) {
			return Notice{}, ErrSpend
		}
		subject = fmt.Sprintf("Your workspace %s has reached its daily spend cap", ev.Workspace)
		body = fmt.Sprintf("%s spent $%.2f of its $%.2f daily spend cap on %s (UTC). Work and history are kept. Wait for the next UTC day before starting new work.", ev.Workspace, ev.Spent, ev.Cap, ev.At.UTC().Format(dayFormat))
	case CauseRotationFailed:
		subject = fmt.Sprintf("Your workspace %s is still using its previous key", ev.Workspace)
		body = fmt.Sprintf("The replacement key for %s did not pass its check, so the workspace keeps using its previous key. Replace the key with one the model provider accepts; the previous key stays in use until a replacement passes.", ev.Workspace)
	case CauseSuspended:
		subject = fmt.Sprintf("Your workspace %s has been suspended", ev.Workspace)
		body = fmt.Sprintf("%s has been suspended. Work and history are kept. Ask the operator to restore the workspace.", ev.Workspace)
	case CauseClosed:
		if ev.Days < 1 {
			return Notice{}, ErrExportWindow
		}
		subject = fmt.Sprintf("Your workspace %s has been closed", ev.Workspace)
		body = fmt.Sprintf("%s has been closed. Work and history are kept for %d days so you can export them. Request an export of the workspace before the window closes.", ev.Workspace, ev.Days)
	default:
		return Notice{}, ErrUnknownCause
	}

	return Notice{
		Subject:   subject,
		Body:      body,
		DedupeKey: dedupeKey(ev.Workspace, ev.Cause, ev.At),
	}, nil
}

// dedupeKey is the one place the key's shape is made.
func dedupeKey(workspace string, cause Cause, at time.Time) string {
	return workspace + ":" + string(cause) + ":" + at.UTC().Format(dayFormat)
}

// badWorkspaceID reports whether id does not read as one id in a subject and a
// DedupeKey: whitespace or a control character splits it (a newline can forge a
// header), and a colon makes the key ambiguous.
func badWorkspaceID(id string) bool {
	for _, r := range id {
		if r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// validSpend reports whether a spend cap event states what its notice says:
// money that is a number.
func validSpend(spent, cap float64) bool {
	if math.IsNaN(spent) || math.IsInf(spent, 0) || spent < 0 {
		return false
	}
	if math.IsNaN(cap) || math.IsInf(cap, 0) || cap <= 0 {
		return false
	}
	return true
}

// Deduper records which notices a caller has sent, so a workspace's owner
// receives at most one notice per cause per workspace per UTC day.
type Deduper interface {
	// Claim records a notice for workspace and cause on the UTC day of at and
	// reports whether it is that day's first: false means a notice for the same
	// workspace, cause and day was already claimed and must not be sent again.
	Claim(workspace string, cause Cause, at time.Time) bool
}

// Memory is an in-memory Deduper for one process. Its state is bounded by the
// workspaces and causes it has seen: a claim on a new day replaces the day the
// map remembers for that workspace and cause.
//
// The zero value is ready to use, and every method is safe for concurrent use.
type Memory struct {
	mu   sync.Mutex
	last map[claimKey]string
}

// claimKey is the workspace and cause a Memory remembers one day for.
type claimKey struct {
	workspace string
	cause     Cause
}

// Claim records a notice for workspace and cause on the UTC day of at and
// reports whether it is that day's first.
func (m *Memory) Claim(workspace string, cause Cause, at time.Time) bool {
	day := at.UTC().Format(dayFormat)
	key := claimKey{workspace: workspace, cause: cause}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = make(map[claimKey]string)
	}
	if m.last[key] == day {
		return false
	}
	m.last[key] = day
	return true
}
