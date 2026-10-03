// Package beads provides rig identity bead management.
package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RigState represents the operational state of a rig.
type RigState string

const (
	// RigStateActive means the rig is operational and accepting work.
	RigStateActive RigState = "active"
	// RigStateArchived means the rig is no longer in use.
	RigStateArchived RigState = "archived"
	// RigStateMaintenance means the rig is temporarily offline for maintenance.
	RigStateMaintenance RigState = "maintenance"
)

// ValidRigState returns true if the given state is a recognized rig state.
func ValidRigState(s RigState) bool {
	switch s {
	case RigStateActive, RigStateArchived, RigStateMaintenance:
		return true
	}
	return false
}

// RigFields contains the fields specific to rig identity beads.
type RigFields struct {
	Repo   string   // Git URL for the rig's repository
	Prefix string   // Beads prefix for this rig (e.g., "gt", "bd")
	State  RigState // Operational state: active, archived, maintenance
}

// FormatRigDescription formats the description field for a rig identity bead.
func FormatRigDescription(name string, fields *RigFields) string {
	if fields == nil {
		return ""
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Rig identity bead for %s.", name))
	lines = append(lines, "")

	if fields.Repo != "" {
		lines = append(lines, fmt.Sprintf("repo: %s", fields.Repo))
	}
	if fields.Prefix != "" {
		lines = append(lines, fmt.Sprintf("prefix: %s", fields.Prefix))
	}
	if fields.State != "" {
		lines = append(lines, fmt.Sprintf("state: %s", string(fields.State)))
	}

	return strings.Join(lines, "\n")
}

// ParseRigFields extracts rig fields from an issue's description.
func ParseRigFields(description string) *RigFields {
	fields := &RigFields{}

	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "null" || value == "" {
			value = ""
		}

		switch strings.ToLower(key) {
		case "repo":
			fields.Repo = value
		case "prefix":
			fields.Prefix = value
		case "state":
			fields.State = RigState(value)
		}
	}

	return fields
}

// EnsureRigBead returns the rig identity bead, creating it if it doesn't exist.
// This is idempotent: if the bead already exists, it is returned as-is.
// Handles races and Dolt query hiccups where Show may fail even when the bead
// exists (gt-d8681).
func EnsureRigBead(c Client, name string, fields *RigFields) (*Issue, error) {
	prefix := "gt"
	if fields != nil && fields.Prefix != "" {
		prefix = fields.Prefix
	}
	id := RigBeadIDWithPrefix(prefix, name)

	// Try to find existing bead first
	if existing, err := c.Show(id); err == nil {
		return existing, nil
	}

	// Not found — try to create
	created, createErr := CreateRigBead(c, name, fields)
	if createErr == nil {
		return created, nil
	}

	// Create failed (likely duplicate key from race or Dolt hiccup) — retry Show
	if existing, err := c.Show(id); err == nil {
		return existing, nil
	}

	return nil, fmt.Errorf("ensuring rig bead %s: %w", id, createErr)
}

// CreateRigBead creates a rig identity bead for tracking rig metadata.
// The ID format is: <prefix>-rig-<name> (e.g., gt-rig-gastown)
// The ID is constructed internally from fields.Prefix and name.
// The created_by field is populated from BD_ACTOR env var for provenance tracking.
//
// A *Beads create runs bd directly: a rig bead is bd's custom issue_type
// "rig", so the database's custom types are configured before the write, and
// Client.Create has no field that sets an issue type on create (CreateOptions
// .Type is the deprecated label form). Any other Client is one database and
// one schema and runs createRigBeadOverClient.
func CreateRigBead(c Client, name string, fields *RigFields) (*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return createRigBeadOnStore(b, name, fields)
	}
	return createRigBeadOverClient(c, name, fields)
}

// createRigBeadOnStore is CreateRigBead's bd-backed path.
func createRigBeadOnStore(b *Beads, name string, fields *RigFields) (*Issue, error) {
	id, err := rigBeadIdentity(name, fields)
	if err != nil {
		return nil, err
	}
	description := FormatRigDescription(name, fields)

	// Ensure target database keeps rig as a durable custom type, not an
	// infra/wisp type. Failing closed avoids silently creating ephemeral rig
	// identity beads when type config cannot be persisted.
	if err := EnsureCustomTypes(b.getResolvedBeadsDir()); err != nil {
		return nil, fmt.Errorf("ensuring rig bead types: %w", err)
	}

	args := []string{"create", "--json",
		"--id=" + id,
		"--title=" + name,
		"--description=" + description,
		"--labels=gt:rig",
		"--type=rig",
	}
	if NeedsForceForID(id) {
		args = append(args, "--force")
	}

	// Default actor from BD_ACTOR env var for provenance tracking
	// Uses getActor() to respect isolated mode (tests)
	if actor := b.getActor(); actor != "" {
		args = append(args, "--actor="+actor)
	}

	out, err := b.run(args...)
	if err != nil && strings.Contains(err.Error(), "invalid issue type") {
		// The database rejected type=rig even though EnsureCustomTypes reported
		// types configured: a stale .gt-types-configured sentinel can cache a
		// lie when an older gt wrote type config to the wrong target (gt-8po).
		// Invalidate the cache, re-configure against the database, retry once.
		InvalidateTypeConfigCache(b.getResolvedBeadsDir())
		if typesErr := EnsureCustomTypes(b.getResolvedBeadsDir()); typesErr != nil {
			return nil, fmt.Errorf("re-ensuring rig bead types after invalid-type error: %w (create error: %v)", typesErr, err)
		}
		out, err = b.run(args...)
	}
	if err != nil {
		return nil, err
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing bd create output: %w", err)
	}

	return &issue, nil
}

