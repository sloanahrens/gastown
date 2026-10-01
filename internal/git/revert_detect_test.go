//go:build !integration

package git

import "testing"

// moveLine is what lets the relocation reading see code that crossed a package
// boundary: the move rewrites the qualifiers around the code it carries, so the
// comparison has to be made without them (gt-bbk1f). What it must not rewrite
// is a string literal — a path, a format string, a shell command — where a dot
// is content rather than a name.
func TestMoveLineStripsQualifiersOutsideLiterals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		line string
		want string
	}{
		{
			// Whitespace goes first, inside the literal as everywhere else
			// (squashLine), and the dots the literal holds survive it.
			name: "a receiver renamed by the move",
			line: "\treturn result, fmt.Errorf(\"%s %s is the operator's work\", params.BeadID)",
			want: "returnresult,Errorf(\"%s%sistheoperator'swork\",BeadID)",
		},
		{
			name: "a package prefix",
			line: "\tsummary: sling.MatchLimit,",
			want: "summary:MatchLimit,",
		},
		{
			name: "a chain of three collapses to its last name",
			line: "\tspi.originalHold = &beadHold{Status: s.OriginalHold.Status}",
			want: "originalHold=&beadHold{Status:Status}",
		},
		{
			name: "a string literal keeps its dots",
			line: "\turl := \"github.com/steveyegge/gastown\"",
			want: "url:=\"github.com/steveyegge/gastown\"",
		},
		{
			name: "a backquoted string keeps its dots",
			line: "\tcmd := `git log -1 foo.bar`",
			want: "cmd:=`gitlog-1foo.bar`",
		},
		{
			name: "an escape does not end the literal",
			line: "\tmsg := \"a \\\" b.c\"",
			want: "msg:=\"a\\\"b.c\"",
		},
		{
			name: "a rune literal is left alone",
			line: "\tsep := '.'",
			want: "sep:='.'",
		},
		{
			name: "a float is not a chain",
			line: "\ttimeout := 1.5 * time.Second",
			want: "timeout:=1.5*Second",
		},
		{
			name: "an identifier that is not qualified",
			line: "\twhitespace := strings.Fields(line)",
			want: "whitespace:=Fields(line)",
		},
		{
			name: "nothing but whitespace",
			line: "\t \t",
			want: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := moveLine(tt.line); got != tt.want {
				t.Errorf("moveLine(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}
