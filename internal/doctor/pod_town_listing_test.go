package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/townstate"
)

// TestPodTownListingRoundTrip: the listing parses back and names every town copy
// file the town holds with its sha256 and mtime.
func TestPodTownListingRoundTrip(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"mayor/town.json":            `{"name":"gt"}`,
		"mayor/rigs.json":            `{"version":1,"rigs":{"alpha":{"git_url":"x"}}}`,
		"settings/config.json":       `{"log":"info"}`,
		"settings/themes/names.txt":  "nux\n",
		"settings/themes/notes.md":   "not a theme\n",
		"settings/daemon.env":        "TOKEN=secret\n",
		"alpha/settings/config.json": `{}`,
		"directives/common.md":       "be kind\n",
		".beads/config.yaml":         "issue-prefix: gt\n",
	}
	root := macTownFixture(t, macTownOlder, files)

	data, err := PodTownListing(root)
	if err != nil {
		t.Fatalf("PodTownListing: %v", err)
	}
	got, err := parsePodTownListing(data)
	if err != nil {
		t.Fatalf("the listing does not parse: %v\n%s", err, data)
	}
	want, err := townstate.TownCopyPaths(os.DirFS(root), townstate.TownCopyRows([]string{"alpha"}))
	if err != nil {
		t.Fatalf("TownCopyPaths: %v", err)
	}
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, slices.Sorted(slices.Values(want))) {
		t.Fatalf("listed names = %v, want the table expansion %v", names, slices.Sorted(slices.Values(want)))
	}
	for _, name := range want {
		sum := sha256.Sum256([]byte(files[name]))
		if got[name].sum != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: sha256 = %s, want the file's own", name, got[name].sum)
		}
		if got[name].mtime.Unix() != macTownOlder.Unix() {
			t.Errorf("%s: mtime = %s, want %s", name, got[name].mtime, macTownOlder)
		}
	}
}

// TestPodTownListingRefuses: an unreadable registry, an unlistable row, or a
// town file that cannot be hashed is an error with no listing, never a shorter
// listing that reads as absent files.
func TestPodTownListingRefuses(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"mayor/town.json":      "{}",
		"settings/config.json": "{}",
	}
	cases := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{name: "malformed registry", extra: map[string]string{"mayor/rigs.json": "{not json"}, want: "reading the rig registry"},
		{name: "rig name outside one segment", extra: map[string]string{"mayor/rigs.json": `{"version":1,"rigs":{"../escape":{"git_url":"x"}}}`}, want: "not a single path segment"},
		{name: "town file is a directory", extra: map[string]string{"settings/config.json": ""}, want: "is not a regular file"},
		{name: "directives is a file", extra: map[string]string{"directives": "x"}, want: "listing the town's copy rows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := maps.Clone(base)
			maps.Copy(files, tc.extra)
			data, err := PodTownListing(macTownFixture(t, macTownOlder, files))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PodTownListing = %q, %v, want an error containing %q", data, err, tc.want)
			}
			if data != nil {
				t.Errorf("listing = %q, want none on error", data)
			}
		})
	}
}

// TestPodTownListingRefusesEmptyTown: a root with none of the copy's files is refused, not listed as empty.
func TestPodTownListingRefusesEmptyTown(t *testing.T) {
	t.Parallel()
	if data, err := PodTownListing(t.TempDir()); err == nil || !strings.Contains(err.Error(), "holds none") {
		t.Fatalf("PodTownListing = %q, %v, want a refusal naming the empty town", data, err)
	}
}
