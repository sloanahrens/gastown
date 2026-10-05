package land

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// BackoffRecord is one bead whose landing is failing and waiting for its next
// try: the stage the last attempt failed at, how many attempts have failed in
// a row, when the worker will try again, and the last error's first line. The
// landing worker writes it into the rig's backoff file so a landing that keeps
// failing is visible in the dashboard and in town health, not only in
// daemon.log (gt-fn9e6.44).
type BackoffRecord struct {
	BeadID string `json:"bead"`
	// Stage is the landing stage the last attempt failed at, the text after
	// "landing failed at" (push, ci, review, merge, ...).
	Stage string `json:"stage"`
	// Failures is the run of consecutive failures behind the worker's backoff.
	Failures int `json:"failures"`
	// NextTry is when the worker may try the bead again.
	NextTry time.Time `json:"next_try"`
	// Error is the last error's first line.
	Error string `json:"error"`
}

// BackoffState is one rig's landing backoff as of At: every bead whose landing
// failed and is waiting for a retry. An empty Beads is a healthy snapshot, and
// is what tells a reader the file is a live one with nothing failing rather
// than a file from before the worker ever wrote.
type BackoffState struct {
	Rig   string          `json:"rig"`
	At    time.Time       `json:"at"`
	Beads []BackoffRecord `json:"beads"`
}

// BackoffFile is <town>/.runtime/landings/<rig>.backoff.json, the snapshot the
// landing worker rewrites after each pass. It sits beside the rig's landings
// file and is never one of its records: the landings file answers "who landed
// what", and a failure is not a landing.
type BackoffFile struct {
	Path string
}

// RigBackoffFile is the backoff file of one rig. A rig name that could leave
// the directory is refused.
func RigBackoffFile(townRoot, rig string) (*BackoffFile, error) {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
		return nil, fmt.Errorf("invalid rig name %q for a backoff file", rig)
	}
	return &BackoffFile{Path: filepath.Join(constants.TownRuntimePath(townRoot), "landings", rig+".backoff.json")}, nil
}

// Write replaces the file with st: a reader sees either the previous snapshot
// or this one, never a half-written one. An empty state is written rather than
// the file being removed, so "nothing is failing" reads differently from "no
// worker is writing".
func (f *BackoffFile) Write(st BackoffState) error {
	if st.Beads == nil {
		st.Beads = []BackoffRecord{}
	}
	dir := filepath.Dir(f.Path)
	if err := ensureLandingsDir(dir); err != nil {
		return err
	}
	body, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encoding backoff state: %w", err)
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(dir, filepath.Base(f.Path)+".tmp")
	if err != nil {
		return fmt.Errorf("creating backoff temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting backoff file mode: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing backoff state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing backoff state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing backoff file: %w", err)
	}
	if err := os.Rename(tmpName, f.Path); err != nil {
		return fmt.Errorf("replacing backoff file: %w", err)
	}
	return nil
}

// ensureLandingsDir makes dir if needed and refuses one another account could
// write to, so no one can forge the records the landing worker files there.
func ensureLandingsDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating landings dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("checking landings dir: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("refusing landings dir %s: mode %v is not a private directory", dir, info.Mode())
	}
	return checkOwner(dir, info)
}
