package beads

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/flock"
)

// The functions in this file are the merge-request and agent-bead helpers
// written over Client, so any Client (internal/beads/beadsfake in unit tests)
// runs the same logic *Beads does. Each one hands a *Beads to its method,
// which adds what only a bd-backed store has: prefix routing to another
// database, the per-process caches, the cross-process agent-bead lock and,
// for merge requests, the one-round-trip SQL read of the wisps table. The
// logic after that is shared: the *Beads method ends in the same unexported
// function the Client path runs. beadsfake.RunClientContract pins the two
// paths to the same answers.
//
// The agent-bead writes below take that split one step further: they route
// through ForAgentBead themselves and take the agent-bead lock when the
// Client keeps one, so UpdateAgentDescriptionFields and the one-field setters
// built on it run the same read-modify-write under the same lock whatever
// the Client is. The reads differ only in what a Client can answer from
// memory: ListAgentBeads and GetAgentBead consult a *Beads' preload snapshot
// and fall through to a query for everyone else.
//
// The creates are the one write family that keeps its *Beads branch: creating
// an agent bead also chooses its canonical database and configures that
// database's custom types, which a bare Client cannot name (gt-8we).
// CreateOrReopenAgentBead's reopen and ResetAgentBeadForReuse's routing are on
// the store for the same reason.

// ListMergeRequests returns the merge-request beads matching opts, durable
// issues and wisps alike, hydrated with their full details. opts.Label
// defaults to gt:merge-request for the wisps; a wisp's status matches
// opts.Status, with "" meaning open and "all" any. opts.Rig drops MRs whose
// description names another rig.
func ListMergeRequests(c Client, opts ListOptions) ([]*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return b.ListMergeRequests(opts)
	}
	opts.Ephemeral = false
	issues, err := c.List(opts)
	if err != nil {
		return nil, err
	}
	label := opts.Label
	if label == "" {
		label = "gt:merge-request"
	}
	// As on *Beads, a failed wisp read degrades to the issues alone.
	wisps, _ := c.List(ListOptions{Label: label, Status: "all", Priority: -1, Ephemeral: true})
	return finishMergeRequests(c, issues, wisps, opts)
}

// finishMergeRequests merges the listed wisps into the listed issues (the
// issue wins a duplicate ID), applies the status and rig filters, and
// hydrates the result.
func finishMergeRequests(c Client, issues, wisps []*Issue, opts ListOptions) ([]*Issue, error) {
	seen := make(map[string]bool, len(issues))
	for _, issue := range issues {
		seen[issue.ID] = true
	}
	for _, w := range wisps {
		if seen[w.ID] || !mrWispStatusMatches(w.Status, opts.Status) {
			continue
		}
		seen[w.ID] = true
		issues = append(issues, w)
	}
	return hydrateMergeRequestDetails(c, filterMergeRequestsByRig(issues, opts.Rig))
}

// GetAgentBead returns an agent bead and its parsed fields; nil, nil, nil
// when id does not exist, and an error when it exists but is not an agent
// bead.
func GetAgentBead(c Client, id string) (*Issue, *AgentFields, error) {
	if b, ok := c.(*Beads); ok {
		return b.GetAgentBead(id)
	}
	return getAgentBead(c, id)
}

