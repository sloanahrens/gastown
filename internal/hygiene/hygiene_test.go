package hygiene

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// planted joins pieces so no source line of this file forms a shape the
// scanner reports. The test file must pass its own rules.
func planted(parts ...string) string {
	return strings.Join(parts, "")
}

func singleFile(name, body string) fstest.MapFS {
	return fstest.MapFS{name: &fstest.MapFile{Data: []byte(body)}}
}

func sortedPaths(fsys fstest.MapFS) []string {
	paths := make([]string, 0, len(fsys))
	for name := range fsys {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

func shippedBlocked(t *testing.T) map[string]struct{} {
	t.Helper()
	blocked, err := BlockedWords()
	if err != nil {
		t.Fatalf("BlockedWords: %v", err)
	}
	return blocked
}

func blockedFrom(t *testing.T, words ...string) map[string]struct{} {
	t.Helper()
	var body strings.Builder
	for _, word := range words {
		fmt.Fprintf(&body, "%s\n", HashWord(word))
	}
	blocked, err := LoadBlocked(strings.NewReader(body.String()))
	if err != nil {
		t.Fatalf("LoadBlocked: %v", err)
	}
	return blocked
}

func scanFS(t *testing.T, fsys fstest.MapFS, allow []Allow) []Finding {
	t.Helper()
	findings, err := Scan(fsys, sortedPaths(fsys), DefaultRules(), shippedBlocked(t), allow)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return findings
}

func validate(t *testing.T, fsys fstest.MapFS, allow []Allow) error {
	t.Helper()
	return ValidateAllow(fsys, sortedPaths(fsys), DefaultRules(), shippedBlocked(t), allow)
}

func assertFindings(t *testing.T, got []Finding, want ...Finding) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d findings %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestShapeRulesFirePerRule(t *testing.T) {
	t.Parallel()
	billing := planted("0A1B2C", "-", "3D4E5F", "-", "6A7B8C")
	home := planted("/Users/", "alice", "/project/file.go")
	privateTemp := planted("/private/", "tmp", "/note.txt")
	sessionID := planted("session", "_id: ", "1234abcd", "-", "5678")
	claudeProject := planted(".claude/", "projects/abc/transcript.txt")

	cases := []struct {
		name    string
		line    string
		rule    string
		matched string
	}{
		{"billing id", "charged account " + billing + " today", RuleBillingID, billing},
		{"home directory", "opened " + home, RuleHomePath, home},
		{"private temp", "wrote " + privateTemp, RulePrivateTemp, privateTemp},
		{"session id", "recorded " + sessionID, RuleAgentSession, sessionID},
		{"claude project", "stored " + claudeProject, RuleAgentSession, claudeProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := scanFS(t, singleFile("docs/note.md", "intro\n"+tc.line+"\noutro\n"), nil)
			assertFindings(t, findings, Finding{
				Path:     "docs/note.md",
				Rule:     tc.rule,
				Line:     2,
				LineHash: HashLine(tc.line),
			})
			if strings.Contains(findings[0].String(), tc.matched) {
				t.Errorf("finding %q echoes the matched text", findings[0].String())
			}
		})
	}
}

func TestBlockedWordIsFoundInContentAndInPathsAndNeverEchoed(t *testing.T) {
	t.Parallel()
	word := planted("ahr", "ens")
	blocked := shippedBlocked(t)
	if _, ok := blocked[HashWord(word)]; !ok {
		t.Fatalf("the fixture word is not hashed in blocked.sha256")
	}
	contentLine := "mentions " + word + " in prose"
	path := "docs/" + word + "/note.md"
	fsys := fstest.MapFS{
		"docs/readme.md": {Data: []byte("intro\n" + contentLine + "\n")},
		path:             {Data: []byte("nothing here\n")},
	}

	findings, err := Scan(fsys, sortedPaths(fsys), nil, blocked, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertFindings(t, findings,
		Finding{Path: path, Rule: RuleBlockedWord},
		Finding{Path: "docs/readme.md", Rule: RuleBlockedWord, Line: 2, LineHash: HashLine(contentLine)},
	)
	if strings.Contains(findings[1].String(), word) {
		t.Errorf("content finding %q echoes the blocked word", findings[0].String())
	}
}

func TestBlockedWordMatchesWholeWordsOnly(t *testing.T) {
	t.Parallel()
	blocked := blockedFrom(t, "zebra")
	cases := []struct {
		name string
		line string
		want int
	}{
		{"standalone", "zebra", 1},
		{"after a slash", "crew/zebra", 1},
		{"before a hyphen", "zebra-x", 1},
		{"upper case", "ZEBRA", 1},
		{"with a trailing digit", "zebra2", 0},
		{"with a leading letter", "xzebra", 0},
		{"with a leading digit", "2zebra", 0},
		{"with a trailing letter", "zebras", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := Scan(singleFile("docs/a.md", tc.line+"\n"), []string{"docs/a.md"}, nil, blocked, nil)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(findings) != tc.want {
				t.Fatalf("got %d findings %v, want %d", len(findings), findings, tc.want)
			}
		})
	}

	t.Run("shipped word", func(t *testing.T) {
		word := planted("ahr", "ens")
		blocked := shippedBlocked(t)
		for _, tc := range []struct {
			line string
			want int
		}{
			{word, 1},
			{"crew/" + word, 1},
			{word + "-x", 1},
			{word + "2", 0},
			{"x" + word, 0},
		} {
			findings, err := Scan(singleFile("docs/a.md", tc.line+"\n"), []string{"docs/a.md"}, nil, blocked, nil)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(findings) != tc.want {
				t.Errorf("line %q: got %d findings, want %d", tc.line, len(findings), tc.want)
			}
		}
	})
}

func TestWordBoundaryIsLettersAndDigitsNotUnderscoreOnly(t *testing.T) {
	t.Parallel()
	blocked := blockedFrom(t, "zebra")
	for _, tc := range []struct {
		line string
		want int
	}{
		{"zebra_note", 1},
		{"note_zebra", 1},
		{"zebra-x", 1},
		{"zebra2", 0},
		{"2zebra", 0},
	} {
		findings, err := Scan(singleFile("docs/a.md", tc.line+"\n"), []string{"docs/a.md"}, nil, blocked, nil)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(findings) != tc.want {
			t.Errorf("line %q: got %d findings, want %d", tc.line, len(findings), tc.want)
		}
	}
}

func TestAllowIsScopedToOneLineOfOneFile(t *testing.T) {
	t.Parallel()
	firstID := planted("0A1B2C", "-", "3D4E5F", "-", "6A7B8C")
	secondID := planted("0B1B2C", "-", "3D4E5F", "-", "6A7B8C")
	firstLine := "charged " + firstID
	secondLine := "charged " + secondID
	fsys := fstest.MapFS{
		"docs/one.md": {Data: []byte("intro\n" + firstLine + "\n" + secondLine + "\n")},
		"docs/two.md": {Data: []byte("intro\n" + firstLine + "\n")},
	}
	allow := []Allow{{
		Path:   "docs/one.md",
		Rule:   RuleBillingID,
		Line:   HashLine(firstLine),
		Reason: "fixture billing id, not an account",
	}}

	findings := scanFS(t, fsys, allow)
	assertFindings(t, findings,
		Finding{Path: "docs/one.md", Rule: RuleBillingID, Line: 3, LineHash: HashLine(secondLine)},
		Finding{Path: "docs/two.md", Rule: RuleBillingID, Line: 2, LineHash: HashLine(firstLine)},
	)
}

func TestAFileLevelRowNeverExcusesALineFinding(t *testing.T) {
	t.Parallel()
	line := "charged " + planted("0A1B2C", "-", "3D4E5F", "-", "6A7B8C")
	fsys := singleFile("docs/note.md", "intro\n"+line+"\n")
	allow := []Allow{{Path: "docs/note.md", Rule: RuleBillingID, Reason: "file-level row"}}

	assertFindings(t, scanFS(t, fsys, allow),
		Finding{Path: "docs/note.md", Rule: RuleBillingID, Line: 2, LineHash: HashLine(line)})
	if err := validate(t, fsys, allow); err == nil {
		t.Fatal("ValidateAllow accepted a file-level row for a line finding")
	}
}

func TestEmptyPathAllowCoversNothingAndIsRefused(t *testing.T) {
	t.Parallel()
	line := "charged " + planted("0A1B2C", "-", "3D4E5F", "-", "6A7B8C")
	fsys := singleFile("docs/note.md", "intro\n"+line+"\n")
	allow := []Allow{{Rule: RuleBillingID, Line: HashLine(line), Reason: "empty path"}}

	assertFindings(t, scanFS(t, fsys, allow),
		Finding{Path: "docs/note.md", Rule: RuleBillingID, Line: 2, LineHash: HashLine(line)})
	if err := validate(t, fsys, allow); err == nil {
		t.Fatal("ValidateAllow accepted a row with an empty path")
	}
}

func TestValidateAllowRefusesDrift(t *testing.T) {
	t.Parallel()
	line := "charged " + planted("0A1B2C", "-", "3D4E5F", "-", "6A7B8C")
	fsys := singleFile("docs/note.md", "intro\n"+line+"\n")

	t.Run("valid row", func(t *testing.T) {
		allow := []Allow{{Path: "docs/note.md", Rule: RuleBillingID, Line: HashLine(line), Reason: "fixture id"}}
		if err := validate(t, fsys, allow); err != nil {
			t.Fatalf("ValidateAllow refused a current row: %v", err)
		}
	})

	t.Run("line gone", func(t *testing.T) {
		allow := []Allow{{Path: "docs/note.md", Rule: RuleBillingID, Line: HashLine("a removed line"), Reason: "fixture id"}}
		err := validate(t, fsys, allow)
		if err == nil {
			t.Fatal("ValidateAllow accepted a row whose line is gone")
		}
		if !strings.Contains(err.Error(), "not in the file") {
			t.Errorf("error %q does not say the line is gone", err)
		}
	})

	t.Run("untracked path", func(t *testing.T) {
		allow := []Allow{{Path: "docs/missing.md", Rule: RuleBillingID, Line: HashLine(line), Reason: "fixture id"}}
		if err := validate(t, fsys, allow); err == nil {
			t.Fatal("ValidateAllow accepted a row for an untracked path")
		}
	})

	t.Run("empty reason", func(t *testing.T) {
		allow := []Allow{{Path: "docs/note.md", Rule: RuleBillingID, Line: HashLine(line)}}
		if err := validate(t, fsys, allow); err == nil {
			t.Fatal("ValidateAllow accepted a row with an empty reason")
		}
	})
}

func TestAStaleRowForAFixedLineIsRefusedEvenWhenTheLineIsStillThere(t *testing.T) {
	t.Parallel()
	line := "contact " + planted("alice", "@example.com") + " please"
	body := "intro\n" + line + "\n"
	if !lineHashPresent([]byte(body), HashLine(line)) {
		t.Fatal("fixture line is missing")
	}
	allow := []Allow{{
		Path:   "docs/note.md",
		Rule:   RuleEmail,
		Line:   HashLine(line),
		Reason: "fixture address, not the operator's",
	}}

	err := validate(t, singleFile("docs/note.md", body), allow)
	if err == nil {
		t.Fatal("ValidateAllow accepted a row whose rule no longer fires")
	}
	if !strings.Contains(err.Error(), "does not fire") {
		t.Errorf("error %q does not say the rule stopped firing", err)
	}
}

func TestATrackedBinaryFileIsAFindingUnlessExcusedByPath(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"assets/logo.png":  {Data: []byte("PNG\x00\x01rest")},
		"assets/other.png": {Data: []byte("PNG\x00\x01rest")},
	}
	assertFindings(t, scanFS(t, fsys, nil),
		Finding{Path: "assets/logo.png", Rule: RuleBinaryFile},
		Finding{Path: "assets/other.png", Rule: RuleBinaryFile},
	)

	allow := []Allow{{Path: "assets/logo.png", Rule: RuleBinaryFile, Reason: "fixture image"}}
	assertFindings(t, scanFS(t, fsys, allow),
		Finding{Path: "assets/other.png", Rule: RuleBinaryFile})
	if err := validate(t, fsys, allow); err != nil {
		t.Fatalf("ValidateAllow refused a current binary row: %v", err)
	}
}

