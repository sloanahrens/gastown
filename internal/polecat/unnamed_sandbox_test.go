package polecat

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// TestUnnamedSandboxLines: with the seat's record and agent bead gone, each
// distinct gateway is asked once and its answer becomes a line: a gateway that
// holds the sandbox is named with its phase as not removed because the agent
// bead is gone; one that cannot answer is named with its error and the gateways
// after it are still asked; not found adds no line (gt-xo4p78).
func TestUnnamedSandboxLines(t *testing.T) {
	t.Parallel()
	const name = "gt-abacus"
	gone := errors.New("Error: sandbox not found: gt-abacus")
	hold := func(gateway string) string {
		return fmt.Sprintf("gateway %q holds sandbox %s in phase Error: it is not removed because its agent bead is gone, so its work is not proven on origin", gateway, name)
	}
	unanswered := fmt.Sprintf("gateway %q could not answer for sandbox %s: gateway timed out", "k8s", name)
	tests := []struct {
		name      string
		gateways  []string
		answers   map[string]error
		wantAsked []string
		wantLines []string
	}{
		{"all_not_found", []string{"k8s", "other"}, map[string]error{"k8s": gone, "other": gone}, []string{"k8s", "other"}, nil},
		{"held", []string{"k8s"}, map[string]error{"k8s": nil}, []string{"k8s"}, []string{hold("k8s")}},
		{"unanswered_then_held", []string{"k8s", "other"}, map[string]error{"k8s": errors.New("gateway timed out")}, []string{"k8s", "other"}, []string{unanswered, hold("other")}},
		{"duplicate_gateway", []string{"k8s", "k8s", "other"}, map[string]error{"k8s": nil, "other": gone}, []string{"k8s", "other"}, []string{hold("k8s")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			status := func(gateway, sandboxName string) (sandbox.Status, error) {
				if sandboxName != name {
					t.Errorf("asked for %q, want %q", sandboxName, name)
				}
				asked = append(asked, gateway)
				return sandbox.Status{Name: name, Phase: "Error"}, tt.answers[gateway]
			}
			got := unnamedSandboxLines(status, name, tt.gateways)
			if !reflect.DeepEqual(asked, tt.wantAsked) {
				t.Errorf("asked %v, want %v", asked, tt.wantAsked)
			}
			if !reflect.DeepEqual(got, tt.wantLines) {
				t.Errorf("lines = %q, want %q", got, tt.wantLines)
			}
		})
	}
}
