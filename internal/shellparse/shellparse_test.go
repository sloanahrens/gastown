package shellparse_test

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/shellparse"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// heredocView is the test's view of one here-document.
type heredocView struct {
	Delimiter string
	Quoted    bool
	Body      string
}

// segmentView is the test's view of one command site.
type segmentView struct {
	Words    []string
	Assigns  []string
	Redirs   []string
	Heredocs []heredocView
	Subs     int
}

// summarize turns a parsed Script into the values the table asserts.
func summarize(script shellparse.Script) []segmentView {
	var out []segmentView
	for _, seg := range script.Segments {
		view := segmentView{Subs: len(seg.Subs)}
		for _, w := range seg.Words {
			view.Words = append(view.Words, w.Text)
		}
		for _, a := range seg.Assigns {
			view.Assigns = append(view.Assigns, a.Text)
		}
		for _, r := range seg.Redirs {
			view.Redirs = append(view.Redirs, r.N+r.Op+r.Word.Text)
		}
		for _, h := range seg.Heredocs {
			view.Heredocs = append(view.Heredocs, heredocView{h.Delimiter, h.Quoted, h.Body})
		}
		out = append(out, view)
	}
	return out
}

func TestParseConstructs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		command  string
		want     []segmentView
		wantSubs int
	}{
		{
			name:    "pipeline",
			command: "echo a | grep b",
			want:    []segmentView{{Words: []string{"echo", "a"}}, {Words: []string{"grep", "b"}}},
		},
		{
			name:    "and list",
			command: "echo a && echo b",
			want:    []segmentView{{Words: []string{"echo", "a"}}, {Words: []string{"echo", "b"}}},
		},
		{
			name:    "or list",
			command: "echo a || echo b",
			want:    []segmentView{{Words: []string{"echo", "a"}}, {Words: []string{"echo", "b"}}},
		},
		{
			name:    "semicolon list",
			command: "echo a; echo b",
			want:    []segmentView{{Words: []string{"echo", "a"}}, {Words: []string{"echo", "b"}}},
		},
		{
			name:    "newline list",
			command: "echo a\necho b",
			want:    []segmentView{{Words: []string{"echo", "a"}}, {Words: []string{"echo", "b"}}},
		},
		{
			name:    "subshell",
			command: "(cd /src; echo x)",
			want:    []segmentView{{Words: []string{"cd", "/src"}}, {Words: []string{"echo", "x"}}},
		},
		{
			name:    "brace group",
			command: "{ echo z; }",
			want:    []segmentView{{Words: []string{"echo", "z"}}},
		},
		{
			name:    "if body",
			command: "if true; then echo y; else echo n; fi",
			want: []segmentView{
				{Words: []string{"true"}},
				{Words: []string{"echo", "y"}},
				{Words: []string{"echo", "n"}},
			},
		},
		{
			name:    "for body",
			command: "for i in 1 2; do echo $i; done",
			want:    []segmentView{{Words: []string{"echo", "$i"}}},
		},
		{
			name:    "while body",
			command: "while true; do break; done",
			want:    []segmentView{{Words: []string{"true"}}, {Words: []string{"break"}}},
		},
		{
			name:    "case body",
			command: "case x in a) echo y;; esac",
			want:    []segmentView{{Words: []string{"echo", "y"}}},
		},
		{
			name:    "command substitution",
			command: "echo $(date)",
			want:    []segmentView{{Words: []string{"echo", "$(date)"}, Subs: 1}},
		},
		{
			name:    "backtick substitution",
			command: "echo `date`",
			want:    []segmentView{{Words: []string{"echo", "`date`"}, Subs: 1}},
		},
		{
			name:    "process substitution",
			command: "diff <(ls a) >(wc)",
			want:    []segmentView{{Words: []string{"diff", "<(ls a)", ">(wc)"}, Subs: 2}},
		},
		{
			name:    "here document unquoted delimiter",
			command: "cat <<EOF\nhello $USER\nEOF\n",
			want: []segmentView{{
				Words:    []string{"cat"},
				Heredocs: []heredocView{{Delimiter: "EOF", Quoted: false, Body: "hello $USER\n"}},
			}},
		},
		{
			name:    "here document quoted delimiter",
			command: "cat <<'EOF'\nhello $USER\nEOF\n",
			want: []segmentView{{
				Words:    []string{"cat"},
				Heredocs: []heredocView{{Delimiter: "EOF", Quoted: true, Body: "hello $USER\n"}},
			}},
		},
		{
			// bash strips every leading tab of the body and of the delimiter
			// line, so this program's body is "hello\n".
			name:    "here document dash strips tabs",
			command: "cat <<-EOF\n\t\thello\n\tEOF\n",
			want: []segmentView{{
				Words:    []string{"cat"},
				Heredocs: []heredocView{{Delimiter: "EOF", Quoted: false, Body: "hello\n"}},
			}},
		},
		{
			name:    "here string",
			command: `cat <<< "hi"`,
			want:    []segmentView{{Words: []string{"cat"}, Redirs: []string{"<<<hi"}}},
		},
		{
			name:    "redirections",
			command: "echo hi > out 2>&1 < in",
			want: []segmentView{{
				Words:  []string{"echo", "hi"},
				Redirs: []string{">out", "2>&1", "<in"},
			}},
		},
		{
			name:    "quoting",
			command: `echo 'a b' "c d" $'e\nf'`,
			want:    []segmentView{{Words: []string{"echo", "a b", "c d", "e\nf"}}},
		},
		{
			name:    "backslash newline",
			command: "echo a\\\nb",
			want:    []segmentView{{Words: []string{"echo", "ab"}}},
		},
		{
			name:    "leading assignments",
			command: "FOO=1 BAR=$x cmd arg",
			want:    []segmentView{{Assigns: []string{"FOO=1", "BAR=$x"}, Words: []string{"cmd", "arg"}}},
		},
		{
			name:    "function definition",
			command: "f() { echo hi; }",
			want:    []segmentView{{Words: []string{"echo", "hi"}}},
		},
		{
			name:    "comment",
			command: "# rm -rf /\nrm safe",
			want:    []segmentView{{Words: []string{"rm", "safe"}}},
		},
		{
			name:     "substitution in a for header",
			command:  "for i in $(seq 3); do echo $i; done",
			want:     []segmentView{{Words: []string{"echo", "$i"}}},
			wantSubs: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			got := summarize(script)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) segments = %#v, want %#v", tt.command, got, tt.want)
			}
			if len(script.Subs) != tt.wantSubs {
				t.Errorf("Parse(%q) script subs = %d, want %d", tt.command, len(script.Subs), tt.wantSubs)
			}
		})
	}
}

