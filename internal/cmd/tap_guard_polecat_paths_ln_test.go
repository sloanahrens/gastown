package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPolecatPathGuardLnSymlinkText pins how "ln -s TEXT LINK" is judged: the
// kernel interprets relative TEXT against the directory the link is created
// in, not the session cwd, so a link planted in scratch that points into the
// town must be refused at creation.
func TestPolecatPathGuardLnSymlinkText(t *testing.T) {
	p := newPolecatTestTown(t)
	tmp := polecatSymlinkTownTmp(t, p)
	town, err := filepath.EvalSymlinks(p.town)
	if err != nil {
		t.Fatalf("resolving the town: %v", err)
	}
	mayor := filepath.Join(town, "mayor")
	deepLinkDir := filepath.Join(tmp, "L", "a", "b", "c", "d", "e")
	mustMkdirAll(t, deepLinkDir)
	deepText, err := filepath.Rel(deepLinkDir, filepath.Join(mayor, "PWNED"))
	if err != nil {
		t.Fatalf("computing link text: %v", err)
	}
	// Into an existing directory: the link is created at <tmp>/<base(TEXT)>,
	// so TEXT is relative to <tmp>.
	dirText, err := filepath.Rel(tmp, filepath.Join(mayor, "PWNED"))
	if err != nil {
		t.Fatalf("computing link text: %v", err)
	}
	ownText, err := filepath.Rel(filepath.Join(p.worktree, "sub"), filepath.Join(p.worktree, "x.go"))
	if err != nil {
		t.Fatalf("computing link text: %v", err)
	}
	mustMkdirAll(t, filepath.Join(p.worktree, "sub"))

	cases := []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{"relative text into the town, link in scratch", "ln -s " + deepText + " " + filepath.Join(deepLinkDir, "dl"), true},
		{"relative text into the town, flags clustered", "ln -sfn " + deepText + " " + filepath.Join(deepLinkDir, "dl"), true},
		{"relative text into the town, link dir is an existing directory", "ln -s " + dirText + " " + tmp, true},
		{"relative text into the town via -t", "ln -s -t " + tmp + " " + dirText, true},
		{"relative text into the town via --target-directory=", "ln --symbolic --target-directory=" + tmp + " " + dirText, true},
		{"abbreviated --symbolic", "ln --sym " + deepText + " " + filepath.Join(deepLinkDir, "dl"), true},
		{"abbreviated --target-directory=", "ln -s --target-dir=" + tmp + " " + dirText, true},
		{"abbreviated --target-directory, value as next word", "ln -s --t " + tmp + " " + dirText, true},
		{"relative text inside the worktree", "ln -s " + ownText + " " + filepath.Join(p.worktree, "sub", "link"), false},
		{"relative text into scratch", "ln -s ../x " + filepath.Join(tmp, "L", "link"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.run(t, "Bash", commandInput(tc.command))
			if tc.wantBlock && err == nil {
				t.Errorf("command %q: expected BLOCK, got allow", tc.command)
			}
			if !tc.wantBlock && err != nil {
				t.Errorf("command %q: expected allow, got block: %v", tc.command, err)
			}
		})
	}
	// The cases above assert where the kernel would point each link; prove one
	// of them for real, so the expected text is not just this test's opinion.
	link := filepath.Join(deepLinkDir, "proof")
	mustSymlink(t, deepText, link)
	if err := os.WriteFile(link, []byte("probe\n"), 0o644); err != nil {
		t.Fatalf("kernel write through %s: %v", link, err)
	}
	if _, err := os.Stat(filepath.Join(mayor, "PWNED")); err != nil {
		t.Errorf("link text %q from %s did not reach the mayor dir: %v", deepText, deepLinkDir, err)
	}
	if strings.HasPrefix(deepText, "/") {
		t.Fatalf("link text %q must be relative for this test to mean anything", deepText)
	}
}

func TestParseLnArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args      string
		symbolic  bool
		relative  bool
		targetDir string
		texts     string
		dest      string
	}{
		{"a b", false, false, "", "a", "b"},
		{"-s a b", true, false, "", "a", "b"},
		{"-sfn a b", true, false, "", "a", "b"},
		{"--symbolic --force a b", true, false, "", "a", "b"},
		{"-s a b c dir", true, false, "", "a,b,c", "dir"},
		{"-s a", true, false, "", "a", ""},
		{"-s -t dir a b", true, false, "dir", "a,b", ""},
		{"-stdir a", true, false, "dir", "a", ""},
		{"--target-directory=dir -s a", true, false, "dir", "a", ""},
		{"--target-directory dir -s a", true, false, "dir", "a", ""},
		{"-sS .bak a b", true, false, "", "a", "b"},
		{"--suffix .bak -s a b", true, false, "", "a", "b"},
		{"-sr a b", true, true, "", "a", "b"},
		{"-s -- -a b", true, false, "", "-a", "b"},
		// getopt_long accepts any unambiguous prefix of a long option.
		{"--sym a b", true, false, "", "a", "b"},
		{"--symb --forc a b", true, false, "", "a", "b"},
		{"--rel --sym a b", true, true, "", "a", "b"},
		{"--target-dir=dir -s a", true, false, "dir", "a", ""},
		{"--target=dir -s a", true, false, "dir", "a", ""},
		{"--t dir -s a", true, false, "dir", "a", ""},
		{"-s --suf .bak a b", true, false, "", "a", "b"},
		{"-s --su=.bak a b", true, false, "", "a", "b"},
		// An ambiguous or unknown spelling makes ln exit before creating a link.
		{"--s a b", false, false, "", "a", "b"},
		{"--n a b", false, false, "", "a", "b"},
		{"--bogus a b", false, false, "", "a", "b"},
		// --backup takes its value with "=" only, so the next word is an operand.
		{"--backup -s a b", true, false, "", "a", "b"},
		{"--b=numbered -s a b", true, false, "", "a", "b"},
		{"--no-target-directory -s a b", true, false, "", "a", "b"},
	}
	for _, tc := range cases {
		got := parseLnArgs(strings.Fields(tc.args))
		if got.symbolic != tc.symbolic || got.relative != tc.relative || got.targetDir != tc.targetDir ||
			strings.Join(got.texts, ",") != tc.texts || got.dest != tc.dest {
			t.Errorf("parseLnArgs(%q) = %+v, want symbolic=%v relative=%v targetDir=%q texts=%q dest=%q",
				tc.args, got, tc.symbolic, tc.relative, tc.targetDir, tc.texts, tc.dest)
		}
	}
}

func TestPathDirPart(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"link":          ".",
		"a/link":        "a",
		"a/sym/../link": "a/sym/..",
		"/link":         "/",
		"/a/b/link/":    "/a/b",
		"/":             "/",
	}
	for in, want := range cases {
		if got := pathDirPart(in); got != want {
			t.Errorf("pathDirPart(%q) = %q, want %q", in, got, want)
		}
	}
}
