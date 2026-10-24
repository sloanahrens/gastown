package cmd

import (
	"strings"
	"testing"
)

// TestMailAndNudgeHelpSayNothingReadsOtherSessions: gt mail and gt nudge
// help must say that nothing reads another session's mail or nudge, name the
// ways to reach the operator and overseer, and name no retired role.
func TestMailAndNudgeHelpSayNothingReadsOtherSessions(t *testing.T) {
	t.Parallel()
	const sentence = "Nothing reads another session's mail or nudge in this town"
	reachWith := []string{"gt escalate", "gt ask", "comment on the bead"}
	retiredRoles := []string{"refinery", "witness", "mayor", "deacon"}

	for _, command := range []struct {
		name  string
		short string
		long  string
	}{
		{name: "mail", short: mailCmd.Short, long: mailCmd.Long},
		{name: "nudge", short: nudgeCmd.Short, long: nudgeCmd.Long},
	} {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()
			long := strings.Join(strings.Fields(command.long), " ")
			if !strings.Contains(long, sentence) {
				t.Errorf("help should say %q:\n%s", sentence, command.long)
			}
			for _, want := range reachWith {
				if !strings.Contains(long, want) {
					t.Errorf("help should name %q for reaching the operator and overseer:\n%s", want, command.long)
				}
			}
			for _, text := range []string{command.short, command.long} {
				lower := strings.ToLower(text)
				for _, role := range retiredRoles {
					if strings.Contains(lower, role) {
						t.Errorf("help names retired role %q:\n%s", role, text)
					}
				}
			}
		})
	}
}

// TestMailSendHelpNamesNoRetiredRole: gt mail send's address list must name no
// retired role, so the help cannot advertise an address the town no longer
// has (gt-wusjq).
func TestMailSendHelpNamesNoRetiredRole(t *testing.T) {
	t.Parallel()
	lower := strings.ToLower(mailSendCmd.Long)
	for _, role := range []string{"refinery", "witness", "mayor", "deacon"} {
		if strings.Contains(lower, role) {
			t.Errorf("gt mail send help names retired role %q:\n%s", role, mailSendCmd.Long)
		}
	}
}
