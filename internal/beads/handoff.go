// Package beads provides handoff bead operations for agent workflow management.
package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// The handoff, mail-sweep and molecule-attachment helpers below are free
// functions over Client so any Client (internal/beads/beadsfake in unit
// tests) runs the same logic *Beads did. The one store concern they keep is
// the per-bead flock around a molecule attach/detach: its file lives in the
// store's own .beads directory, so only a *Beads can name it (see lockBeadFor).

// Issue status constants kept as untyped strings for backward compatibility.
// The typed versions (IssueStatus) are in status.go.
const (
	// StatusPinned is the status for pinned beads that never get closed.
	StatusPinned = "pinned"

	// StatusHooked is the status for beads on an agent's hook (work assignment).
	StatusHooked = "hooked"
)

// HandoffBeadTitle returns the well-known title for a role's handoff bead.
func HandoffBeadTitle(role string) string {
	return role + " Handoff"
}

// FindHandoffBead returns the role's pinned handoff bead, or nil when there
// is none.
func FindHandoffBead(c Client, role string) (*Issue, error) {
	issues, err := c.List(ListOptions{Status: StatusPinned, Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("listing pinned issues: %w", err)
	}

	targetTitle := HandoffBeadTitle(role)
	for _, issue := range issues {
		if issue.Title == targetTitle {
			return issue, nil
		}
	}

	return nil, nil
}

// FindAllHandoffBeads returns every pinned handoff bead keyed by role, one
// list call rather than the N+1 FindHandoffBead would cost.
func FindAllHandoffBeads(c Client) (map[string]*Issue, error) {
	issues, err := c.List(ListOptions{Status: StatusPinned, Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("listing pinned issues: %w", err)
	}

	result := make(map[string]*Issue)
	for _, issue := range issues {
		// Handoff bead titles follow the pattern "<role> Handoff"
		if strings.HasSuffix(issue.Title, " Handoff") {
			role := strings.TrimSuffix(issue.Title, " Handoff")
			result[role] = issue
		}
	}

	return result, nil
}

// GetOrCreateHandoffBead returns the handoff bead for a role, creating and
// pinning it when the role has none.
func GetOrCreateHandoffBead(c Client, role string) (*Issue, error) {
	// Check if it exists
	existing, err := FindHandoffBead(c, role)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	issue, err := c.Create(CreateOptions{
		Title:       HandoffBeadTitle(role),
		Labels:      []string{"gt:task"},
		Priority:    2,
		Description: "", // Empty until first handoff
		Actor:       role,
	})
	if err != nil {
		return nil, fmt.Errorf("creating handoff bead: %w", err)
	}

	// Update to pinned status. If this fails, clean up the orphaned bead
	// to prevent duplicates on retry (FindHandoffBead only searches pinned beads).
	status := StatusPinned
	if err := c.Update(issue.ID, UpdateOptions{Status: &status}); err != nil {
		// Best-effort cleanup — ignore delete error since pin failure is the real problem
		_ = c.CloseWithReason("orphaned: failed to pin", issue.ID)
		return nil, fmt.Errorf("setting handoff bead to pinned: %w", err)
	}

	// Re-fetch to get updated status. If this fails, the bead is already
	// created and pinned — a retry of GetOrCreateHandoffBead will find it.
	return c.Show(issue.ID)
}

// UpdateHandoffContent replaces the handoff bead's description with content,
// creating the bead when the role has none.
func UpdateHandoffContent(c Client, role, content string) error {
	issue, err := GetOrCreateHandoffBead(c, role)
	if err != nil {
		return err
	}

	return c.Update(issue.ID, UpdateOptions{Description: &content})
}

// ClearHandoffContent empties the role's handoff bead description; a role
// with no handoff bead is not an error.
func ClearHandoffContent(c Client, role string) error {
	issue, err := FindHandoffBead(c, role)
	if err != nil {
		return err
	}
	if issue == nil {
		return nil // Nothing to clear
	}

	empty := ""
	return c.Update(issue.ID, UpdateOptions{Description: &empty})
}

// ClearMailResult contains statistics from a ClearMail operation.
type ClearMailResult struct {
	Closed  int // Number of messages closed
	Cleared int // Number of pinned messages cleared (content removed)
}

// ClearMail closes the open non-pinned messages and empties the pinned ones,
// which stay open.
func ClearMail(c Client, reason string) (*ClearMailResult, error) {
	// List all open messages
	issues, err := c.List(ListOptions{
		Status:   "open",
		Label:    "gt:message",
		Priority: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}

	result := &ClearMailResult{}

	// Separate pinned from non-pinned
	var toClose []string
	var toClear []*Issue

	for _, issue := range issues {
		if issue.Status == StatusPinned {
			toClear = append(toClear, issue)
		} else {
			toClose = append(toClose, issue.ID)
		}
	}

	// Close non-pinned messages in batch. When bd refuses some and closes
	// the rest (a *PartialCloseError), count only the closed ones and go on
	// to the pinned messages; any other failure closed nothing.
	var closeErr error
	if len(toClose) > 0 {
		err := c.CloseWithReason(reason, toClose...)
		result.Closed = len(ClosedIDs(toClose, err))
		if err != nil {
			if !errors.Is(err, ErrCloseRefused) {
				return nil, fmt.Errorf("closing messages: %w", err)
			}
			closeErr = fmt.Errorf("closing messages: %w", err)
		}
	}

	// Clear pinned messages — continue on error so partial progress isn't lost
	empty := ""
	var clearErrs []error
	for _, issue := range toClear {
		if err := c.Update(issue.ID, UpdateOptions{Description: &empty}); err != nil {
			clearErrs = append(clearErrs, fmt.Errorf("clearing pinned message %s: %w", issue.ID, err))
			continue
		}
		result.Cleared++
	}

	if len(clearErrs) > 0 {
		return result, errors.Join(closeErr, fmt.Errorf("partial failure clearing %d/%d pinned messages: %w",
			len(clearErrs), len(toClear), errors.Join(clearErrs...)))
	}

	return result, closeErr
}

// CloseStaleHookedMailBeads closes agentID's gt:message beads in status=hooked
// and returns how many it closed, so a new handoff mail does not accumulate
// behind the ones an ended session left hooked. (GH#3859)
func CloseStaleHookedMailBeads(c Client, agentID string) (int, error) {
	hooked, err := c.List(ListOptions{
		Status:   StatusHooked,
		Label:    "gt:message",
		Assignee: agentID,
		Priority: -1,
	})
	if err != nil {
		return 0, fmt.Errorf("listing hooked mail beads: %w", err)
	}
	if len(hooked) == 0 {
		return 0, nil
	}
	ids := make([]string, len(hooked))
	for i, h := range hooked {
		ids[i] = h.ID
	}
	if err := c.ForceCloseWithReason("handoff: superseded by new session", ids...); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// lockBead acquires a cross-process advisory lock for a bead operation.
// Returns a cleanup function that releases the lock.
// Lock files are stored in <beadsDir>/locks/<beadID>.flock.
func (b *Beads) lockBead(beadID string) (func(), error) {
	locksDir := filepath.Join(b.getResolvedBeadsDir(), "locks")
	if err := os.MkdirAll(locksDir, 0755); err != nil {
		return nil, fmt.Errorf("creating locks directory: %w", err)
	}
	lockPath := filepath.Join(locksDir, beadID+".flock")
	return lock.FlockAcquire(lockPath)
}

// beadLocker is the per-bead cross-process lock a store holds around a
// read-modify-write of one pinned bead. Only *Beads implements it: the lock
// file lives in the store's own .beads directory.
type beadLocker interface {
	lockBead(beadID string) (func(), error)
}

// lockBeadFor takes c's lock for beadID and returns the release. A Client
// that keeps no such lock (beadsfake in unit tests) releases nothing: there
// is no file to guard, and no second process to race.
func lockBeadFor(c Client, beadID string) (func(), error) {
	l, ok := c.(beadLocker)
	if !ok {
		return func() {}, nil
	}
	unlock, err := l.lockBead(beadID)
	if err != nil {
		return nil, fmt.Errorf("acquiring bead lock: %w", err)
	}
	return unlock, nil
}

// AttachMolecule records moleculeID as pinnedBeadID's attached molecule,
// stamping the attach time. On a *Beads the read-modify-write runs under the
// bead's flock.
func AttachMolecule(c Client, pinnedBeadID, moleculeID string) (*Issue, error) {
	// Acquire per-bead lock to serialize concurrent attach/detach operations
	unlock, err := lockBeadFor(c, pinnedBeadID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch the pinned bead
	issue, err := c.Show(pinnedBeadID)
	if err != nil {
		return nil, fmt.Errorf("fetching pinned bead: %w", err)
	}

	// Only allow pinned beads (permanent records like role definitions)
	if issue.Status != StatusPinned {
		return nil, fmt.Errorf("issue %s is not pinned (status: %s)", pinnedBeadID, issue.Status)
	}

	// Build attachment fields with current timestamp
	fields := &AttachmentFields{
		AttachedMolecule: moleculeID,
		AttachedAt:       currentTimestamp(),
	}

	// Update description with attachment fields
	newDesc := SetAttachmentFields(issue, fields)

	// Update the issue
	if err := c.Update(pinnedBeadID, UpdateOptions{Description: &newDesc}); err != nil {
		return nil, fmt.Errorf("updating pinned bead: %w", err)
	}

	// Re-fetch to return updated state
	return c.Show(pinnedBeadID)
}

// DetachMolecule removes pinnedBeadID's molecule attachment, returning the
// bead unchanged when there is nothing attached. On a *Beads the
// read-modify-write runs under the bead's flock.
func DetachMolecule(c Client, pinnedBeadID string) (*Issue, error) {
	// Acquire per-bead lock to serialize concurrent attach/detach operations
	unlock, err := lockBeadFor(c, pinnedBeadID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch the pinned bead
	issue, err := c.Show(pinnedBeadID)
	if err != nil {
		return nil, fmt.Errorf("fetching pinned bead: %w", err)
	}

	// Check if there's anything to detach
	if ParseAttachmentFields(issue) == nil {
		return issue, nil // Nothing to detach
	}

	// Clear attachment fields by passing nil
	newDesc := SetAttachmentFields(issue, nil)

	// Update the issue
	if err := c.Update(pinnedBeadID, UpdateOptions{Description: &newDesc}); err != nil {
		return nil, fmt.Errorf("updating pinned bead: %w", err)
	}

	// Re-fetch to return updated state
	return c.Show(pinnedBeadID)
}

// GetAttachment returns pinnedBeadID's attachment fields, nil when no
// molecule is attached.
func GetAttachment(c Client, pinnedBeadID string) (*AttachmentFields, error) {
	issue, err := c.Show(pinnedBeadID)
	if err != nil {
		return nil, err
	}

	return ParseAttachmentFields(issue), nil
}

// currentTimestamp returns the current time in ISO 8601 format.
func currentTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}
