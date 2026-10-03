// Package beads provides audit logging for molecule operations.
package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DetachAuditEntry represents an audit log entry for a detach operation.
type DetachAuditEntry struct {
	Timestamp        string `json:"timestamp"`
	Operation        string `json:"operation"` // "detach", "burn", "squash"
	PinnedBeadID     string `json:"pinned_bead_id"`
	DetachedMolecule string `json:"detached_molecule"`
	DetachedBy       string `json:"detached_by,omitempty"` // Agent that triggered detach
	Reason           string `json:"reason,omitempty"`      // Optional reason for detach
	PreviousState    string `json:"previous_state,omitempty"`
}

// DetachOptions specifies optional context for a detach operation.
type DetachOptions struct {
	Operation string // "detach", "burn", "squash" - defaults to "detach"
	Agent     string // Who is performing the detach
	Reason    string // Optional reason for the detach
}

// detachAuditLogger is the store that keeps a detach audit log: the file
// lives in the store's own .beads directory, which a Client in general has no
// way to name. A Client that implements it (a *Beads) records the entry; one
// that does not (beadsfake in unit tests) detaches without one.
type detachAuditLogger interface {
	LogDetachAudit(entry DetachAuditEntry) error
}

// DetachMoleculeWithAudit removes molecule attachment from a pinned bead,
// logging the operation when the Client keeps an audit log. On a *Beads the
// read-modify-write runs under the bead's flock.
func DetachMoleculeWithAudit(c Client, pinnedBeadID string, opts DetachOptions) (*Issue, error) {
	// Acquire per-bead lock to serialize concurrent attach/detach operations
	unlock, err := lockBeadFor(c, pinnedBeadID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch the pinned bead first to get previous state
	issue, err := c.Show(pinnedBeadID)
	if err != nil {
		return nil, fmt.Errorf("fetching pinned bead: %w", err)
	}

	// Get current attachment info for audit
	attachment := ParseAttachmentFields(issue)
	if attachment == nil {
		return issue, nil // Nothing to detach
	}

	// Log the detach operation
	operation := opts.Operation
	if operation == "" {
		operation = "detach"
	}
	entry := DetachAuditEntry{
		Timestamp:        currentTimestamp(),
		Operation:        operation,
		PinnedBeadID:     pinnedBeadID,
		DetachedMolecule: attachment.AttachedMolecule,
		DetachedBy:       opts.Agent,
		Reason:           opts.Reason,
		PreviousState:    issue.Status,
	}
	if logger, ok := c.(detachAuditLogger); ok {
		if err := logger.LogDetachAudit(entry); err != nil {
			// Log error but don't fail the detach operation
			fmt.Fprintf(os.Stderr, "Warning: failed to write audit log: %v\n", err)
		}
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

// LogDetachAudit appends an audit entry to the audit log file.
// The audit log is stored in the resolved .beads directory as audit.log in JSONL format.
// This follows any beads redirect so audit entries go to the correct location.
func (b *Beads) LogDetachAudit(entry DetachAuditEntry) (retErr error) {
	auditPath := filepath.Join(b.getResolvedBeadsDir(), "audit.log")

	// Marshal entry to JSON
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling audit entry: %w", err)
	}

	// Append to audit log file
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("closing audit log: %w", err)
		}
	}()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing audit entry: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing audit log: %w", err)
	}

	return nil
}
