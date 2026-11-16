package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// A sandbox seat's work usage is read from the transcripts its own Claude
// sessions wrote, and written as the seat ends. A sandbox seat's transcripts are
// the copy its sandbox was copied to before it was deleted
// (session.CopiedSandboxTranscripts); a seat with no copy reads the host's Claude
// project directories for its host worktree. The worktree goes with the seat's
// directories, so the record is written before they are removed. A reused
// worktree keeps its predecessor's transcripts: those last written before the
// seat started are left out. A seat with none writes no record, and its end logs
// why.

// seatUsageHook holds the seams of the seat-end usage record.
type seatUsageHook struct {
	// list names the transcripts under a worktree. It has no default: a nil list
	// is refused, so a seat end never reads the host's Claude config and home
	// unless it was wired to. sandboxSeatEndHost wires agentlog.ProjectTranscripts.
	list func(townRoot, workDir string) ([]string, error)
	// store is the town's usage_records rows the record is filed in beside its
	// file. It has no default: an end that is not wired writes the file and
	// reports the row's absence, so a missing wiring is logged, never silent.
	// sandboxSeatEndHost wires the daemon's store.
	store usageRecordStore
	// write files a record as its file and its row in one call; nil is
	// writeUsageRecord over store. A *usageRowNotWrittenError means the file is
	// written and the row is not. A test replaces it to see the call, and it is
	// what keeps the unit tier off bd.
	write func(usageDir string, rec dashboard.UsageRecord, sessionIDs []string) error
}

// errNoSeatStart is the refusal when a seat has transcripts but no record names
// when it started: they cannot be told from a predecessor's, so none is counted.
var errNoSeatStart = errors.New("its sandbox record is gone, so its start is unknown and its transcripts cannot be told from its predecessor's")

// errSeatUsageUnlisted is the refusal when no transcript lister is wired: the
// seat's transcripts are not read, and no record is written.
var errSeatUsageUnlisted = errors.New("no transcript lister is wired, so its transcripts are not read")

// errSeatNoTranscriptOnHost is the refusal when the host's Claude project
// directories hold no transcript for the seat's worktree. The lister does not say
// why none is there, so the reason offers the sandbox location as a possibility only.
var errSeatNoTranscriptOnHost = errors.New("no transcript of its sessions is in the host's Claude project directories for its worktree, so none is read; if its sessions ran in a sandbox, they may have written theirs under /sandbox/.claude/projects, which the host does not hold; the copy taken out of that sandbox before it was deleted holds none of them, or none was taken")

// seatStartedAt is when the seat started: the creation stamp of its sandbox
// record. When a teardown has already removed that record, the removal left the
// stamp on the seat-end ledger line it wrote, so the start is read from the line
// whose record was alive at submitted (gt-kovllv). A seat that has neither has no
// start, and the zero time says so.
func (h *patrolScanHost) seatStartedAt(rig, name string, submitted time.Time) (time.Time, error) {
	sessionName := h.sessionName(rig, name)
	rec, ok, err := session.ReadSandboxRecord(h.town(), sessionName)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading its sandbox record for its start: %w", err)
	}
	if ok {
		return rec.CreatedAt, nil
	}
	start, _, skipped, err := session.SandboxSeatStartFromLedger(h.town(), sessionName, submitted)
	for _, s := range skipped {
		h.d.logger.Printf("sandbox seat end %s/%s: skipped %v: it names no seat, so the other seats' starts still stand", rig, name, s)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading its start from the seat-end ledger: %w", err)
	}
	return start, nil
}

// writeSeatUsage writes the usage record of the seat rig/name, which ends now
// and started at start, before its directories are removed. startErr is what
// seatStartedAt reported. A seat with no transcript on the host, or none written
// since it started, writes nothing and says why. A seat with no lister wired is
// refused before anything is read. It returns the path of the record file when
// one was written, and incomplete: the reasons some transcript could not be read
// in full, which the record already says its totals may understate. Every
// outcome that writes a record and not its row, or writes no record at all,
// returns a non-nil err, the reason; a path beside that err means the file is
// there and the row is not. The caller logs the outcome once: not written, the
// file written but not its row, or written but not read in full. The removal
// goes on either way.
func (h *patrolScanHost) writeSeatUsage(rig, name string, landing sandboxSeatLanding, start time.Time, startErr error) (written string, incomplete, err error) {
	list := h.seatUsage.list
	if list == nil {
		return "", nil, errSeatUsageUnlisted
	}
	write := h.seatUsage.write
	if write == nil {
		store := h.seatUsage.store
		write = func(usageDir string, rec dashboard.UsageRecord, sessionIDs []string) error {
			return writeUsageRecord(h.d.ctxOrBackground(), store, usageDir, rec, sessionIDs)
		}
	}
	clone := polecat.SandboxSeatHostDirsFor(filepath.Join(h.town(), rig), rig, name).ClonePath
	// The copy taken from the sandbox before it was deleted is read first: the
	// sandbox's own transcripts are the seat's, and the host holds none of them.
	paths, skippedByCap, err := session.CopiedSandboxTranscripts(h.town(), rig, name)
	if err != nil {
		return "", nil, fmt.Errorf("listing its copied transcripts: %w", err)
	}
	fromCopy := len(paths) > 0
	if !fromCopy && skippedByCap > 0 {
		// The cap left out every transcript the copy would have held: the record is
		// not written, and the reason names the cap rather than a missing copy.
		return "", nil, fmt.Errorf("its transcript copy holds none of its transcripts: the cap left out all %d of them", skippedByCap)
	}
	if !fromCopy {
		paths, err = list(h.town(), clone)
		if err != nil {
			return "", nil, fmt.Errorf("listing its transcripts: %w", err)
		}
	}
	if len(paths) == 0 {
		if startErr != nil {
			return "", nil, startErr
		}
		return "", nil, errSeatNoTranscriptOnHost
	}
	if startErr != nil {
		return "", nil, startErr
	}
	if start.IsZero() {
		return "", nil, errNoSeatStart
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
		if len(readErrs) == 0 {
			// Every transcript was last written before the start: each is a predecessor's.
			return "", nil, fmt.Errorf("none of its %d transcripts was written since it started, so none is its own", len(paths))
		}
		return "", nil, errors.Join(readErrs...)
	}
	workspace, err := usageattrib.WorkspaceOf(h.town(), landing.BeadID)
	if err != nil {
		return "", nil, errors.Join(err, errors.Join(readErrs...))
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
	path := usageattrib.WorkRecordPath(usageDir, landing.BeadID)
	if err := write(usageDir, rec, ids); err != nil {
		var rowOnly *usageRowNotWrittenError
		if !errors.As(err, &rowOnly) {
			path = ""
		}
		return path, nil, errors.Join(err, errors.Join(readErrs...))
	}
	// The cap cut the oldest of the seat's transcripts from the copy, so the record
	// leaves them out and says so.
	if fromCopy && skippedByCap > 0 {
		readErrs = append(readErrs, fmt.Errorf("transcript copy cut by the cap: %d files skipped", skippedByCap))
	}
	return path, errors.Join(readErrs...), nil
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