func TestForbiddenDirectory(t *testing.T) {
	t.Parallel()
	for _, dir := range ForbiddenDirs {
		for _, path := range []string{dir + "/notes.md", "docs/" + dir + "/notes.md"} {
			t.Run(path, func(t *testing.T) {
				assertFindings(t, scanFS(t, singleFile(path, "clean text\n"), nil),
					Finding{Path: path, Rule: RuleForbiddenDir})
			})
		}
		t.Run(dir+" as a prefix is allowed", func(t *testing.T) {
			path := "docs/" + dir + "-notes/notes.md"
			assertFindings(t, scanFS(t, singleFile(path, "clean text\n"), nil))
		})
	}
}

func TestCleanTreeHasNoFindings(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"main.go":       {Data: []byte("package main\n\nfunc main() {}\n")},
		"README.md":     {Data: []byte("A tool that keeps the home directory tidy.\n")},
		"docs/guide.md": {Data: []byte("Run the session command to start.\n")},
	}
	assertFindings(t, scanFS(t, fsys, nil))
}

func TestAnAddressFollowedByAColonInProseIsAFinding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want int
	}{
		{"reserved example.com", planted("alice", "@example.com"), 0},
		{"subdomain of example.com", planted("bob", "@team.example.com"), 0},
		{"reserved example.org", planted("carol", "@example.org"), 0},
		{"reserved example.net", planted("dave", "@example.net"), 0},
		{"invalid top label", planted("eve", "@host.invalid"), 0},
		{"test top label", planted("frank", "@svc.test"), 0},
		{"example top label", planted("grace", "@host.example"), 0},
		{"localhost", planted("heidi", "@localhost"), 0},
		{"gastown local", planted("ivan", "@gastown.local"), 0},
		{"gastown local subdomain", planted("judy", "@crew.gastown.local"), 0},
		{"sentence period", planted("alice", "@example.com", "."), 0},
		{"numeric last label", planted("bob", "@10.0.0.1"), 0},
		{"credential in a url", planted("user", ":secret@10.0.0.1"), 0},
		{"ssh remote", planted("git", "@github.com:org/repo.git"), 0},
		{"notexample.com", planted("mallory", "@notexample.com"), 1},
		{"example.com inside another domain", planted("niaj", "@example.com.evil.io"), 1},
		{"example.com with a suffix", planted("olivia", "@example.com-x.io"), 1},
		{"invalid as a middle label", planted("peggy", "@x.invalid.io"), 1},
		{"testing.io", planted("sybil", "@testing.io"), 1},
		{"plain address", planted("trent", "@github.com"), 1},
		{"gitx user", planted("gitx", "@github.com:org/repo"), 1},
		{"xgit user", planted("xgit", "@github.com:org/repo"), 1},
		{"writer ssh lookalike", planted("writer", "@github.com:org/repo"), 1},
		{"ssh remote followed by prose", planted("git@", "host: prose"), 1},
		{"address followed by a colon", planted("writer", "@example.com:"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := "contact " + tc.text + " please"
			findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil)
			if len(findings) != tc.want {
				t.Fatalf("got %d findings %v, want %d", len(findings), findings, tc.want)
			}
			if tc.want == 1 && findings[0].Rule != RuleEmail {
				t.Errorf("rule = %q, want %q", findings[0].Rule, RuleEmail)
			}
		})
	}
}

