package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// gt done refuses a COMPLETED submit while the last LOCAL ROUND on the work
// bead leaves a blocking finding unanswered (gt-mxz8kx). A blocking finding is
// a major (or critical) one; minor and info findings never block. It is
// answered when the branch head has moved past the round's head and a LOCAL
// ROUND REPLY for its id follows the block. There is no flag that skips it.

// localRoundSubmitCheck reads the work bead's notes and the branch head through
// function fields, so the test drives it with fakes.
type localRoundSubmitCheck struct {
	// readNotes returns the notes of the work bead as they are now.
	readNotes func(bead string) (string, error)
	// branchHead returns the head the branch is at now.
	branchHead func() (string, error)
}

// check returns nil when the submit may go on. It refuses when the notes cannot
// be read, when the last block does not parse, when the head cannot be read
// while a finding blocks, and when a blocking finding is unanswered.
func (c localRoundSubmitCheck) check(bead string) error {
	notes, err := c.readNotes(bead)
	if err != nil {
		return fmt.Errorf("could not read the notes of %s: %w", bead, err)
	}
	last, ok := lastLocalRoundLine(notes)
	if !ok || unavailableBlockRE.MatchString(last) {
		return nil
	}
	round, ok := land.ParseLastLocalRound(notes)
	if !ok {
		return fmt.Errorf("the last LOCAL ROUND block on %s does not parse; fix the notes before gt done submits", bead)
	}
	blocking := blockingLocalRoundFindings(round)
	if len(blocking) == 0 {
		return nil
	}
	head, err := c.branchHead()
	if err != nil {
		return fmt.Errorf("could not read the branch head: %w", err)
	}
	if head == "" {
		return errors.New("the branch head is empty; gt done cannot tell whether the local round was answered")
	}
	// An empty round head matches every head, so a malformed block cannot
	// count as moved.
	moved := !strings.HasPrefix(head, round.Head)
	answered := map[string]bool{}
	if moved {
		for _, r := range land.ParseLocalRoundReplies(notes, round.Round) {
			answered[r.FindingID] = true
		}
	}
	var open []land.Finding
	for _, f := range blocking {
		if !answered[f.ID] {
			open = append(open, f)
		}
	}
	if len(open) == 0 {
		return nil
	}
	return unansweredLocalRoundError(bead, round.Round, open)
}

// lastLocalRoundLine is the last line that opens a LOCAL ROUND block, whether
// or not the block parses. ok is false when notes holds no block.
func lastLocalRoundLine(notes string) (string, bool) {
	last, found := "", false
	for _, line := range strings.Split(notes, "\n") {
		if strings.HasPrefix(line, reviewPreviewBlockPrefix) {
			last, found = line, true
		}
	}
	return last, found
}

// blockingLocalRoundFindings are the findings of round that refuse a submit:
// the major and critical ones, compared the way the landing review compares
// severities.
func blockingLocalRoundFindings(round land.LocalRound) []land.Finding {
	var out []land.Finding
	for _, f := range round.Findings {
		switch strings.ToLower(strings.TrimSpace(f.Severity)) {
		case "major", "critical":
			out = append(out, f)
		}
	}
	return out
}

// unansweredLocalRoundError names each unanswered finding with its file, line
// and title, and says how to answer it. It carries no captured command output.
func unansweredLocalRoundError(bead string, round int, open []land.Finding) error {
	var b strings.Builder
	fmt.Fprintf(&b, "gt done refused: %d major finding(s) of LOCAL ROUND (%d) on %s are unanswered:", len(open), round, bead)
	for _, f := range open {
		fmt.Fprintf(&b, "\n  %s %s:%d %s", f.ID, f.Path, f.Line, f.Title)
	}
	b.WriteString("\nFix each finding and commit the fix, then answer it with gt review-preview reply <id> --fixed -m \"<what changed>\" (or --disputed -m \"<reason>\"). Then run gt done again.")
	return errors.New(b.String())
}

// gateDoneLocalRound runs the local-round check for a polecat's COMPLETED
// submit in cwd, before anything is written. A status other than COMPLETED
// submits nothing, and a crew submit is not a polecat round, so both pass.
// The work bead is the one --issue names, else the one the branch names, as
// review-preview reply reads it.
func gateDoneLocalRound(status, cwd, explicitIssue string, getenv func(string) string) error {
	if strings.ToUpper(status) != ExitCompleted || done.IsCrewRun(getenv, cwd) {
		return nil
	}
	g := git.NewGit(cwd)
	store := beads.New(cwd)
	return localRoundSubmitCheck{
		readNotes:  func(id string) (string, error) { return reviewPreviewNotes(id, store.Show) },
		branchHead: func() (string, error) { return g.Rev("HEAD") },
	}.checkFor(explicitIssue, g.CurrentBranch)
}

// checkFor runs check on the work bead: the explicit issue, else the one the
// branch names. branch is read only when no issue was given.
func (c localRoundSubmitCheck) checkFor(explicitIssue string, branch func() (string, error)) error {
	if explicitIssue != "" {
		return c.check(explicitIssue)
	}
	name, err := branch()
	if err != nil {
		return fmt.Errorf("getting current branch: %w", err)
	}
	bead, ok := reviewPreviewBeadFromBranch(name)
	if !ok {
		return fmt.Errorf("branch %s names no work bead, so gt done cannot read its LOCAL ROUND; pass --issue", name)
	}
	return c.check(bead)
}

// submitAfterLocalRound runs submit only when the local-round gate lets the
// submit go on. A refusal returns before submit writes the READY TO LAND block
// or the label.
func submitAfterLocalRound(gate, submit func() error) error {
	if err := gate(); err != nil {
		return err
	}
	return submit()
}