func getAgentBead(c Client, id string) (*Issue, *AgentFields, error) {
	issue, err := c.Show(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return agentBeadFields(id, issue)
}

// ClearAgentActiveMRIfMatches clears an agent bead's active_mr only while it
// still names expectedMR, and reports whether it wrote the clear. A missing
// agent bead is not an error (false, nil); a bead that is not an agent bead
// is.
func ClearAgentActiveMRIfMatches(c Client, id, expectedMR string) (bool, error) {
	if b, ok := c.(*Beads); ok {
		return b.ClearAgentActiveMRIfMatches(id, expectedMR)
	}
	return clearAgentActiveMRIfMatches(c, strings.TrimSpace(id), strings.TrimSpace(expectedMR))
}

// clearAgentActiveMRIfMatches is the read-compare-write, with id and
// expectedMR already trimmed. *Beads runs it under the agent-bead lock.
func clearAgentActiveMRIfMatches(c Client, id, expectedMR string) (bool, error) {
	if id == "" || expectedMR == "" {
		return false, nil
	}
	issue, err := c.Show(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if !IsAgentBead(issue) {
		return false, fmt.Errorf("%s is not an agent bead", id)
	}
	fields := ParseAgentFields(issue.Description)
	if strings.TrimSpace(fields.ActiveMR) != expectedMR {
		return false, nil
	}
	fields.ActiveMR = ""
	description := FormatAgentDescription(issue.Title, fields)
	if err := c.Update(id, UpdateOptions{Description: &description}); err != nil {
		return false, err
	}
	return true, nil
}

// ForAgentBead returns the Client agent-bead operations should go through:
// (*Beads).ForAgentBead for a *Beads, which routes each agent bead ID to its
// canonical database, and c itself for any other Client, which is one
// database.
func ForAgentBead(c Client) Client {
	if b, ok := c.(*Beads); ok {
		return b.ForAgentBead()
	}
	return c
}

// agentBeadLocker is the per-ID cross-process lock a store holds around a
// read-modify-write of one agent bead. Only *Beads implements it: the lock
// file lives in the store's own .beads directory, which a Client in general
// has no way to name.
type agentBeadLocker interface {
	lockAgentBead(id string) (*flock.Flock, error)
}

// lockAgentBeadFor takes c's agent-bead lock for id and returns the release.
// A Client that keeps no such lock (beadsfake in unit tests) releases
// nothing: there is no file to guard, and no second process to race.
func lockAgentBeadFor(c Client, id string) (func(), error) {
	l, ok := c.(agentBeadLocker)
	if !ok {
		return func() {}, nil
	}
	fl, err := l.lockAgentBead(id)
	if err != nil {
		return nil, fmt.Errorf("locking agent bead %s: %w", id, err)
	}
	return func() { unlockAgentBead(fl) }, nil
}

// UpdateAgentDescriptionFields updates the agent description fields updates
// sets, in one read-modify-write of the bead's description. The write goes to
// the agent's canonical database (ForAgentBead) and, on a *Beads, under the
// agent-bead lock.
func UpdateAgentDescriptionFields(c Client, id string, updates AgentFieldUpdates) error {
	if err := updates.validate(); err != nil {
		return err
	}
	target := ForAgentBead(c)
	release, err := lockAgentBeadFor(target, id)
	if err != nil {
		return err
	}
	defer release()
	return updateAgentDescriptionFields(target, id, updates)
}

// ListAgentBeads returns every agent bead in one pass, keyed by ID: the
// durable issues carrying the gt:agent label, merged with the wisps that are
// agent beads, an issue winning a duplicate ID.
//
// Agent beads are infrastructure rows that bd list hides by default, so the
// issues half asks for them; the wisp half is the fallback existence source
// for agents that live as wisps, which is also why it is read unfiltered —
// see agentBeadsFromWisps. A *Beads answers from its PreloadBeads snapshot
// when one covers gt:agent.
func ListAgentBeads(c Client) (map[string]*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return b.ListAgentBeads()
	}
	issues, err := c.List(ListOptions{Label: "gt:agent", IncludeInfra: true, Priority: -1})
	if err != nil {
		return nil, err
	}
	issuesByID := make(map[string]*Issue, len(issues))
	for _, issue := range issues {
		issuesByID[issue.ID] = issue
	}
	// As on *Beads, a failed wisp read degrades to the issues alone.
	wisps, _ := c.List(ListOptions{Ephemeral: true, Priority: -1})
	return mergeAgentBeadSources(issuesByID, agentBeadsFromWisps(wisps)), nil
}

// agentBeadsFromWisps selects the agent beads among wisps, keyed by ID: on
// the type and label test, then on the ID shape for the wisps whose JSON
// omits issue_type and labels (isAgentBeadByID).
func agentBeadsFromWisps(wisps []*Issue) map[string]*Issue {
	result := make(map[string]*Issue)
	for _, w := range wisps {
		if IsAgentBead(w) || isAgentBeadByID(w.ID) {
			result[w.ID] = w
		}
	}
	return result
}

// UpdateAgentState sets an agent bead's agent_state field. bd >= 0.62 has no
// supported `bd agent state` writer, so the state is written into the
// description; readers mirror that contract via ResolveAgentState.
func UpdateAgentState(c Client, id string, state string) error {
	return UpdateAgentDescriptionFields(c, id, AgentFieldUpdates{AgentState: &state})
}

// UpdateAgentCleanupStatus sets the cleanup_status field: the polecat's
// self-reported git state (clean, has_uncommitted, has_stash, has_unpushed).
func UpdateAgentCleanupStatus(c Client, id string, cleanupStatus string) error {
	return UpdateAgentDescriptionFields(c, id, AgentFieldUpdates{CleanupStatus: &cleanupStatus})
}

// UpdateAgentActiveMR sets active_mr, the merge request the agent currently
// holds; "" clears the field.
func UpdateAgentActiveMR(c Client, id string, activeMR string) error {
	return UpdateAgentDescriptionFields(c, id, AgentFieldUpdates{ActiveMR: &activeMR})
}

// UpdateAgentNotificationLevel sets the notification_level (DND) field. Valid
// levels are verbose, normal and muted; "" resets to the normal default.
func UpdateAgentNotificationLevel(c Client, id string, level string) error {
	return UpdateAgentDescriptionFields(c, id, AgentFieldUpdates{NotificationLevel: &level})
}

// GetAgentNotificationLevel returns an agent's notification level, "normal"
// when the agent bead or the field is absent.
func GetAgentNotificationLevel(c Client, id string) (string, error) {
	_, fields, err := GetAgentBead(c, id)
	if err != nil {
		return "", err
	}
	if fields == nil || fields.NotificationLevel == "" {
		return NotifyNormal, nil
	}
	return fields.NotificationLevel, nil
}

// UpdateAgentCompletion writes all of meta to the agent bead in one
// read-modify-write: the completion state gt done records for the witness to
// read instead of POLECAT_DONE mail (gt-x7t9).
func UpdateAgentCompletion(c Client, id string, meta *CompletionMetadata) error {
	mrFailed := meta.MRFailed
	pushFailed := meta.PushFailed
	return UpdateAgentDescriptionFields(c, id, AgentFieldUpdates{
		ExitType:        &meta.ExitType,
		MRID:            &meta.MRID,
		Branch:          &meta.Branch,
		LastSourceIssue: &meta.HookBead,
		MRFailed:        &mrFailed,
		PushFailed:      &pushFailed,
		CompletionTime:  &meta.CompletionTime,
	})
}

// ClearAgentCompletion removes every completion metadata field, resetting an
// agent bead that is being re-slung with new work.
func ClearAgentCompletion(c Client, id string) error {
	empty := ""
	notFailed := false
	return UpdateAgentDescriptionFields(c, id, AgentFieldUpdates{
		ExitType:        &empty,
		MRID:            &empty,
		Branch:          &empty,
		LastSourceIssue: &empty,
		MRFailed:        &notFailed,
		PushFailed:      &notFailed,
		CompletionTime:  &empty,
	})
}

// CreateAgentBead creates an agent bead. A *Beads create lands in the ID's
// canonical (prefix-routed) database and configures that database's custom
// types before it writes (gt-8we, gt-fcxe9.11) — store concerns a bare Client
// has no way to name, so they stay on the *Beads method. Any other Client is
// one database and one schema, and runs createAgentBead.
func CreateAgentBead(c Client, id, title string, fields *AgentFields) (*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return b.CreateAgentBead(id, title, fields)
	}
	return createAgentBead(c, id, title, fields)
}

