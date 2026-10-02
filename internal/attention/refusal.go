package attention

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// The refusal ledger is gt done's own record of a submission it refused, and
// the daemon's revert-refused collector reads it back. It lives beside the
// queue but is written by a short-lived CLI process rather than the daemon:
// the refusal is over in the refused pane the moment it prints, so nothing
// else would carry it to the operator (gt-vsct7.6).
const (
	// RefusalsFileName is the append-only ledger of refused submissions.
	RefusalsFileName = "refusals.jsonl"
	// RefusalsRotatedName is where refusals.jsonl is rotated at EventsMaxBytes.
	RefusalsRotatedName = "refusals.jsonl.1"
	// RefusalsLockFileName is the flock refusals.jsonl is appended under.
	RefusalsLockFileName = "refusals.lock"
)

// KindRevertGuard is the refusal kind for a branch gt done refused because it
// undoes work already merged to the target (gt-63sz). It is the kind recorded
// in the ledger, distinct from the queue item kind KindRevertRefused.
const KindRevertGuard = "revert-guard"

// Refusal is one submission gt done refused, as the polecat's process recorded
// it. Head is the branch head the refusal named; the collector keys the item
// by it so a resubmission at another head clears it.
type Refusal struct {
	TS      time.Time `json:"ts"`
	Bead    string    `json:"bead"`
	Rig     string    `json:"rig"`
	Worker  string    `json:"worker"`
	Branch  string    `json:"branch"`
	Head    string    `json:"head"`
	Kind    string    `json:"kind"`
	Summary string    `json:"summary"`
}

// RefusalsPath is refusals.jsonl: the append-only refusal ledger.
func RefusalsPath(townRoot string) string {
	return filepath.Join(Dir(townRoot), RefusalsFileName)
}

// RotatedRefusalsPath is refusals.jsonl.1: the previous ledger, kept for one
// rotation.
func RotatedRefusalsPath(townRoot string) string {
	return filepath.Join(Dir(townRoot), RefusalsRotatedName)
}

// AppendRefusal appends r to refusals.jsonl under the flock, creating the file
// 0600 and rotating it to refusals.jsonl.1 at EventsMaxBytes. The ledger is the
// one queue file a polecat process writes, so the lock and the mode keep a
// second refused submission from interleaving a line or leaving a
// world-readable record.
func AppendRefusal(townRoot string, r Refusal) error {
	if err := os.MkdirAll(Dir(townRoot), 0o755); err != nil {
		return err
	}
	unlock, err := lock.FlockAcquire(filepath.Join(Dir(townRoot), RefusalsLockFileName))
	if err != nil {
		return err
	}
	defer unlock()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding refusal: %w", err)
	}
	// The ledger is read whole every heartbeat, so it is bounded like
	// events.jsonl: a refusal is a rare record, and one rotated away is far
	// past the window the daemon judges refusals against.
	if err := rotateIfFull(RefusalsPath(townRoot), RotatedRefusalsPath(townRoot), EventsMaxBytes); err != nil {
		return err
	}
	f, err := os.OpenFile(RefusalsPath(townRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: a path built from the town root
	if err != nil {
		return err
	}
	_, werr := f.Write(append(data, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// ReadRefusals returns every refusal in refusals.jsonl, oldest first. A line
// that does not parse is skipped for the same reason a partial line in
// events.jsonl is: losing one unreadable refusal beats failing the read that
// lists the rest.
func ReadRefusals(townRoot string) ([]Refusal, error) {
	f, err := os.Open(RefusalsPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Refusal
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var r Refusal
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, scanner.Err()
}
