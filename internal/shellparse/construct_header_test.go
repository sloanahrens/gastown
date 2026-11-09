package shellparse_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/shellparse"
)

// subTree is the test's view of one substitution: its body text, whether it was
// written with backquotes, and the substitutions nested in it.
type subTree struct {
	Raw       string
	Backquote bool
	Subs      []subTree
}

// viewSubs turns a list of substitutions into the values the table asserts.
func viewSubs(subs []shellparse.Script) []subTree {
	var out []subTree
	for _, sub := range subs {
		out = append(out, subTree{Raw: sub.Raw, Backquote: sub.Backquote, Subs: viewSubs(sub.Subs)})
	}
	return out
}

// TestParseHeaderArithmeticBodies checks that an arithmetic expansion in a
// construct header ([[ ]], case, for-in) is exposed as a substitution whose Raw
// is the body text between "$((" and "))", for each place the header reads one.
func TestParseHeaderArithmeticBodies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []subTree
	}{
		{name: "[[ ]] arithmetic", command: "[[ $((printenv)) ]]", want: []subTree{{Raw: "printenv"}}},
		{name: "[[ ]] quoted arithmetic", command: `[[ "$((printenv))" ]]`, want: []subTree{{Raw: "printenv"}}},
		{name: "[[ ]] arithmetic inside a word", command: "[[ a$((printenv))b ]]", want: []subTree{{Raw: "printenv"}}},
		{name: "[[ ]] bracket arithmetic", command: "[[ $[printenv] ]]", want: []subTree{{Raw: "printenv"}}},
		{name: "case word arithmetic", command: "case $((printenv)) in *) ;; esac", want: []subTree{{Raw: "printenv"}}},
		{name: "case word quoted arithmetic", command: `case "$((printenv))" in *) ;; esac`, want: []subTree{{Raw: "printenv"}}},
		{name: "case word arithmetic inside a word", command: "case a$((printenv))b in *) ;; esac", want: []subTree{{Raw: "printenv"}}},
		{name: "case pattern arithmetic", command: "case x in $((printenv))) echo ;; esac", want: []subTree{{Raw: "printenv"}}},
		{name: "for-in arithmetic", command: "for x in $((printenv)); do echo $x; done", want: []subTree{{Raw: "printenv"}}},
		{name: "for-in quoted arithmetic", command: `for x in "$((printenv))"; do echo $x; done`, want: []subTree{{Raw: "printenv"}}},
		{name: "for-in arithmetic inside a word", command: "for x in a$((printenv))b; do echo $x; done", want: []subTree{{Raw: "printenv"}}},
		{name: "source text keeps its spaces", command: "[[ $(( 1 + 2 )) ]]", want: []subTree{{Raw: " 1 + 2 "}}},
		{
			name:    "nested arithmetic nests under its outer body",
			command: "[[ $((1+$((printenv)))) ]]",
			want:    []subTree{{Raw: "1+$((printenv))", Subs: []subTree{{Raw: "printenv"}}}},
		},
		{
			name:    "header arithmetic keeps its order with a command substitution",
			command: "[[ $((x)) == $(printenv) ]]",
			want:    []subTree{{Raw: "x"}, {Raw: "printenv"}},
		},
		{
			name:    "parameter word with a process substitution exposes its arithmetic too",
			command: "for x in ${v:-<(cat) $((printenv))}; do echo $x; done",
			want:    []subTree{{Raw: "cat"}, {Raw: "printenv"}},
		},
		{
			name:    "command substitution in a header is one substitution, its arithmetic not repeated",
			command: "for x in $(echo $((printenv))); do echo $x; done",
			want:    []subTree{{Raw: "echo $((printenv))"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if got := viewSubs(script.Subs); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) header subs = %#v, want %#v", tt.command, got, tt.want)
			}
		})
	}
}

// TestParseArithmeticOutsideHeaderStaysOutOfSubs pins the scope of the change: an
// arithmetic expansion in a command word, an arithmetic command or a let clause
// adds no substitution, so only the construct headers expose arithmetic bodies.
func TestParseArithmeticOutsideHeaderStaysOutOfSubs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{name: "command word", command: "echo $((printenv))"},
		{name: "arithmetic command", command: "(( printenv ))"},
		{name: "let clause", command: "let x=$((printenv))"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if len(script.Subs) != 0 {
				t.Errorf("Parse(%q) header subs = %#v, want none", tt.command, viewSubs(script.Subs))
			}
			for i, seg := range script.Segments {
				if len(seg.Subs) != 0 {
					t.Errorf("Parse(%q) segment %d subs = %#v, want none", tt.command, i, viewSubs(seg.Subs))
				}
			}
		})
	}
}

