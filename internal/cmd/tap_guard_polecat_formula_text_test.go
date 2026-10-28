package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The tap guard refuses a bead comment or note whose text the shell would build
// at run time, so a formula step written that way is refused the moment a
// polecat copies it and the step cannot finish (gt-uljn1). This scan runs the
// guard's own matcher over every embedded formula, so a step added with a
// refused shape fails here rather than in a polecat's session.

// polecatFormulaCommand is one command a formula hands an agent to run.
type polecatFormulaCommand struct {
	at   int    // 1-based line the command starts on
	text string // the command, as the agent would copy it
}

// polecatFormulaRefusedAt returns the line a refused command sits on: the first
// of its lines the guard refuses alone, else the first that posts bead text,
// since a text spanning lines is refused on the line that opens it.
func polecatFormulaRefusedAt(cmd polecatFormulaCommand) int {
	for i, line := range strings.Split(cmd.text, "\n") {
		if polecatFormulaJudge(line).refuses() {
			return cmd.at + i
		}
	}
	return polecatFormulaBeadWriteLine(strings.Split(cmd.text, "\n"), cmd.at)
}

// polecatFormulaViolation is one command the guard refuses, carrying the
// guard's whole verdict so a failing scan prints the same guidance a polecat's
// refused command would show.
type polecatFormulaViolation struct {
	at          int
	reason      string
	alternative string
}

// polecatFormulaBeadWrite matches the words of a bead text write the guard
// judges, so a violation is reported on the line that posts it rather than on
// the line its fence opened.
var polecatFormulaBeadWrite = regexp.MustCompile(`\b(?:bead (?:comment|note)|bd (?:comments?\b|note\b))`)

// polecatFormulaProseWrite matches the whole command a prose line names, from
// the tool word on, so a bead write in the middle of a sentence is judged the
// way an agent copying it would run it.
var polecatFormulaProseWrite = regexp.MustCompile(`\b(?:gt\s+bead\s+(?:comment|note)|bd\s+(?:comments?\b|note\b))`)

// polecatFormulaCommands returns every command one formula carries: each fenced
// block read whole, as the shell reads its lines together; and outside the
// fences, each markdown code span, each line whose first word names gt or bd,
// and each bead write named mid-sentence, since those are the places a command
// an agent copies out of prose lives. A fence left open runs to the end of the
// file, as a renderer reads it.
func polecatFormulaCommands(content string) []polecatFormulaCommand {
	var out []polecatFormulaCommand
	var block []string
	blockAt := 0
	inFence := false
	flush := func() {
		if len(block) == 0 {
			return
		}
		out = append(out, polecatFormulaCommand{at: blockAt, text: strings.Join(block, "\n")})
		block = nil
	}
	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			if inFence {
				flush()
			} else {
				block = nil
				blockAt = i + 2 // the block's first line, 1-based
			}
			inFence = !inFence
		case inFence:
			block = append(block, line)
		default:
			out = append(out, polecatFormulaProseCommands(line, i+1)...)
		}
	}
	flush()
	return out
}

// polecatFormulaBeadWriteLine returns the line a block's first bead text write
// sits on, blockAt when the block carries none.
func polecatFormulaBeadWriteLine(block []string, blockAt int) int {
	for i, line := range block {
		if polecatFormulaBeadWrite.MatchString(line) {
			return blockAt + i
		}
	}
	return blockAt
}

// polecatFormulaProseCommands returns the commands one prose line carries: each
// code span, a line that starts with gt or bd, and a bead write named partway
// through a sentence with no backticks around it.
func polecatFormulaProseCommands(line string, at int) []polecatFormulaCommand {
	var out []polecatFormulaCommand
	for _, span := range polecatFormulaCodeSpans(line) {
		out = append(out, polecatFormulaCommand{at: at, text: span})
	}
	prose := polecatFormulaProseText(line)
	if words := strings.Fields(prose); len(words) > 0 && (words[0] == "gt" || words[0] == "bd") {
		out = append(out, polecatFormulaCommand{at: at, text: prose})
	} else if loc := polecatFormulaProseWrite.FindStringIndex(prose); loc != nil {
		out = append(out, polecatFormulaCommand{at: at, text: prose[loc[0]:]})
	}
	return out
}

