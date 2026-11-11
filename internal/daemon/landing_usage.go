package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
// is an earlier run's, and one last written after it ended is the next round's
// or a later run's; both are left out. A second review of the same bead adds its
// own sessions to the record, and a repeat of the same sessions changes nothing.

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
	// The bead id names the record file, so it is checked before anything is
	// read or written.
	if !plainBeadID(u.BeadID) {
		s.logf("landing_worker: om usage record not written: bead id %q is not a plain id", u.BeadID)
		return
	}
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
		// A directory that cannot be read may hide this run's transcripts, and
		// a record with no session cannot be written, so nothing is counted.
		s.logf("landing_worker: %s: om usage: its review transcripts cannot be listed, so no usage record is written: %v", u.BeadID, err)
		return
	}
	var sessions []usageattrib.SessionUsage
	readErrs := 0
	for _, path := range paths {
		sess, err := reviewSession(path, u)
		if err != nil {
			readErrs++
			s.logf("landing_worker: %s: om usage: transcript %s cannot be read, so its record is incomplete: %v", u.BeadID, path, err)
		}
		if sess != nil {
			sessions = append(sessions, *sess)
		}
	}
	if len(sessions) == 0 {
		if readErrs == 0 {
			s.noTranscript(u.BeadID)
			return
		}
		s.logf("landing_worker: %s: om usage record not written: none of its review transcripts could be read", u.BeadID)
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
		// The review returned, so its run finished. A transcript that could not
		// be read whole leaves the record incomplete.
		Complete: readErrs == 0,
	}, sessions)
	usageDir := filepath.Join(constants.TownRuntimePath(s.townRoot), "usage")
	if err := write(usageDir, rec, ids); err != nil {
		s.logf("landing_worker: %s: om usage record: %v", u.BeadID, err)
	}
}

// reviewSession reads one transcript of the review's run. A transcript last
// written after the review ended is left out here; one written before it started
// is left out by seatSession.
func reviewSession(path string, u land.ReviewUsage) (*usageattrib.SessionUsage, error) {
	if info, err := os.Stat(path); err == nil && info.ModTime().After(u.End) {
		return nil, nil
	}
	return seatSession(path, u.Start)
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

// plainBeadID reports whether id can name a file on its own: letters, digits,
// dot, hyphen and underscore, with no ".." run in it. The usage writer cleans
// its file name too; this refuses the id before any read or write.
func plainBeadID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return !strings.Contains(id, "..")
}
