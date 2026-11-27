package rehearsal

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/cmd"
	"github.com/steveyegge/gastown/internal/polecat"
)

// TestRehearsalStrandedReadsListItemJSON encodes cmd.PolecatListItem, the type
// `gt polecat list --json` writes, so a renamed or retagged field fails here.
func TestRehearsalStrandedReadsListItemJSON(t *testing.T) {
	t.Parallel()
	listing, err := json.Marshal([]cmd.PolecatListItem{
		{Rig: "load", Name: "alpha", State: polecat.StateWorking, Issue: "gt-1", SessionRunning: true},
		{Rig: "load", Name: "beta", State: polecat.StateWorking},
		{Rig: "load", Name: "gamma", State: polecat.StateIdle},
		{Rig: "load", Name: "delta", State: polecat.StateWorking, SessionRunning: true},
	})
	if err != nil {
		t.Fatalf("encoding list items: %v", err)
	}
	got, err := Stranded(listing, "load")
	if err != nil {
		t.Fatalf("Stranded(%s): %v", listing, err)
	}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Stranded(%s) = %v, want %v", listing, got, want)
	}
}

func TestRehearsalStranded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, listing string
		want          []string
		wantErr       bool
	}{
		{"bead holder", `[{"rig":"load","name":"alpha","state":"working","issue":"gt-1","session_running":true}]`, []string{"alpha"}, false},
		{"sessionless non-idle", `[{"rig":"load","name":"beta","state":"working"}]`, []string{"beta"}, false},
		{"idle clean", `[{"rig":"load","name":"gamma","state":"idle"}]`, nil, false},
		{"idle holding a bead", `[{"rig":"load","name":"zeta","state":"idle","issue":"gt-3"}]`, []string{"zeta"}, false},
		{"live session no bead", `[{"rig":"load","name":"delta","state":"working","session_running":true}]`, nil, false},
		{"other rig", `[{"rig":"other","name":"eps","state":"stalled","issue":"gt-2"}]`, nil, false},
		{"bad json", `[{"rig":`, nil, true},
		{"not an array", `{"rig":"load"}`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Stranded([]byte(tt.listing), "load")
			if tt.wantErr {
				if err == nil || got != nil {
					t.Fatalf("Stranded(%q) = %v, %v; want nil names and an error", tt.listing, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Stranded(%q): %v", tt.listing, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Stranded(%q) = %v, want %v", tt.listing, got, tt.want)
			}
		})
	}
}
