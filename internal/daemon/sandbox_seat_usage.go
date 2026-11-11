package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// A sandbox seat's work usage is read from the transcripts its own Claude
// sessions wrote in its host worktree, and written as the seat ends. The
// worktree goes with the seat's directories, so the record is written before
// they are removed. A reused worktree keeps its predecessor's transcripts: those
// last written before the seat started are left out.

// seatUsageHook holds the seams of the seat-end usage record. A nil function
// is the production one.
type seatUsageHook struct {
	// list names the transcripts under a worktree; nil is
	// agentlog.ProjectTranscripts.
	list func(townRoot, workDir string) ([]string, error)
	// write merges a record into the usage tree; nil is usageattrib.Write.
	write func(usageDir string, rec dashboard.UsageRecord, sessionIDs []string) error
}

// errNoSeatStart is the refusal when a seat has transcripts but no record names
// when it started: they cannot be told from a predecessor's, so none is counted.
var errNoSeatStart = errors.New("its sandbox record is gone, so its start is unknown and its transcripts cannot be told from its predecessor's")

// seatStartedAt is when the seat started: the creation stamp of its sandbox
// record. A seat whose record is gone has no start, and the zero time says so.
// The caller reads it before anything below removes the record.
func (h *patrolScanHost) seatStartedAt(rig, name string) (time.Time, error) {
	rec, ok, err := session.ReadSandboxRecord(h.town(), h.sessionName(rig, name))
	if err != nil {
		return time.Time{}, fmt.Errorf("reading its sandbox record for its start: %w", err)
	}
	if !ok {
		return time.Time{}, nil
	}
	return rec.CreatedAt, nil
}

// writeSeatUsage writes the usage record of the seat rig/name, which ends now
// and started at start, before its directories are removed. startErr is what
// seatStartedAt reported. A seat with no transcript written since it started
// writes nothing. The error is for the caller to log once: the removal goes on
// either way.
func (h *patrolScanHost) writeSeatUsage(rig, name string, landing sandboxSeatLanding, start time.Time, startErr error) error {
	list := h.seatUsage.list
	if list == nil {
		list = agentlog.ProjectTranscripts
	}
	write := h.seatUsage.write
	if write == nil {
		write = usageattrib.Write
	}
	clone := polecat.SandboxSeatHostDirsFor(filepath.Join(h.town(), rig), rig, name).ClonePath
	paths, err := list(h.town(), clone)
	if err != nil {
		return fmt.Errorf("listing its transcripts: %w", err)
	}
	if len(paths) == 0 {
		return startErr
	}
	if startErr != nil {
		return startErr
	}
	if start.IsZero() {
		return errNoSeatStart
	}

	var sessions []usageattrib.SessionUsage
	var readErrs []error
	for _, path := range paths {
		s, err := seatSession(path, start)
		if err != nil {
			readErrs = append(readErrs, err)
		}
		if s != nil {
			sessions = append(sessions, *s)
		}
	}
	if len(sessions) == 0 {
		return errors.Join(readErrs...)
	}
	workspace, err := usageattrib.WorkspaceOf(h.town(), landing.BeadID)
	if err != nil {
		return errors.Join(err, errors.Join(readErrs...))
	}
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	rec := usageattrib.Build(usageattrib.Facts{
		Bead:      landing.BeadID,
		Kind:      dashboard.UsageKindWork,
		Class:     dashboard.UsageClassPolecat,
		Rig:       rig,
		Seat:      &name,
		Workspace: workspace,
		Start:     start,
		End:       h.d.clk().Now(),
		// The seat ends only once its landing is recorded, so its run finished.
		Complete: true,
	}, sessions)
	usageDir := filepath.Join(constants.TownRuntimePath(h.town()), "usage")
	return errors.Join(write(usageDir, rec, ids), errors.Join(readErrs...))
}

// seatSession reads one transcript the seat's sessions wrote, with the messages
// of the subagents that session started. It returns nil, and no error, when the
// transcript was last written before start: a predecessor's, left out. A
// transcript that cannot be read is returned incomplete, with the reasons as
// err, so its record says its totals may understate the run.
func seatSession(path string, start time.Time) (*usageattrib.SessionUsage, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if id == "" {
		return nil, fmt.Errorf("a transcript with no session id, left out: %s", filepath.Base(path))
	}
	info, err := os.Stat(path)
	if err != nil {
		return &usageattrib.SessionUsage{ID: id}, fmt.Errorf("statting its transcript: %w", err)
	}
	if info.ModTime().Before(start) {
		return nil, nil
	}
	s := usageattrib.SessionUsage{ID: id, Complete: true}
	var errs []error
	own, err := readSeatUsage(path)
	s.Main = own
	if err != nil {
		s.Complete = false
		errs = append(errs, err)
	}
	subDir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	entries, err := os.ReadDir(subDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.Complete = false
		errs = append(errs, fmt.Errorf("listing its subagents: %w", err))
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "agent-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		sub, err := readSeatUsage(filepath.Join(subDir, e.Name()))
		s.Subagent = append(s.Subagent, sub...)
		if err != nil {
			s.Complete = false
			errs = append(errs, err)
		}
	}
	return &s, errors.Join(errs...)
}

// readSeatUsage reads the assistant messages of one transcript file. A file
// that cannot be read, or has a line that does not parse, is an error; the
// messages that were read are returned all the same.
func readSeatUsage(path string) ([]usageattrib.Message, error) {
	msgs, bad, err := usageattrib.IterAssistantUsage(path)
	if err != nil {
		return msgs, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	if bad > 0 {
		return msgs, fmt.Errorf("%s has %d lines that do not parse", filepath.Base(path), bad)
	}
	return msgs, nil
}
