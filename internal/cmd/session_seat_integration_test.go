//go:build integration

package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// sessionAddressVerbs are the subcommands that take a <rig>/<name> address, in
// the order a caller meets them.
func sessionAddressVerbs() []struct {
	name string
	run  func(*cobra.Command, []string) error
} {
	return []struct {
		name string
		run  func(*cobra.Command, []string) error
	}{
		{"start", runSessionStart},
		{"restart", runSessionRestart},
		{"status", runSessionStatus},
		{"attach", runSessionAttach},
		{"capture", runSessionCapture},
		{"inject", runSessionInject},
	}
}

// TestIntegrationSessionVerbsAgreeOnAnUnknownPolecat pins the contract this fix exists
// for (gt-pud2g): one address gets one answer, so a caller cannot read success
// or a live-but-stopped polecat out of a seat that does not exist.
//
// Before the shared resolver, `gt session restart` printed
// "✓ Session restarted. Attach with: …" and `gt session status` printed
// "State: ○ stopped" (exit 0) for a name `gt session start` refused as not
// found. A witness recovering a stalled polecat reads the restart line and
// moves on, leaving no session and nobody watching it.
func TestIntegrationSessionVerbsAgreeOnAnUnknownPolecat(t *testing.T) {
	t.Chdir(setupSessionSeatTown(t, "amber"))

	// inject refuses without a message before it resolves anything; give it
	// one so the not-found answer is what the verb is measured on.
	oldMessage := sessionMessage
	sessionMessage = "hello"
	t.Cleanup(func() { sessionMessage = oldMessage })

	var first string
	for _, verb := range sessionAddressVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			var err error
			stdout, _ := captureStdio(t, func() {
				err = verb.run(nil, []string{"gastown/ghost"})
			})

			if err == nil {
				t.Fatalf("gt session %s gastown/ghost = success, want not-found", verb.name)
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("gt session %s error = %q, want not-found", verb.name, err)
			}
			if stdout != "" {
				t.Errorf("gt session %s printed %q on a refused address, want nothing", verb.name, stdout)
			}

			if first == "" {
				first = err.Error()
				return
			}
			if err.Error() != first {
				t.Errorf("gt session %s answered %q, but an earlier verb answered %q; one address, one answer",
					verb.name, err, first)
			}
		})
	}
}
