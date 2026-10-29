package crew

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/seattoken"
)

// trackerEnv is the environment a crew session gets when its rig is on the
// tracker: the tracker's URL and the path of the crew member's own token file.
// The token enters neither argv nor the environment, only the file it names. A
// rig on Dolt gets nothing here, so its environment is unchanged (gt-p85pw.83).
func (m *Manager) trackerEnv(townRoot, name string) (map[string]string, error) {
	onTracker, err := rigOnTracker(townRoot, m.rig.Name)
	if err != nil {
		return nil, fmt.Errorf("crew %s: %w", name, err)
	}
	if !onTracker {
		return nil, nil
	}
	// The endpoint is resolved before a token is minted, so a broken town
	// config issues no token that nothing would use.
	ep, err := config.ResolveTrackerEndpoint(townRoot)
	if err != nil {
		return nil, fmt.Errorf("crew %s: tracker endpoint: %w", name, err)
	}
	issuer := m.trackerIssuer
	if issuer == nil {
		host, err := seattoken.NewHostIssuer(townRoot)
		if err != nil {
			return nil, fmt.Errorf("crew %s: tracker token issuer: %w", name, err)
		}
		issuer = host
	}
	seat := intent.Seat{Rig: m.rig.Name, Role: "crew", Name: name}
	path, err := seattoken.New(townRoot).Ensure(context.Background(), seat, issuer)
	if err != nil {
		return nil, fmt.Errorf("crew %s: tracker token: %w", name, err)
	}
	return map[string]string{
		"TRACKER_URL":        ep.URL(),
		"TRACKER_TOKEN_FILE": path,
	}, nil
}

// rigOnTracker reports whether the town's route for rigName names the tracker
// backend. A route belongs to a rig by its first path segment, as
// beads.GetPrefixForRig reads it. A rig with no route is on Dolt. An unreadable
// route table is an error, never "Dolt": a tracker rig must not start on a
// session that would then write to the wrong store.
func rigOnTracker(townRoot, rigName string) (bool, error) {
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return false, fmt.Errorf("reading the route table: %w", err)
	}
	for _, r := range routes {
		first, _, _ := strings.Cut(r.Path, "/")
		if first == rigName {
			return r.BackendName() == beads.BackendTracker, nil
		}
	}
	return false, nil
}
