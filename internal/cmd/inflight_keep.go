package cmd

import (
	"sync"

	"github.com/steveyegge/gastown/internal/townflight"
)

// keptInFlight holds every Register release for the life of the process: the
// release owns the entry's lock, and a collected file would close it early.
var keptInFlight struct {
	mu       sync.Mutex
	releases []func() error
}

// keepInFlight lists label as in flight in townRoot until the process exits,
// and returns Register's error.
func keepInFlight(townRoot, label string) error {
	release, err := townflight.Register(townRoot, label)
	if err != nil {
		return err
	}
	keptInFlight.mu.Lock()
	defer keptInFlight.mu.Unlock()
	keptInFlight.releases = append(keptInFlight.releases, release)
	return nil
}