// createAgentBead is the Client create path, over Client.Create so a fake can
// script it. Client.Create supplies --force for an ID whose prefix the database
// would not read (NeedsForceForID) and records the actor the raw call passed.
func createAgentBead(c Client, id, title string, fields *AgentFields) (*Issue, error) {
	// Guard against flag-like titles (gt-e0kx5: --help garbage beads)
	if IsFlagLikeTitle(title) {
		return nil, fmt.Errorf("refusing to create agent bead: %w (got %q)", ErrFlagTitle, title)
	}
	return c.Create(CreateOptions{
		ID:          id,
		Title:       title,
		Description: FormatAgentDescription(title, fields),
		Labels:      []string{"gt:agent"},
		// Priority -1 leaves bd's own default in place, as this create's raw
		// argv did by not passing --priority at all.
		Priority: -1,
	})
}

// CreateOrReopenAgentBead creates an agent bead, or updates the bead already
// under that ID when the create is refused. A *Beads create-failure path goes
// through bd's reopen, which no Client method covers; any other Client falls
// back to the status update the store itself falls back to when reopen fails.
func CreateOrReopenAgentBead(c Client, id, title string, fields *AgentFields) (*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return storeCreateOrReopenAgentBead(b, id, title, fields)
	}
	return createOrReopenAgentBead(c, id, title, fields)
}

