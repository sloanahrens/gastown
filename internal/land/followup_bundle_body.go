package land

import (
	"fmt"
	"strings"
	"time"
)

// This file writes the day bundle's body and the pure edit that appends a
// later landing to one (gt-skdmfw, gt-8y81kw); the bundle's identity is its
// labels (followup_bundle.go).

// FollowupAddition is one landing's append to an open bundle.
type FollowupAddition struct{ Lines, Boxes, Labels []string }

// bundleSpec is the bead a landing's group opens for its package and UTC day.
func bundleSpec(w Work, res Result, day time.Time, g followupGroup) FollowupSpec {
	labels := []string{FollowUpLabel}
	for _, c := range g.Findings {
		labels = append(labels, Fingerprint(c.Path, c.Title))
	}
	labels = append(labels, BundleLabels(day, g.Package)...)
	acceptance := followupAcceptance(g.Findings)
	in := "the repository root"
	if g.Package != "" {
		in = g.Package
	}
	return FollowupSpec{
		Title:      fmt.Sprintf("Follow-ups in %s for %s", in, day.UTC().Format(time.DateOnly)),
		Body:       bundleBody(w, res, g, acceptance),
		Acceptance: acceptance,
		Labels:     labels,
		Priority:   followUpPriority,
	}
}

// bundleBody is followupBody's shaped sections with every Goal bullet naming
// its landing, so followupIDsForCommit finds the landing in a later pass.
func bundleBody(w Work, res Result, g followupGroup, acceptance string) string {
	body := followupBody(w, res, g, acceptance)
	// The Goal's bullets are the lines right after its prose paragraph, one per
	// finding in order: rewrite them by position, never by finding the Detail
	// text, which may appear elsewhere in the body.
	head, rest, _ := strings.Cut(body, "\n\n")
	lines := strings.SplitAfter(rest, "\n")
	for i, c := range g.Findings {
		if i < len(lines) && lines[i] == "- "+c.Detail+"\n" {
			lines[i] = bundleGoalLine(w, res, c) + "\n"
		}
	}
	return head + "\n\n" + strings.Join(lines, "")
}

// bundleGoalLine spells the landed commit in full: followupIDsForCommit matches
// the commit a landing was recorded at, not a shortened form.
func bundleGoalLine(w Work, res Result, c FollowupCandidate) string {
	return fmt.Sprintf("- %s (from %s, landed %s)", c.Detail, w.BeadID, res.LandedCommit)
}

// bundleAddition is what appending this landing's group to an open bundle writes.
func bundleAddition(w Work, res Result, g followupGroup) FollowupAddition {
	add := FollowupAddition{}
	if acc := followupAcceptance(g.Findings); acc != "" {
		add.Boxes = strings.Split(acc, "\n")
	}
	for _, c := range g.Findings {
		add.Lines = append(add.Lines, bundleGoalLine(w, res, c))
		add.Labels = append(add.Labels, Fingerprint(c.Path, c.Title))
	}
	return add
}
