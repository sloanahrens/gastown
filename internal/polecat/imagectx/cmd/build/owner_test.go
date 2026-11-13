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

// TestBuildRefusesAnOwnerItCannotResolve: -forge-owner and -town-root reach the
// owner resolution before any file is read or any source is built, so a value
// that is not an owner name, and a named town with no town.json, each stop the
// build with a message naming the cause (gt-ucigk3).
func TestBuildRefusesAnOwnerItCannotResolve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"a flag that is not an owner name", []string{"-forge-owner", "org/team"}, "forge owner"},
		{"a named town with no town.json", []string{"-town-root", t.TempDir()}, "has no town.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"-rev", buildTestSHA, "-out", t.TempDir()}, tt.args...)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 1 {
				t.Fatalf("run = %d, want 1; stderr: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tt.wantErr)
			}
		})
	}
}
