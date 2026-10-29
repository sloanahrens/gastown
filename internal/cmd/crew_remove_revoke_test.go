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

func (r *crewRevoker) Revoke(_ context.Context, actor string) error {
	r.actors = append(r.actors, actor)
	return r.err
}

// crewTokenIssuer hands out one token, for the file a test seeds.
type crewTokenIssuer struct{}

func (crewTokenIssuer) Issue(context.Context, string) (string, error) {
	return "trk_crew-token-value", nil
}

// crewRemoveTown builds the town the way a town is laid out: a mayor/town.json
// marks the town root, the rig sits under it, and the town's route table puts
// the rig on the tracker. The member's workspace is in the rig, and its token
// file is in the town's runtime directory, where crew remove looks for it.
func crewRemoveTown(t *testing.T) (rigPath, crewPath, tokenPath string) {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath = filepath.Join(town, "gastown")
	crewPath = filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewPath, 0o755); err != nil {
		t.Fatalf("mkdir crew workspace: %v", err)
	}
	beadsDir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"gt-","path":"gastown/mayor/rig","backend":"tracker"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}
	seat := seattoken.CrewSeat("gastown", "max")
	tokenPath, err := seattoken.New(town).Ensure(context.Background(), seat, crewTokenIssuer{})
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

// A rig whose town root cannot be resolved is never read as a rig on Dolt: its
// route lookup would find no route table and report success without revoking.
// The revoke is skipped and the warning says why, naming the seat, and the
// removal still succeeds because the workspace is already gone.
func TestCrewRemoveUnresolvedTownWarnsInsteadOfReadingAsDolt(t *testing.T) {
	t.Parallel()
	// A rig under no town: nothing above it carries a mayor/town.json.
	rigPath := filepath.Join(t.TempDir(), "gastown")
	crewPath := filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewPath, 0o755); err != nil {
		t.Fatal(err)
	}
	revoker := &crewRevoker{}
	var warnings []string

	err := runCrewRemoveWith([]string{"max"}, "gastown", true, false, crewRemoveEnvFor(rigPath, revoker, &warnings))

	if err != nil {
		t.Fatalf("crew remove = %v, want the removal to succeed", err)
	}
	if _, statErr := os.Stat(crewPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("crew workspace stat = %v, want it gone", statErr)
	}
	if len(revoker.actors) != 0 {
		t.Errorf("revoked %v with no town root to read the route table or the token file from", revoker.actors)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "gastown/crew/max") || !strings.Contains(warnings[0], "town root") {
		t.Errorf("warnings = %q, want one naming the seat and the unresolved town root", warnings)
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