// createRigBeadOverClient is CreateRigBead's path for a Client that is not
// bd-backed: one database, one schema, no custom-type configuration to run,
// and no Client method that sets a bd issue_type on create.
func createRigBeadOverClient(c Client, name string, fields *RigFields) (*Issue, error) {
	id, err := rigBeadIdentity(name, fields)
	if err != nil {
		return nil, err
	}
	return c.Create(CreateOptions{
		ID:          id,
		Title:       name,
		Description: FormatRigDescription(name, fields),
		Labels:      []string{"gt:rig"},
		// Priority -1 leaves bd's own default in place, as the raw create's
		// argv did by not passing --priority at all.
		Priority: -1,
	})
}

// rigBeadIdentity validates a rig bead's name and state and returns its ID.
func rigBeadIdentity(name string, fields *RigFields) (string, error) {
	// Guard against flag-like rig names (gt-e0kx5: --help garbage beads)
	if IsFlagLikeTitle(name) {
		return "", fmt.Errorf("refusing to create rig bead: %w (got %q)", ErrFlagTitle, name)
	}

	if fields != nil && fields.State != "" && !ValidRigState(fields.State) {
		return "", fmt.Errorf("invalid rig state %q: must be one of active, archived, maintenance", fields.State)
	}

	prefix := "gt"
	if fields != nil && fields.Prefix != "" {
		prefix = fields.Prefix
	}
	return RigBeadIDWithPrefix(prefix, name), nil
}

// GetRigBead retrieves a rig bead by name.
// Returns ErrNotFound if the rig does not exist.
func GetRigBead(c Client, name string) (*Issue, *RigFields, error) {
	return GetRigByID(c, RigBeadID(name))
}

// GetRigByID retrieves a rig bead by its full ID.
// Returns ErrNotFound if the rig does not exist.
func GetRigByID(c Client, id string) (*Issue, *RigFields, error) {
	issue, err := c.Show(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}

	if !HasLabel(issue, "gt:rig") {
		return nil, nil, fmt.Errorf("bead %s is not a rig bead (missing gt:rig label)", id)
	}

	fields := ParseRigFields(issue.Description)
	return issue, fields, nil
}

// UpdateRigBead updates the fields for a rig bead.
func UpdateRigBead(c Client, name string, fields *RigFields) (*Issue, error) {
	issue, _, err := GetRigBead(c, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("rig %q not found", name)
		}
		return nil, err
	}

	description := FormatRigDescription(name, fields)

	if err := c.Update(issue.ID, UpdateOptions{Description: &description}); err != nil {
		return nil, err
	}

	updated, err := c.Show(issue.ID)
	if err != nil {
		return nil, fmt.Errorf("fetching updated rig: %w", err)
	}
	return updated, nil
}

// DeleteRigBead permanently deletes a rig bead.
func DeleteRigBead(c Client, name string) error {
	return c.DeleteIssues(RigBeadID(name))
}

// ListRigBeads returns all rig beads, keyed by their beads prefix.
func ListRigBeads(c Client) (map[string]*RigFields, error) {
	// Limit 0 overrides bd's default page, so a large town is not silently
	// cut short (B1-04).
	issues, err := c.List(ListOptions{Label: "gt:rig", Priority: -1})
	if err != nil {
		return nil, err
	}

	result := make(map[string]*RigFields, len(issues))
	for _, issue := range issues {
		fields := ParseRigFields(issue.Description)
		if fields.Prefix != "" {
			result[fields.Prefix] = fields
		}
	}

	return result, nil
}

// RigBeadIDWithPrefix generates a rig identity bead ID using the specified prefix.
// Format: <prefix>-rig-<name> (e.g., gt-rig-gastown)
func RigBeadIDWithPrefix(prefix, name string) string {
	return fmt.Sprintf("%s-rig-%s", prefix, name)
}

// RigBeadID generates a rig identity bead ID using "gt" prefix.
// For non-gastown rigs, use RigBeadIDWithPrefix with the rig's configured prefix.
func RigBeadID(name string) string {
	return RigBeadIDWithPrefix("gt", name)
}
