package presubmit

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// addition is the lines one file gains in a -U0 diff, starting at line start of the new file.
type addition struct {
	path  string
	start int
	lines []string
}

// unifiedDiff writes the -U0 diff that adds each addition to its file, as git diff would.
func unifiedDiff(adds ...addition) string {
	var b strings.Builder
	for _, a := range adds {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", a.path, a.path, a.path, a.path)
		fmt.Fprintf(&b, "@@ -0,0 +%d,%d @@\n", a.start, len(a.lines))
		for _, l := range a.lines {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String()
}

// fakeGit answers the two git commands ChangedDecls runs, from in-memory sources: the diff it is
// given and the file contents at HEAD. A path not in head is an error, so a read of a file the
// test did not expect fails the row.
type fakeGit struct {
	diff     string
	diffErr  error
	head     map[string]string
	showErr  error
	diffArgs []string
}

func (g *fakeGit) run(args []string) ([]byte, error) {
	if len(args) > 0 && args[0] == "diff" {
		g.diffArgs = args
		if g.diffErr != nil {
			return nil, g.diffErr
		}
		return []byte(g.diff), nil
	}
	if len(args) == 2 && args[0] == "show" && strings.HasPrefix(args[1], "HEAD:") {
		if g.showErr != nil {
			return nil, g.showErr
		}
		p := strings.TrimPrefix(args[1], "HEAD:")
		src, ok := g.head[p]
		if !ok {
			return nil, fmt.Errorf("no %s at HEAD", p)
		}
		return []byte(src), nil
	}
	return nil, fmt.Errorf("unexpected git %v", args)
}

const srcAB = "package p\n\nfunc F() {\n\tx := 1\n\t_ = x\n}\n"

func TestChangedDecls(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		diff  string
		head  map[string]string
		want  map[string][]string
		total int
	}{
		{
			name:  "a changed function is collected under its directory and counted once",
			diff:  unifiedDiff(addition{path: "pkg/a.go", start: 4, lines: []string{"\tx := 1", "\t_ = x"}}),
			head:  map[string]string{"pkg/a.go": srcAB},
			want:  map[string][]string{"pkg": {"F"}},
			total: 1,
		},
		{
			name: "a comment line outside every declaration collects nothing",
			diff: unifiedDiff(addition{path: "pkg/a.go", start: 3, lines: []string{"// note"}}),
			head: map[string]string{"pkg/a.go": "package p\n\n// note\n\nfunc F() {}\n"},
			want: map[string][]string{},
		},
		{
			name: "two files in two directories give two keys",
			diff: unifiedDiff(
				addition{path: "a/x.go", start: 3, lines: []string{"\t_ = 1"}},
				addition{path: "b/y.go", start: 3, lines: []string{"\t_ = 2"}},
			),
			head: map[string]string{
				"a/x.go": "package a\n\nfunc A() {\n\t_ = 1\n}\n",
				"b/y.go": "package b\n\nfunc B() {\n\t_ = 2\n}\n",
			},
			want:  map[string][]string{"a": {"A"}, "b": {"B"}},
			total: 2,
		},
		{
			name: "a test file in the diff is not read",
			diff: unifiedDiff(addition{path: "pkg/a_test.go", start: 3, lines: []string{"\t_ = 1"}}),
			head: map[string]string{"pkg/a_test.go": "package p\n\nfunc T() {\n\t_ = 1\n}\n"},
			want: map[string][]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{diff: tt.diff, head: tt.head}
			got, err := ChangedDecls(g.run, "main")
			if err != nil {
				t.Fatalf("ChangedDecls() error = %v", err)
			}
			if !maps.EqualFunc(got.Names, tt.want, slices.Equal) {
				t.Errorf("ChangedDecls() Names = %v, want %v", got.Names, tt.want)
			}
			if got.Declarations != tt.total {
				t.Errorf("ChangedDecls() Declarations = %d, want %d", got.Declarations, tt.total)
			}
		})
	}
}

func TestChangedDeclsRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		git     fakeGit
		base    string
		wantMsg string
	}{
		{
			name: "a runner error from the diff is returned",
			git:  fakeGit{diffErr: fmt.Errorf("boom")},
			base: "main",
		},
		{
			name: "a runner error reading a file at HEAD is returned",
			git: fakeGit{
				diff:    unifiedDiff(addition{path: "pkg/a.go", start: 4, lines: []string{"\t_ = x"}}),
				head:    map[string]string{"pkg/a.go": srcAB},
				showErr: fmt.Errorf("boom"),
			},
			base: "main",
		},
		{
			name: "a file that does not parse is returned",
			git: fakeGit{
				diff: unifiedDiff(addition{path: "pkg/a.go", start: 3, lines: []string{"func f( {"}}),
				head: map[string]string{"pkg/a.go": "package p\n\nfunc f( {\n"},
			},
			base: "main",
		},
		{
			name: "a changed path git quoted is returned and says so",
			git: fakeGit{
				diff: "diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\n" +
					"--- \"a/caf\\303\\251.go\"\n" +
					"+++ \"b/caf\\303\\251.go\"\n" +
					"@@ -0,0 +1 @@\n" +
					"+func F() {}\n",
				head: map[string]string{"café.go": "package p\n\nfunc F() {}\n"},
			},
			base:    "main",
			wantMsg: "quoted",
		},
		{
			name: "an empty base is returned",
			git:  fakeGit{diff: unifiedDiff(addition{path: "pkg/a.go", start: 4, lines: []string{"\t_ = x"}})},
			base: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			got, err := ChangedDecls(g.run, tt.base)
			if err == nil {
				t.Fatalf("ChangedDecls(%q) error = nil, want an error; Names = %v", tt.base, got.Names)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("ChangedDecls(%q) error = %v, want it to mention %q", tt.base, err, tt.wantMsg)
			}
		})
	}
}

func TestChangedDeclsDiffsAgainstBase(t *testing.T) {
	t.Parallel()
	g := &fakeGit{diff: ""}
	if _, err := ChangedDecls(g.run, "origin/main"); err != nil {
		t.Fatalf("ChangedDecls() error = %v", err)
	}
	want := []string{"diff", "--unified=0", "--end-of-options", "origin/main...HEAD", "--", "*.go"}
	if !slices.Equal(g.diffArgs, want) {
		t.Errorf("ChangedDecls() ran git %v, want %v", g.diffArgs, want)
	}
}
