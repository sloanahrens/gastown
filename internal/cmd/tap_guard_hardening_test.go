package cmd

import (
	"os"
	"strings"
	"testing"
	"time"
)

// GT_ROLE names the session; a stale GT_POLECAT left in the environment of a
// refinery or crew session must not turn it into a polecat (gt-pb77k). Every
// other polecat check in the guard family already works this way.
func TestPolecatContextGTRoleWinsOverStaleGTPolecat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, polecat, role string
		want                bool
	}{
		{"refinery with a stale GT_POLECAT", "nux", "gastown/refinery", false},
		{"crew with a stale GT_POLECAT", "nux", "gastown/crew/sloan", false},
		{"polecat role with GT_POLECAT", "nux", "gastown/polecats/nux", true},
		{"GT_POLECAT alone, no role", "nux", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proc := fakeGuardProcess(map[string]string{"GT_POLECAT": c.polecat, "GT_ROLE": c.role}, "/tmp/neutral")
			if got := isPolecatContext(proc); got != c.want {
				t.Errorf("isPolecatContext() = %v, want %v", got, c.want)
			}
			if got := isPolecatOrRefineryContext(proc); got != c.want {
				t.Errorf("isPolecatOrRefineryContext() = %v, want %v", got, c.want)
			}
		})
	}
}

// A failed escalation must not silence every later denial of the same shape
// (gt-pb77k): the marker records the failure and a retry is allowed once the
// retry window has passed, while a success still suppresses repeats for good.
func TestParkedPromptEscalationRetriesAfterFailure(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	hook := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))
	marker := promptEscalationMarker(hook, promptShape(hook), tmp)

	failing, failedCalls := fakeParkedPromptEscalator(false)
	if got := escalateParkedPromptOnce(hook, tmp, failing); !strings.Contains(got, "Recording the escalation failed") {
		t.Fatalf("first attempt = %q, want the failure message", got)
	}
	// Right away the failure is remembered, so a retry loop does not hammer Dolt.
	if got := escalateParkedPromptOnce(hook, tmp, failing); strings.Contains(got, "Recording the escalation failed") || len(*failedCalls) != 1 {
		t.Fatalf("immediate retry reached the escalator again (calls=%d, got %q)", len(*failedCalls), got)
	}
	// After the window the failed escalation is attempted again, and success sticks.
	old := time.Now().Add(-2 * parkedPromptRetryAfter)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	working, workingCalls := fakeParkedPromptEscalator(true)
	if got := escalateParkedPromptOnce(hook, tmp, working); !strings.Contains(got, "recorded as a bead") || len(*workingCalls) != 1 {
		t.Fatalf("retry after the window = %q (calls=%d), want a recorded escalation", got, len(*workingCalls))
	}
	if got := escalateParkedPromptOnce(hook, tmp, working); !strings.Contains(got, "already reported") || len(*workingCalls) != 1 {
		t.Fatalf("after success a repeat must be suppressed, got %q (calls=%d)", got, len(*workingCalls))
	}
}

func TestDangerousRmRfFlagSpellings(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"rm -rf /",
		"rm -r -f /",
		"rm -f -r /",
		"rm -fr /*",
		"rm --recursive --force /",
		"rm -rf --no-preserve-root /",
		"rm -r --no-preserve-root /",
		"rm -rf //",
		"echo ok && rm -r -f /",
	}
	for _, command := range blocked {
		if matchesDangerousRmRf(lowerTokens(command)) == "" {
			t.Errorf("matchesDangerousRmRf(%q) allowed, want blocked", command)
		}
	}
	allowed := []string{
		"rm -rf /tmp/x",
		"rm -r -f build",
		"rm -f /",      // not recursive: rm refuses a directory
		"rm -r /tmp/x", // not forced
		"ls -rf /",
		"echo rm -rf /", // text, not a command
	}
	for _, command := range allowed {
		if reason := matchesDangerousRmRf(lowerTokens(command)); reason != "" {
			t.Errorf("matchesDangerousRmRf(%q) = %q, want allowed", command, reason)
		}
	}
}

func TestPolecatPathsTreatsFindDeleteAndXargsAsWrites(t *testing.T) {
	t.Parallel()
	writes := []struct {
		base string
		args []string
	}{
		{"find", []string{"../x", "-name", "*.go", "-delete"}},
		{"find", []string{"../x", "-exec", "rm", "{}", ";"}},
		{"find", []string{"../x", "-execdir", "rm", "{}", "+"}},
		{"xargs", []string{"rm", "../x/a"}},
	}
	for _, w := range writes {
		if !isWriteCapableCommand(w.base, w.args) {
			t.Errorf("isWriteCapableCommand(%q, %q) = false, want true", w.base, w.args)
		}
	}
	if isWriteCapableCommand("find", []string{"../x", "-name", "*.go", "-print"}) {
		t.Error("a read-only find was treated as a write")
	}
}
