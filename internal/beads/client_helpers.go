package beads

import (
	"errors"
	"fmt"
	"strings"
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

// UpdateAgentDescriptionFields updates the agent description fields updates
// sets, in one read-modify-write of the bead's description. A *Beads also
// routes id to its canonical database and holds the agent-bead lock.
func UpdateAgentDescriptionFields(c Client, id string, updates AgentFieldUpdates) error {
	if b, ok := c.(*Beads); ok {
		return b.UpdateAgentDescriptionFields(id, updates)
	}
	if err := updates.validate(); err != nil {
		return err
	}
	return updateAgentDescriptionFields(c, id, updates)
}
