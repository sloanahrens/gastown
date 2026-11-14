// Package estlog writes the estimate log: one JSON line per estimate, holding
// the numbers the estimator produced beside the hash of the text it read, so a
// retrain fits on what the estimator saw after a bead's text has changed.
package estlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ErrNoTownRoot reports a call with no town root, which names no log.
var ErrNoTownRoot = errors.New("storypoints/estlog: needs a town root")

// Path returns the estimate log file of townRoot,
// <townRoot>/.runtime/storypoints/estimates.jsonl; an empty town root names no
// file.
func Path(townRoot string) string {
	if townRoot == "" {
		return ""
	}
	return filepath.Join(townRoot, ".runtime", "storypoints", "estimates.jsonl")
}

// Record is one estimate as the log keeps it.
type Record struct {
	// BeadID is the bead the estimate was made for.
	BeadID string `json:"bead_id"`
	// DescriptionHash is the hash of the text the estimate read, so a retrain
	// joins a landing to the features the bead had when it was estimated.
	DescriptionHash string `json:"description_hash"`
	// Shape holds the bead's twelve shape features.
	Shape Shape `json:"shape"`
	// Embedding is the vector the embedding ridge read; nil when the estimate
	// had no embedding input.
	Embedding []float32 `json:"embedding"`
	// Minutes holds each component's predicted minutes.
	Minutes Minutes `json:"minutes"`
	// BlendedMinutes is the blend of the components before it was rounded to a
	// point.
	BlendedMinutes float64 `json:"blended_minutes"`
	// WeightsVersion names the weights the estimate used, as
	// storypoints.Weights.Version names them.
	WeightsVersion string `json:"weights_version"`
	// Time is when the estimate was made.
	Time time.Time `json:"time"`
}

// Shape holds the twelve shape features under the names the weights file uses.
// It is the log's own copy: the file format outlives the in-memory
// storypoints.Shape, whose fields carry no JSON names, so the names a reader of
// the log decodes are fixed here.
type Shape struct {
	Boxes   float64 `json:"boxes"`
	Paths   float64 `json:"paths"`
	Pkgs    float64 `json:"pkgs"`
	DLen    float64 `json:"dlen"`
	NCons   float64 `json:"ncons"`
	NLab    float64 `json:"nlab"`
	PodEng  float64 `json:"podeng"`
	Tests   float64 `json:"tests"`
	NBt     float64 `json:"nbt"`
	RigGT   float64 `json:"rig_gt"`
	RigDV   float64 `json:"rig_dv"`
	IsHaiku float64 `json:"is_haiku"`
}

// Minutes holds each component's predicted minutes; a nil field is a component
// the estimate did not have.
type Minutes struct {
	Shape *float64 `json:"shape,omitempty"`
	Embed *float64 `json:"embed,omitempty"`
	Jev   *float64 `json:"jev,omitempty"`
}

// Append writes r to the estimate log of townRoot as one JSON line, creating
// the file and the directory that holds it. Each call is one write to a file
// opened for append, so two writers may append at once without a line being
// torn or interleaved.
func Append(townRoot string, r Record) error {
	return appendRecord(osLogFS{}, townRoot, r)
}

// logFS is the two filesystem calls an append makes: the directory the log
// lives in, and the append-only handle the line goes through. Production passes
// osLogFS; a test passes an in-memory fake, so every failure branch runs
// without a disk.
type logFS interface {
	// MkdirAll creates dir and the parents it needs.
	MkdirAll(dir string, perm os.FileMode) error
	// OpenAppend opens path for appending, creating the file.
	OpenAppend(path string) (io.WriteCloser, error)
}

// osLogFS is the host filesystem.
type osLogFS struct{}

// MkdirAll creates dir and the parents it needs.
func (osLogFS) MkdirAll(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) }

// OpenAppend opens path for appending, creating it.
func (osLogFS) OpenAppend(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}

// appendRecord is Append through fsys, so a test can drive the directory, open,
// write, short-write and close failures a healthy host does not offer.
func appendRecord(fsys logFS, townRoot string, r Record) error {
	path := Path(townRoot)
	if path == "" {
		return ErrNoTownRoot
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("storypoints/estlog: encoding the record for %s: %w", path, err)
	}
	line = append(line, '\n')
	if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("storypoints/estlog: creating the directory for %s: %w", path, err)
	}
	f, err := fsys.OpenAppend(path)
	if err != nil {
		return fmt.Errorf("storypoints/estlog: opening %s: %w", path, err)
	}
	n, writeErr := f.Write(line)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("storypoints/estlog: writing %s: %w", path, errors.Join(writeErr, closeErr))
	}
	if n != len(line) {
		short := fmt.Errorf("short write (%d of %d bytes)", n, len(line))
		return fmt.Errorf("storypoints/estlog: writing %s: %w", path, errors.Join(short, closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("storypoints/estlog: closing %s: %w", path, closeErr)
	}
	return nil
}
