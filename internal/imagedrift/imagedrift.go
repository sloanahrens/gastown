// Package imagedrift is the marker a sling leaves when the sandbox image lags
// the installed gt (session.ErrImageDrift), so the slings behind it, in the
// same process or in the background processes the daemon starts, do not each
// create and burn a molecule for the same refusal (gt-uisom).
//
// The marker is a file beside the install-pending marker. It holds the agent
// whose seat was refused and when, and it speaks for HoldFor: after that one
// sling tries again, which is how a rebuilt image is noticed.
package imagedrift

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/installpending"
)

// FileName is the marker's name in the install-pending directory.
const FileName = "image-drift.json"

// HoldFor is how long a refusal speaks for the slings behind it.
const HoldFor = 2 * time.Minute

// FutureSkew is how far past now a marker's time may lie before it is not
// trusted: two clocks disagreeing by less than this is not an old refusal.
const FutureSkew = time.Minute

// Marker is one refusal: the agent whose seat's image lags and when a sling
// was refused for it.
type Marker struct {
	At    time.Time `json:"at"`
	Agent string    `json:"agent"`
}

// Store is the marker file. The zero Store has no file: it records nothing
// and holds nothing.
type Store struct{ path string }

// StoreFor is the marker of townRoot's town.
func StoreFor(townRoot string) Store {
	return Store{path: filepath.Join(installpending.Dir(townRoot), FileName)}
}

// StoreIn is the marker at path.
func StoreIn(path string) Store { return Store{path: path} }

// Write records a refusal of agent's seat at at, replacing an earlier marker.
func (s Store) Write(at time.Time, agent string) error {
	if s.path == "" {
		return nil
	}
	if agent == "" {
		return errors.New("image-drift marker needs the refused agent")
	}
	return atomicfile.EnsureDirAndWriteJSON(s.path, Marker{At: at.UTC(), Agent: agent})
}

// Held reports whether a refusal of agent's seat within HoldFor of now stands,
// and the marker that says so. A missing marker, another agent's, an expired
// one and one dated more than FutureSkew ahead hold nothing. A file that
// cannot be read or parsed is an error, and a caller that only wants to avoid
// repeated work goes on without the hold.
func (s Store) Held(now time.Time, agent string) (bool, Marker, error) {
	if s.path == "" {
		return false, Marker{}, nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, Marker{}, nil
	}
	if err != nil {
		return false, Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return false, Marker{}, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	age := now.Sub(m.At)
	if m.Agent != agent || age >= HoldFor || age < -FutureSkew {
		return false, m, nil
	}
	return true, m, nil
}

// ClearFor removes the marker when it names agent, which is what a sling that
// reached a running session proves: that seat's image is current. Another
// agent's marker, and a missing one, stay as they are.
func (s Store) ClearFor(agent string) error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		// A marker nobody can read holds nothing; removing it is the repair.
		return s.remove()
	}
	if m.Agent != agent {
		return nil
	}
	return s.remove()
}

// remove deletes the marker file; one that is already gone is not an error.
func (s Store) remove() error {
	err := os.Remove(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