// polecatFormulaProseText blanks the text of each code span, keeping the line's
// length and offsets, so a bead write inside backticks is judged once, as the
// span.
func polecatFormulaProseText(line string) string {
	var b strings.Builder
	rest := line
	for {
		open := strings.IndexByte(rest, '`')
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		tail := rest[open+1:]
		end := strings.IndexByte(tail, '`')
		if end < 0 {
			// An unmatched backtick leaves the rest of the line as prose.
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:open+1])
		b.WriteString(strings.Repeat(" ", end))
		b.WriteByte('`')
		rest = tail[end+1:]
	}
}

// polecatFormulaCodeSpans returns the text inside each backtick-quoted span of a
// prose line: what an agent reading the line copies and runs. A backtick with no
// partner leaves the rest of the line alone.
func polecatFormulaCodeSpans(line string) []string {
	var spans []string
	for {
		open := strings.IndexByte(line, '`')
		if open < 0 {
			return spans
		}
		rest := line[open+1:]
		end := strings.IndexByte(rest, '`')
		if end < 0 {
			return spans
		}
		spans = append(spans, rest[:end])
		line = rest[end+1:]
	}
}

// polecatFormulaViolations returns the formula's commands the guard refuses, in
// file order.
func polecatFormulaViolations(content string) []polecatFormulaViolation {
	var out []polecatFormulaViolation
	for _, cmd := range polecatFormulaCommands(content) {
		if verdict := polecatFormulaJudge(cmd.text); verdict.refuses() {
			out = append(out, polecatFormulaViolation{
				at:          polecatFormulaRefusedAt(cmd),
				reason:      verdict.reason,
				alternative: verdict.alternative,
			})
		}
	}
	return out
}

// polecatFormulaVerdict is the guard's whole answer for one command: the reason
// it refuses the command and the alternative text it prints beneath that reason.
// Both travel together so neither is dropped on the way to a failure message.
type polecatFormulaVerdict struct {
	reason      string
	alternative string
}

// refuses reports whether the guard refused the command. An empty reason is the
// guard's own acceptance: matchesPolecatAttribution has no third, unjudged
// answer, so nothing reads as accepted because the scan could not judge it.
func (v polecatFormulaVerdict) refuses() bool { return v.reason != "" }

// polecatFormulaJudge returns the guard's verdict for a command run in a
// polecat's session. The tokens are built the way the guard builds them: heredoc
// bodies are data, so they come off first.
func polecatFormulaJudge(command string) polecatFormulaVerdict {
	reason, alternative := matchesPolecatAttribution(shellTokenize(stripHeredocBodies(command)), true, nil)
	return polecatFormulaVerdict{reason: reason, alternative: alternative}
}

