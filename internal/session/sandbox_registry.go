package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// SandboxRecord says that a polecat's session lives in an OpenShell sandbox.
// One file per session under <town>/.runtime/sandboxes/<session>.json,
// written by the openshell backend's Prepare and removed by its Delete. The
// tmux router reads it (sandboxRouting below) so any client in any gt
// process reaches the session; the daemon's listings count it as a live
// session. It is durable on purpose: a daemon restart must still route to
// a sandbox that is running.
type SandboxRecord struct {
	Session string `json:"session"`
	Rig     string `json:"rig"`
	Polecat string `json:"polecat"`
	// Workspace is the id of the workspace the rig belongs to, so the seat's
	// hours can be metered after its record is gone (plan row M5).
	Workspace string    `json:"workspace,omitempty"`
	Gateway   string    `json:"gateway,omitempty"`
	Profile   string    `json:"profile"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	// Branch and WorkDir locate the clone inside the sandbox, so a stop can
	// push its commits before the sandbox is deleted (gt-hwpfq). Empty on a
	// record written before that push existed.
	Branch  string `json:"branch,omitempty"`
	WorkDir string `json:"work_dir,omitempty"`
}

// SandboxRegistryDir is where the records live.
func SandboxRegistryDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "sandboxes")
}

func sandboxRecordPath(townRoot, session string) string {
	return filepath.Join(SandboxRegistryDir(townRoot), session+".json")
}

// SandboxSeatLedgerPath is the seat-end ledger: one line per removed record,
// so a seat's hours exist with or without the bead that named it (plan row M5).
func SandboxSeatLedgerPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "metrics", "sandbox-seats.jsonl")
}

// sandboxSeatEndLine is one line of the seat-end ledger: the record a removal
// deleted, stamped with when it ended. WorkBead is the bead the seat was
// submitted on, from its intent record; the key is absent when that record
// names none or cannot be read.
type sandboxSeatEndLine struct {
	Session   string    `json:"session"`
	Rig       string    `json:"rig"`
	Polecat   string    `json:"polecat"`
	Workspace string    `json:"workspace"`
	Profile   string    `json:"profile"`
	CreatedAt time.Time `json:"created_at"`
	EndedAt   time.Time `json:"ended_at"`
	WorkBead  string    `json:"work_bead,omitempty"`
}

// sandboxSeatBead is the bead rec's seat was submitted on, read from the seat's
// intent record. An absent record, or one that names no bead, is "" with no
// error. A record that names no seat has no intent to read, so it has no bead
// and no error either. A record whose intent cannot be read is an error: the
// bead is unknown, and the removal reports it.
// The intent is keyed by seat, not by session, so it can name a bead from
// another assignment: only a submission made at or after the record was
// created, and still standing, is this record's bead. The submission's own
// time is SubmittedAt; UpdatedAt moves on any change to the record, so it
// cannot tell submissions apart. Any other intent is "".
func sandboxSeatBead(townRoot string, rec SandboxRecord) (string, error) {
	if !plainPathName(rec.Rig) || !plainPathName(rec.Polecat) {
		return "", nil
	}
	seat := intent.Seat{Rig: rec.Rig, Role: "polecats", Name: rec.Polecat}
	r, err := intent.Read(townRoot, seat)
	if err != nil {
		return "", fmt.Errorf("reading the intent of seat %s for its seat-end line: %w", seat, err)
	}
	if !r.Submitted() || r.SubmittedAt.IsZero() || r.SubmittedAt.Before(rec.CreatedAt) {
		return "", nil
	}
	return r.WorkBead, nil
}

// appendSandboxSeatEnd appends rec's seat-end line to townRoot's ledger, naming
// bead when it is set.
func appendSandboxSeatEnd(townRoot string, rec SandboxRecord, bead string, now time.Time) error {
	// The line holds only strings and times, so its JSON cannot fail to encode.
	line, _ := json.Marshal(sandboxSeatEndLine{
		Session:   rec.Session,
		Rig:       rec.Rig,
		Polecat:   rec.Polecat,
		Workspace: rec.Workspace,
		Profile:   rec.Profile,
		CreatedAt: rec.CreatedAt.UTC(),
		EndedAt:   now.UTC(),
		WorkBead:  bead,
	})
	path := SandboxSeatLedgerPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302: operational metering data, not secrets
	if err != nil {
		return err
	}
	return writeSeatLine(f, line)
}

// writeSeatLine appends line and closes f; a failed write still closes it.
func writeSeatLine(f *os.File, line []byte) error {
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// SandboxSeatStartFromLedger is when the sandbox record of session started, for a
// seat whose record a removal has already deleted. The removal wrote that record's
// seat-end line, so the line is the start it handed on: the one line of session
// whose record was alive when the seat submitted at submitted (created at or
// before it, ended at or after it). A line for an earlier seat of the same name
// ended before the submission and is left out. ok is false when no line matches,
// which is a record that never existed. A line that does not parse is skipped
// and returned in skipped, one error a line naming its line number: it cannot be
// attributed to any seat, so the other seats still get their starts. Unless it
// names session: such a line may be this seat's own, so the seat is refused
// rather than matched past it. A stamp-less submission is refused, and so are two
// matches that name different records: the start is unknown, never guessed.
func SandboxSeatStartFromLedger(townRoot, session string, submitted time.Time) (start time.Time, ok bool, skipped []error, err error) {
	if submitted.IsZero() {
		return time.Time{}, false, nil, errors.New("its submission has no stamp, so its record cannot be told from a predecessor's")
	}
	data, err := os.ReadFile(SandboxSeatLedgerPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil, nil
	}
	if err != nil {
		return time.Time{}, false, nil, fmt.Errorf("reading the seat-end ledger: %w", err)
	}
	var found []time.Time
	for i, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line sandboxSeatEndLine
		if err := json.Unmarshal(raw, &line); err != nil {
			bad := fmt.Errorf("seat-end ledger line %d does not parse: %w", i+1, err)
			if bytes.Contains(raw, []byte(session)) {
				return time.Time{}, false, nil, fmt.Errorf("%w; it names %s, so it may be its own record", bad, session)
			}
			skipped = append(skipped, bad)
			continue
		}
		if line.Session != session || line.CreatedAt.After(submitted) || line.EndedAt.Before(submitted) {
			continue
		}
		found = append(found, line.CreatedAt)
	}
	switch {
	case len(found) == 0:
		return time.Time{}, false, skipped, nil
	case len(found) > 1 && !allSameTime(found):
		return time.Time{}, false, skipped, fmt.Errorf("seat-end ledger names %d records of %s alive at its submission", len(found), session)
	}
	return found[0], true, skipped, nil
}

// allSameTime reports whether every stamp in ts is the same instant.
func allSameTime(ts []time.Time) bool {
	for _, t := range ts[1:] {
		if !t.Equal(ts[0]) {
			return false
		}
	}
	return true
}

// sandboxRecordWorkspace is the workspace id a record for rig carries. The
// town's workspace map is keyed by rig prefix, not rig name, so the rig's
// prefix is resolved from the town registry first; a registry this town cannot
// read, or one that does not name the rig with a prefix, is an error, never the
// "gt" fallback. A map that cannot be read is an error too, never an empty or
// guessed id (plan rows M1 and M5).
func sandboxRecordWorkspace(townRoot, rig string) (string, error) {
	prefix, err := sandboxRigPrefix(townRoot, rig)
	if err != nil {
		return "", err
	}
	m, err := workspaceid.Load(townRoot)
	if err != nil {
		return "", err
	}
	return m.For(prefix)
}

// sandboxRigPrefix is the beads prefix the town's registry gives rig. Only a
// registry file that is absent takes config.GetRigPrefix's default "gt": a town
// with no mayor/rigs.json (its registry is mayor/town.json) must still spawn,
// and failing closed there would break every sandbox. A registry that exists
// must name the rig with a beads prefix, because "gt" for a present but
// unusable entry guesses the workspace of another rig.
func sandboxRigPrefix(townRoot, rig string) (string, error) {
	path := filepath.Join(townRoot, "mayor", "rigs.json")
	cfg, err := config.LoadRigsConfig(path)
	if errors.Is(err, config.ErrNotFound) {
		return config.GetRigPrefix(townRoot, rig), nil
	}
	if err != nil {
		return "", fmt.Errorf("sandbox record: rig %q has no readable beads prefix in %s: %w", rig, path, err)
	}
	entry, ok := cfg.Rigs[rig]
	if !ok || entry.BeadsConfig == nil {
		return "", fmt.Errorf("sandbox record: rig %q has no beads prefix in %s", rig, path)
	}
	prefix := strings.TrimSuffix(entry.BeadsConfig.Prefix, "-")
	if prefix == "" {
		return "", fmt.Errorf("sandbox record: rig %q has no beads prefix in %s", rig, path)
	}
	return prefix, nil
}

// WriteSandboxRecord records r for its session.
func WriteSandboxRecord(townRoot string, r SandboxRecord) error {
	if !validSessionName(r.Session) {
		return fmt.Errorf("sandbox record: bad session name %q", r.Session)
	}
	if err := os.MkdirAll(SandboxRegistryDir(townRoot), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(sandboxRecordPath(townRoot, r.Session), append(data, '\n'), 0o644)
}

// ReadSandboxRecord returns the record for session; ok is false when there
// is none (the session is a host tmux session, or nothing).
func ReadSandboxRecord(townRoot, session string) (r SandboxRecord, ok bool, err error) {
	if !validSessionName(session) {
		return r, false, nil
	}
	data, err := os.ReadFile(sandboxRecordPath(townRoot, session))
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, false, fmt.Errorf("sandbox record %s: %w", session, err)
	}
	return r, true, nil
}

// RemoveSandboxRecord forgets session, writing its seat-end line once the
// record is gone. Idempotent: a record that does not exist, or one another
// removal already took, writes no line, and a failed removal writes none
// either, so a retry cannot line the same seat twice. A ledger that cannot be
// written still leaves the removal done, its error joined with the removal's
// (plan row M5). A seat whose bead cannot be read gets its line without the bead,
// and that error is joined the same way.
func RemoveSandboxRecord(townRoot, session string) error {
	return RemoveSandboxRecordAt(townRoot, session, time.Now)
}

// RemoveSandboxRecordAt is RemoveSandboxRecord with its clock injected: the
// line's ended_at is now(), so a caller with a clock of its own pins it.
func RemoveSandboxRecordAt(townRoot, session string, now func() time.Time) error {
	return removeSandboxRecordWith(townRoot, session, now, os.Remove)
}

// removeSandboxRecordWith is RemoveSandboxRecordAt with the unlink injected, so
// a test can fail a removal and drive its retry.
func removeSandboxRecordWith(townRoot, session string, now func() time.Time, remove func(string) error) error {
	if !validSessionName(session) {
		return nil
	}
	rec, ok, readErr := ReadSandboxRecord(townRoot, session)
	removeErr := remove(sandboxRecordPath(townRoot, session))
	switch {
	case errors.Is(removeErr, os.ErrNotExist):
		// Gone already: whoever removed it wrote its line, or there was none.
		// Either way this call writes nothing.
		removeErr, ok = nil, false
	case removeErr != nil:
		// The record still stands, so the seat has not ended: no line yet.
		ok = false
	}
	if readErr != nil {
		// An unreadable record is not a missing one: the seat existed and its
		// line cannot be written, so say so rather than meter nothing.
		return errors.Join(fmt.Errorf("reading %s for its seat-end line: %w", session, readErr), removeErr)
	}
	var ledgerErr, beadErr error
	if ok {
		bead, err := sandboxSeatBead(townRoot, rec)
		beadErr = err
		ledgerErr = appendSandboxSeatEnd(townRoot, rec, bead, now())
	}
	return errors.Join(ledgerErr, beadErr, removeErr)
}

// ListSandboxSessions returns the recorded session names, sorted; a
// missing or unreadable registry lists nothing.
func ListSandboxSessions(townRoot string) []string {
	out, _ := listSandboxSessions(townRoot)
	return out
}

// listSandboxSessions is ListSandboxSessions reporting a read error; a
// missing directory is an empty list, not an error.
func listSandboxSessions(townRoot string) ([]string, error) {
	entries, err := os.ReadDir(SandboxRegistryDir(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// sessionNameRe is the shape of a session name that may name a record
// file: a tmux session name as PrefixRegistry forms them, never a path
// element, never "." or "..", never hidden.
var sessionNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validSessionName(session string) bool {
	return session != "." && session != ".." && sessionNameRe.MatchString(session)
}

// ValidSandboxSessionName reports whether session may name a sandbox record,
// the rule WriteSandboxRecord enforces. A caller that finds a record by name
// asks it here and never keeps a copy of the pattern.
func ValidSandboxSessionName(session string) bool { return validSessionName(session) }

// SandboxExecPrefix is the wrapper argv that runs a command inside the
// sandbox of session on gateway: the exec prefix a routed tmux client uses.
func SandboxExecPrefix(gateway, session string) []string {
	argv := []string{"openshell"}
	if gateway != "" {
		argv = append(argv, "-g", gateway)
	}
	return append(argv, "sandbox", "exec", "-n", session, "--no-tty", "--no-login-shell", "--")
}

// SandboxTmuxSocket is the tmux socket name of the server inside a sandbox.
const SandboxTmuxSocket = "gt"
