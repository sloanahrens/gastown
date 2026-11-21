package bdgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// maxCallReadBytes bounds what ReadCalls reads from each live log: its tail,
// where the dashboard's recent window lives. A full 50 MB file is never loaded
// into the gateway's memory.
const maxCallReadBytes int64 = 8 << 20

// CallEntry is one call the log recorded: exactly the fields Log writes, so a
// flag's value, a title or a comment body cannot appear in an entry the
// dashboard serves.
type CallEntry struct {
	TS     string `json:"ts"`
	Class  string `json:"class"`
	Caller string `json:"caller"`
	// Actor is the acting agent the caller named in BEADS_ACTOR, or BD_ACTOR
	// when that is empty. A call naming neither logs no actor.
	Actor string `json:"actor,omitempty"`
	Verb  string `json:"verb"`
	// Write reports whether the verb is one that may change beads, so a line
	// says whether the call wrote without the reader knowing the policy. A line
	// written before the key existed decodes to nil: its write is unknown, not
	// a read.
	Write      *bool    `json:"write"`
	Flags      []string `json:"flags"`
	Beads      []string `json:"beads"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	Error      string   `json:"error,omitempty"`
	// Refusal is the reason code of a call the gateway refused before exec. A
	// call that ran carries none.
	Refusal string `json:"refusal,omitempty"`
	// RefusedFlag is the flag an unplaceable argv was refused for, as the scan
	// spells it (--version, -x) and never its value. Only an ambiguous-flag
	// refusal carries one, and the line omits it when empty.
	RefusedFlag string `json:"refused_flag,omitempty"`
	Conflict    bool   `json:"conflict,omitempty"`
}

// stamped is one parsed call log line with its stamp, the key ReadCalls merges on.
type stamped struct {
	entry CallEntry
	ts    time.Time
}

// ReadCalls returns the call log entries stamped after since, oldest first, at
// most limit. dir holds the live call.log and, under --log-per-pod, one
// subdirectory per pod with its own call.log. Every such file is read as its
// last maxCallReadBytes, never a rotated one; the entries of all of them are
// merged by stamp, and since and limit apply to the merged list. A missing log
// is no entries and no error. A pod log that cannot be read is an error that
// names its directory: skipping it would show a partial window as the whole.
func ReadCalls(dir string, since time.Time, limit int) ([]CallEntry, error) {
	return readCalls(dir, since, limit, os.ReadDir)
}

// readCalls is ReadCalls with the directory listing injected: a real directory
// cannot fail a listing without permissions a test cannot set portably, so a
// test passes a listing that fails.
func readCalls(dir string, since time.Time, limit int, readDir func(string) ([]os.DirEntry, error)) ([]CallEntry, error) {
	if dir == "" {
		return nil, errors.New("bdgateway: read calls needs a log directory")
	}
	if limit <= 0 {
		return nil, errors.New("bdgateway: read calls needs a positive limit")
	}
	kept, err := readCallFile(filepath.Join(dir, DefaultCallLogName))
	if err != nil {
		return nil, err
	}
	pods, err := podLogDirs(dir, readDir)
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		entries, err := readCallFile(filepath.Join(pod, DefaultCallLogName))
		if err != nil {
			return nil, fmt.Errorf("bdgateway: read pod call log in %s: %w", pod, err)
		}
		kept = append(kept, entries...)
	}
	kept = slices.DeleteFunc(kept, func(s stamped) bool { return !s.ts.After(since) })
	// Log stamps a call before it queues the line, and each pod writes its own
	// file, so the merged list can be out of stamp order. The stable sort keeps
	// the root log's lines ahead of a pod's on a tie, and the pods in name order.
	slices.SortStableFunc(kept, func(a, b stamped) int { return a.ts.Compare(b.ts) })
	if len(kept) > limit {
		kept = kept[:limit]
	}
	out := make([]CallEntry, len(kept))
	for i, s := range kept {
		out[i] = s.entry
	}
	return out, nil
}

// podLogDirs lists dir's subdirectories, the per-pod directories --log-per-pod
// writes, in name order. A missing dir has none; any other listing failure is
// an error, never an empty list.
func podLogDirs(dir string, readDir func(string) ([]os.DirEntry, error)) ([]string, error) {
	entries, err := readDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bdgateway: list call log directory: %w", err)
	}
	var pods []string
	for _, e := range entries {
		if e.IsDir() {
			pods = append(pods, filepath.Join(dir, e.Name()))
		}
	}
	return pods, nil
}

// readCallFile parses the entries of one call log file, each with its stamp, in
// file order. A missing file is no entries and no error. A torn line, or one
// whose stamp does not parse, is skipped: it cannot be placed in the merge.
func readCallFile(path string) ([]stamped, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bdgateway: open call log: %w", err)
	}
	data, readErr := readCallLogTail(f)
	if closeErr := f.Close(); readErr == nil && closeErr != nil {
		readErr = fmt.Errorf("bdgateway: close call log: %w", closeErr)
	}
	if readErr != nil {
		return nil, readErr
	}

	var out []stamped
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e CallEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// A torn line is one crash wide; the lines around it still read, as
			// the landings file's reader does (gt-jr90x).
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			// Without a stamp the entry cannot be placed in the merge.
			continue
		}
		out = append(out, stamped{entry: e, ts: ts})
	}
	return out, nil
}

// callLogFile is the part of *os.File readCallLogTail uses; a test substitutes
// one whose calls fail to drive each error path.
type callLogFile interface {
	Stat() (os.FileInfo, error)
	Seek(offset int64, whence int) (int64, error)
	Read(p []byte) (int, error)
}

// readCallLogTail reads at most the last maxCallReadBytes of f, plus the byte
// before them. When the read starts inside a line, the fragment up to its first
// newline is dropped: a partial line is not an entry. When that byte is a
// newline, the window opens on a whole line and nothing is dropped.
func readCallLogTail(f callLogFile) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("bdgateway: measure call log: %w", err)
	}
	offset := max(info.Size()-maxCallReadBytes, 0)
	lead := min(offset, 1)
	if _, err := f.Seek(offset-lead, io.SeekStart); err != nil {
		return nil, fmt.Errorf("bdgateway: seek call log: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCallReadBytes+lead))
	if err != nil {
		return nil, fmt.Errorf("bdgateway: read call log: %w", err)
	}
	if lead == 0 {
		return data, nil
	}
	if len(data) == 0 {
		// The log shrank after it was measured; there is no tail left to read.
		return nil, nil
	}
	if data[0] == '\n' {
		return data[1:], nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return data[i+1:], nil
	}
	return nil, nil
}
