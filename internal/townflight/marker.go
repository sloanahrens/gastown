package townflight

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrDraining reports that the daemon is shutting down and Register refused to start the command.
var ErrDraining = errors.New("daemon is shutting down")

func drainMarkerPath(townRoot string) string { return filepath.Join(townRoot, "daemon", "draining") }

// StartDrain marks townRoot as draining, so Register refuses new commands.
func StartDrain(townRoot string) error {
	path := drainMarkerPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating daemon directory: %w", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("writing drain marker: %w", err)
	}
	return nil
}

// ClearDrain removes townRoot's drain marker; a town with no marker is not draining.
func ClearDrain(townRoot string) error {
	if err := os.Remove(drainMarkerPath(townRoot)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing drain marker: %w", err)
	}
	return nil
}
