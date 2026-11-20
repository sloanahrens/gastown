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

// deletedHunk is a hunk that removes lines from path and adds none: the lines from start of the
// old file. whole marks a file the diff deletes outright, so the hunk is all of its old content.
type deletedHunk struct {
	path  string
	start int
	lines []string
	whole bool
}

// deleteDiff writes the -U0 diff that removes each hunk's lines, as git diff would.
func deleteDiff(hs ...deletedHunk) string {
	var b strings.Builder
	for _, h := range hs {
		newPath, newRange := fmt.Sprintf("b/%s", h.path), fmt.Sprintf("+%d,0", h.start-1)
		if h.whole {
			newPath, newRange = "/dev/null", "+0,0"
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ %s\n", h.path, h.path, h.path, newPath)
		fmt.Fprintf(&b, "@@ -%d,%d %s @@\n", h.start, len(h.lines), newRange)
		for _, l := range h.lines {
			b.WriteString("-" + l + "\n")
		}
	}
	return b.String()
}

// fakeGit answers the git commands ChangedDecls runs, from in-memory sources: the diff it is
// given, the file contents at HEAD, and the file contents at the merge base. mergeBase is the
// commit "git merge-base" prints; empty prints nothing. A path not in the map is an error, so a
// read of a file the test did not expect fails the row.
type fakeGit struct {
	diff      string
	diffErr   error
	head      map[string]string
	showErr   error
	mergeBase string
	mergeErr  error
	atBase    map[string]string
	baseErr   error
	diffArgs  []string
}

func (g *fakeGit) run(args []string) ([]byte, error) {
	if len(args) > 0 && args[0] == "diff" {
		g.diffArgs = args
		if g.diffErr != nil {
			return nil, g.diffErr
		}
		return []byte(g.diff), nil
	}
	if len(args) > 0 && args[0] == "merge-base" {
		if g.mergeErr != nil {
			return nil, g.mergeErr
		}
		return []byte(g.mergeBase + "\n"), nil
	}
	if len(args) == 2 && args[0] == "show" {
		rev, p, _ := strings.Cut(args[1], ":")
		switch {
		case rev == "HEAD":
			if g.showErr != nil {
				return nil, g.showErr
			}
			return fileAt(g.head, p)
		case rev != "" && rev == g.mergeBase:
			if g.baseErr != nil {
				return nil, g.baseErr
			}
			return fileAt(g.atBase, p)
		}
	}
	return nil, fmt.Errorf("unexpected git %v", args)
}

// fileAt returns the content of p in files, or an error when the file is not there.
func fileAt(files map[string]string, p string) ([]byte, error) {
	src, ok := files[p]
	if !ok {
		return nil, fmt.Errorf("no %s", p)
	}
	return []byte(src), nil
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

func TestChangedDeclsDeleted(t *testing.T) {
	t.Parallel()
	const gone = "package p\n\nfunc Gone() {\n\t_ = 1\n}\n\nfunc Kept() {}\n"
	const between = "package p\n\nfunc a() {}\n\nfunc b() {\n\t_ = 1\n}\n\nfunc c() {}\n"
	const whole = "package p\n\nimport \"fmt\"\n\ntype T struct{}\n\nvar V = 1\n\nfunc F() { fmt.Println() }\n\nfunc (T) M() {}\n"
	tests := []struct {
		name  string
		git   fakeGit
		want  map[string][]string
		total int
	}{
		{
			name: "a pure-deletion hunk that removes a function lists its base name",
			git: fakeGit{
				diff:      deleteDiff(deletedHunk{path: "pkg/a.go", start: 3, lines: []string{"func Gone() {", "\t_ = 1", "}", ""}}),
				head:      map[string]string{"pkg/a.go": "package p\n\nfunc Kept() {}\n"},
				mergeBase: mergeSHA,
				atBase:    map[string]string{"pkg/a.go": gone},
			},
			want:  map[string][]string{"pkg": {"Gone"}},
			total: 1,
		},
		{
			name: "a function deleted between two others names only the deleted one",
			git: fakeGit{
				diff:      deleteDiff(deletedHunk{path: "pkg/a.go", start: 5, lines: []string{"func b() {", "\t_ = 1", "}", ""}}),
				head:      map[string]string{"pkg/a.go": "package p\n\nfunc a() {}\n\nfunc c() {}\n"},
				mergeBase: mergeSHA,
				atBase:    map[string]string{"pkg/a.go": between},
			},
			want:  map[string][]string{"pkg": {"b"}},
			total: 1,
		},
		{
			name: "a file deleted outright lists every top-level declaration it had at the merge base",
			git: fakeGit{
				diff:      deleteDiff(deletedHunk{path: "pkg/gone.go", start: 1, whole: true, lines: strings.Split(strings.TrimSuffix(whole, "\n"), "\n")}),
				mergeBase: mergeSHA,
				atBase:    map[string]string{"pkg/gone.go": whole},
			},
			want:  map[string][]string{"pkg": {"F", "M", "T", "V"}},
			total: 4,
		},
		{
			name: "a replaced function names both the function it removed and the one it adds",
			git: fakeGit{
				diff: "diff --git a/pkg/a.go b/pkg/a.go\n--- a/pkg/a.go\n+++ b/pkg/a.go\n" +
					"@@ -3 +3 @@\n-func G() {}\n+func H() {}\n",
				head:      map[string]string{"pkg/a.go": "package p\n\nfunc H() {}\n\nfunc F() {\n\t_ = 2\n}\n"},
				mergeBase: mergeSHA,
				atBase:    map[string]string{"pkg/a.go": "package p\n\nfunc G() {}\n\nfunc F() {\n\t_ = 2\n}\n"},
			},
			want:  map[string][]string{"pkg": {"G", "H"}},
			total: 2,
		},
		{
			name: "a deleted test file is not read",
			git: fakeGit{
				diff:      deleteDiff(deletedHunk{path: "pkg/a_test.go", start: 3, lines: []string{"func T() {}"}}),
				mergeBase: mergeSHA,
				atBase:    map[string]string{"pkg/a_test.go": "package p\n\nfunc T() {}\n"},
			},
			want: map[string][]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
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

// mergeSHA is the merge base the deletion rows name; a file is read at it only if the code asks
// git for it under this commit.
const mergeSHA = "5e7a1c0"

func TestChangedDeclsDeletedRefuses(t *testing.T) {
	t.Parallel()
	gone := deleteDiff(deletedHunk{path: "pkg/a.go", start: 3, lines: []string{"func Gone() {}"}})
	tests := []struct {
		name    string
		git     fakeGit
		wantMsg string
	}{
		{
			name:    "a failed git merge-base is returned",
			git:     fakeGit{diff: gone, mergeErr: fmt.Errorf("boom")},
			wantMsg: "merge-base",
		},
		{
			name:    "a merge-base that prints nothing is returned",
			git:     fakeGit{diff: gone, atBase: map[string]string{"pkg/a.go": "package p\n\nfunc Gone() {}\n"}},
			wantMsg: "no common ancestor",
		},
		{
			name: "a failed git show of the merge-base file is returned",
			git: fakeGit{
				diff:      gone,
				mergeBase: mergeSHA,
				baseErr:   fmt.Errorf("boom"),
			},
			wantMsg: "at the merge base",
		},
		{
			name: "a file the merge base does not hold is returned",
			git: fakeGit{
				diff:      gone,
				mergeBase: mergeSHA,
				atBase:    map[string]string{},
			},
			wantMsg: "at the merge base",
		},
		{
			name: "a deleted file git quoted is returned and says so",
			git: fakeGit{
				diff: "diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\n" +
					"--- \"a/caf\\303\\251.go\"\n" +
					"+++ /dev/null\n" +
					"@@ -1 +0,0 @@\n" +
					"-func F() {}\n",
				mergeBase: mergeSHA,
			},
			wantMsg: "quoted",
		},
		{
			name: "a removed-line hunk header whose count overflows is returned",
			git: fakeGit{
				diff:      "diff --git a/pkg/a.go b/pkg/a.go\n--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -1,99999999999999999999 +0,0 @@\n-func F() {}\n",
				mergeBase: mergeSHA,
			},
			wantMsg: "unreadable hunk header",
		},
		{
			name: "a merge-base file that does not parse is returned",
			git: fakeGit{
				diff:      deleteDiff(deletedHunk{path: "pkg/a.go", start: 3, lines: []string{"func Gone() {}"}}),
				mergeBase: mergeSHA,
				atBase:    map[string]string{"pkg/a.go": "package p\n\nfunc f( {\n"},
			},
			wantMsg: "at the merge base",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			got, err := ChangedDecls(g.run, "main")
			if err == nil {
				t.Fatalf("ChangedDecls() error = nil, want an error; Names = %v", got.Names)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("ChangedDecls() error = %v, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestChangedDeclsDeletedHunks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		diff    string
		want    []removal
		wantErr bool
	}{
		{
			name: "a pure-deletion hunk lists its old lines",
			diff: deleteDiff(deletedHunk{path: "pkg/a.go", start: 3, lines: []string{"a", "b", "c", "d"}}),
			want: []removal{{path: "pkg/a.go", start: 3, count: 4}},
		},
		{
			name: "a count git leaves out is one line",
			diff: "--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -5 +4,0 @@\n-x\n",
			want: []removal{{path: "pkg/a.go", start: 5, count: 1}},
		},
		{
			name: "a hunk that also adds lines lists its old lines",
			diff: "diff --git a/pkg/a.go b/pkg/a.go\n--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -3 +3 @@\n-func G() {}\n+func H() {}\n",
			want: []removal{{path: "pkg/a.go", start: 3, count: 1}},
		},
		{
			name: "a file deleted outright lists its old path and every line",
			diff: deleteDiff(deletedHunk{path: "pkg/gone.go", start: 1, whole: true, lines: []string{"package p", "", "func F() {}"}}),
			want: []removal{{path: "pkg/gone.go", start: 1, count: 3}},
		},
		{
			name: "a created file lists nothing",
			diff: "--- /dev/null\n+++ b/pkg/new.go\n@@ -0,0 +1,2 @@\n+a\n+b\n",
		},
		{
			name: "a deleted file git quoted lists an empty path",
			diff: "--- \"a/caf\\303\\251.go\"\n+++ /dev/null\n@@ -1 +0,0 @@\n-func F() {}\n",
			want: []removal{{path: "", start: 1, count: 1}},
		},
		{
			name:    "an unreadable hunk header is returned",
			diff:    "--- a/pkg/a.go\n@@ bogus @@\n-x\n",
			wantErr: true,
		},
		{
			name:    "an old start that overflows is returned",
			diff:    "--- a/pkg/a.go\n@@ -99999999999999999999,1 +0,0 @@\n-x\n",
			wantErr: true,
		},
		{
			name:    "an old count that overflows is returned",
			diff:    "--- a/pkg/a.go\n@@ -1,99999999999999999999 +0,0 @@\n-x\n",
			wantErr: true,
		},
		{
			name:    "a new count that overflows is returned",
			diff:    "--- a/pkg/a.go\n@@ -1 +1,99999999999999999999 @@\n",
			wantErr: true,
		},
		{
			name:    "a line over the scanner bound is returned",
			diff:    "--- a/pkg/a.go\n@@ -1 +0,0 @@\n-" + strings.Repeat("x", 2*maxLine) + "\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := removedLines(strings.NewReader(tt.diff))
			if (err != nil) != tt.wantErr {
				t.Fatalf("removedLines() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("removedLines() = %v, want %v", got, tt.want)
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
			name: "a diff with an unreadable hunk header is returned",
			git:  fakeGit{diff: "+++ b/pkg/a.go\n@@ bogus @@\n+func F() {}\n"},
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
	want := []string{"diff", "--unified=0", "--no-renames", "--end-of-options", "origin/main...HEAD", "--", "*.go"}
	if !slices.Equal(g.diffArgs, want) {
		t.Errorf("ChangedDecls() ran git %v, want %v", g.diffArgs, want)
	}
}
