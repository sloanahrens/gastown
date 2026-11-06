package hygiene

import (
	"bytes"
	"embed"
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

// rulesThatNeverMatch is a valid rule set with nothing to report, so a test
// that exercises only the blocklist is not widened by a default rule that
// could add findings.
func rulesThatNeverMatch() []Rule {
	return []Rule{{Name: "test-never-matches", Match: func(string) bool { return false }}}
}

func scanFS(t *testing.T, fsys fstest.MapFS, allow []Allow) []Finding {
	t.Helper()
	findings, _, err := Scan(fsys, sortedPaths(fsys), DefaultRules(), shippedBlocked(t), allow)
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

func TestHomePathNameMustStartWithLetterOrDigit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		want int
	}{
		{"dot config is not an account", planted("/home/", ".config", "/x"), 0},
		{"local directory is not an account", planted("/Users/", ".local", "/x"), 0},
		{"account starting with a letter", planted("/home/", "real", "/x"), 1},
		{"account starting with a digit", planted("/home/", "7even", "/x"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := "opened " + tc.path
			findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil)
			if len(findings) != tc.want {
				t.Fatalf("got %d findings %v, want %d", len(findings), findings, tc.want)
			}
			if tc.want == 1 && findings[0].Rule != RuleHomePath {
				t.Errorf("rule = %q, want %q", findings[0].Rule, RuleHomePath)
			}
		})
	}
}

func TestNonPersonalHomeNamesAreExemptByWholeName(t *testing.T) {
	t.Parallel()

	// The map is the list. This pins the contract its doc comment states — a
	// name documentation, a fixture, CI, the town's service account or the
	// macOS shared directory uses as a placeholder is not a finding — with one
	// name per category rather than a copy of the list, so adding a name does
	// not fail a test that only mirrored data.
	t.Run("documented placeholder categories are exempt", func(t *testing.T) {
		for _, name := range []string{"user", "fixture", "ci", "gt", "Shared"} {
			for _, base := range []string{"/Users/", "/home/"} {
				line := "opened " + planted(base, name, "/project/file.go")
				if findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil); len(findings) != 0 {
					t.Errorf("%q: got %d findings %v, want none", line, len(findings), findings)
				}
			}
		}
	})

	t.Run("every documented name is exempt in both roots", func(t *testing.T) {
		for name := range nonPersonalHomeNames {
			for _, base := range []string{"/Users/", "/home/"} {
				line := "opened " + planted(base, name, "/project/file.go")
				if findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil); len(findings) != 0 {
					t.Errorf("%q: got %d findings %v, want none", line, len(findings), findings)
				}
			}
		}
	})

	t.Run("a lookalike is still an account", func(t *testing.T) {
		cases := []struct{ base, name string }{
			{"/Users/", "sloanx"},
			{"/home/", "usera"},
			{"/home/", "user2"},
			{"/home/", "xop"},
			{"/Users/", "user.evil"},
		}
		for _, tc := range cases {
			line := "opened " + planted(tc.base, tc.name, "/project/file.go")
			findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil)
			if len(findings) != 1 || findings[0].Rule != RuleHomePath {
				t.Errorf("%q: got %v, want one %q finding", line, findings, RuleHomePath)
			}
		}
	})

	t.Run("a second path sharing a separator is still examined", func(t *testing.T) {
		line := "opened " + planted("/home/", "user", "/home/", "real", "/x")
		findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil)
		if len(findings) != 1 || findings[0].Rule != RuleHomePath {
			t.Errorf("%q: got %v, want one %q finding", line, findings, RuleHomePath)
		}
	})

	t.Run("the blocked word still fires on its own name", func(t *testing.T) {
		name := planted("slo", "an")
		line := "opened " + planted("/Users/", name, "/project/file.go")
		findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil)
		var blocked bool
		for _, finding := range findings {
			if finding.Rule == RuleBlockedWord {
				blocked = true
			}
		}
		if !blocked {
			t.Errorf("got %v, want a %q finding", findings, RuleBlockedWord)
		}
	})
}

