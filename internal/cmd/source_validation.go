package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

type submitSourceIssue struct {
	ID              string
	Issue           *beads.Issue
	BD              beads.Client
	CurrentBeadsDir string
	RoutedBeadsDir  string
}

// sourceStoreOpener opens the store at cwd pinned to beadsDir, the database
// an issue routes to.
type sourceStoreOpener func(cwd, beadsDir string) beads.Client

// openSourceStore is the bd store a sourceStoreOpener names.
func openSourceStore(cwd, beadsDir string) beads.Client {
	return beads.NewWithBeadsDir(cwd, beadsDir)
}

func routedIssueBeads(cwd, issueID string) (beads.Client, string, string) {
	return routedIssueBeadsIn(cwd, issueID, openSourceStore)
}

// routedIssueBeadsIn is routedIssueBeads whose store open opens.
func routedIssueBeadsIn(cwd, issueID string, open sourceStoreOpener) (beads.Client, string, string) {
	currentBeadsDir := beads.ResolveBeadsDir(cwd)
	routedBeadsDir := beads.ResolveBeadsDirForID(currentBeadsDir, issueID)
	return open(cwd, routedBeadsDir), currentBeadsDir, routedBeadsDir
}

func sourceRouteContext(currentBeadsDir, routedBeadsDir string) string {
	return fmt.Sprintf("current_db=%s routed_db=%s", currentBeadsDir, routedBeadsDir)
}

func resolveSubmitSourceIssue(cwd, issueID string) (*submitSourceIssue, error) {
	return resolveSubmitSourceIssueIn(cwd, issueID, openSourceStore)
}

// resolveSubmitSourceIssueIn is resolveSubmitSourceIssue whose source store
// open opens.
func resolveSubmitSourceIssueIn(cwd, issueID string, open sourceStoreOpener) (*submitSourceIssue, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return nil, fmt.Errorf("source_issue is required")
	}

	sourceBD, currentBeadsDir, routedBeadsDir := routedIssueBeadsIn(cwd, issueID, open)
	issue, err := sourceBD.Show(issueID)
	if err != nil {
		return nil, fmt.Errorf("source_issue %s could not be resolved (%s): %w", issueID, sourceRouteContext(currentBeadsDir, routedBeadsDir), err)
	}
	if err := validateConcreteSourceIssue(issueID, issue); err != nil {
		return nil, err
	}
	return &submitSourceIssue{
		ID:              issueID,
		Issue:           issue,
		BD:              sourceBD,
		CurrentBeadsDir: currentBeadsDir,
		RoutedBeadsDir:  routedBeadsDir,
	}, nil
}

func validateConcreteSourceIssue(issueID string, issue *beads.Issue) error {
	if reason := beads.ConcreteWorkIssueRejectReason(issue); reason != "" {
		return fmt.Errorf("source_issue %s is not concrete (%s)", issueID, reason)
	}
	return nil
}
