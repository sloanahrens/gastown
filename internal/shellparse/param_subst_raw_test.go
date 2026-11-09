package shellparse_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/shellparse"
)

// TestParseParamExpansionSubstitutionRaw checks that a process or command
// substitution written in the word of a parameter expansion is reported with
// the body the command wrote, for the default, alternative, assign and error
// forms, and for one expansion nested in another.
func TestParseParamExpansionSubstitutionRaw(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{name: "default with process substitution", command: "echo ${v:-<(ls a)}", want: []string{"ls a"}},
		{name: "alternative with process substitution", command: "echo ${v:+<(ls a)}", want: []string{"ls a"}},
		{name: "assign with process substitution", command: "echo ${v=<(ls a)}", want: []string{"ls a"}},
		{name: "error with process substitution", command: "echo ${v?<(ls a)}", want: []string{"ls a"}},
		{name: "output process substitution", command: "echo ${v:->(wc)}", want: []string{"wc"}},
		{name: "two process substitutions in one word", command: "echo ${v:-<(ls a) >(wc)}", want: []string{"ls a", "wc"}},
		{name: "process substitution with a command substitution inside", command: "echo ${v:-<(cat $(ls))}", want: []string{"cat $(ls)", "ls"}},
		{name: "process substitution with a here-document inside", command: "echo ${v:-<(cat <<EOF\nhi\nEOF\n)}", want: []string{"cat <<EOF\nhi\nEOF\n"}},
		{name: "default with command substitution", command: "echo ${v:-$(date)}", want: []string{"date"}},
		{name: "alternative with backquote", command: "echo ${v:+`date`}", want: []string{"date"}},
		{name: "assign with command substitution", command: "echo ${v=$(date)}", want: []string{"date"}},
		{name: "error with command substitution", command: "echo ${v?$(date)}", want: []string{"date"}},
		{name: "process substitution in a nested expansion", command: "echo ${v:-${w:-<(ls a)}}", want: []string{"ls a"}},
		{name: "process substitution in double quotes is text", command: `echo "${v:-<(ls a)}"`, want: nil},
		{name: "process substitution in single quotes is text", command: "echo ${v:-'<(ls a)'}", want: nil},
		{name: "process substitution in a here-document body is text", command: "cat <<EOF\n${v:-<(ls a)}\nEOF\n", want: nil},
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

// TestParseParamExpansionUnterminatedSubstitutionIsUnparseable checks that a
// process substitution inside a parameter expansion that never closes is
// refused as unreadable, with no panic and no partial result.
func TestParseParamExpansionUnterminatedSubstitutionIsUnparseable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{name: "process substitution closed by the expansion", command: "echo ${v:-<(ls a}"},
		{name: "expansion never closed", command: "echo ${v:-<(ls a"},
		{name: "command substitution never closed", command: "echo ${v:-$(date}"},
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
