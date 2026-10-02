package attention

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/lock"
)

// The three files the queue lives in, under <town>/.runtime/attention/.
const (
	// DirName is the directory under the town runtime path.
	DirName = "attention"
	// StateFileName is the current set, rewritten by the daemon each tick.
	StateFileName = "state.json"
	// EventsFileName is the append-only transition log.
	EventsFileName = "events.jsonl"
	// EventsRotatedName is where events.jsonl is rotated at EventsMaxBytes.
	EventsRotatedName = "events.jsonl.1"
	// AcksFileName is the acknowledged keys, written only by gt attention ack.
	AcksFileName = "acks.json"
	// LockFileName is the flock acks.json is written under.
	LockFileName = "acks.lock"
)

// EventsMaxBytes is the size events.jsonl may reach before AppendEvents
// rotates it to events.jsonl.1.
const EventsMaxBytes = 1 << 20

// Dir is <town>/.runtime/attention/, the one directory the queue lives in.
func Dir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), DirName)
}

// StatePath is state.json: the current set, written by the daemon each tick
// and read by gt attention.
func StatePath(townRoot string) string { return filepath.Join(Dir(townRoot), StateFileName) }

// EventsPath is events.jsonl: the append-only transition log.
func EventsPath(townRoot string) string { return filepath.Join(Dir(townRoot), EventsFileName) }

// RotatedEventsPath is events.jsonl.1: the previous log, kept for one rotation.
func RotatedEventsPath(townRoot string) string {
	return filepath.Join(Dir(townRoot), EventsRotatedName)
}

// AcksPath is acks.json: the acknowledged keys.
func AcksPath(townRoot string) string { return filepath.Join(Dir(townRoot), AcksFileName) }

// ReadState loads state.json. A missing file reads as an empty state with no
// error, so a town whose daemon has not written yet still works.
func ReadState(townRoot string) (State, error) {
	var s State
	data, err := os.ReadFile(StatePath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse %s: %w", StateFileName, err)
	}
	return s, nil
}

// WriteState replaces state.json with s, atomically.
func WriteState(townRoot string, s State) error {
	if s.Items == nil {
		s.Items = []Item{}
	}
	return atomicfile.EnsureDirAndWriteJSON(StatePath(townRoot), s)
}

// AppendEvents appends each event as one JSON line to events.jsonl, rotating
// the file to events.jsonl.1 first once it has reached EventsMaxBytes.
func AppendEvents(townRoot string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	if err := os.MkdirAll(Dir(townRoot), 0o755); err != nil {
		return err
	}
	path := EventsPath(townRoot)
	rotated := RotatedEventsPath(townRoot)
	for _, e := range events {
		if err := rotateIfFull(path, rotated, EventsMaxBytes); err != nil {
			return err
		}
		data, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encoding event %s: %w", e.Key, err)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: a path built from the town root
		if err != nil {
			return err
		}
		_, werr := f.Write(append(data, '\n'))
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// rotateIfFull renames path to rotated when it has reached max bytes. A file
// that does not exist, or is smaller, is left alone.
func rotateIfFull(path, rotated string, max int64) error {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Size() < max {
		return nil
	}
	return os.Rename(path, rotated)
}

// ReadEvents returns every transition in events.jsonl, oldest first. A line
// that does not parse is skipped: a process killed mid-write leaves one, and
// losing that line beats failing the read that lists the rest.
func ReadEvents(townRoot string) ([]Event, error) {
	f, err := os.Open(EventsPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

// ReadAcks loads acks.json. A missing file reads as no acks with no error.
func ReadAcks(townRoot string) (Acks, error) {
	var a Acks
	data, err := os.ReadFile(AcksPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return a, fmt.Errorf("parse %s: %w", AcksFileName, err)
	}
	return a, nil
}

// WriteAcks replaces acks.json with acks, under the flock so it cannot race
// gt attention ack.
func WriteAcks(townRoot string, acks Acks) error {
	unlock, err := lockAcks(townRoot)
	if err != nil {
		return err
	}
	defer unlock()
	return writeAcksLocked(townRoot, acks)
}

// Acknowledge records key as acknowledged at at, under the flock. The read
// and the write happen inside the lock, so two concurrent acks both survive.
// A key that is already acked is left with its original time.
func Acknowledge(townRoot, key string, at time.Time) error {
	if key == "" {
		return errors.New("ack needs a key")
	}
	unlock, err := lockAcks(townRoot)
	if err != nil {
		return err
	}
	defer unlock()
	acks, err := ReadAcks(townRoot)
	if err != nil {
		return err
	}
	if AckedKey(acks, key) {
		return nil
	}
	acks.Acks = append(acks.Acks, Ack{Key: key, At: at})
	return writeAcksLocked(townRoot, acks)
}

// lockAcks creates the queue directory and takes the acks flock. The caller
// must call the returned function to release it.
func lockAcks(townRoot string) (func(), error) {
	if err := os.MkdirAll(Dir(townRoot), 0o755); err != nil {
		return nil, err
	}
	return lock.FlockAcquire(filepath.Join(Dir(townRoot), LockFileName))
}

// writeAcksLocked writes acks.json atomically, ordered by key. The caller
// holds the flock.
func writeAcksLocked(townRoot string, acks Acks) error {
	if acks.Acks == nil {
		acks.Acks = []Ack{}
	}
	sort.SliceStable(acks.Acks, func(i, j int) bool { return acks.Acks[i].Key < acks.Acks[j].Key })
	return atomicfile.EnsureDirAndWriteJSON(AcksPath(townRoot), acks)
}
