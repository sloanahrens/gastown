package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/seattoken"
)

// crewRevoker records the actors it was asked to revoke, and fails as set.
type crewRevoker struct {
	err    error
	actors []string
}

func (r *crewRevoker) Revoke(_ context.Context, actor string) (int, error) {
	r.actors = append(r.actors, actor)
	if r.err != nil {
		return 0, r.err
	}
	return 1, nil
}

// crewTokenIssuer hands out one token, for the file a test seeds.
type crewTokenIssuer struct{}

func (crewTokenIssuer) Issue(context.Context, string) (string, error) {
	return "trk_crew-token-value", nil
}

// crewRemoveTown builds the rig crew remove runs against: a crew workspace, a
// route table putting the rig on the tracker, and the member's token file.
func crewRemoveTown(t *testing.T) (rigPath, crewPath, tokenPath string) {
	t.Helper()
	rigPath = t.TempDir()
	crewPath = filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewPath, 0o755); err != nil {
		t.Fatalf("mkdir crew workspace: %v", err)
	}
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"gt-","path":"gastown/mayor/rig","backend":"tracker"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}
	seat := seattoken.CrewSeat("gastown", "max")
	tokenPath, err := seattoken.New(rigPath).Ensure(context.Background(), seat, crewTokenIssuer{})
	if err != nil {
		t.Fatalf("seeding the member's token file: %v", err)
	}
	return rigPath, crewPath, tokenPath
}

// crewRemoveEnvFor is the env crew remove runs with: no live session, no git,
// and the token revoker the case supplies.
func crewRemoveEnvFor(rigPath string, revoker seattoken.Revoker, warnings *[]string) crewRemoveEnv {
	r := &rig.Rig{Name: "gastown", Path: rigPath}
	return crewRemoveEnv{
		member: func(string, string) (*crew.Manager, *rig.Rig, error) {
			return crew.NewManager(r, nil, nil), r, nil
		},
		hasSession: func(string) (bool, error) { return false, nil },
		revoke: func(ctx context.Context, townRoot string, seat intent.Seat) error {
			return seattoken.RevokeSeat(ctx, townRoot, seat, revoker)
		},
		warn: func(format string, args ...any) {
			*warnings = append(*warnings, fmt.Sprintf(format, args...))
		},
		store: func(string) beads.Client { return beadsfake.New() },
	}
}

// gt crew remove ends the member's tracker token once the workspace is gone.
func TestCrewRemoveRevokesTheMemberSeat(t *testing.T) {
	t.Parallel()
	rigPath, crewPath, tokenPath := crewRemoveTown(t)
	revoker := &crewRevoker{}
	var warnings []string

	err := runCrewRemoveWith([]string{"max"}, "gastown", true, false, crewRemoveEnvFor(rigPath, revoker, &warnings))

	if err != nil {
		t.Fatalf("crew remove = %v, want nil", err)
	}
	if _, statErr := os.Stat(crewPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("crew workspace stat = %v, want it gone", statErr)
	}
	if want := []string{"gastown/crew/max"}; !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v", revoker.actors, want)
	}
	if _, statErr := os.Stat(tokenPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("token file stat = %v, want it gone", statErr)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none", warnings)
	}
}

// A failed revoke warns naming the seat and leaves the removal a success: the
// workspace is already gone.
func TestCrewRemoveRevokeFailureWarnsAndRemoves(t *testing.T) {
	t.Parallel()
	rigPath, crewPath, _ := crewRemoveTown(t)
	revoker := &crewRevoker{err: errors.New("the tracker is down")}
	var warnings []string

	err := runCrewRemoveWith([]string{"max"}, "gastown", true, false, crewRemoveEnvFor(rigPath, revoker, &warnings))

	if err != nil {
		t.Fatalf("crew remove = %v, want the warning to leave the removal a success", err)
	}
	if _, statErr := os.Stat(crewPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("crew workspace stat = %v, want it gone", statErr)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "gastown/crew/max") {
		t.Errorf("warnings = %q, want one naming the seat", warnings)
	}
}
