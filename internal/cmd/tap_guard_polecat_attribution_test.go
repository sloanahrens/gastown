package cmd

import (
	"strings"
	"testing"
)

// The accepted-risk gate trusts a comment's free-text author, so a polecat may
// not set one, may not write a comment it cannot be checked for, and may not
// post an ACCEPTED-RISK: record (gt-z71j0).

var (
	attributionPolecat  = guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": "gastown/polecats/ruby"}, "")}
	attributionOverseer = guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": "town/crew/overseer"}, "")}
)

func TestPolecatAttributionRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    string // reason; "" means allowed
	}{
		{"author flag", `bd comments add gt-x --author "Sloan Ahrens" "looks fine"`, polecatAttributionReason},
		{"author equals form", `bd comments add gt-x --author="Sloan Ahrens" hi`, polecatAttributionReason},
		{"short author", `bd comments add gt-x -a Sloan hi`, polecatAttributionReason},
		{"short author equals form", `bd comments add gt-x -a=Sloan hi`, polecatAttributionReason},
		{"short author attached value", `bd comments add gt-x -aSloan hi`, polecatAttributionReason},
		{"short author behind a boolean", `bd comments add gt-x -qa Sloan hi`, polecatAttributionReason},
		{"author on comments add after a global flag", `bd comments --json add gt-x --author Sloan hi`, polecatAttributionReason},
		{"singular comment with author", `bd comment gt-x --author=Sloan hi`, polecatAttributionReason},
		{"actor on comments add", `bd comments add gt-x --actor Sloan hi`, polecatAttributionReason},
		{"actor on update", `bd update gt-x --actor=Sloan --title t`, polecatAttributionReason},
		{"actor on close", `bd close gt-x --actor Sloan`, polecatAttributionReason},
		{"actor on create", `bd create "a title" --actor Sloan`, polecatAttributionReason},
		{"actor before the verb", `bd --actor Sloan comments add gt-x hi`, polecatAttributionReason},
		{"actor on note", `bd note gt-x --actor Sloan hi`, polecatAttributionReason},
		{"author on another write verb", `bd update gt-x --author Sloan --title t`, polecatAttributionReason},

		{"actor env on gt bead comment", `BD_ACTOR='Sloan Ahrens' gt bead comment gt-1 hello`, polecatAttributionReason},
		{"actor env on bd comments add", `BD_ACTOR='Sloan Ahrens' bd comments add gt-1 hello`, polecatAttributionReason},
		{"exported actor", `export BD_ACTOR=town/crew/overseer`, polecatAttributionReason},
		{"beads actor env", `BEADS_ACTOR=Sloan bd comments add gt-1 hello`, polecatAttributionReason},
		{"bare actor assignment", `BD_ACTOR=Sloan; bd comments add gt-1 hello`, polecatAttributionReason},
		{"actor behind env", `env BD_ACTOR=Sloan bd comments add gt-1 hello`, polecatAttributionReason},
		{"actor behind env flag", `env -u FOO BD_ACTOR=Sloan bd comments add gt-1 hello`, polecatAttributionReason},
		{"actor behind declare", `declare -x BD_ACTOR=Sloan`, polecatAttributionReason},
		{"actor after another assignment", `X=1 BD_ACTOR=Sloan bd comments add gt-1 hello`, polecatAttributionReason},
		{"empty actor falls back to the git user", `BD_ACTOR= bd comments add gt-1 hello`, polecatAttributionReason},
		{"actor chained after another command", `cd /tmp && export BD_ACTOR=Sloan`, polecatAttributionReason},
		{"actor inside bash -c", `bash -c "BD_ACTOR=Sloan bd comments add gt-1 hello"`, polecatAttributionReason},
		{"own role as actor", `BD_ACTOR=gastown/polecats/ruby bd comments add gt-1 hello`, ""},
		{"own role exported", `export BD_ACTOR=gastown/polecats/ruby`, ""},
		{"other variable", `FOO=Sloan bd comments add gt-1 hello`, ""},
		{"actor named in comment text", `bd comments add gt-1 "BD_ACTOR=Sloan is refused"`, ""},
		{"actor named after the command", `echo BD_ACTOR=Sloan`, ""},

		{"accepted-risk comment", `bd comments add gt-x "ACCEPTED-RISK: the retry is bounded"`, polecatAcceptedRiskReason},
		{"accepted-risk lower case", `bd comments add gt-x "accepted-risk: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk padded", `bd comments add gt-x "  ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk unquoted words", `bd comments add gt-x ACCEPTED-RISK: fine`, polecatAcceptedRiskReason},
		{"accepted-risk after a double dash", `bd comments add gt-x -- "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk singular comment", `bd comment gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk bd note", `bd note gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk append-notes", `bd update gt-x --append-notes "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk append-notes equals", `bd update gt-x --append-notes="ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk notes", `bd update gt-x --notes "accepted-risk: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk create notes", `bd create "a title" --notes "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk create append-notes equals", `bd create "a title" --append-notes="ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk gt bead comment", `gt bead comment gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},
		{"accepted-risk gt bead note", `gt bead note gt-x "ACCEPTED-RISK: fine"`, polecatAcceptedRiskReason},

		{"accepted-risk built by a substitution", `bd comments add gt-x "$(echo 'ACCEPTED-RISK: ok')"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a backtick substitution", "bd comments add gt-x \"`echo ACCEPTED-RISK: ok`\"", polecatUnresolvedTextReason},
		{"accepted-risk built by a variable", `T='ACCEPTED-RISK: x'; bd comments add gt-x "$T"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a braced variable", `T='ACCEPTED-RISK: x'; bd comments add gt-x "${T}"`, polecatUnresolvedTextReason},
		{"accepted-risk built by an unquoted variable", `T='ACCEPTED-RISK: x'; bd comments add gt-x $T`, polecatUnresolvedTextReason},
		{"accepted-risk split around a substitution", `bd comments add gt-x "ACCEPTED-$(echo RISK:) ok"`, polecatUnresolvedTextReason},
		{"accepted-risk prefix then variable", `bd comments add gt-x "ACC$X"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a substitution in a note", `bd note gt-x "$(printf 'ACCEPTED-RISK: ok')"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a variable in gt bead note", `gt bead note gt-x "$T"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a substitution in gt bead comment", `gt bead comment gt-x "$(echo 'ACCEPTED-RISK: ok')"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a variable in append-notes", `bd update gt-x --append-notes "$(echo 'ACCEPTED-RISK: ok')"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a variable in append-notes equals", `bd update gt-x --append-notes="$T"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a variable in notes", `bd update gt-x --notes "$T"`, polecatUnresolvedTextReason},
		{"accepted-risk built by a variable in create notes", `bd create "a title" --notes "$T"`, polecatUnresolvedTextReason},

		{"comment text from a short file flag", `bd comments add gt-x -f notes.txt`, polecatCommentInputReason},
		{"comment text from a file flag", `bd comments add gt-x --file notes.txt`, polecatCommentInputReason},
		{"comment text from a file flag equals form", `bd comments add gt-x --file=notes.txt`, polecatCommentInputReason},
		{"comment text from stdin", `bd comment gt-x --stdin`, polecatCommentInputReason},
		{"note text from stdin in a pipe", `echo fine | bd note gt-x --stdin`, polecatCommentInputReason},

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
		{"ordinary create notes", `bd create "a title" --notes "context"`, ""},
		{"dash a as comment text", `bd comments add gt-x -- "-a is the author flag"`, ""},
		{"dash a inside comment text", `bd comments add gt-x "found the -a flag"`, ""},
		{"marker not at the start", `bd comments add gt-x "see the ACCEPTED-RISK: record on gt-y"`, ""},
		{"marker in a title", `bd update gt-x --title "ACCEPTED-RISK: tracking"`, ""},
		{"marker in a search", `bd search "ACCEPTED-RISK:"`, ""},
		{"marker in an echo", `echo "ACCEPTED-RISK: fine"`, ""},
		{"actor on a read verb", `bd --actor Sloan show gt-x`, ""},
		{"listing comments with actor", `bd comments gt-x --actor Sloan`, ""},
		{"author flag on another verb", `bd show gt-x --author Sloan`, ""},
		{"short a is the assignee on update", `bd update gt-x -a "Sloan Ahrens"`, ""},
		{"variable quoted in prose", `bd comments add gt-x "the config reads $HOME/.config/gt"`, ""},
		{"substitution in prose", `bd comments add gt-x "found it in $(git rev-parse --show-toplevel)"`, ""},
		{"variable after a marker-free word", `bd note gt-x "rig $RIG is set"`, ""},
		{"substitution in prose note append", `bd update gt-x --append-notes "rebuilt with $(go env GOROOT)"`, ""},
		{"variable named in prose about the marker", `bd comments add gt-x "the marker is $T, see the guard"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, _ := evaluateDangerousCommand(tt.command, 0, attributionPolecat)
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
		`bd comments add gt-x -qa Sloan hi`,
		`bd update gt-x --actor Sloan --append-notes "ACCEPTED-RISK: fine"`,
		`bd create "a title" --notes "ACCEPTED-RISK: fine"`,
		`bd comments add gt-x -f notes.txt`,
		`gt bead comment gt-x "ACCEPTED-RISK: fine"`,
		`gt bead note gt-x "ACCEPTED-RISK: fine"`,
		`bd note gt-x "ACCEPTED-RISK: fine"`,
		`BD_ACTOR='Sloan Ahrens' bd comments add gt-x hi`,
		`export BD_ACTOR=town/crew/overseer`,
		`bd comments add gt-x "$(echo 'ACCEPTED-RISK: ok')"`,
		`T='ACCEPTED-RISK: x'; bd comments add gt-x "$T"`,
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

	input = `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(`bd note gt-x --stdin`) + `}}`
	buf.Reset()
	if err := tapGuardDangerous(strings.NewReader(input), &buf, attributionPolecat.proc); err == nil {
		t.Fatal("a polecat's stdin note was allowed through the hook")
	}
	if !strings.Contains(buf.String(), "post the comment inline") {
		t.Errorf("stderr missing the inline instruction:\n%s", buf.String())
	}
}
