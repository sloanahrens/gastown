// Package overseer is the files the overseer session leaves under
// <town>/.runtime/overseer/: a heartbeat that says whether it is alive, a
// journal of what it did by kind, and the marker the daemon's silent-overseer
// rule keeps so it pushes once per silent period.
//
// The overseer is a person-like role, not a supervised process: nothing here
// restarts, kills or unhooks anything, and nothing reads the heartbeat except
// to report it. The polecat heartbeat (internal/polecat/heartbeat.go) is a
// different file with different readers; its consumers reap, which is why this
// package does not share it (gt-3a6h6).
package overseer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/events"
)

// The heartbeat states the overseer reports with gt overseer beat.
const (
	StateWorking = "working"
	StateIdle    = "idle"
	StateAway    = "away"
)

// States lists the heartbeat states, in the order the command prints them.
func States() []string { return []string{StateWorking, StateIdle, StateAway} }

// ValidState reports whether s is a heartbeat state.
func ValidState(s string) bool { return slices.Contains(States(), s) }

// The journal kinds: what an intervention was, closed so the weekly count is a
// backlog of things to automate and not a pile of free-text labels.
const (
	KindFileBead   = "file_bead"
	KindReview     = "review"
	KindConfigEdit = "config_edit"
	KindHold       = "hold"
	KindInstall    = "install"
	KindNukeSeat   = "nuke_seat"
	KindAnswer     = "answer"
	KindDecision   = "decision"
	KindFixByHand  = "fix_by_hand"
)

// Kinds lists the journal kinds, in the order a refusal names them.
func Kinds() []string {
	return []string{
		KindFileBead, KindReview, KindConfigEdit, KindHold, KindInstall,
		KindNukeSeat, KindAnswer, KindDecision, KindFixByHand,
	}
}

// ValidKind reports whether k is a journal kind.
func ValidKind(k string) bool { return slices.Contains(Kinds(), k) }

// MaxTextRunes is how long a journal line's text may be; longer text is cut.
const MaxTextRunes = 300

// The journal's retention. The panel reads seven days; the rest is history for
// a person reading the file.
const (
	JournalMaxAge   = 90 * 24 * time.Hour
	JournalMaxBytes = 1 << 20
)

// Dir is the directory the overseer's files live in.
func Dir(townRoot string) string { return filepath.Join(townRoot, ".runtime", "overseer") }

// HeartbeatPath is heartbeat.json.
func HeartbeatPath(townRoot string) string { return filepath.Join(Dir(townRoot), "heartbeat.json") }

// JournalPath is journal.jsonl.
func JournalPath(townRoot string) string { return filepath.Join(Dir(townRoot), "journal.jsonl") }

// SilentPushPath is silent-push.json, the daemon's marker.
func SilentPushPath(townRoot string) string {
	return filepath.Join(Dir(townRoot), "silent-push.json")
}

// Heartbeat is heartbeat.json: when the overseer last said it was active, and
// what it was doing.
type Heartbeat struct {
	TS      time.Time `json:"ts"`
	State   string    `json:"state"`
	Session string    `json:"session,omitempty"`
}

// WriteHeartbeat replaces heartbeat.json atomically.
func WriteHeartbeat(townRoot string, hb Heartbeat) error {
	if !ValidState(hb.State) {
		return fmt.Errorf("unknown state %q; allowed: %s", hb.State, strings.Join(States(), ", "))
	}
	return atomicfile.EnsureDirAndWriteJSON(HeartbeatPath(townRoot), hb)
}

// ReadHeartbeat reads heartbeat.json. A missing file is (zero, false, nil): the
// overseer has not beaten yet. A file that does not parse is an error, so a
// reader never takes a corrupt heartbeat for a silent overseer.
func ReadHeartbeat(townRoot string) (Heartbeat, bool, error) {
	data, err := os.ReadFile(HeartbeatPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return Heartbeat{}, false, nil
	}
	if err != nil {
		return Heartbeat{}, false, err
	}
	var hb Heartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return Heartbeat{}, false, fmt.Errorf("parsing %s: %w", HeartbeatPath(townRoot), err)
	}
	return hb, true, nil
}

