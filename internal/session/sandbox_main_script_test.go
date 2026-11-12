package session

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// fakeMainSession is the agent session name the script is run for in tests.
const fakeMainSession = "gt-test-polecat"

// The script's text is checked here, not run: the marker write and the hold
// must follow the poll loop, and no branch that exits 1 before the loop may
// write the marker or hold. The run tests execute the script under sh
// (sandbox_main_script_run_integration_test.go).

// The marker write and the hold are the script's tail: after the poll loop ends,
// the marker is written and the hold runs once, then the script exits 0.
func TestSandboxMainScriptTextHoldsOnlyAfterThePollLoop(t *testing.T) {
	t.Parallel()
	const loopEnd = `sleep "$P"; done; `
	tail := `touch "$D/` + sandboxEndedMarker + `" || exit 1; sleep "$H"; exit 0`
	end := strings.Index(sandboxMainScript, loopEnd)
	if end < 0 {
		t.Fatalf("the script has no poll loop end %q", loopEnd)
	}
	if !strings.HasSuffix(sandboxMainScript, tail) {
		t.Fatalf("the script does not end with %q", tail)
	}
	if got := strings.Index(sandboxMainScript, tail); got != end+len(loopEnd) {
		t.Fatalf("the marker write and hold start at %d, want directly after the poll loop at %d", got, end+len(loopEnd))
	}
	head := sandboxMainScript[:end]
	if strings.Contains(head, sandboxEndedMarker) || strings.Contains(head, `"$H"`) {
		t.Fatalf("a branch before the poll loop writes the marker or holds: %q", head)
	}
	if n := strings.Count(sandboxMainScript, sandboxEndedMarker); n != 1 {
		t.Fatalf("the marker name appears %d times, want once", n)
	}
	if n := strings.Count(sandboxMainScript, `sleep "$H"`); n != 1 {
		t.Fatalf("the hold appears %d times, want once", n)
	}
}

// The hold $6 is the profile's bound, and the production argv carries it in
// the position the script reads it from.
func TestSandboxMainScriptHoldDefaultMatchesConfig(t *testing.T) {
	t.Parallel()
	if sandboxEndedHoldDefault != strconv.Itoa(config.DefaultSandboxEndedHoldSeconds) {
		t.Fatalf("sandboxEndedHoldDefault = %q, config default = %d: they must match", sandboxEndedHoldDefault, config.DefaultSandboxEndedHoldSeconds)
	}
	want := []string{"sh", "-c", sandboxMainScript, "sh", fakeMainSession, "", "", "", "", "600"}
	if got := sandboxMainArgs(fakeMainSession, 600); !reflect.DeepEqual(got, want) {
		t.Fatalf("sandboxMainArgs = %q, want %q", got, want)
	}
}

// A profile that was never converted from settings holds for the config default,
// not for zero: a zero hold would reproduce the deletion that loses transcripts.
func TestOpenShellEndedHoldFallsBackToTheDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		profile int
		want    int
	}{
		{name: "an unset profile holds for the default", profile: 0, want: config.DefaultSandboxEndedHoldSeconds},
		{name: "a set profile holds for its own bound", profile: 3600, want: 3600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &openshellBackend{profile: Profile{EndedHoldSeconds: tt.profile}}
			if got := b.endedHold(); got != tt.want {
				t.Fatalf("endedHold() = %d, want %d", got, tt.want)
			}
		})
	}
}
