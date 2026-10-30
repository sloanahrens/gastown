package shelltier

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// checkErr fails unless err is nil when want is empty, or is non-nil and names want.
func checkErr(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one naming %q", err, want)
	}
}

// without returns entries minus the one at path.
func without(entries []Entry, path string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Path != path {
			out = append(out, e)
		}
	}
	return out
}

func TestManifestCoversEveryTrackedShellTest(t *testing.T) {
	t.Parallel()
	failingLister := func() ([]string, error) { return nil, errors.New("boom") }
	tests := []struct {
		name    string
		list    Lister
		entries []Entry
		wantErr string
	}{
		{"manifest covers the tracked scripts", trackedLister, Manifest(), ""},
		{"a tracked script with no entry is named", func() ([]string, error) {
			return append(slices.Clone(trackedShellTests), "scripts/new_test.sh"), nil
		}, Manifest(), "scripts/new_test.sh"},
		{"a deleted entry leaves its script uncovered", trackedLister, without(Manifest(), "scripts/install-local_test.sh"), "scripts/install-local_test.sh"},
		{"a lister error is reported, not read as covered", failingLister, Manifest(), "boom"},
		{"an empty listing is an error, not covered", func() ([]string, error) { return nil, nil }, Manifest(), "no tracked shell tests listed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkErr(t, CheckCovered(tc.list, tc.entries), tc.wantErr)
		})
	}
}

func TestManifestEntriesExist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []Entry
		wantErr string
	}{
		{"every manifest path is on disk", Manifest(), ""},
		{"a bogus path is named", append(Manifest(), Entry{Path: "scripts/bogus_test.sh"}), "scripts/bogus_test.sh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkErr(t, CheckExist(tc.entries, onDisk), tc.wantErr)
		})
	}
}

func TestManifestOrderIsToday(t *testing.T) {
	t.Parallel()
	// The first 13 are the scripts/test-makefile.sh order, ending with
	// makefile-gate_test.sh; the last three are the extras the shell tier adds.
	want := []string{
		"scripts/check-install-path_test.sh",
		"scripts/polecat-go-floor_test.sh",
		"scripts/install-local_test.sh",
		"scripts/deep-review_test.sh",
		"scripts/flake-sweep_test.sh",
		"scripts/gate-vs-load_test.sh",
		"scripts/forgejo-provision_test.sh",
		"scripts/forgejo-probe_test.sh",
		"scripts/forgejo-resync_test.sh",
		"scripts/forgejo-cutover_test.sh",
		"deploy/k8s/kind/kind_test.sh",
		"scripts/resource-sampler_test.sh",
		"scripts/makefile-gate_test.sh",
		".githooks/pre-push_test.sh",
		"scripts/post-land-shell_test.sh",
		"scripts/town_metrics_test.sh",
	}
	var got []string
	for _, e := range Manifest() {
		got = append(got, e.Path)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("manifest order\n got: %q\nwant: %q", got, want)
	}
}

func TestManifestStartsSerialWithNoTriggersOfItsOwn(t *testing.T) {
	t.Parallel()
	for _, e := range Manifest() {
		if e.Mode != Serial || len(e.Triggers) != 0 {
			t.Errorf("%s: Mode=%v Triggers=%q, want Serial with no triggers of its own", e.Path, e.Mode, e.Triggers)
		}
	}
}

func TestTriggersOrDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry Entry
		want  []string
	}{
		{
			"no triggers and a reason runs on the defaults only",
			Entry{StubsGo: "none: names no go command"},
			DefaultTriggers,
		},
		{
			"no triggers and no reason also runs on every Go change",
			Entry{},
			append(slices.Clone(DefaultTriggers), GoTriggers...),
		},
		{
			"own triggers and no reason also run on every Go change",
			Entry{Triggers: []string{"scripts/x/"}},
			append([]string{"scripts/x/"}, GoTriggers...),
		},
		{
			"own triggers and a reason run on their own paths only",
			Entry{Triggers: []string{"scripts/x/"}, StubsGo: "stub: scripts/x_test.sh:3 puts a fake go on PATH"},
			[]string{"scripts/x/"},
		},
		{
			"a malformed reason is no exemption and runs on every Go change",
			Entry{StubsGo: "bogus"},
			append(slices.Clone(DefaultTriggers), GoTriggers...),
		},
		{
			"a bare none: prefix is no exemption",
			Entry{StubsGo: "none:"},
			append(slices.Clone(DefaultTriggers), GoTriggers...),
		},
		{
			"a stub: prefix with only spaces after it is no exemption",
			Entry{StubsGo: "stub:   "},
			append(slices.Clone(DefaultTriggers), GoTriggers...),
		},
		{
			"a go: declaration is no exemption and runs on every Go change",
			Entry{StubsGo: "go: the case runs the real go"},
			append(slices.Clone(DefaultTriggers), GoTriggers...),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.TriggersOrDefault(); !slices.Equal(got, tc.want) {
				t.Fatalf("TriggersOrDefault() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEveryEntryDeclaresItsGoUse(t *testing.T) {
	t.Parallel()
	for _, e := range Manifest() {
		t.Run(e.Path, func(t *testing.T) {
			if !hasReason(e.StubsGo) && !hasGoDeclaration(e.StubsGo) {
				t.Fatalf("StubsGo = %q, want a none: or stub: reason or a go: declaration, each followed by text", e.StubsGo)
			}
		})
	}
}

func TestMakefileGateRunsOnShelltierChanges(t *testing.T) {
	t.Parallel()
	// The selection is the production matcher's: a change to a Go file under
	// internal/shelltier must run the go: row and must not run a none: row.
	goChange := []string{"internal/shelltier/manifest.go"}
	var gateSeen, exemptSeen bool
	for _, e := range Manifest() {
		switch {
		case e.Path == "scripts/makefile-gate_test.sh":
			if !tiertrigger.Runs(e.TriggersOrDefault(), goChange, false) {
				t.Fatalf("a change to %s does not run %s (triggers %q)", goChange[0], e.Path, e.TriggersOrDefault())
			}
			gateSeen = true
		case strings.HasPrefix(e.StubsGo, "none:") && hasReason(e.StubsGo):
			if tiertrigger.Runs(e.TriggersOrDefault(), goChange, false) {
				t.Errorf("%s is exempt (%q) but runs on a change to %s", e.Path, e.StubsGo, goChange[0])
			}
			exemptSeen = true
		}
	}
	if !gateSeen {
		t.Fatal("scripts/makefile-gate_test.sh is not in the manifest")
	}
	if !exemptSeen {
		t.Fatal("no none: entry in the manifest, so the exemption is not shown")
	}
}

func TestHasGoDeclarationNeedsTextAfterThePrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"go:", false},
		{"go:   ", false},
		{"go: x", true},
		{"none: x", false},
		{"stub: x", false},
		{"whatever", false},
	}
	for _, tc := range tests {
		if got := hasGoDeclaration(tc.s); got != tc.want {
			t.Errorf("hasGoDeclaration(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestHasReasonNeedsTextAfterThePrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		s    string
		want bool
	}{
		{"none: names no go command", true},
		{"stub: scripts/x_test.sh:3 puts a fake go on PATH", true},
		{"none:", false},
		{"stub:", false},
		{"stub:   ", false},
		{"bogus", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := hasReason(tc.s); got != tc.want {
			t.Errorf("hasReason(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestGitListerAsksGitForTheShellTests(t *testing.T) {
	t.Parallel()
	var calls []string
	run := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return nil, nil
	}
	if _, err := GitLister(run)(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"git ls-files *_test.sh"}; !slices.Equal(calls, want) {
		t.Fatalf("commands = %q, want %q", calls, want)
	}
}

func TestGitListerDropsTestdataFixtures(t *testing.T) {
	t.Parallel()
	run := func(name string, args ...string) ([]byte, error) {
		return []byte("scripts/a_test.sh\nscripts/testdata/fixture_test.sh\ntestdata/root_test.sh\n\nscripts/b_test.sh\n"), nil
	}
	got, err := GitLister(run)()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"scripts/a_test.sh", "scripts/b_test.sh"}; !slices.Equal(got, want) {
		t.Fatalf("GitLister() = %q, want %q", got, want)
	}
}

func TestGitListerReportsGitFailure(t *testing.T) {
	t.Parallel()
	run := func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("exit status 128")
	}
	_, err := GitLister(run)()
	checkErr(t, err, "exit status 128")
}