// TestParseHeredocBodies checks each body against what bash passes for the same
// fixture. An unquoted delimiter decodes only an escaped backslash, dollar or
// backtick and a line continuation; a quoted delimiter decodes nothing; <<-
// strips the leading tabs from the raw source before either rule runs.
func TestParseHeredocBodies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "unquoted keeps backslash quote",
			command: "cat <<EOF\nsay \\\"hi\\\" and \\'bye\\'\nEOF\n",
			want:    "say \\\"hi\\\" and \\'bye\\'\n",
		},
		{
			name:    "unquoted decodes escaped backslash",
			command: "cat <<EOF\na\\\\b\nEOF\n",
			want:    "a\\b\n",
		},
		{
			name:    "unquoted decodes escaped dollar",
			command: "cat <<EOF\n\\$HOME\nEOF\n",
			want:    "$HOME\n",
		},
		{
			name:    "unquoted decodes escaped backtick",
			command: "cat <<EOF\n\\`x\\`\nEOF\n",
			want:    "`x`\n",
		},
		{
			name:    "unquoted line continuation",
			command: "cat <<EOF\nab\\\ncd\nEOF\n",
			want:    "abcd\n",
		},
		{
			name:    "quoted delimiter decodes nothing",
			command: "cat <<'EOF'\n\\$HOME \\\"hi\\\"\nEOF\n",
			want:    "\\$HOME \\\"hi\\\"\n",
		},
		{
			name:    "dash strips tabs before decoding",
			command: "cat <<-EOF\n\t\tx\\\\y\n\tEOF\n",
			want:    "x\\y\n",
		},
		{
			name:    "dash quoted delimiter strips tabs and decodes nothing",
			command: "cat <<-'EOF'\n\t\tx\\\\y\n\tEOF\n",
			want:    "x\\\\y\n",
		},
		{
			// bash strips the first line's tabs, joins the continuation, and
			// keeps the tabs the second line starts with.
			name:    "dash keeps tabs after a line continuation",
			command: "cat <<-EOF\nab\\\n\tcd\nEOF\n",
			want:    "ab\tcd\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if len(script.Segments) != 1 || len(script.Segments[0].Heredocs) != 1 {
				t.Fatalf("Parse(%q) has no here-document", tt.command)
			}
			if got := script.Segments[0].Heredocs[0].Body; got != tt.want {
				t.Errorf("Parse(%q) body = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

// redirView is the test's view of one redirection on a segment.
type redirView struct {
	Op        string
	Target    string
	Inherited bool
}

func TestParseCompoundRedirections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    [][]redirView
	}{
		{
			name:    "subshell",
			command: "( a; b ) > f",
			want: [][]redirView{
				{{Op: ">", Target: "f", Inherited: true}},
				{{Op: ">", Target: "f", Inherited: true}},
			},
		},
		{
			name:    "brace group",
			command: "{ a; b; } 2>&1",
			want: [][]redirView{
				{{Op: ">&", Target: "1", Inherited: true}},
				{{Op: ">&", Target: "1", Inherited: true}},
			},
		},
		{
			name:    "while body",
			command: "while c; do d; done < f",
			want: [][]redirView{
				{{Op: "<", Target: "f", Inherited: true}},
				{{Op: "<", Target: "f", Inherited: true}},
			},
		},
		{
			name:    "function body",
			command: "f() { a; b; } > out",
			want: [][]redirView{
				{{Op: ">", Target: "out", Inherited: true}},
				{{Op: ">", Target: "out", Inherited: true}},
			},
		},
		{
			name:    "if body",
			command: "if c; then d; fi > out",
			want: [][]redirView{
				{{Op: ">", Target: "out", Inherited: true}},
				{{Op: ">", Target: "out", Inherited: true}},
			},
		},
		{
			name:    "simple command keeps its own",
			command: "echo hi > out",
			want:    [][]redirView{{{Op: ">", Target: "out", Inherited: false}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if len(script.Segments) != len(tt.want) {
				t.Fatalf("Parse(%q) has %d segments, want %d", tt.command, len(script.Segments), len(tt.want))
			}
			for i, seg := range script.Segments {
				var got []redirView
				for _, r := range seg.Redirs {
					got = append(got, redirView{r.Op, r.Word.Text, r.Inherited})
				}
				if !reflect.DeepEqual(got, tt.want[i]) {
					t.Errorf("Parse(%q) segment %d redirs = %#v, want %#v", tt.command, i, got, tt.want[i])
				}
			}
		})
	}
}

func TestParseWordText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		word    int
		text    string
		literal bool
	}{
		{name: "single quotes", command: `echo 'a b'`, word: 1, text: "a b", literal: true},
		{name: "double quote escapes", command: "echo \"a\\\"b\\\\c\\$d\\`e\"", word: 1, text: "a\"b\\c$d`e", literal: true},
		{name: "backslash newline", command: "echo a\\\nb", word: 1, text: "ab", literal: true},
		{name: "unquoted escape", command: `echo a\ b`, word: 1, text: "a b", literal: true},
		{name: "ansi-c decoded", command: `echo $'a\tb\x41é'`, word: 1, text: "a\tbAé", literal: true},
		{name: "ansi-c unicode escape", command: `echo $'a\tb\x41` + `\` + `u00e9'`, word: 1, text: "a\tbAé", literal: true},
		{name: "ansi-c control", command: `echo $'\cA'`, word: 1, text: "\x01", literal: true},
		{name: "ansi-c octal", command: `echo $'\101'`, word: 1, text: "A", literal: true},
		{name: "double quoted expansion", command: `echo "$HOME/x"`, word: 1, text: "$HOME/x", literal: false},
		{name: "command substitution", command: "echo $(cmd)", word: 1, text: "$(cmd)", literal: false},
		{name: "single quoted dollar", command: `echo '$HOME'`, word: 1, text: "$HOME", literal: true},
		{name: "plain word", command: `echo plain`, word: 1, text: "plain", literal: true},
		{name: "double quoted plain", command: `echo "plain"`, word: 1, text: "plain", literal: true},
		{name: "invalid ansi-c escape", command: `echo $'bad\q'`, word: 1, text: `bad\q`, literal: false},
		{name: "invalid unicode escape", command: `echo $'\uD800'`, word: 1, text: `\uD800`, literal: false},
		{name: "tilde", command: `echo ~`, word: 1, text: "~", literal: false},
		{name: "tilde path", command: `echo ~/x`, word: 1, text: "~/x", literal: false},
		{name: "tilde user", command: `echo ~user`, word: 1, text: "~user", literal: false},
		{name: "tilde inside a word", command: `echo a~b`, word: 1, text: "a~b", literal: true},
		{name: "single quoted tilde", command: `echo '~'`, word: 1, text: "~", literal: true},
		{name: "escaped tilde", command: `echo \~`, word: 1, text: "~", literal: true},
		{name: "glob star", command: `echo *.go`, word: 1, text: "*.go", literal: false},
		{name: "glob question", command: `echo a?b`, word: 1, text: "a?b", literal: false},
		{name: "glob bracket", command: `echo [ab]c`, word: 1, text: "[ab]c", literal: false},
		{name: "unmatched bracket", command: `echo [ab`, word: 1, text: "[ab", literal: true},
		{name: "single quoted glob", command: `echo '*'`, word: 1, text: "*", literal: true},
		{name: "double quoted glob", command: `echo "*"`, word: 1, text: "*", literal: true},
		{name: "escaped glob", command: `echo \*`, word: 1, text: "*", literal: true},
		{name: "brace list", command: `echo {a,b}`, word: 1, text: "{a,b}", literal: false},
		{name: "brace sequence", command: `echo {1..3}`, word: 1, text: "{1..3}", literal: false},
		{name: "brace without expansion", command: `echo {a}`, word: 1, text: "{a}", literal: true},
		{name: "quoted brace", command: `echo '{a,b}'`, word: 1, text: "{a,b}", literal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if len(script.Segments) != 1 || len(script.Segments[0].Words) <= tt.word {
				t.Fatalf("Parse(%q) has no word %d", tt.command, tt.word)
			}
			word := script.Segments[0].Words[tt.word]
			if word.Text != tt.text {
				t.Errorf("Parse(%q) word %d Text = %q, want %q", tt.command, tt.word, word.Text, tt.text)
			}
			if word.Literal != tt.literal {
				t.Errorf("Parse(%q) word %d Literal = %t, want %t", tt.command, tt.word, word.Literal, tt.literal)
			}
		})
	}
}