// Silent reports whether the overseer has been quiet longer than after. A
// town with no heartbeat at all is silent: nobody has said they are looking.
func Silent(hb Heartbeat, have bool, now time.Time, after time.Duration) bool {
	return !have || now.Sub(hb.TS) > after
}

// Entry is one journal line.
type Entry struct {
	TS   time.Time `json:"ts"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
	Bead string    `json:"bead,omitempty"`
}

// CleanText collapses s to one line and cuts it to MaxTextRunes.
func CleanText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > MaxTextRunes {
		return string(r[:MaxTextRunes])
	}
	return s
}

// AppendEntry appends e to the journal as one line, in a single write under the
// lock events.Prune takes, then prunes the journal the way the events log is.
// The kind must be one of Kinds, and the text and bead are cleaned.
func AppendEntry(townRoot string, e Entry) error {
	if !ValidKind(e.Kind) {
		return fmt.Errorf("unknown kind %q; allowed: %s", e.Kind, strings.Join(Kinds(), ", "))
	}
	e.Text = CleanText(e.Text)
	if e.Text == "" {
		return errors.New("the text is empty")
	}
	e.Bead = strings.Join(strings.Fields(e.Bead), " ")
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	path := JournalPath(townRoot)
	if err := os.MkdirAll(Dir(townRoot), 0o755); err != nil {
		return err
	}
	if err := appendLocked(path, line); err != nil {
		return err
	}
	// The line is already written; a prune that fails or finds the lock busy
	// leaves the file longer for one more call, and the next append tries again.
	_, _ = events.Prune(path, events.PruneOptions{MaxAge: JournalMaxAge, MaxBytes: JournalMaxBytes}, e.TS)
	return nil
}

func appendLocked(path string, line []byte) error {
	fl := flock.New(path + ".lock")
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("locking the journal: %w", err)
	}
	defer fl.Unlock()                                                       //nolint:errcheck // best-effort unlock
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: a path built from the town root
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadJournal returns the journal oldest first. A line that does not parse is
// skipped: a writer killed mid-write leaves one, and losing it beats failing
// the read that lists the rest. A missing journal is empty.
func ReadJournal(townRoot string) ([]Entry, error) {
	f, err := os.Open(JournalPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind == "" || e.TS.IsZero() {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Recent returns the newest n entries, newest first.
func Recent(entries []Entry, n int) []Entry {
	out := make([]Entry, 0, n)
	for i := len(entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, entries[i])
	}
	return out
}

// KindCount is how many interventions of one kind the journal holds.
type KindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// CountByKind counts the entries within window of now, by kind, most frequent
// first and ties by kind name. A kind with no entries is left out.
func CountByKind(entries []Entry, now time.Time, window time.Duration) []KindCount {
	cutoff := now.Add(-window)
	counts := map[string]int{}
	for _, e := range entries {
		if e.TS.After(cutoff) {
			counts[e.Kind]++
		}
	}
	out := make([]KindCount, 0, len(counts))
	for k, n := range counts {
		out = append(out, KindCount{Kind: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// SilentPush is silent-push.json: the heartbeat a silent-overseer push was
// last sent for. Heartbeat is zero when the overseer had never beaten.
type SilentPush struct {
	Heartbeat time.Time `json:"heartbeat"`
	PushedAt  time.Time `json:"pushed_at"`
}

// ReadSilentPush reads the marker. A missing file is (zero, false, nil).
func ReadSilentPush(townRoot string) (SilentPush, bool, error) {
	data, err := os.ReadFile(SilentPushPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return SilentPush{}, false, nil
	}
	if err != nil {
		return SilentPush{}, false, err
	}
	var p SilentPush
	if err := json.Unmarshal(data, &p); err != nil {
		return SilentPush{}, false, fmt.Errorf("parsing %s: %w", SilentPushPath(townRoot), err)
	}
	return p, true, nil
}

// WriteSilentPush replaces the marker atomically.
func WriteSilentPush(townRoot string, p SilentPush) error {
	return atomicfile.EnsureDirAndWriteJSON(SilentPushPath(townRoot), p)
}
