package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townstate"
)

var (
	macTownOlder = time.Unix(1_700_000_000, 0)
	macTownNewer = time.Unix(1_700_000_600, 0)
)

// macTownFixture writes files under a new Mac town root, each stamped with mtime.
// A value of "" makes the entry a directory, so a test can put one where a file
// is expected.
func macTownFixture(t *testing.T, mtime time.Time, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if content == "" {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// podListingLine is one listing line for a claim file holding content.
func podListingLine(content string, mtime time.Time, name string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%s\t%d\t%s", hex.EncodeToString(sum[:]), mtime.Unix(), name)
}

// podListingFile writes the listing lines to a file and returns its path.
func podListingFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pod-town.tsv")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// missingNames returns the paths the check named as missing on the pod.
func missingNames(details []string) []string {
	var names []string
	for _, d := range details {
		rest, ok := strings.CutPrefix(d, "missing on pod: ")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, " (")
		names = append(names, name)
	}
	return names
}

// TestMacTownStateListsMacFileAbsentOnPod: a town file the Mac holds and the claim
// lacks is named by path, and the check refuses the cutover over it.
func TestMacTownStateListsMacFileAbsentOnPod(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{
		"mayor/town.json":      "{}",
		"settings/config.json": "{}",
	})
	listing := podListingFile(t, podListingLine("{}", macTownOlder, "mayor/town.json"))

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
	}
	if got := missingNames(res.Details); !slices.Equal(got, []string{"settings/config.json"}) {
		t.Errorf("missing = %v, want [settings/config.json]", got)
	}
}

// TestMacTownStateNamesNewerSideWhenBytesDiffer: a file both sides hold with other
// bytes is a warning, and the message says which copy was written last.
func TestMacTownStateNamesNewerSideWhenBytesDiffer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		macTime  time.Time
		podTime  time.Time
		wantSide string
	}{
		{name: "mac newer", macTime: macTownNewer, podTime: macTownOlder, wantSide: "the Mac copy is newer"},
		{name: "pod newer", macTime: macTownOlder, podTime: macTownNewer, wantSide: "the pod copy is newer"},
		{name: "same second", macTime: macTownOlder, podTime: macTownOlder, wantSide: "same mtime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := macTownFixture(t, tc.macTime, map[string]string{"mayor/town.json": `{"mac":true}`})
			listing := podListingFile(t, podListingLine(`{"pod":true}`, tc.podTime, "mayor/town.json"))

			res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
			if res.Status != StatusWarning {
				t.Fatalf("Status = %v, want StatusWarning: %+v", res.Status, res)
			}
			want := "differs: mayor/town.json (" + tc.wantSide + ")"
			if !slices.Equal(res.Details, []string{want}) {
				t.Errorf("Details = %v, want [%s]", res.Details, want)
			}
		})
	}
}

// TestMacTownStatePassesWhenPodHasEverything: a claim that holds every Mac town
// file with the same bytes passes.
func TestMacTownStatePassesWhenPodHasEverything(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"mayor/town.json":      `{"name":"gt"}`,
		"settings/config.json": `{"log":"info"}`,
	}
	root := macTownFixture(t, macTownOlder, files)
	listing := podListingFile(t,
		podListingLine(files["settings/config.json"], macTownNewer, "settings/config.json"),
		podListingLine(files["mayor/town.json"], macTownOlder, "mayor/town.json"),
	)

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK: %+v", res.Status, res)
	}
	if len(res.Details) != 0 {
		t.Errorf("Details = %v, want none", res.Details)
	}
}

// TestMacTownStateComparesTheTableExpansion: the names the check reports are the
// copy table's expansion over the Mac town, so a row added to townstate.TownCopyRows
// reaches this check, and a file no row names does not.
func TestMacTownStateComparesTheTableExpansion(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{
		"mayor/town.json":                   "{}",
		"mayor/rigs.json":                   `{"version":1,"rigs":{"alpha":{"git_url":"http://forgejo:3000/sloan/alpha.git"}}}`,
		"settings/config.json":              "{}",
		"settings/models.json":              "{}",
		"settings/placement.json":           "{}",
		"settings/regime-events.json":       "[]",
		"settings/themes/gastown-names.txt": "nux\n",
		"settings/themes/notes.md":          "not a theme\n",
		"settings/daemon.env":               "GT_TOKEN=secret\n",
		"alpha/settings/config.json":        "{}",
		"directives/common.md":              "be kind\n",
		"directives/crew/deep.md":           "deeper\n",
		".beads/config.yaml":                "issue-prefix: gt\n",
	})
	listing := podListingFile(t)

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
	}
	want, err := townstate.TownCopyPaths(os.DirFS(root), townstate.TownCopyRows([]string{"alpha"}))
	if err != nil {
		t.Fatalf("TownCopyPaths: %v", err)
	}
	if got := missingNames(res.Details); !slices.Equal(got, want) {
		t.Errorf("reported missing = %v, want the table expansion %v", got, want)
	}
	if slices.Contains(missingNames(res.Details), "settings/daemon.env") {
		t.Error("an unlisted file was reported as a town file")
	}
}