func TestParseWordRaw(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		word    int
		want    string
		assign  bool
	}{
		{name: "backslash newline kept", command: "echo a\\\nb", word: 1, want: "a\\\nb"},
		{name: "quotes kept", command: `echo 'a b'`, word: 1, want: "'a b'"},
		{name: "expansion kept", command: "echo $(date)", word: 1, want: "$(date)"},
		{name: "assignment kept", command: "FOO=$(x) cmd", word: 0, want: "FOO=$(x)", assign: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			var got string
			if tt.assign {
				got = script.Segments[0].Assigns[0].Raw
			} else {
				got = script.Segments[0].Words[tt.word].Raw
			}
			if got != tt.want {
				t.Errorf("Parse(%q) Raw = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestParseUnparseableNamesConstruct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    string
		// anyPhrase holds the contract for a command whose parser message this
		// package does not classify by name: a typed error with a phrase of ours
		// and no partial script. Pinning the phrase there would pin the parser's
		// wording instead; TestConstructOfUnknownError pins the fallback phrase.
		anyPhrase bool
	}{
		{name: "unterminated here document", command: "cat <<EOF\nbody\n", want: "unterminated here-document"},
		{name: "unbalanced parenthesis", command: "(echo x", want: "unbalanced parenthesis"},
		{name: "stray closing parenthesis", command: "echo )", want: "unbalanced parenthesis"},
		{name: "unterminated single quote", command: "echo 'oops", want: "unterminated quote"},
		{name: "unterminated double quote", command: `echo "oops`, want: "unterminated quote"},
		{name: "unterminated single quote after a CR LF comment line", command: "echo 'x # n \\\r\nls", want: "unterminated quote"},
		{name: "comment backslash before CR LF", command: "bash -c 'git reset --hard' # n \\\r\nls", want: "comment ending in a backslash before a CR LF line end"},
		{name: "unterminated command substitution", command: "echo $(echo", want: "unterminated command substitution"},
		{name: "unterminated parameter expansion", command: "echo ${x", want: "unterminated parameter expansion"},
		{name: "unterminated process substitution", command: "echo <(x", want: "unterminated process substitution"},
		{name: "unbalanced brace", command: "f() { echo x", want: "unbalanced brace"},
		{name: "unterminated if statement", command: "if true; then echo x", want: "unterminated if statement"},
		{name: "unterminated case statement", command: "case x in", want: "unterminated case statement"},
		{name: "unterminated loop", command: "for i in 1; do echo x", want: "unterminated loop"},
		{name: "incomplete command", command: "echo a |", want: "incomplete command"},
		{name: "syntax outside bash", command: "echo ${(f)x}", want: "shell syntax outside bash"},
		{name: "unrecognized syntax", command: "coproc", anyPhrase: true},
		{name: "ansi-c escape decoding to nul", command: `echo $'a\0b'`, want: "ANSI-C escape that decodes to a NUL byte"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want *UnparseableError", tt.command)
			}
			var unparseable *shellparse.UnparseableError
			if !errors.As(err, &unparseable) {
				t.Fatalf("Parse(%q) error = %T %v, want *UnparseableError", tt.command, err, err)
			}
			if tt.anyPhrase {
				if unparseable.Construct == "" {
					t.Errorf("Parse(%q) Construct is empty, want a phrase of ours", tt.command)
				}
			} else if unparseable.Construct != tt.want {
				t.Errorf("Parse(%q) Construct = %q, want %q", tt.command, unparseable.Construct, tt.want)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse(%q) error %q does not name %q", tt.command, err.Error(), tt.want)
			}
			if len(script.Segments) != 0 || len(script.Subs) != 0 {
				t.Errorf("Parse(%q) returned a partial script: %#v", tt.command, script)
			}
		})
	}
}

