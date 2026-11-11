package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/config"
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

// usageReadBound caps how long the landing path waits for one review's transcript
// read. The read is synchronous on the post-verdict landing path, so it must end.
const usageReadBound = 10 * time.Second

// errReadTimedOut is the cause a read that outlasts usageReadBound is recorded with.
var errReadTimedOut = errors.New("transcript read timed out")

// omUsageSink writes the usage record of each om review the landing worker of
// one rig runs. A nil function is the production one.
type omUsageSink struct {
	townRoot string
	// list names the transcripts under a review directory; nil lists the Claude
	// project directories of townRoot through configDirFor and userHomeDir.
	list func(ctx context.Context, townRoot, workDir string) ([]string, error)
	// configDirFor and userHomeDir resolve the Claude config dir and the home the
	// production lister reads; nil is config.ClaudeConfigDir and os.UserHomeDir.
	configDirFor func() (string, error)
	userHomeDir  func() (string, error)
	// bound returns the context one read runs under; nil is a usageReadBound timeout.
	bound func(ctx context.Context) (context.Context, context.CancelFunc)
	// write merges a record into the usage tree; nil is usageattrib.Write.
	write func(usageDir string, rec dashboard.UsageRecord, sessionIDs []string) error
	// mark writes the incomplete record of a run whose transcripts could not be
	// listed; nil is usageattrib.MarkIncomplete.
	mark func(usageDir string, f usageattrib.Facts, reason string) error
	logf func(format string, args ...any)

	mu sync.Mutex
	// quiet holds the beads whose review ran with no transcript, so that is
	// logged once per bead, not once per review.
	quiet map[string]bool
}

// record writes the usage record of one om review. It returns nothing: a
// failure is logged here and never reaches the landing, which has its verdict.
func (s *omUsageSink) record(ctx context.Context, u land.ReviewUsage) {
	// The bead id names the record file, so it is checked before anything is
	// read or written.
	if !plainBeadID(u.BeadID) {
		s.logf("landing_worker: om usage record not written: bead id %q is not a plain id", u.BeadID)
		return
	}
	workspace, err := usageattrib.WorkspaceOf(s.townRoot, u.BeadID)
	if err != nil {
		s.logf("landing_worker: %s: om usage record not written: %v", u.BeadID, err)
		return
	}
	facts := usageattrib.Facts{
		Bead:      u.BeadID,
		Kind:      dashboard.UsageKindOM,
		Class:     dashboard.UsageClassOMReview,
		Rig:       u.Rig,
		Workspace: workspace,
		Start:     u.Start,
		End:       u.End,
		// The review returned, so its run finished. A transcript that could not
		// be read whole leaves the record incomplete.
		Complete: true,
	}
	run, err := s.readRun(ctx, u)
	if err != nil {
		// A directory that cannot be read may hide this run's transcripts. No
		// session is counted, but the review still leaves a record that says so.
		s.logf("landing_worker: %s: om usage: its review transcripts cannot be read, so its record is marked incomplete: %v", u.BeadID, err)
		s.markIncomplete(facts, err)
		return
	}
	for _, e := range run.readErrs {
		s.logf("landing_worker: %s: om usage: %v", u.BeadID, e)
	}
	if len(run.sessions) == 0 {
		if len(run.readErrs) == 0 {
			s.noTranscript(u.BeadID)
			return
		}
		s.logf("landing_worker: %s: om usage record not written: none of its review transcripts could be read", u.BeadID)
		return
	}
	facts.Complete = len(run.readErrs) == 0
	ids := make([]string, 0, len(run.sessions))
	for _, sess := range run.sessions {
		ids = append(ids, sess.ID)
	}
	rec := usageattrib.Build(facts, run.sessions)
	write := s.write
	if write == nil {
		write = usageattrib.Write
	}
	if err := write(s.usageDir(), rec, ids); err != nil {
		s.logf("landing_worker: %s: om usage record: %v", u.BeadID, err)
	}
}

// reviewRead is what one review run's transcripts held: the sessions read, and
// one error for each transcript that could not be read whole, naming its path.
type reviewRead struct {
	sessions []usageattrib.SessionUsage
	readErrs []error
}

// readRun lists and reads the review run's transcripts under one bound. The read
// is synchronous on the landing path, so one that outlasts the bound is abandoned
// and reported as errReadTimedOut; it finishes in the background and its result
// is dropped.
func (s *omUsageSink) readRun(ctx context.Context, u land.ReviewUsage) (reviewRead, error) {
	bctx, cancel := s.boundOf(ctx)
	defer cancel()
	type result struct {
		read reviewRead
		err  error
	}
	done := make(chan result, 1)
	go func() {
		read, err := s.readTranscripts(bctx, u)
		done <- result{read, err}
	}()
	select {
	case r := <-done:
		// A read that finished after the bound passed is still a timeout: its
		// transcripts may have been cut short.
		if bctx.Err() == nil {
			return r.read, r.err
		}
	case <-bctx.Done():
	}
	return reviewRead{}, errReadTimedOut
}

// readTranscripts lists the review directory's transcripts and reads each one.
// A listing error is returned; a transcript that cannot be read is an entry in
// readErrs, and its readable sibling is still counted.
func (s *omUsageSink) readTranscripts(ctx context.Context, u land.ReviewUsage) (reviewRead, error) {
	paths, err := s.lister(ctx, u.Dir)
	if err != nil {
		return reviewRead{}, err
	}
	var read reviewRead
	for _, path := range paths {
		sess, err := reviewSession(path, u)
		if err != nil {
			read.readErrs = append(read.readErrs, fmt.Errorf("transcript %s cannot be read, so its record is incomplete: %w", path, err))
		}
		if sess != nil {
			read.sessions = append(read.sessions, *sess)
		}
	}
	return read, nil
}

// lister lists the transcripts of one review directory.
func (s *omUsageSink) lister(ctx context.Context, dir string) ([]string, error) {
	if s.list != nil {
		return s.list(ctx, s.townRoot, dir)
	}
	configDirFor := s.configDirFor
	if configDirFor == nil {
		configDirFor = config.ClaudeConfigDir
	}
	userHomeDir := s.userHomeDir
	if userHomeDir == nil {
		userHomeDir = os.UserHomeDir
	}
	return agentlog.ProjectTranscriptsFrom(s.townRoot, dir, configDirFor, userHomeDir)
}

// boundOf returns the context one read runs under.
func (s *omUsageSink) boundOf(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.bound != nil {
		return s.bound(ctx)
	}
	return context.WithTimeout(ctx, usageReadBound)
}

// markIncomplete writes the incomplete record of a run whose transcripts could
// not be read at all. Its error field holds the first line of cause.
func (s *omUsageSink) markIncomplete(f usageattrib.Facts, cause error) {
	mark := s.mark
	if mark == nil {
		mark = usageattrib.MarkIncomplete
	}
	reason, _, _ := strings.Cut(cause.Error(), "\n")
	if err := mark(s.usageDir(), f, reason); err != nil {
		s.logf("landing_worker: %s: om usage record: %v", f.Bead, err)
	}
}

// usageDir is the tree the usage records of this town are kept in.
func (s *omUsageSink) usageDir() string {
	return filepath.Join(constants.TownRuntimePath(s.townRoot), "usage")
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
