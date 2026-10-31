package cmd

import (
	"errors"
	"strings"
	"testing"
)

// The hook delivers a SessionStart command's STDOUT to the model and shows its
// stderr to the user only (verified 2026-10-09 against Claude Code 2.1.296).
// So the database-error banner must travel in the payload, first and never
// dropped, and the "no work, run gt done" directive must not follow it
// (gt-h7ntn, GH#2638).

func TestHookQueryErrorBannerNamesTheErrorAndForbidsDone(t *testing.T) {
	t.Parallel()
	banner := hookQueryErrorBanner(errors.New("dial tcp 127.0.0.1:3307: connection refused"))
	for _, want := range []string{"DO NOT RUN", "connection refused", "NOT an empty hook", "Do NOT close any beads"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner lacks %q:\n%s", want, banner)
		}
	}
}

// gt-0chpv: the banner is what a sandboxed polecat acts on. It must name the bd
// probe and its retries, and say a shell connection to 3307 is not an outage,
// or the polecat polls the port from bash (denied by policy) and parks.
func TestHookQueryErrorBannerNamesTheBdProbeAndTheBashDenial(t *testing.T) {
	t.Parallel()
	banner := hookQueryErrorBanner(errors.New("lookup control-plane: i/o timeout"))
	for _, want := range []string{
		"bd list --limit 1",
		"up to 3 times",
		"Port 3307 from bash, nc or curl is denied by the sandbox policy and is not evidence of an outage",
		"Escalate only if all 3 probes fail",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner lacks %q:\n%s", want, banner)
		}
	}
}

func TestPrimePayloadPutsHookErrorFirstAndKeepsItUnderBudget(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 4000)
	parts := primeParts{
		hookError:  func() string { return "BANNER\n" },
		session:    func() string { return "SESSION\n" },
		hookedWork: func() string { return "" },
		directives: func() string { return big },
		memories:   func() string { return big },
		startup:    func() string { return "STARTUP\n" },
	}
	payload := assemblePrimePayload(parts, "", false, false)

	full := payload.render(0)
	if !strings.HasPrefix(full, "BANNER\n") {
		t.Fatalf("banner is not first: %.40q", full)
	}
	budgeted := payload.render(1000)
	if !strings.HasPrefix(budgeted, "BANNER\n") {
		t.Errorf("budgeting dropped or moved the banner: %.60q", budgeted)
	}
}

func TestStartupDirectiveIsSkippedWhenTheHookQueryErrored(t *testing.T) {
	t.Parallel()
	if primeStartupDirectiveAllowed(errors.New("db down")) {
		t.Error("the NO WORK startup directive must not print when the hook query failed")
	}
	if !primeStartupDirectiveAllowed(nil) {
		t.Error("a clean query with no hooked bead must still get the startup directive")
	}
}