// storeCreateOrReopenAgentBead is CreateOrReopenAgentBead's *Beads path: a
// polecat nuked and re-spawned under the same name leaves a bead under the ID,
// so a refused create updates it instead of failing on the unique constraint.
// Its create is CreateAgentBead, which lands in the ID's canonical
// (prefix-routed) database and configures that database's custom types; its
// reopen is bd's own, which the Client path has no method for, so a legacy
// closed bead comes back open before the update. Both the reopen and the
// update run under the store's agent-bead lock (gt-joazs).
func storeCreateOrReopenAgentBead(b *Beads, id, title string, fields *AgentFields) (*Issue, error) {
	// First try to create the bead (no lock needed - create is atomic)
	issue, err := b.CreateAgentBead(id, title, fields)
	if err == nil {
		return issue, nil
	}

	// Create failed - need to do Show→Reopen→Update which requires locking
	// to prevent concurrent modifications (e.g. nuke clearing fields while
	// spawn is updating them). See gt-joazs.
	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return nil, fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { unlockAgentBead(fl) }()

	// Create failed - check if bead already exists (handles both open and closed states)
	createErr := err

	// Dual-scope: the bead may live in its canonical (rig-local) database or,
	// for legacy agents, in the town database (gt-8we).
	target := b.resolveAgentBead(id)

	existing, showErr := target.Show(id)
	if showErr != nil {
		// Bead doesn't exist (or can't be read) - return original create error
		return nil, createErr
	}

	// If bead is closed, reopen it first
	if existing.Status == "closed" {
		if _, reopenErr := target.run("reopen", id, "--reason=re-spawning agent"); reopenErr != nil {
			// Reopen failed - try setting status to open via update as fallback
			// This handles Dolt backends where bd reopen may not work
			openStatus := "open"
			if updateErr := target.Update(id, UpdateOptions{Status: &openStatus}); updateErr != nil {
				return nil, fmt.Errorf("could not reopen agent bead %s (reopen: %v, update: %v, original: %v)",
					id, reopenErr, updateErr, createErr)
			}
		}
	}

	// Update the bead with new fields and ensure gt:agent label is set.
	// Agent beads use type=task (a valid built-in type) and are identified
	// by the gt:agent label, not by type (see IsAgentBead).
	description := FormatAgentDescription(title, fields)
	updateOpts := UpdateOptions{
		Title:       &title,
		Description: &description,
		SetLabels:   labelsForAgentBeadReuse(existing.Labels),
	}
	if err := target.Update(id, updateOpts); err != nil {
		return nil, fmt.Errorf("updating agent bead: %w", err)
	}

	// Note: role slot no longer set - role definitions are config-based
	// Note: hook_bead slot no longer set - bd slot removed in v0.62 (hq-l6mm5)

	// Return the updated bead
	return target.Show(id)
}

