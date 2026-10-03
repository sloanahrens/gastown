package beads

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// FormatSlingContextDescription serializes SlingContextFields as JSON.
// The context bead description is entirely scheduler-owned, so we use
// JSON instead of key-value lines — no user content collision, no delimiter.
func FormatSlingContextDescription(fields *capacity.SlingContextFields) string {
	b, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ParseSlingContextFields deserialises a context bead description.
// Returns nil if the description is not valid JSON.
func ParseSlingContextFields(description string) *capacity.SlingContextFields {
	var fields capacity.SlingContextFields
	if err := json.Unmarshal([]byte(description), &fields); err != nil {
		return nil
	}
	return &fields
}

// SlingContextStore is the Client surface the sling-context helpers below
// need: create, list, update, close, and one typed dependency edge. Client
// satisfies it, so *Beads and internal/beads/beadsfake both do; a caller
// holding less than a Client declares the same method set.
type SlingContextStore interface {
	Create(opts CreateOptions) (*Issue, error)
	Update(id string, opts UpdateOptions) error
	CloseWithReason(reason string, ids ...string) error
	List(opts ListOptions) ([]*Issue, error)
	AddTypedDependency(issue, dependsOn, depType string) error
}

// A Client is enough to run every helper below.
var _ SlingContextStore = (Client)(nil)

// CreateSlingContext creates an ephemeral sling context bead that tracks
// scheduling state for a work bead. The work bead is never modified.
//
// The bead takes bd's default issue type: CreateOptions carries no issue_type,
// and the label is what identifies a sling context.
func CreateSlingContext(c SlingContextStore, workBeadTitle, workBeadID string, fields *capacity.SlingContextFields) (*Issue, error) {
	title := fmt.Sprintf("sling-context: %s", workBeadTitle)
	if len(title) > 200 {
		title = title[:200]
	}

	issue, err := c.Create(CreateOptions{
		Title:       title,
		Description: FormatSlingContextDescription(fields),
		Labels:      []string{capacity.LabelSlingContext},
		Ephemeral:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("creating sling context: %w", err)
	}

	// Add tracks dependency: context bead → work bead
	if err := c.AddTypedDependency(issue.ID, workBeadID, "tracks"); err != nil {
		// Non-fatal: the context bead was created, just missing the dep link.
		// This can happen if the work bead is in a different DB and external refs aren't set up.
		fmt.Printf("Warning: could not add tracks dep %s → %s: %v\n", issue.ID, workBeadID, err)
	}

	return issue, nil
}

// FindOpenSlingContext finds an open sling context for the given work bead ID.
// Used for idempotency checks. Returns (nil, nil, nil) if none found.
func FindOpenSlingContext(c SlingContextStore, workBeadID string) (*Issue, *capacity.SlingContextFields, error) {
	contexts, err := ListOpenSlingContexts(c)
	if err != nil {
		return nil, nil, err
	}

	for _, ctx := range contexts {
		fields := ParseSlingContextFields(ctx.Description)
		if fields != nil && fields.WorkBeadID == workBeadID {
			return ctx, fields, nil
		}
	}

	return nil, nil, nil
}

// openSlingContexts lists the open sling context beads.
var openSlingContexts = ListOptions{
	Status:    "open",
	Label:     capacity.LabelSlingContext,
	Priority:  -1,
	Limit:     0,
	Ephemeral: true,
}

// ListOpenSlingContexts returns all open sling context beads.
func ListOpenSlingContexts(c SlingContextStore) ([]*Issue, error) {
	return c.List(openSlingContexts)
}

// CloseSlingContext closes a sling context bead with a reason.
// Idempotent: suppresses "already closed" errors so retries are safe.
func CloseSlingContext(c SlingContextStore, contextID, reason string) error {
	err := c.CloseWithReason(reason, contextID)
	if err != nil && strings.Contains(err.Error(), "already closed") {
		return nil // Idempotent — already in desired state
	}
	return err
}

// UpdateSlingContextFields updates the description (fields) of a sling context bead.
func UpdateSlingContextFields(c SlingContextStore, contextID string, fields *capacity.SlingContextFields) error {
	description := FormatSlingContextDescription(fields)
	return c.Update(contextID, UpdateOptions{Description: &description})
}
