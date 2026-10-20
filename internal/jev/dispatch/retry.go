package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
)

// maxAttempts is how many passes may fail to get an answer for one placement
// before the pass gives up on it. It bounds the cost of a bead Jev cannot
// answer while leaving room for a transient failure, so the queue never
// blocks behind one bead and no bead is dropped on its first failure.
const maxAttempts = 3

// retries is the retry queue: how many passes have failed to get an answer for
// each placement, keyed by retryKey.
type retries map[string]int

// retryKey names one placement in the retry queue. The stamp is part of the
// key, so a bead slung again is a new placement with no failures behind it.
func retryKey(id string, at time.Time) string {
	return id + "@" + at.UTC().Format(time.RFC3339Nano)
}

// readRetries returns the queue at path. A missing file is an empty queue.
func readRetries(path string) (retries, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return retries{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the retry queue: %w", err)
	}
	var q retries
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, fmt.Errorf("parsing the retry queue %s: %w", path, err)
	}
	if q == nil {
		q = retries{}
	}
	return q, nil
}

// writeRetries saves the queue through a temporary file, so a crash never
// leaves a half-written queue. An empty queue removes the file.
func writeRetries(path string, q retries) error {
	if len(q) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing the retry queue: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(q)
	if err != nil {
		return fmt.Errorf("encoding the retry queue: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the retry directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing the retry queue: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving the retry queue: %w", err)
	}
	return nil
}

// beadFault reports whether err blames the bead's request rather than the
// pass's reach: Jev refused this request (a 4xx other than 401) or accepted it
// and answered unusably. Only a bead fault counts an attempt against the bead;
// any other failure — a timeout, a lost connection, a 429 that outlived its
// retry, a 401, a 5xx — stops the pass where it is, so an outage costs no
// attempts and drops nothing.
func beadFault(err error) bool {
	var se *jev.StatusError
	if errors.As(err, &se) {
		return se.Status >= 400 && se.Status < 500 && se.Status != http.StatusUnauthorized
	}
	return errors.Is(err, jev.ErrBadResponse)
}
