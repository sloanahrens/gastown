package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// LiftRequest is the operator's request that the daemon lift the red hold.
type LiftRequest struct {
	Reason string    `json:"reason"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// LiftRefusal is the daemon's answer to a lift request it did not grant: the
// red hold kinds still holding automatic dispatch when it looked.
type LiftRefusal struct {
	At    time.Time `json:"at"`
	Still []string  `json:"still"`
}

// RedHoldLiftPath returns the lift request file for a town, under its daemon directory.
func RedHoldLiftPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "red-hold.lift")
}

// RedHoldRefusalPath returns the lift refusal file for a town, under its daemon directory.
func RedHoldRefusalPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "red-hold.lift-refused")
}

// WriteLift publishes r atomically.
func WriteLift(townRoot string, r LiftRequest) error {
	return atomicfile.EnsureDirAndWriteJSON(RedHoldLiftPath(townRoot), r)
}

// ReadLift reads the published request, reporting false when the file is
// absent and an error when it cannot be read or parsed as JSON.
func ReadLift(townRoot string) (LiftRequest, bool, error) {
	return readLiftFile[LiftRequest](RedHoldLiftPath(townRoot))
}

// ClearLift removes the request; a missing file is not an error.
func ClearLift(townRoot string) error { return clearLiftFile(RedHoldLiftPath(townRoot)) }

// WriteRefusal publishes r atomically.
func WriteRefusal(townRoot string, r LiftRefusal) error {
	return atomicfile.EnsureDirAndWriteJSON(RedHoldRefusalPath(townRoot), r)
}

// ReadRefusal reads the published refusal as ReadLift reads the request.
func ReadRefusal(townRoot string) (LiftRefusal, bool, error) {
	return readLiftFile[LiftRefusal](RedHoldRefusalPath(townRoot))
}

// ClearRefusal removes the refusal; a missing file is not an error.
func ClearRefusal(townRoot string) error { return clearLiftFile(RedHoldRefusalPath(townRoot)) }

func readLiftFile[T any](path string) (T, bool, error) {
	var v T
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	return v, true, json.Unmarshal(data, &v)
}

func clearLiftFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
