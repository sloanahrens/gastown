package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// An om review's usage is read from the transcripts its run wrote in the review
// directory, and written as the review returns (gt-6qc3nw.13). The run is bounded
// by its own start and end: a transcript last written before the review started
// is an earlier run's and is left out, so a second review of the same bead adds
// its own sessions to the record and a repeat of the same sessions changes nothing.

// omUsageSink writes the usage record of each om review the landing worker of
// one rig runs. A nil function is the production one.
type omUsageSink struct {
	townRoot string
	// list names the transcripts under a review directory; nil is
	// agentlog.ProjectTranscripts.
	list func(townRoot, workDir string) ([]string, error)
	// write merges a record into the usage tree; nil is usageattrib.Write.
	write func(usageDir string, rec dashboard.UsageRecord, sessionIDs []string) error
	logf  func(format string, args ...any)

	mu sync.Mutex
	// quiet holds the beads whose review ran with no transcript, so that is
	// logged once per bead, not once per review.
	quiet map[string]bool
}

// record writes the usage record of one om review. It returns nothing: a
// failure is logged here and never reaches the landing, which has its verdict.
func (s *omUsageSink) record(_ context.Context, u land.ReviewUsage) {
	list := s.list
	if list == nil {
		list = agentlog.ProjectTranscripts
	}
	write := s.write
	if write == nil {
		write = usageattrib.Write
	}
	paths, err := list(s.townRoot, u.Dir)
	if err != nil {
		s.logf("landing_worker: %s: om usage: listing its review transcripts: %v", u.BeadID, err)
		return
	}
	var sessions []usageattrib.SessionUsage
	var readErrs []error
	for _, path := range paths {
		sess, err := seatSession(path, u.Start)
		if err != nil {
			readErrs = append(readErrs, err)
		}
		if sess != nil {
			sessions = append(sessions, *sess)
		}
	}
	if len(sessions) == 0 {
		s.noTranscript(u.BeadID)
		return
	}
	workspace, err := usageattrib.WorkspaceOf(s.townRoot, u.BeadID)
	if err != nil {
		s.logf("landing_worker: %s: om usage record not written: %v", u.BeadID, err)
		return
	}
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}
	rec := usageattrib.Build(usageattrib.Facts{
		Bead:      u.BeadID,
		Kind:      dashboard.UsageKindOM,
		Class:     dashboard.UsageClassOMReview,
		Rig:       u.Rig,
		Workspace: workspace,
		Start:     u.Start,
		End:       u.End,
		// The review returned, so its run finished.
		Complete: true,
	}, sessions)
	usageDir := filepath.Join(constants.TownRuntimePath(s.townRoot), "usage")
	if err := errors.Join(write(usageDir, rec, ids), errors.Join(readErrs...)); err != nil {
		s.logf("landing_worker: %s: om usage record: %v", u.BeadID, err)
	}
}

// noTranscript notes that a review wrote no transcript, once for the bead. The
// first review of a bead that wrote none says so; the rest say nothing.
func (s *omUsageSink) noTranscript(bead string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quiet[bead] {
		return
	}
	if s.quiet == nil {
		s.quiet = map[string]bool{}
	}
	s.quiet[bead] = true
	s.logf("landing_worker: %s: om usage: its review wrote no transcript, so no usage record is written", bead)
}