func TestParseNestedSubstitutions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		// want is the command name of each nested script, outermost first.
		want []string
	}{
		{
			name:    "from the command name",
			command: "$(printf deep) arg",
			want:    []string{"printf"},
		},
		{
			name:    "from an argument",
			command: "echo $(printf deep)",
			want:    []string{"printf"},
		},
		{
			name:    "from an assignment value",
			command: "X=$(printf deep) cmd",
			want:    []string{"printf"},
		},
		{
			name:    "from a redirection target",
			command: "echo > $(printf deep)",
			want:    []string{"printf"},
		},
		{
			name:    "from a here-document body",
			command: "cat <<EOF\n$(printf deep)\nEOF\n",
			want:    []string{"printf"},
		},
		{
			name: "through a double quoted word in a here-document fed script",
			command: `bash <<OUTER
bash -c "echo $(echo "$(echo "$(echo "$(printf deep)")")")"
OUTER
`,
			want: []string{"echo", "echo", "echo", "printf"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			for level, want := range tt.want {
				if len(script.Segments) == 0 {
					t.Fatalf("level %d: no segment", level)
				}
				subs := script.Segments[0].Subs
				if len(subs) != 1 {
					t.Fatalf("level %d: subs = %d, want 1", level, len(subs))
				}
				script = subs[0]
				if len(script.Segments) == 0 || len(script.Segments[0].Words) < 2 {
					t.Fatalf("level %d: no segment with a command and an argument", level)
				}
				words := script.Segments[0].Words
				if words[0].Text != want {
					t.Fatalf("level %d: command = %q, want %q", level, words[0].Text, want)
				}
				if level == len(tt.want)-1 && words[1].Text != "deep" {
					t.Fatalf("deepest level: argument = %q, want %q", words[1].Text, "deep")
				}
			}
		})
	}
}