// TestParseHeaderArithmeticStaysInNestedCommand checks that a command substitution
// in a header runs its commands as ordinary segments: the arithmetic in its
// words is read through the Word text, not exposed again as a substitution.
func TestParseHeaderArithmeticStaysInNestedCommand(t *testing.T) {
	t.Parallel()
	script, err := shellparse.Parse("for x in $(echo $((printenv))); do echo $x; done")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(script.Subs) != 1 || len(script.Subs[0].Segments) != 1 {
		t.Fatalf("Parse header subs = %#v, want one substitution with one segment", viewSubs(script.Subs))
	}
	nested := script.Subs[0]
	if got := viewSubs(nested.Segments[0].Subs); len(got) != 0 {
		t.Errorf("nested segment subs = %#v, want none", got)
	}
	if got := viewSubs(nested.Subs); len(got) != 0 {
		t.Errorf("nested header subs = %#v, want none", got)
	}
}

// TestParseSubstitutionBackquoteFlag checks that a substitution carries Backquote
// when it was written with backquotes and not when it was written as "$(...)",
// in a construct header and in a command word alike.
func TestParseSubstitutionBackquoteFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []subTree
	}{
		{name: "[[ ]] backquote", command: "[[ `printenv` ]]", want: []subTree{{Raw: "printenv", Backquote: true}}},
		{name: "[[ ]] dollar-paren", command: "[[ $(printenv) ]]", want: []subTree{{Raw: "printenv"}}},
		{name: "case word backquote", command: "case `printenv` in *) ;; esac", want: []subTree{{Raw: "printenv", Backquote: true}}},
		{name: "for-in backquote", command: "for x in `printenv`; do echo $x; done", want: []subTree{{Raw: "printenv", Backquote: true}}},
		{
			name:    "escaped backquote inside dollar-paren in a for-in list",
			command: "for x in $(echo \\`printenv\\`); do echo $x; done",
			want:    []subTree{{Raw: "echo \\`printenv\\`"}},
		},
		{
			name:    "escaped backquote inside dollar-paren in a [[ ]]",
			command: "[[ $(echo \\`printenv\\`) ]]",
			want:    []subTree{{Raw: "echo \\`printenv\\`"}},
		},
		{
			name:    "escaped backquote inside dollar-paren in a case word",
			command: "case $(echo \\`printenv\\`) in *) ;; esac",
			want:    []subTree{{Raw: "echo \\`printenv\\`"}},
		},
		{
			name:    "backquote inside a backquote in a for-in list",
			command: "for x in `echo \\`printenv\\``; do echo $x; done",
			want:    []subTree{{Raw: "echo \\`printenv\\`", Backquote: true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if got := viewSubs(script.Subs); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) header subs = %#v, want %#v", tt.command, got, tt.want)
			}
		})
	}
}

// TestParseSegmentSubstitutionBackquoteFlag checks the flag on a substitution in
// a command word, which the existing Subs of a segment carry.
func TestParseSegmentSubstitutionBackquoteFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []subTree
	}{
		{name: "backquote", command: "echo `date`", want: []subTree{{Raw: "date", Backquote: true}}},
		{name: "dollar-paren", command: "echo $(date)", want: []subTree{{Raw: "date"}}},
		{name: "process substitution", command: "diff <(ls a)", want: []subTree{{Raw: "ls a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if len(script.Segments) == 0 {
				t.Fatalf("Parse(%q) has no segments", tt.command)
			}
			if got := viewSubs(script.Segments[0].Subs); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) segment subs = %#v, want %#v", tt.command, got, tt.want)
			}
		})
	}
}

// TestParseHeaderArithmeticUnreadable checks that a header arithmetic expansion
// the parser cannot read is refused with the unreadable-command error, and does
// not panic.
func TestParseHeaderArithmeticUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{name: "[[ ]] unterminated", command: "[[ $((printenv) ]]"},
		{name: "for-in unterminated", command: "for x in $((1+2); do echo $x; done"},
		{name: "case word unterminated", command: "case $((printenv in *) ;; esac"},
		{name: "nested unterminated", command: "[[ $((1+$((printenv))) ]]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := shellparse.Parse(tt.command)
			var unparseable *shellparse.UnparseableError
			if !errors.As(err, &unparseable) {
				t.Errorf("Parse(%q) error = %v, want *UnparseableError", tt.command, err)
			}
		})
	}
}
