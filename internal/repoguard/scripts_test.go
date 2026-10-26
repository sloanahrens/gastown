package repoguard

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The alarm text is pinned here, not read back from the guard: a test that
// reuses the production string passes when the message loses the rule or the
// offending path.
const (
	unlistedAlarm = "not in .repoguard-allowlist; write it in Go, or list it with a reason if it must stay a script"
	noReasonAlarm = ".repoguard-allowlist entry has no reason; each line is path<TAB>reason"
	staleAlarm    = ".repoguard-allowlist entry names no tracked file; delete the line so the list only shrinks"
	grownAlarm    = ".repoguard-allowlist entry is not in the base allowlist; the list only shrinks"
)

// TestCheckScriptsFixtures drives every branch of the guard through injected
// readers: an unlisted script, an entry with no reason, a stale entry and a
// grown allowlist, plus the trees that must pass.
func TestCheckScriptsFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		tracked   []string
		allowlist []Entry
		base      []Entry
		noBase    bool // --init with no base allowlist: the growth check is skipped
		want      []string
	}{
		{
			name:      "clean",
			tracked:   []string{"main.go", "scripts/probe.sh"},
			allowlist: []Entry{{Path: "scripts/probe.sh", Reason: "needs the real shell"}},
			base:      []Entry{{Path: "scripts/probe.sh", Reason: "needs the real shell"}},
		},
		{
			name:      "files that are not scripts are not judged",
			tracked:   []string{"README.md", "internal/app.go", "data.json"},
			allowlist: nil,
			base:      nil,
		},
		{
			name:    "unlisted script",
			tracked: []string{"scripts/new.sh"},
			want:    []string{"scripts/new.sh: " + unlistedAlarm},
		},
		{
			name:      "upper-case suffix is a script",
			tracked:   []string{"SETUP.SH"},
			allowlist: nil,
			want:      []string{"SETUP.SH: " + unlistedAlarm},
		},
		{
			name:      "listed entry with no reason",
			tracked:   []string{"scripts/a.sh"},
			allowlist: []Entry{{Path: "scripts/a.sh"}},
			base:      []Entry{{Path: "scripts/a.sh", Reason: "fleet script"}},
			want:      []string{"scripts/a.sh: " + noReasonAlarm},
		},
		{
			name:      "stale entry",
			tracked:   []string{"main.go"},
			allowlist: []Entry{{Path: "scripts/old.sh", Reason: "was needed"}},
			base:      []Entry{{Path: "scripts/old.sh", Reason: "was needed"}},
			want:      []string{"scripts/old.sh: " + staleAlarm},
		},
		{
			// The script is listed, so only the base comparison can refuse it.
			// Delete the growth check and this case accepts a new script.
			name:      "allowlist grew against a smaller base",
			tracked:   []string{"scripts/a.sh", "scripts/new.sh"},
			allowlist: []Entry{{Path: "scripts/a.sh", Reason: "fleet script"}, {Path: "scripts/new.sh", Reason: "also fleet"}},
			base:      []Entry{{Path: "scripts/a.sh", Reason: "fleet script"}},
			want:      []string{"scripts/new.sh: " + grownAlarm},
		},
		{
			name:      "no base allowlist with --init",
			tracked:   []string{"scripts/a.sh"},
			allowlist: []Entry{{Path: "scripts/a.sh", Reason: "fleet script"}},
			noBase:    true,
		},
		{
			name:      "every violation at once, sorted",
			tracked:   []string{"scripts/a.sh", "scripts/unlisted.rb"},
			allowlist: []Entry{{Path: "scripts/a.sh"}, {Path: "scripts/old.pl", Reason: "was needed"}},
			base:      []Entry{{Path: "scripts/a.sh", Reason: "fleet script"}},
			want: []string{
				"scripts/a.sh: " + noReasonAlarm,
				"scripts/old.pl: " + grownAlarm,
				"scripts/old.pl: " + staleAlarm,
				"scripts/unlisted.rb: " + unlistedAlarm,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var base Reader
			if !tc.noBase {
				base = fixedReader(tc.base)
			}
			findings, err := CheckScripts(
				func() ([]string, error) { return tc.tracked, nil },
				fixedReader(tc.allowlist),
				base,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(findings, tc.want) {
				t.Errorf("findings = %q, want %q", findings, tc.want)
			}
		})
	}
}

// TestCheckScriptsReaderErrors: a lister or reader that cannot answer makes
// the guard fail rather than pass, since an empty tree and an unread one must
// not look alike.
func TestCheckScriptsReaderErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("git is not on PATH")
	clean := fixedReader(nil)
	tests := []struct {
		name     string
		list     Lister
		read     Reader
		readBase Reader
	}{
		{name: "lister", list: func() ([]string, error) { return nil, wantErr }, read: clean},
		{name: "allowlist", list: func() ([]string, error) { return nil, nil }, read: func() ([]Entry, error) { return nil, wantErr }},
		{name: "base allowlist", list: func() ([]string, error) { return nil, nil }, read: clean, readBase: func() ([]Entry, error) { return nil, wantErr }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings, err := CheckScripts(tc.list, tc.read, tc.readBase)
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want %v", err, wantErr)
			}
			if findings != nil {
				t.Errorf("findings = %q with a reader error, want nil", findings)
			}
		})
	}
}

// TestParseAllowlist pins the format: # comments and blank lines are not
// entries, a tab separates path from reason, and a line without a tab is an
// entry with no reason rather than a parse failure.
func TestParseAllowlist(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"# scripts that cannot be Go",
		"",
		"scripts/a.sh\tneeds the real shell",
		"  scripts/b.sh  \t  needs bash  ",
		"scripts/c.sh",
		"scripts/d.sh\t",
	}, "\n")
	got, err := ParseAllowlist(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Path: "scripts/a.sh", Reason: "needs the real shell"},
		{Path: "scripts/b.sh", Reason: "needs bash"},
		{Path: "scripts/c.sh"},
		{Path: "scripts/d.sh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAllowlist = %q, want %q", got, want)
	}
}

func fixedReader(entries []Entry) Reader {
	return func() ([]Entry, error) { return entries, nil }
}