func TestTitleCaseTopLevelLabelIsStillAnAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		addr string
	}{
		{"title-case top label", planted("jane.doe@", "gmail.Com")},
		{"title-case second label", planted("jane.doe@", "Gmail.Com")},
		{"upper-case top label", planted("jane.doe@", "gmail.COM")},
		{"title-case two-letter top label", planted("jane.doe@", "acme.Io")},
		{"title-case top label outside any list", planted("jane.doe@", "acme.Club")},
		{"title-case short top label outside any list", planted("jane.doe@", "x.Shop")},
		// Func and List are shaped like Shop: a Title-case label is a domain
		// whatever it spells, so these identifier-looking tails are findings.
		{"title-case identifier-looking top label", planted("jane.doe@", "pkg.Func")},
		{"title-case method chain tail", planted("jane.doe@", "landworker.RetryBeads.List")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := "contact " + tc.addr + " please"
			findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil)
			if len(findings) != 1 || findings[0].Rule != RuleEmail {
				t.Fatalf("got %v, want one %q finding", findings, RuleEmail)
			}
		})
	}
}

func TestIdentifierShapedDomainIsNoAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		addr string
		want int
	}{
		// The reverted-rule row: a camel-case last label is no address (gt-qq01cp).
		{"metric column, camel-case last label", planted("x@", "metrics.queueLandingColumn"), 0},
		{"bare identifier top label, camel-case", planted("x@", "metrics.landingColumn"), 0},
		{"capitalised identifier with a digit", planted("x@", "caller.Retry2"), 0},
		{"capitalised identifier with an underscore", planted("x@", "caller.Do_thing"), 0},
		{"upper-case top label is a host", planted("a@", "host.COM"), 1},
		{"lower-case top label is a host", planted("a@", "host.com"), 1},
		{"mixed-case host with a lower-case top label", planted("a@", "Host.com"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := "contact " + tc.addr + " please"
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

func TestEmailNeedsADottedDomain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want int
	}{
		{"workflow action pin", "actions/checkout@" + "v4", 0},
		{"image digest", "image@" + "sha256:9e79b8", 0},
		{"tool tag", "bd@" + "latest", 0},
		{"source ref", "gt-source@" + "abc", 0},
		{"short branch", "main@" + "a", 0},
		{"shell prompt", "user@" + "host:~$", 0},
		{"one-label domain", "a@" + "host", 0},
		{"plain address", "a@" + "host.com", 1},
		{"address in a subdomain", "a@" + "sub.host.co.uk", 1},
		{"dotted internal host", "a@" + "db.internal", 1},
		{"one-letter last label", "a@" + "b.c", 0},
		{"digit last label", "a@" + "x.1", 0},
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

	findings, _, err := Scan(fsys, sortedPaths(fsys), rulesThatNeverMatch(), blocked, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertFindings(t, findings,
		Finding{Path: path, Rule: RuleBlockedWord},
		Finding{Path: "docs/readme.md", Rule: RuleBlockedWord, Line: 2, LineHash: HashLine(contentLine)},
	)
	if content := findings[1].String(); strings.Contains(content, word) {
		t.Errorf("content finding %q echoes the blocked word", content)
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
			findings, _, err := Scan(singleFile("docs/a.md", tc.line+"\n"), []string{"docs/a.md"}, rulesThatNeverMatch(), blocked, nil)
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
			findings, _, err := Scan(singleFile("docs/a.md", tc.line+"\n"), []string{"docs/a.md"}, rulesThatNeverMatch(), blocked, nil)
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
		findings, _, err := Scan(singleFile("docs/a.md", tc.line+"\n"), []string{"docs/a.md"}, rulesThatNeverMatch(), blocked, nil)
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

func TestForbiddenDirectoryContentsAreNotScanned(t *testing.T) {
	t.Parallel()
	word := planted("ahr", "ens")
	body := strings.Join([]string{
		"intro",
		"contact " + planted("alice", "@github.com") + " please",
		"mentions " + word + " in prose",
		"opened " + planted("/Users/", "zephyr", "/project/file.go"),
		"wrote " + planted("/private/", "tmp", "/note.txt"),
		"recorded " + planted("session", "_id: ", "1234abcd", "-", "5678"),
		"",
	}, "\n")

	for _, dir := range ForbiddenDirs {
		t.Run(dir, func(t *testing.T) {
			path := dir + "/notes.md"
			assertFindings(t, scanFS(t, singleFile(path, body), nil),
				Finding{Path: path, Rule: RuleForbiddenDir})
		})
	}

	t.Run("a file outside those directories is still scanned", func(t *testing.T) {
		findings := scanFS(t, singleFile("docs/notes.md", body), nil)
		for _, rule := range []string{
			RuleEmail, RuleBlockedWord, RuleHomePath, RulePrivateTemp, RuleAgentSession,
		} {
			var found bool
			for _, finding := range findings {
				if finding.Rule == rule && finding.Line > 0 {
					found = true
				}
			}
			if !found {
				t.Errorf("rule %q found no line finding outside the forbidden directories", rule)
			}
		}
	})
}

func TestExcusedForbiddenPathIsReportedAndNotScanned(t *testing.T) {
	t.Parallel()
	word := planted("ahr", "ens")
	body := strings.Join([]string{
		"contact " + planted("alice", "@github.com") + " please",
		"mentions " + word + " in prose",
		"",
	}, "\n")
	path := ".beads/backup/events.jsonl"
	allow := []Allow{{Path: path, Rule: RuleForbiddenDir, Reason: "upstream bead export"}}

	t.Run("excused path is reported and its contents are not scanned", func(t *testing.T) {
		findings, excused, err := Scan(singleFile(path, body), []string{path}, DefaultRules(), shippedBlocked(t), allow)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("an excused forbidden file produced %d findings %v, want none", len(findings), findings)
		}
		want := []ExcusedForbiddenPath{{Path: path, Reason: "upstream bead export"}}
		if !reflect.DeepEqual(excused, want) {
			t.Errorf("excused = %v, want %v", excused, want)
		}
	})

	t.Run("the row still validates", func(t *testing.T) {
		if err := validate(t, singleFile(path, body), allow); err != nil {
			t.Fatalf("ValidateAllow refused an excused forbidden path: %v", err)
		}
	})

	t.Run("an unexcused forbidden file still reports its path finding", func(t *testing.T) {
		other := ".beads/backup/issues.jsonl"
		findings, excused, err := Scan(singleFile(other, body), []string{other}, DefaultRules(), shippedBlocked(t), allow)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		assertFindings(t, findings, Finding{Path: other, Rule: RuleForbiddenDir})
		if len(excused) != 0 {
			t.Errorf("excused = %v, want none", excused)
		}
	})
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
		{"dotless host followed by prose", planted("writer", "@host: prose"), 1},
		{"dotless host with a path", planted("bob", "@host:/srv/x"), 0},
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

// A reserved domain is read by its host, so the :port, /path or ?query after it
// does not make an address out of a test or example host. The twin of each row
// is the same shape on a real domain, which stays a finding.
func TestReservedDomainIsReadByItsHostNotItsPortOrPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want int
	}{
		{"reserved host with a port", planted("proxy", "@proxy.example:3128"), 0},
		{"reserved host with a path after a colon", planted("deploy", "@private-host.example.test:repos/app.git"), 0},
		{"reserved host with a query", planted("svc", "@host.example?x=1"), 0},
		{"reserved host with a userinfo, port and path", planted("user:pw", "@db.example:5432/gt"), 0},
		{"reserved host with an id, port and path", planted("svc:s3cret/x", "@forgejo.example:3000/acme"), 0},
		{"reserved host with an escaped password", planted("tracker:p%40ss", "@db.example.test:5433/tracker"), 0},
		{"twin: real host with a port", planted("proxy", "@acme.com:3128"), 1},
		{"twin: real host with a path after a colon", planted("deploy", "@private-host.acme.com:repos/app.git"), 1},
		{"twin: real host with a query", planted("svc", "@host.acme.com?x=1"), 1},
		{"twin: real host with a userinfo, port and path", planted("tracker:p%40ss", "@db.acme.com:5433/tracker"), 1},
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

// The attribution address and GitHub's ssh user are not anybody's, so they are
// exempt as addresses in every form. A lookalike of either stays a finding.
func TestKnownNoReplyAndSSHAddressesAreExemptInEveryForm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want int
	}{
		{"attribution address", planted("noreply", "@anthropic.com"), 0},
		{"attribution address in angle brackets", "<" + planted("noreply", "@anthropic.com") + ">", 0},
		{"ssh -T", "ssh -T " + planted("git", "@github.com"), 0},
		{"ssh URL", "ssh://" + planted("git", "@github.com") + "/o/r.git", 0},
		{"git+ssh URL", "git+ssh://" + planted("git", "@github.com") + "/o/r.git", 0},
		{"permission denied prose", planted("git", "@github.com") + ": Permission denied", 0},
		{"twin: personal address at the attribution domain", planted("jane", "@anthropic.com"), 1},
		{"twin: personal address at the ssh host", planted("jane", "@github.com"), 1},
		{"twin: lookalike local part", planted("xnoreply", "@anthropic.com"), 1},
		{"twin: lookalike ssh user", planted("x-git", "@github.com"), 1},
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

// A dotless name inside a branch path is a ref, not an address, even when a
// colon and a space follow it. The same name after a space is still prose.
func TestDotlessNameInABranchPathIsNotAnAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want int
	}{
		{"branch name before a colon", "branch " + planted("polecat/toast/gt-work", "@abc123") + ": dolt locked", 0},
		{"twin: dotless scp form after a space", planted("user", "@host: ") + "dolt locked", 1},
		{"twin: url user with a dotless host", planted("ssh://jane", "@laptop: ") + "dolt locked", 1},
		{"twin: one segment before a dotless host", planted("docs/jane", "@laptop: ") + "dolt locked", 1},
		{"twin: polecat name without a bead segment", planted("polecat/jane", "@laptop: ") + "dolt locked", 1},
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

// A home segment that sits inside a host or fixture path is a directory of that
// path, not a home. A path that starts with a home root keeps its later home
// segments as findings, and a Users segment in the middle of a path stays one.
func TestHomeSegmentMidPathIsNotAHomeDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want int
	}{
		{"host directory holds a home segment", planted("/host/home/", "gt-backups", "/dolt"), 0},
		{"fixture directory holds a home segment", planted("/fixture/home/", "forgejo", "/.env"), 0},
		{"twin: second home segment under a home root", planted("/home/", "user", "/home/", "real", "/x"), 1},
		{"twin: home directory", planted("/home/", "jane", "/x"), 1},
		{"twin: quoted home directory", planted("\"/Users/", "jane", "/x\""), 1},
		{"twin: users segment under a host directory", planted("/host/Users/", "jane", "/x"), 1},
		{"twin: account name under a host directory", planted("/host/home/", "jane", "/x"), 1},
		{"twin: account name under a srv directory", planted("/srv/home/", "jane", "/x"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := "opened " + tc.text
			findings := scanFS(t, singleFile("docs/note.md", line+"\n"), nil)
			if len(findings) != tc.want {
				t.Fatalf("got %d findings %v, want %d", len(findings), findings, tc.want)
			}
			if tc.want == 1 && findings[0].Rule != RuleHomePath {
				t.Errorf("rule = %q, want %q", findings[0].Rule, RuleHomePath)
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
	if _, _, err := Scan(fstest.MapFS{}, []string{"docs/missing.md"}, DefaultRules(), shippedBlocked(t), nil); err == nil {
		t.Fatal("Scan reported a missing file as clean")
	}
}

func TestScanRefusesAnEmptyRuleSetOrBlocklist(t *testing.T) {
	t.Parallel()
	fsys := singleFile("docs/note.md", "clean text\n")
	path := "docs/note.md"
	cases := []struct {
		name    string
		paths   []string
		rules   []Rule
		blocked map[string]struct{}
		want    string
	}{
		{"no rules", []string{path}, nil, shippedBlocked(t), "no rules"},
		{"empty rule set", []string{path}, []Rule{}, shippedBlocked(t), "no rules"},
		{"no blocklist", []string{path}, DefaultRules(), nil, "no blocklist"},
		{"empty blocklist", []string{path}, DefaultRules(), map[string]struct{}{}, "no blocklist"},
		{"no rules and no paths", nil, nil, shippedBlocked(t), "no rules"},
		{"no blocklist and no paths", nil, DefaultRules(), nil, "no blocklist"},
		{"no rules and an empty path slice", []string{}, nil, shippedBlocked(t), "no rules"},
		{"no blocklist and an empty path slice", []string{}, DefaultRules(), map[string]struct{}{}, "no blocklist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, excused, err := Scan(fsys, tc.paths, tc.rules, tc.blocked, nil)
			if err == nil {
				t.Fatalf("Scan reported %d findings and %d excused as clean", len(findings), len(excused))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// TestValidateAllowRefusesAnEmptyRuleSetOrBlocklist passes no allow rows, so
// the scanner never runs on a path: the inputs must be refused before the row
// loop, or the call reports every row (and a scan with nothing to apply) valid.
func TestValidateAllowRefusesAnEmptyRuleSetOrBlocklist(t *testing.T) {
	t.Parallel()
	fsys := singleFile("docs/note.md", "clean text\n")
	cases := []struct {
		name    string
		rules   []Rule
		blocked map[string]struct{}
		want    string
	}{
		{"no rules", nil, shippedBlocked(t), "no rules"},
		{"empty rule set", []Rule{}, shippedBlocked(t), "no rules"},
		{"no blocklist", DefaultRules(), nil, "no blocklist"},
		{"empty blocklist", DefaultRules(), map[string]struct{}{}, "no blocklist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAllow(fsys, nil, tc.rules, tc.blocked, nil)
			if err == nil {
				t.Fatal("ValidateAllow reported an empty rule set or blocklist as valid")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
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
			if _, _, err := Scan(fsys, []string{"docs/a.md"}, []Rule{rule}, shippedBlocked(t), nil); err == nil {
				t.Errorf("Scan accepted rule %q", rule.Name)
			}
		}
	})

	t.Run("invalid path", func(t *testing.T) {
		t.Parallel()
		if _, _, err := Scan(singleFile("docs/a.md", "plain text\n"), []string{"../escape"}, DefaultRules(), shippedBlocked(t), nil); err == nil {
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
		if err := ValidateAllow(fstest.MapFS{}, []string{"docs/a.md"}, DefaultRules(), shippedBlocked(t), allow); err == nil {
			t.Fatal("ValidateAllow reported an unreadable file as valid")
		}
	})
}

// ownSources holds the package's own files at compile time, so the self-scan
// reads what was built rather than whatever sits in the working directory.
//
//go:embed blocked.sha256 hygiene.go hygiene_test.go
var ownSources embed.FS

// TestTheScannerDoesNotFlagItsOwnFiles keeps every rule safe to publish: no
// line of the package's own sources matches one of its rules.
func TestTheScannerDoesNotFlagItsOwnFiles(t *testing.T) {
	t.Parallel()
	blocked := shippedBlocked(t)
	paths := []string{"blocked.sha256", "hygiene.go", "hygiene_test.go"}
	findings, _, err := Scan(ownSources, paths, DefaultRules(), blocked, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, finding := range findings {
		t.Errorf("the scanner flags its own file: %s", finding)
	}
}
