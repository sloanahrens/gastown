package shellparse_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/shellparse"
)

// substRaws lists the Raw text of every substitution in script, outer before
// inner, in the order the script holds them: a header substitution first, then
// the substitutions of each segment.
func substRaws(script shellparse.Script) []string {
	var out []string
	for _, sub := range script.Subs {
		out = append(out, sub.Raw)
		out = append(out, substRaws(sub)...)
	}
	for _, seg := range script.Segments {
		for _, sub := range seg.Subs {
			out = append(out, sub.Raw)
			out = append(out, substRaws(sub)...)
		}
	}
	return out
}

// TestParseSubstitutionRaw checks that each substitution carries the body text
// the command wrote between its delimiters, as written, for every form the
// parser reads a substitution in.
func TestParseSubstitutionRaw(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{name: "dollar-paren", command: "echo $(date +%s)", want: []string{"date +%s"}},
		{name: "empty dollar-paren", command: "echo $()", want: []string{""}},
		{name: "backtick", command: "echo `date`", want: []string{"date"}},
		{name: "empty backtick", command: "echo ``", want: []string{""}},
		{name: "nested dollar-paren", command: "echo $(echo $(id -u))", want: []string{"echo $(id -u)", "id -u"}},
		{name: "backtick inside dollar-paren", command: "echo $(echo `id`)", want: []string{"echo `id`", "id"}},
		{name: "escaped backtick nested in backtick", command: "echo `echo \\`id\\``", want: []string{"echo \\`id\\`", "id"}},
		{name: "dollar-paren in double quotes", command: `echo "a $(printf '%s' "x y")"`, want: []string{`printf '%s' "x y"`}},
		{name: "backtick in double quotes", command: "echo \"`date`\"", want: []string{"date"}},
		{name: "heredoc inside", command: "x=$(cat <<EOF\nhi\nEOF\n)", want: []string{"cat <<EOF\nhi\nEOF\n"}},
		{name: "process substitutions", command: "diff <(ls a) >(wc)", want: []string{"ls a", "wc"}},
		{name: "header substitution", command: "for f in $(ls); do echo \"$f\"; done", want: []string{"ls"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if got := substRaws(script); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) substitution Raw = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

// TestParseProcSubstWordRaw checks the body of a process substitution that
// makes up a whole word, which the word carries as its own ProcSubs entry.
func TestParseProcSubstWordRaw(t *testing.T) {
	t.Parallel()
	script, err := shellparse.Parse("diff <(ls a) b")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(script.Segments) != 1 || len(script.Segments[0].Words) < 2 {
		t.Fatalf("Parse: want one segment with the process substitution word, got %+v", summarize(script))
	}
	procs := script.Segments[0].Words[1].ProcSubs
	if len(procs) != 1 || procs[0].Script.Raw != "ls a" {
		t.Errorf("ProcSubs = %+v, want one entry whose body is %q", procs, "ls a")
	}
}

// TestParseUnterminatedSubstitutionIsUnparseable checks that a substitution
// the command never closes is refused as unreadable, with no panic and no
// partial result.
func TestParseUnterminatedSubstitutionIsUnparseable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{name: "dollar-paren", command: "echo $(date"},
		{name: "backtick", command: "echo `date"},
		{name: "heredoc inside", command: "x=$(cat <<EOF\nhi\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			var unreadable *shellparse.UnparseableError
			if !errors.As(err, &unreadable) {
				t.Fatalf("Parse(%q) error = %v, want *UnparseableError", tt.command, err)
			}
			if len(script.Segments) != 0 || len(script.Subs) != 0 {
				t.Errorf("Parse(%q) returned a partial result %+v with its error", tt.command, script)
			}
		})
	}
}