// createOrReopenAgentBead is CreateOrReopenAgentBead's Client path: create,
// then on failure the locked read-modify-write that reopens a closed bead and
// rewrites title, description and labels. It takes the agent-bead lock when c
// keeps one, the same guard the store's path runs under (gt-joazs).
func createOrReopenAgentBead(c Client, id, title string, fields *AgentFields) (*Issue, error) {
	issue, createErr := createAgentBead(c, id, title, fields)
	if createErr == nil {
		return issue, nil
	}

	target := ForAgentBead(c)
	release, lockErr := lockAgentBeadFor(target, id)
	if lockErr != nil {
		return nil, lockErr
	}
	defer release()

	existing, showErr := target.Show(id)
	if showErr != nil {
		// No bead under this ID to reopen: the original create error stands.
		return nil, createErr
	}
	if existing.Status == "closed" {
		open := "open"
		if updateErr := target.Update(id, UpdateOptions{Status: &open}); updateErr != nil {
			return nil, fmt.Errorf("could not reopen agent bead %s (update: %v, original: %v)",
				id, updateErr, createErr)
		}
	}

	description := FormatAgentDescription(title, fields)
	if updateErr := target.Update(id, UpdateOptions{
		Title:       &title,
		Description: &description,
		SetLabels:   labelsForAgentBeadReuse(existing.Labels),
	}); updateErr != nil {
		return nil, fmt.Errorf("updating agent bead: %w", updateErr)
	}
	return target.Show(id)
}

// ResetAgentBeadForReuse clears every mutable field on an agent bead without
// closing it, leaving agent_state=nuked so a re-spawn updates it in place
// instead of running bd's close/reopen cycle (gt-14b8o). A *Beads reset routes
// the ID to its canonical database; any other Client is one database.
func ResetAgentBeadForReuse(c Client, id, reason string) error {
	if b, ok := c.(*Beads); ok {
		return storeResetAgentBeadForReuse(b, id)
	}
	target := ForAgentBead(c)
	release, err := lockAgentBeadFor(target, id)
	if err != nil {
		return err
	}
	defer release()
	return resetAgentBeadForReuse(target, id)
}

// storeResetAgentBeadForReuse is ResetAgentBeadForReuse's *Beads path: the ID
// routes to its canonical database and the lock is the store's own. The
// read-modify-write is shared with the Client path (resetAgentBeadForReuse).
func storeResetAgentBeadForReuse(b *Beads, id string) error {
	// Lock the agent bead to prevent concurrent read-modify-write races.
	// Without this, a concurrent CreateOrReopenAgentBead could overwrite
	// the nuked state we're about to set. See gt-joazs.
	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { unlockAgentBead(fl) }()

	// Dual-scope: rig-local (canonical) first, town fallback for legacy
	// agent beads (gt-8we).
	target := b.resolveAgentBead(id)
	return resetAgentBeadForReuse(target, id)
}

// resetAgentBeadForReuse is the read-modify-write of ResetAgentBeadForReuse,
// with the ID already routed and the agent-bead lock already held.
func resetAgentBeadForReuse(c Client, id string) error {
	issue, err := c.Show(id)
	if err != nil {
		return err
	}

	// Preserve the immutable fields (title, role_type, rig); clear the rest.
	fields := ParseAgentFields(issue.Description)
	fields.HookBead = ""
	fields.ActiveMR = ""
	fields.CleanupStatus = ""
	fields.Mode = ""
	fields.AgentState = string(AgentStateNuked)
	fields.ExitType = ""
	fields.MRID = ""
	fields.Branch = ""
	fields.LastSourceIssue = ""
	fields.MRFailed = false
	fields.PushFailed = false
	fields.CompletionTime = ""

	description := FormatAgentDescription(issue.Title, fields)
	if err := c.Update(id, UpdateOptions{Description: &description}); err != nil {
		return fmt.Errorf("resetting agent bead fields: %w", err)
	}
	return nil
}