func TestAllowRowsRoundTripThroughJSON(t *testing.T) {
	t.Parallel()
	rows := []Allow{
		{Path: "b.md", Rule: RuleEmail, Line: "0123456789abcdef", Reason: "fixture address"},
		{Path: "a.md", Rule: RuleForbiddenDir, Reason: "stays in this repository"},
	}
	var buf bytes.Buffer
	if err := SaveAllow(&buf, rows); err != nil {
		t.Fatalf("SaveAllow: %v", err)
	}
	if !strings.Contains(buf.String(), `"path"`) || !strings.Contains(buf.String(), `"reason"`) {
		t.Errorf("saved JSON does not use the row field names: %s", buf.String())
	}
	got, err := LoadAllow(&buf)
	if err != nil {
		t.Fatalf("LoadAllow: %v", err)
	}
	want := []Allow{rows[1], rows[0]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

func TestLoadAllowRefusesUnknownFields(t *testing.T) {
	t.Parallel()
	body := `[{"path":"a.md","rule":"email address","reason":"x","excuse":"y"}]`
	if _, err := LoadAllow(strings.NewReader(body)); err == nil {
		t.Fatal("LoadAllow accepted an unknown field")
	}
}

func TestBlockedFileHoldsOnlyHashes(t *testing.T) {
	t.Parallel()
	blocked := shippedBlocked(t)
	if len(blocked) != 4 {
		t.Fatalf("blocked.sha256 has %d entries, want 4", len(blocked))
	}
	for hash := range blocked {
		if len(hash) != 64 || !isHex(hash) {
			t.Errorf("blocked.sha256 holds a non-hash entry")
		}
	}

	for _, body := range []string{"not-a-hash\n", strings.Repeat("a", 63) + "\n", strings.Repeat("g", 64) + "\n"} {
		if _, err := LoadBlocked(strings.NewReader(body)); err == nil {
			t.Errorf("LoadBlocked accepted a malformed line")
		}
	}
	if _, err := LoadBlocked(strings.NewReader("not-a-hash\n")); err != nil && strings.Contains(err.Error(), "not-a-hash") {
		t.Errorf("LoadBlocked error echoes the line: %v", err)
	}

	set, err := LoadBlocked(strings.NewReader("# comment\n\n" + strings.Repeat("a", 64) + "\n"))
	if err != nil {
		t.Fatalf("LoadBlocked refused comments and blank lines: %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("LoadBlocked read %d entries, want 1", len(set))
	}
}

func TestScanRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := Scan(fstest.MapFS{}, []string{"docs/missing.md"}, DefaultRules(), nil, nil); err == nil {
		t.Fatal("Scan reported a missing file as clean")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failure") }

func TestUnreadableAndMalformedInputsAreErrors(t *testing.T) {
	t.Parallel()

	t.Run("blocked list read", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadBlocked(failingReader{}); err == nil {
			t.Fatal("LoadBlocked reported a read failure as an empty list")
		}
	})

	t.Run("allow encode", func(t *testing.T) {
		t.Parallel()
		rows := []Allow{{Path: "a.md", Rule: RuleEmail, Reason: "fixture"}}
		if err := SaveAllow(failingWriter{}, rows); err == nil {
			t.Fatal("SaveAllow reported a write failure as success")
		}
	})

	t.Run("rule without a matcher", func(t *testing.T) {
		t.Parallel()
		fsys := singleFile("docs/a.md", "plain text\n")
		for _, rule := range []Rule{
			{Match: func(string) bool { return false }},
			{Name: "no matcher"},
		} {
			if _, err := Scan(fsys, []string{"docs/a.md"}, []Rule{rule}, nil, nil); err == nil {
				t.Errorf("Scan accepted rule %q", rule.Name)
			}
		}
	})

	t.Run("invalid path", func(t *testing.T) {
		t.Parallel()
		if _, err := Scan(singleFile("docs/a.md", "plain text\n"), []string{"../escape"}, DefaultRules(), nil, nil); err == nil {
			t.Fatal("Scan accepted a path outside its filesystem")
		}
	})

	t.Run("allow row without a rule", func(t *testing.T) {
		t.Parallel()
		allow := []Allow{{Path: "docs/a.md", Reason: "no rule"}}
		if err := validate(t, singleFile("docs/a.md", "plain text\n"), allow); err == nil {
			t.Fatal("ValidateAllow accepted a row with no rule")
		}
	})

	t.Run("allow row for a file that cannot be read", func(t *testing.T) {
		t.Parallel()
		allow := []Allow{{Path: "docs/a.md", Rule: RuleEmail, Line: "0123456789abcdef", Reason: "fixture"}}
		if err := ValidateAllow(fstest.MapFS{}, []string{"docs/a.md"}, DefaultRules(), nil, allow); err == nil {
			t.Fatal("ValidateAllow reported an unreadable file as valid")
		}
	})
}

// TestTheScannerDoesNotFlagItsOwnFiles keeps every rule safe to publish: no
// line of the package's own sources matches one of its rules.
func TestTheScannerDoesNotFlagItsOwnFiles(t *testing.T) {
	t.Parallel()
	blocked := shippedBlocked(t)
	paths := []string{"blocked.sha256", "hygiene.go", "hygiene_test.go"}
	findings, err := Scan(os.DirFS("."), paths, DefaultRules(), blocked, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, finding := range findings {
		t.Errorf("the scanner flags its own file: %s", finding)
	}
}
