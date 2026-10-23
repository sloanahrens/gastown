package cmd

import (
	"strings"
	"testing"
)

// The accepted-risk gate trusts a comment's free-text author, so a polecat may
// not set one and may not post an ACCEPTED-RISK: record (gt-z71j0).

var (
	attributionPolecat  = guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": "gastown/polecats/ruby"}, "")}
	attributionOverseer = guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": "town/crew/overseer"}, "")}
)

func TestPolecatAttributionRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    string // reason substring; "" means allowed
	}{
		{"author flag", `bd comments add gt-x --author "Sloan Ahrens" "looks fine"`, polecatAttributionReason},
		{"author equals form", `bd comments add gt-x --author="Sloan Ahrens" hi`, polecatAttributionReason},
		{"short author", `bd comments add gt-x -a Sloan hi`, polecatAttributionReason},
		{"singular comment with author", `bd comment gt-x --author=Sloan hi`, polecatAttributionReason},
		{"actor on comments add", `bd comments add gt-x --actor Sloan hi`, polecatAttributionReason},
		{"actor on update", `bd update gt-x --actor=Sloan --title t`, polecatAttributionReason},
		{"actor on close", `bd close gt-x --actor Sloan`, polecatAttributionReason},
		{"actor before the verb", `bd --actor Sloan comments add gt-x hi`, polecatAttributionReason},
		{"actor on note", `bd note gt-x --actor Sloan hi`, polecatAttributionReason},

		{"accepted-risk comment", `bd comments add gt-x "ACCEPTED-RISK: the retry is bounded"`, polecatAcceptedRiskReason},
		{"accepted-risk lower case", `bd comments add gt-x "accepted-risk: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk padded", `bd comments add gt-x "  ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk unquoted words", `bd comments add gt-x ACCEPTED-RISK: fine`, polecatAcceptedRiskReason},
		{"accepted-risk singular comment", `bd comment gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk bd note", `bd note gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk append-notes", `bd update gt-x --append-notes "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk append-notes equals", `bd update gt-x --append-notes="ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk notes", `bd update gt-x --notes "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk gt bead comment", `gt bead comment gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk gt bead note", `gt bead note gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},

		{"global value flag before the verb", `bd --db /p/x.db comments add gt-x --author Sloan hi`, polecatAttributionReason},
		{"format flag before the verb", `bd --format json comments add gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"path to bd", `/usr/local/bin/bd comments add gt-x -a Sloan hi`, polecatAttributionReason},
		{"path to gt", `/usr/local/bin/gt bead note gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"chained after another command", `cd /tmp && bd comments add gt-x --author Sloan hi`, polecatAttributionReason},
		{"chained second of two", `bd show gt-x; gt bead comment gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"env prefix", `BD_X=1 bd comments add gt-x -a Sloan hi`, polecatAttributionReason},
		{"inside bash -c", `bash -c 'bd comments add gt-x --author Sloan hi'`, polecatAttributionReason},
		{"inside command substitution", `echo $(bd note gt-x "ACCEPTED-RISK: fine")`, polecatAcceptedRiskReason},

		{"ordinary comment", `bd comments add gt-x "found the cause in tap_guard.go"`, ""},
		{"ordinary note", `gt bead note gt-x "Findings: the retry never resets"`, ""},
		{"ordinary append-notes", `bd update gt-x --append-notes "progress"`, ""},
		{"marker not at the start", `bd comments add gt-x "see the ACCEPTED-RISK: record on gt-y"`, ""},
		{"marker in a title", `bd update gt-x --title "ACCEPTED-RISK: tracking"`, ""},
		{"marker in a search", `bd search "ACCEPTED-RISK:"`, ""},
		{"marker in an echo", `echo "ACCEPTED-RISK: fine"`, ""},
		{"actor on a read verb", `bd --actor Sloan show gt-x`, ""},
		{"listing comments with actor", `bd comments gt-x --actor Sloan`, ""},
		{"author flag on another verb", `bd show gt-x --author Sloan`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, _ := evaluateDangerousCommand(tt.command, 0, attributionPolecat)
			if tt.want == "" {
				if reason != "" {
					t.Errorf("polecat %q refused (%q), want allowed", tt.command, reason)
				}
				return
			}
			if reason != tt.want {
				t.Errorf("polecat %q reason = %q, want %q", tt.command, reason, tt.want)
			}
		})
	}
}

// The same commands from a session that is not a polecat's pass untouched.
func TestPolecatAttributionLeavesOtherSessionsAlone(t *testing.T) {
	t.Parallel()
	commands := []string{
		`bd comments add gt-x --author "Sloan Ahrens" "ACCEPTED-RISK: the retry is bounded"`,
		`bd comments add gt-x -a Sloan hi`,
		`bd update gt-x --actor Sloan --append-notes "ACCEPTED-RISK: fine"`,
		`gt bead comment gt-x "ACCEPTED-RISK: fine"`,
		`gt bead note gt-x "ACCEPTED-RISK: fine"`,
		`bd note gt-x "ACCEPTED-RISK: fine"`,
	}
	for _, command := range commands {
		if reason, _ := evaluateDangerousCommand(command, 0, attributionOverseer); reason != "" {
			t.Errorf("overseer %q refused (%q), want allowed", command, reason)
		}
	}
}

// The refusal reaches a polecat through the real hook entry point, naming the
// rule and who records an accepted risk instead.
func TestRunTapGuardDangerous_AcceptedRiskRefusalText(t *testing.T) {
	t.Parallel()
	command := `bd comments add gt-x --author "Sloan Ahrens" "ACCEPTED-RISK: fine"`
	input := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
	var buf strings.Builder
	if err := tapGuardDangerous(strings.NewReader(input), &buf, attributionPolecat.proc); err == nil {
		t.Fatal("a polecat's authored comment was allowed through the hook")
	}
	for _, want := range []string{"comment author or actor", "--author, -a and --actor"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, buf.String())
		}
	}

	input = `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(`gt bead note gt-x "ACCEPTED-RISK: fine"`) + `}}`
	buf.Reset()
	if err := tapGuardDangerous(strings.NewReader(input), &buf, attributionPolecat.proc); err == nil {
		t.Fatal("a polecat's ACCEPTED-RISK note was allowed through the hook")
	}
	for _, want := range []string{"overseer or Sloan", "not by a polecat"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, buf.String())
		}
	}
}