// TestPolecatFormulaScanJudgesEachShape pins both verdicts of the scan on
// synthetic formulas: a shape the guard refuses must be found in a fence and in
// prose alike, and a shape it accepts must stay quiet, or a scan that reports
// nothing would pass on the shipped formulas.
func TestPolecatFormulaScanJudgesEachShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		formula string
		want    []polecatFormulaViolation
	}{
		{
			// The shape doc-audit-slice step 5 now uses: one single-quoted
			// argument whose text spans lines.
			name: "single-quoted multi-line comment",
			formula: "description = \"\"\"\n```bash\n" +
				"gt bead comment {{issue}} 'doc-audit run\nfiles: one\n'\n" +
				"```\n\"\"\"\n",
		},
		{
			// The shape doc-audit-slice step 5 had: a heredoc substitution as
			// the text.
			name: "heredoc substitution in a fence",
			formula: "description = \"\"\"\n```bash\n" +
				"gt bead comment {{issue}} \"$(cat <<'EOF'\ndoc-audit run\nEOF\n)\"\n" +
				"```\n\"\"\"\n",
			want: []polecatFormulaViolation{{at: 3, reason: polecatUnresolvedTextReason, alternative: polecatUnresolvedTextAlt}},
		},
		{
			// The shape deep-review-run step 7 had.
			name: "file substitution as the text",
			formula: "description = \"\"\"\n```bash\n" +
				"gt bead note {{issue}} \"finding: x gt-1\"\n" +
				"gt bead comment {{issue}} \"$(cat $D/digest.md)\"\n" +
				"```\n\"\"\"\n",
			want: []polecatFormulaViolation{{at: 4, reason: polecatUnresolvedTextReason, alternative: polecatUnresolvedTextAlt}},
		},
		{
			// The gap a fence-only scan leaves: the step named in a code span.
			name:    "refused command in a prose code span",
			formula: "description = \"\"\"\nThen run `gt bead note {{issue}} \"$(cat $D/notes.txt)\"` and stop.\n\"\"\"\n",
			want:    []polecatFormulaViolation{{at: 2, reason: polecatUnresolvedTextReason, alternative: polecatUnresolvedTextAlt}},
		},
		{
			// A command outside the fences with no backticks around it.
			name:    "refused command on a bare prose line",
			formula: "description = \"\"\"\ngt bead note {{issue}} \"$(cat $D/notes.txt)\"\n\"\"\"\n",
			want:    []polecatFormulaViolation{{at: 2, reason: polecatUnresolvedTextReason, alternative: polecatUnresolvedTextAlt}},
		},
		{
			// A step named partway through a sentence, with no backticks and
			// no line start to mark it.
			name:    "refused command mid-sentence in prose",
			formula: "description = \"\"\"\nThen record it with gt bead note {{issue}} \"$(cat $D/notes.txt)\" and stop.\n\"\"\"\n",
			want:    []polecatFormulaViolation{{at: 2, reason: polecatUnresolvedTextReason, alternative: polecatUnresolvedTextAlt}},
		},
		{
			// An expansion later in the text cannot build the marker, so the
			// guard accepts it and the scan must not report it.
			name: "expansion after literal text",
			formula: "description = \"\"\"\n```bash\n" +
				"gt bead note {{issue}} \"slice: $(tr '\\\\n' ' ' < /tmp/slice.txt)\"\n" +
				"```\n\"\"\"\n",
		},
		{
			// A heredoc body is data the command writes, not a command, so the
			// scan strips it the way the guard does.
			name: "refused shape inside a heredoc body",
			formula: "description = \"\"\"\n```bash\n" +
				"cat > notes.txt <<'EOF'\ngt bead note {{issue}} \"$(cat $D/notes.txt)\"\nEOF\n" +
				"```\n\"\"\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := polecatFormulaViolations(tt.formula)
			if len(got) != len(tt.want) {
				t.Fatalf("violations = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("violation %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestEmbeddedFormulasPostBeadTextTheGuardAccepts holds every shipped formula to
// the polecat bead-text rule: a step that posts a comment or note hands the
// polecat text the guard accepts, or the step cannot run (gt-uljn1).
func TestEmbeddedFormulasPostBeadTextTheGuardAccepts(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "internal", "formula", "formulas", "*.formula.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// A scanner that goes blind fails loudly: the shipped set holds well over
	// this many formulas and commands, so a moved directory or a broken fence
	// walk drops either count rather than passing on nothing.
	if len(paths) < 20 {
		t.Fatalf("found %d embedded formulas, want at least 20 — did the glob stop matching?", len(paths))
	}
	commands := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		cmds := polecatFormulaCommands(string(data))
		commands += len(cmds)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("rel %s: %v", path, err)
		}
		for _, cmd := range cmds {
			if verdict := polecatFormulaJudge(cmd.text); verdict.refuses() {
				t.Errorf("%s:%d: the guard refuses this bead text (%s); a polecat would be told: %s",
					filepath.ToSlash(rel), polecatFormulaRefusedAt(cmd), verdict.reason, verdict.alternative)
			}
		}
	}
	if commands < 300 {
		t.Fatalf("scanned %d commands, want at least 300 — the formula scanner went blind", commands)
	}
}