func TestParseNestedSubstitutionDepth(t *testing.T) {
	t.Parallel()
	script, err := shellparse.Parse("echo $(echo $(echo $(echo $(printf deep))))")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"echo", "echo", "echo", "echo", "printf"}
	for level, command := range want {
		if len(script.Segments) != 1 || len(script.Segments[0].Words) == 0 {
			t.Fatalf("level %d: no segment with words", level)
		}
		words := script.Segments[0].Words
		if words[0].Text != command {
			t.Fatalf("level %d: command = %q, want %q", level, words[0].Text, command)
		}
		if level == len(want)-1 {
			if len(words) != 2 || words[1].Text != "deep" {
				t.Fatalf("deepest segment words = %#v, want [printf deep]", words)
			}
			if len(script.Segments[0].Subs) != 0 {
				t.Fatalf("deepest segment has %d subs, want 0", len(script.Segments[0].Subs))
			}
			return
		}
		if len(script.Segments[0].Subs) != 1 {
			t.Fatalf("level %d: subs = %d, want 1", level, len(script.Segments[0].Subs))
		}
		script = script.Segments[0].Subs[0]
	}
}

// TestParseLineStructure checks the operator that ends each segment and the
// subshells enclosing it, the two facts a guard needs to put the segments back
// in the shell line it judges directory changes on.
func TestParseLineStructure(t *testing.T) {
	t.Parallel()
	type lineSegment struct {
		after  string
		shells []int
	}
	tests := []struct {
		name    string
		command string
		want    []lineSegment
	}{
		{
			name:    "and list and pipeline",
			command: "echo a && echo b | grep c",
			want:    []lineSegment{{after: "&&"}, {after: "|"}, {}},
		},
		{
			name:    "semicolon and background",
			command: "echo a; echo b & echo c",
			want:    []lineSegment{{after: ";"}, {after: "&"}, {}},
		},
		{
			name:    "newline reads as a semicolon",
			command: "echo a\necho b",
			want:    []lineSegment{{after: ";"}, {}},
		},
		{
			name:    "subshell",
			command: "(echo a; echo b) | cat",
			want:    []lineSegment{{after: ";", shells: []int{0}}, {after: "|", shells: []int{0}}, {}},
		},
		{
			name:    "two subshells are different groups",
			command: "echo a; (echo b); (echo c)",
			want:    []lineSegment{{after: ";"}, {after: ";", shells: []int{0}}, {shells: []int{1}}},
		},
		{
			name:    "nested subshells",
			command: "(echo a; (echo b))",
			want:    []lineSegment{{after: ";", shells: []int{0}}, {shells: []int{0, 1}}},
		},
		{
			name:    "brace group is not a subshell",
			command: "{ echo a; echo b; }",
			want:    []lineSegment{{after: ";"}, {}},
		},
		{
			name:    "if condition then else",
			command: "if cd x; then echo a; else echo b; fi",
			want:    []lineSegment{{after: ";"}, {after: ";"}, {}},
		},
		{
			name:    "while condition and body",
			command: "while true; do echo a; done",
			want:    []lineSegment{{after: ";"}, {}},
		},
		{
			name:    "substitution runs in a shell of its own",
			command: "(echo $(echo a; echo b))",
			want:    []lineSegment{{shells: []int{0}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if len(script.Segments) != len(tt.want) {
				t.Fatalf("Parse(%q) segments = %d, want %d", tt.command, len(script.Segments), len(tt.want))
			}
			for i, seg := range script.Segments {
				if seg.After != tt.want[i].after {
					t.Errorf("Parse(%q) segment %d after = %q, want %q", tt.command, i, seg.After, tt.want[i].after)
				}
				if !reflect.DeepEqual(seg.Shells, tt.want[i].shells) && (len(seg.Shells) != 0 || len(tt.want[i].shells) != 0) {
					t.Errorf("Parse(%q) segment %d shells = %v, want %v", tt.command, i, seg.Shells, tt.want[i].shells)
				}
			}
		})
	}
}

// TestParseSourceOffsets checks the byte offsets the walk records for words and
// redirections: a here-string's word run and a target glued to its operator are
// read from them.
func TestParseSourceOffsets(t *testing.T) {
	t.Parallel()
	script, err := shellparse.Parse("echo a b")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	words := script.Segments[0].Words
	for i, want := range []int{0, 5, 7} {
		if words[i].Src != want {
			t.Errorf("word %d Src = %d, want %d", i, words[i].Src, want)
		}
	}
}

func TestParseRedirectionOffsets(t *testing.T) {
	t.Parallel()
	command := "bash <<< make test"
	script, err := shellparse.Parse(command)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	redir := script.Segments[0].Redirs[0]
	if want := strings.Index(command, "make"); redir.Src != want {
		t.Errorf("redirection Src = %d, want %d", redir.Src, want)
	}
	if want := strings.Index(command, "<<<"); redir.OpSrc != want {
		t.Errorf("redirection OpSrc = %d, want %d", redir.OpSrc, want)
	}
	if want := strings.Index(command, "test"); script.Segments[0].Words[1].Src != want {
		t.Errorf("word after the operator Src = %d, want %d", script.Segments[0].Words[1].Src, want)
	}

	glued := "bash 0<<<'make test'"
	script, err = shellparse.Parse(glued)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	redir = script.Segments[0].Redirs[0]
	if redir.Src != redir.OpSrc+len(redir.Op) {
		t.Errorf("glued target Src = %d, OpSrc = %d, op %q; want the target to start where the operator ends", redir.Src, redir.OpSrc, redir.Op)
	}
}

// TestParseHeredocDescriptor checks the descriptor each here-document is read
// on, which decides whether it feeds a shell its script.
func TestParseHeredocDescriptor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command string
		want    string
	}{
		{"bash <<EOF\ntrue\nEOF\n", ""},
		{"bash 0<<EOF\ntrue\nEOF\n", "0"},
		{"bash 3<<EOF\ntrue\nEOF\n", "3"},
		{"bash {fd}<<EOF\ntrue\nEOF\n", "{fd}"},
	}
	for _, tt := range tests {
		script, err := shellparse.Parse(tt.command)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.command, err)
		}
		if got := script.Segments[0].Heredocs[0].N; got != tt.want {
			t.Errorf("Parse(%q) heredoc N = %q, want %q", tt.command, got, tt.want)
		}
	}
}

