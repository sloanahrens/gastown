package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/polecat/imagectx"
)

// buildTestSHA is a full commit for the pins a case writes.
const buildTestSHA = "0123456789abcdef0123456789abcdef01234567"

// TestOwnerPlaceholderExpandsInEachGoReader: readFile expands the {owner}
// placeholder in the image-pins and seed-urls files with the setting's default,
// so they build the URLs they named before the placeholder; the parsers refuse
// a placeholder nothing expanded, and an owner that is not a usable name refuses
// the file (gt-ucigk3).
func TestOwnerPlaceholderExpandsInEachGoReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pinsBody := "bd http://forgejo:3000/{owner}/beads.git " + buildTestSHA + "\n" +
		"om http://forgejo:3000/{owner}/organic-mechanic.git " + buildTestSHA + "\n" +
		"hm http://forgejo:3000/{owner}/history-man.git " + buildTestSHA + "\n" +
		"devops http://forgejo:3000/{owner}/devops.git " + buildTestSHA + "\n"
	pinsPath := filepath.Join(dir, "image-pins")
	urlsPath := filepath.Join(dir, "seed-urls")
	urlsBody := "devops http://forgejo:3000/{owner}/devops.git\nbeads http://forgejo:3000/{owner}/beads.git\n"
	for path, body := range map[string]string{pinsPath: pinsBody, urlsPath: urlsBody} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	pins, err := readFile(pinsPath, config.DefaultForgeOwner, daemonimage.ParsePins)
	if err != nil {
		t.Fatalf("readFile(%s): %v", pinsPath, err)
	}
	for _, pin := range pins {
		if want := "http://forgejo:3000/" + config.DefaultForgeOwner + "/"; !strings.HasPrefix(pin.URL, want) {
			t.Errorf("%s pin URL = %q, want it under %s", pin.Name, pin.URL, want)
		}
	}

	urls, err := readFile(urlsPath, config.DefaultForgeOwner, imagectx.ParseSeedURLs)
	if err != nil {
		t.Fatalf("readFile(%s): %v", urlsPath, err)
	}
	want := map[string]string{
		"devops": "http://forgejo:3000/" + config.DefaultForgeOwner + "/devops.git",
		"beads":  "http://forgejo:3000/" + config.DefaultForgeOwner + "/beads.git",
	}
	for rig, url := range want {
		if urls[rig] != url {
			t.Errorf("seed URL of %s = %q, want %q", rig, urls[rig], url)
		}
	}

	if _, err := daemonimage.ParsePins([]byte(pinsBody)); err == nil {
		t.Error("ParsePins accepted a URL holding the unexpanded placeholder")
	}
	if _, err := imagectx.ParseSeedURLs([]byte(urlsBody)); err == nil {
		t.Error("ParseSeedURLs accepted a URL holding the unexpanded placeholder")
	}
	if _, err := readFile(pinsPath, "", daemonimage.ParsePins); err == nil {
		t.Error("readFile accepted an empty owner")
	}
}

// TestImageBuildForgeOwnerFlag: the same fixture files, with their {owner}
// placeholders, expand to the setting's default when -forge-owner is unset and to
// the flag's owner when it is given, for image-pins and seed-urls alike. The
// flag's value is validated before any file is read (gt-ucigk3).
func TestImageBuildForgeOwnerFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pinsBody := "bd http://forgejo:3000/{owner}/beads.git " + buildTestSHA + "\n" +
		"om http://forgejo:3000/{owner}/organic-mechanic.git " + buildTestSHA + "\n" +
		"hm http://forgejo:3000/{owner}/history-man.git " + buildTestSHA + "\n" +
		"devops http://forgejo:3000/{owner}/devops.git " + buildTestSHA + "\n"
	pinsPath := filepath.Join(dir, "image-pins")
	seedPinsPath := filepath.Join(dir, "seed-pins")
	urlsPath := filepath.Join(dir, "seed-urls")
	files := map[string]string{
		pinsPath:     pinsBody,
		seedPinsPath: "devops " + buildTestSHA + "\n",
		urlsPath:     "devops http://forgejo:3000/{owner}/devops.git\nbeads http://forgejo:3000/{owner}/beads.git\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	defaultOwner, err := buildForgeOwner("", false)
	if err != nil {
		t.Fatalf("buildForgeOwner without the flag: %v", err)
	}
	flagOwner, err := buildForgeOwner("acme", true)
	if err != nil {
		t.Fatalf("buildForgeOwner with -forge-owner acme: %v", err)
	}
	if defaultOwner != config.DefaultForgeOwner || flagOwner != "acme" {
		t.Fatalf("owners = %q and %q, want %q and acme", defaultOwner, flagOwner, config.DefaultForgeOwner)
	}

	fromDefault, err := readBuildInputs(pinsPath, seedPinsPath, urlsPath, defaultOwner)
	if err != nil {
		t.Fatalf("readBuildInputs with the default: %v", err)
	}
	fromFlag, err := readBuildInputs(pinsPath, seedPinsPath, urlsPath, flagOwner)
	if err != nil {
		t.Fatalf("readBuildInputs with -forge-owner: %v", err)
	}
	if want := "http://forgejo:3000/" + config.DefaultForgeOwner + "/beads.git"; fromDefault.bd.URL != want {
		t.Errorf("default bd URL = %q, want %q", fromDefault.bd.URL, want)
	}
	if want := "http://forgejo:3000/acme/beads.git"; fromFlag.bd.URL != want {
		t.Errorf("flag bd URL = %q, want %q", fromFlag.bd.URL, want)
	}
	if want := "http://forgejo:3000/" + config.DefaultForgeOwner + "/beads.git"; fromDefault.urls["beads"] != want {
		t.Errorf("default seed URL of beads = %q, want %q", fromDefault.urls["beads"], want)
	}
	if want := "http://forgejo:3000/acme/beads.git"; fromFlag.urls["beads"] != want {
		t.Errorf("flag seed URL of beads = %q, want %q", fromFlag.urls["beads"], want)
	}
}

// TestBuildRefusesABadForgeOwnerBeforeAnyRead: a -forge-owner that is not an
// owner name, or is given empty, stops the build before it reads the pins files
// (which here do not exist, so a read first would fail as "cannot read"), with a
// message naming the flag (gt-ucigk3).
func TestBuildRefusesABadForgeOwnerBeforeAnyRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		flag []string
	}{
		{"a flag that is not an owner name", []string{"-forge-owner", "org/team"}},
		{"an empty flag", []string{"-forge-owner", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			absent := filepath.Join(t.TempDir(), "absent")
			args := append([]string{
				"-rev", buildTestSHA, "-out", t.TempDir(),
				"-pins", absent, "-seed-pins", absent, "-seed-urls", absent,
			}, tt.flag...)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 1 {
				t.Fatalf("run = %d, want 1; stderr: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "forge owner from -forge-owner") {
				t.Errorf("stderr = %q, want it to name -forge-owner", stderr.String())
			}
			if strings.Contains(stderr.String(), "cannot read") {
				t.Errorf("stderr = %q: the build read a file before refusing the owner", stderr.String())
			}
		})
	}
}
