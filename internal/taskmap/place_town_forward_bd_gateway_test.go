package taskmap

import (
	"strings"
	"testing"
)

// TestTownForwardBdGatewaySitsWithHostTools pins the grouping judgement
// place_town_forward_bd_gateway.go records (gt-bbuomg): the operator loads the
// printed job on the Mac, as with town forward and town forward plist, so the
// entry belongs in host.
func TestTownForwardBdGatewaySitsWithHostTools(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("town forward bd-gateway")
	if !ok {
		t.Fatal(`Lookup("town forward bd-gateway") found no entry`)
	}
	if e.Group != "host" {
		t.Errorf("town forward bd-gateway Group = %q, want \"host\": the operator loads the job on the Mac", e.Group)
	}
}

// TestTownForwardBdGatewayUseNamesTheSituation pins the Use line
// place_town_forward_bd_gateway.go gives the command (gt-bbuomg): it names the
// situation that sends a reader to it, opening "Use this when". The context and
// namespace flags belong to the command's help, so the Use does not restate
// them.
func TestTownForwardBdGatewayUseNamesTheSituation(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("town forward bd-gateway")
	if !ok {
		t.Fatal(`Lookup("town forward bd-gateway") found no entry`)
	}
	if !strings.HasPrefix(e.Use, "Use this when") {
		t.Errorf("town forward bd-gateway Use = %q; want a line starting \"Use this when\" that names the situation", e.Use)
	}
	for _, flag := range []string{"--context", "--namespace"} {
		if strings.Contains(e.Use, flag) {
			t.Errorf("town forward bd-gateway Use = %q; it names the flag %q, which the command's help states", e.Use, flag)
		}
	}
}

// TestTownForwardBdGatewayCheckNamesThePrintedAddress pins the Check to the
// address internal/cmd/town_forward_plist.go prints (gt-bbuomg): the subcommand
// fixes --service to bd-gateway, --port to 7440 and --bind to 127.0.0.1, and
// writes "bd gateway URL: http://127.0.0.1:7440/" to stderr. A Check that named
// another address would send the reader looking for a line the run never prints.
func TestTownForwardBdGatewayCheckNamesThePrintedAddress(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("town forward bd-gateway")
	if !ok {
		t.Fatal(`Lookup("town forward bd-gateway") found no entry`)
	}
	if !strings.Contains(e.Check, "127.0.0.1:7440") {
		t.Errorf("town forward bd-gateway Check = %q, want it to name 127.0.0.1:7440, the address the run prints", e.Check)
	}
	if !strings.Contains(e.Use, "127.0.0.1:7440") {
		t.Errorf("town forward bd-gateway Use = %q; the situation names the gateway at 127.0.0.1:7440, the address the run prints", e.Use)
	}
}