// TestParseProcessSubstitutions checks that a word carries each process
// substitution written in it with its parsed body, so a guard need not cut the
// body out of the word text.
func TestParseProcessSubstitutions(t *testing.T) {
	t.Parallel()
	script, err := shellparse.Parse("bash <(echo a; echo b)")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	subs := script.Segments[0].Words[1].ProcSubs
	if len(subs) != 1 || subs[0].Raw != "<(echo a; echo b)" {
		t.Fatalf("ProcSubs = %#v, want one substitution holding <(echo a; echo b)", subs)
	}
	if got := len(subs[0].Script.Segments); got != 2 {
		t.Fatalf("body segments = %d, want 2", got)
	}
	if got := subs[0].Script.Segments[0].Words[0].Text; got != "echo" {
		t.Errorf("body first word = %q, want %q", got, "echo")
	}

	script, err = shellparse.Parse("bash <(a)<(b)")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	subs = script.Segments[0].Words[1].ProcSubs
	if len(subs) != 2 || subs[0].Raw != "<(a)" || subs[1].Raw != "<(b)" {
		t.Errorf("ProcSubs = %#v, want <(a) then <(b)", subs)
	}

	script, err = shellparse.Parse(`bash "<(echo x)"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if subs := script.Segments[0].Words[1].ProcSubs; len(subs) != 0 {
		t.Errorf("quoted process substitution collected %d bodies, want 0", len(subs))
	}
}

// TestParseCommentLineBackslash checks the line structure bash reads when a "#"
// comment ends in a backslash: the comment stops at its newline, so the next
// line is a command of its own. A backslash that is not in a comment still
// continues the line, and a "#" that is not a comment starts no comment.
func TestParseCommentLineBackslash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []segmentView
	}{
		{
			name:    "comment backslash leaves the next line a command",
			command: "bash -c 'git reset --hard' # n \\\nls",
			want: []segmentView{
				{Words: []string{"bash", "-c", "git reset --hard"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "comment backslash ahead of a piped shell",
			command: "echo 'go test ./...' | sh # n \\\nls",
			want: []segmentView{
				{Words: []string{"echo", "go test ./..."}},
				{Words: []string{"sh"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "comment backslash after a shift in arithmetic",
			command: "echo $((1<<2)) # n \\\nls",
			want: []segmentView{
				{Words: []string{"echo", "$((1<<2))"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "carriage return not before a line feed is comment text",
			command: "echo a # n \\\r x\nls",
			want: []segmentView{
				{Words: []string{"echo", "a"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "carriage return and line feed without a backslash ends the comment",
			command: "echo a # n\r\nls",
			want: []segmentView{
				{Words: []string{"echo", "a"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "carriage return at the end of the text is comment text",
			command: "echo a # n \\\r",
			want:    []segmentView{{Words: []string{"echo", "a"}}},
		},
		{
			name:    "backslash outside a comment continues the line",
			command: "echo a\\\nb",
			want:    []segmentView{{Words: []string{"echo", "ab"}}},
		},
		{
			name:    "hash inside a word is not a comment",
			command: "echo a#b \\\nls",
			want:    []segmentView{{Words: []string{"echo", "a#b", "ls"}}},
		},
		{
			name:    "here-string is not a heredoc, so its comment line ends at the newline",
			command: "sh <<< 'git reset --hard' # n \\\nls",
			want: []segmentView{
				{Words: []string{"sh"}, Redirs: []string{"<<<git reset --hard"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "arithmetic command shift does not open a heredoc",
			command: "(( 1<<2 ))\nbash -c 'git reset --hard' # n \\\nls",
			want: []segmentView{
				{Words: []string{"bash", "-c", "git reset --hard"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "heredoc opened in a substitution reads its body at the newline",
			command: "x=$(cat <<EOF)\nit's\nEOF\nbash -c 'git reset --hard' # n \\\nls",
			want: []segmentView{
				{Assigns: []string{"x=$(cat <<EOF)"}, Subs: 1},
				{Words: []string{"bash", "-c", "git reset --hard"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "length expansion is not a comment",
			command: "echo ${#x} \\\nls",
			want:    []segmentView{{Words: []string{"echo", "${#x}", "ls"}}},
		},
		{
			name:    "ANSI-C escaped quote does not close the string",
			command: "echo $'it\\'s' # n \\\nls",
			want: []segmentView{
				{Words: []string{"echo", "it's"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "hash inside double quotes is text",
			command: "echo \"# n \\\nls\"",
			want:    []segmentView{{Words: []string{"echo", "# n ls"}}},
		},
		{
			name:    "quoted hash is text",
			command: "echo '# n \\'\nls",
			want: []segmentView{
				{Words: []string{"echo", "# n \\"}},
				{Words: []string{"ls"}},
			},
		},
		{
			name:    "heredoc body is data, not a comment",
			command: "cat <<'EOF'\n# n \\\nls\nEOF",
			want: []segmentView{{
				Words:    []string{"cat"},
				Heredocs: []heredocView{{Delimiter: "EOF", Quoted: true, Body: "# n \\\nls\n"}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script, err := shellparse.Parse(tt.command)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.command, err)
			}
			if got := summarize(script); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.command, got, tt.want)
			}
		})
	}
}

// TestParseCommentLineBackslashInSubstitution checks the same line structure
// inside a substitution, which runs in a shell of its own: the command after
// the comment's line is the substitution's second command.
func TestParseCommentLineBackslashInSubstitution(t *testing.T) {
	t.Parallel()
	script, err := shellparse.Parse("x=$(bash -c 'git reset --hard' # n \\\nls)")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(script.Segments) != 1 || len(script.Segments[0].Subs) != 1 {
		t.Fatalf("Parse: want one segment with one substitution, got %+v", summarize(script))
	}
	inner := summarize(script.Segments[0].Subs[0])
	want := []segmentView{
		{Words: []string{"bash", "-c", "git reset --hard"}},
		{Words: []string{"ls"}},
	}
	if !reflect.DeepEqual(inner, want) {
		t.Errorf("substitution = %+v, want %+v", inner, want)
	}
}

// TestParseCommentLineBackslashDeepNesting checks that a command nested past
// the comment scan's bound is refused, not read: the scan stops there and
// cannot see a comment past the stop, and mvdan.cc/sh would otherwise read the
// line after that comment as part of it. The command parses on its own, so the
// refusal is the scan's doing.
func TestParseCommentLineBackslashDeepNesting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"comment backslash after a nest past the bound", strings.Repeat("$(", 1000) + "true" + strings.Repeat(")", 1000) + " # n \\\nls"},
		{"comment backslash after a nest of 300", strings.Repeat("$(", 300) + "true" + strings.Repeat(")", 300) + " # n \\\nls"},
		{"comment backslash before CR LF after a nest past the bound", strings.Repeat("$(", 1000) + "true" + strings.Repeat(")", 1000) + " # n \\\r\nls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := shellparse.Parse(tt.command)
			var unreadable *shellparse.UnparseableError
			if !errors.As(err, &unreadable) {
				t.Fatalf("Parse(deep nesting) error = %v, want *UnparseableError", err)
			}
		})
	}
}

// TestParseDeepNestingWithoutCommentBackslash checks that the scan's bound
// refuses only a command that can hide a comment: the same nest with no
// backslash before a newline is read as it was before the comment scan.
func TestParseDeepNestingWithoutCommentBackslash(t *testing.T) {
	t.Parallel()
	command := strings.Repeat("$(", 300) + "true" + strings.Repeat(")", 300) + "\nls"
	if _, err := shellparse.Parse(command); err != nil {
		t.Fatalf("Parse(deep nesting, no comment backslash) error = %v, want nil", err)
	}
}

// TestParseCommentLineBackslashUnreadable checks that the rewrite does not turn
// a command the parser cannot read into one it can. A quote the comment's line
// leaves open is still refused, and so is a comment ending in a backslash on a
// heredoc operator line, which gt-wnblap decided to keep refusing.
func TestParseCommentLineBackslashUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"unterminated quote on the comment line", "echo 'x # n \\\nls"},
		// bash ends the comment at the LF and runs ls; mvdan joins the CR LF into
		// the comment. The scan refuses the shape instead of reading it one way.
		{"comment backslash before CR LF", "bash -c 'git reset --hard' # n \\\r\nls"},
		// The backslash is the comment's only byte, so it sits at p+1, the
		// nearest offset the scan accepts: the refusal must still fire
		// (gt-q0jecy).
		{"comment backslash directly after the hash before CR LF", "bash -c 'git reset --hard' #\\\r\nls"},
		{"comment backslash before CR LF in a piped shell", "echo 'go test ./internal/beads/...' | sh # n \\\r\nls"},
		{"comment backslash before CR LF on a heredoc operator line", "cat <<'EOF' # n \\\r\nEOF\ngit reset --hard\nEOF"},
		{"unterminated quote on a CR LF comment line", "echo 'x # n \\\r\nls"},
		{"comment backslash on a heredoc operator line", "cat <<'EOF' # note \\\nsudo rm -rf /tmp\nEOF"},
		// bash ends the <<- body at the tab-indented terminator on the next line
		// and runs git reset --hard. mvdan reads the joined text as a body that
		// hides the command and parses it, so only the scan's refusal stops it.
		{"comment backslash on a heredoc operator line before a dash terminator", "cat <<-EOF # n \\\n\tEOF\ngit reset --hard\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := shellparse.Parse(tt.command)
			var unreadable *shellparse.UnparseableError
			if !errors.As(err, &unreadable) {
				t.Fatalf("Parse(%q) error = %v, want *UnparseableError", tt.command, err)
			}
		})
	}
}