// TestMacTownStateRefusesMalformedListing: a listing line the check cannot read is
// a refusal naming no contents, never a skipped line that reads as a missing file.
func TestMacTownStateRefusesMalformedListing(t *testing.T) {
	t.Parallel()
	sum := strings.Repeat("a", 64)
	cases := []struct {
		name string
		line string
	}{
		{name: "too few fields", line: "only-one-field"},
		{name: "short sha", line: "abc\t1\tmayor/town.json"},
		{name: "sha not hex", line: strings.Repeat("z", 64) + "\t1\tmayor/town.json"},
		{name: "mtime not seconds", line: sum + "\tyesterday\tmayor/town.json"},
		{name: "absolute path", line: sum + "\t1\t/etc/passwd"},
		{name: "path escapes the town", line: sum + "\t1\t../mayor/town.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := macTownFixture(t, macTownOlder, map[string]string{"mayor/town.json": "{}"})
			listing := podListingFile(t, tc.line)

			res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
			if res.Status != StatusError || res.Message != "The pod claim listing is malformed" {
				t.Fatalf("result = %+v, want a malformed-listing error", res)
			}
		})
	}
}

// TestMacTownStateRefusesDuplicatePath: a path listed twice is ambiguous about
// which copy the claim holds, so it is refused.
func TestMacTownStateRefusesDuplicatePath(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{"mayor/town.json": "{}"})
	line := podListingLine("{}", macTownOlder, "mayor/town.json")
	listing := podListingFile(t, line, line)

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusError || res.Message != "The pod claim listing is malformed" {
		t.Fatalf("result = %+v, want a malformed-listing error", res)
	}
}

// TestMacTownStateRefusesUnreadableListing: a listing that cannot be read is an
// error, never an empty claim that every file is missing from silently passing.
func TestMacTownStateRefusesUnreadableListing(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{"mayor/town.json": "{}"})
	cases := map[string]string{
		"missing":   filepath.Join(t.TempDir(), "absent.tsv"),
		"directory": t.TempDir(),
	}
	for name, listing := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
			if res.Status != StatusError || !strings.HasPrefix(res.Message, "Cannot read the pod claim listing") {
				t.Fatalf("result = %+v, want an unreadable-listing error", res)
			}
		})
	}
}

// TestMacTownStateRefusesNonRegularTownFile: a directory where the copy expects a
// file is refused by name, not compared as clean.
func TestMacTownStateRefusesNonRegularTownFile(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{
		"mayor/town.json":      "{}",
		"settings/config.json": "",
	})
	listing := podListingFile(t, podListingLine("{}", macTownOlder, "mayor/town.json"))

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusError || res.Message != "Cannot read a Mac town file the copy carries" {
		t.Fatalf("result = %+v, want a non-regular-file error", res)
	}
	if len(res.Details) != 1 || !strings.Contains(res.Details[0], "settings/config.json is not a regular file") {
		t.Errorf("Details = %v, want the file named", res.Details)
	}
}

// TestMacTownStateRefusesUnreadableTownTree: a copy row the Mac cannot list or
// stat is a refusal naming the failure, never a row that quietly drops out.
func TestMacTownStateRefusesUnreadableTownTree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(t *testing.T, root string)
		wantMsg string
	}{
		{
			name: "directives is a file",
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "directives"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "Cannot list the Mac town's copy rows",
		},
		{
			name: "a town file is a symlink loop",
			setup: func(t *testing.T, root string) {
				if err := os.Symlink("config.json", filepath.Join(root, "settings", "config.json")); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "Cannot read a Mac town file the copy carries",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := macTownFixture(t, macTownOlder, map[string]string{
				"mayor/town.json":      "{}",
				"settings/models.json": "{}",
			})
			tc.setup(t, root)
			listing := podListingFile(t, podListingLine("{}", macTownOlder, "mayor/town.json"))

			res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
			if res.Status != StatusError || res.Message != tc.wantMsg {
				t.Fatalf("result = %+v, want StatusError %q", res, tc.wantMsg)
			}
		})
	}
}

// TestMacTownStateRefusesUnreadableRegistry: a rig registry that does not parse
// leaves the rig rows unknown, so the check refuses rather than comparing without them.
func TestMacTownStateRefusesUnreadableRegistry(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{
		"mayor/town.json": "{}",
		"mayor/rigs.json": "{not json",
	})
	listing := podListingFile(t, podListingLine("{}", macTownOlder, "mayor/town.json"))

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusError || !strings.HasPrefix(res.Message, "Cannot read the rig registry") {
		t.Fatalf("result = %+v, want a registry error", res)
	}
}

// TestMacTownStateRefusesRigNameOutsideOneSegment: a rig name the copy table would
// join to the town root as a path is refused before it reaches a path.
func TestMacTownStateRefusesRigNameOutsideOneSegment(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{
		"mayor/town.json": "{}",
		"mayor/rigs.json": `{"version":1,"rigs":{"../escape":{"git_url":"x"}}}`,
	})
	listing := podListingFile(t, podListingLine("{}", macTownOlder, "mayor/town.json"))

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusError || !strings.HasPrefix(res.Message, "The rig registry names a rig") {
		t.Fatalf("result = %+v, want a rig-name refusal", res)
	}
}

// TestMacTownStateRefusesEmptyTown: a Mac town holding none of the copy's files
// has nothing compared, so the check says so instead of passing.
func TestMacTownStateRefusesEmptyTown(t *testing.T) {
	t.Parallel()
	root := macTownFixture(t, macTownOlder, map[string]string{"notes.txt": "x"})
	listing := podListingFile(t)

	res := NewMacTownStateCheck(listing).Run(&CheckContext{TownRoot: root})
	if res.Status != StatusError || !strings.HasPrefix(res.Message, "The Mac town holds none") {
		t.Fatalf("result = %+v, want an empty-town refusal", res)
	}
}
