package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The input files a run reads before it builds anything: each is valid except
// the one a case breaks, so the case proves that one file's failure is refused
// with its name in the message.
const (
	validImagePins = `bd http://fx.invalid/beads.git 0000000000000000000000000000000000000010
om http://fx.invalid/om.git 0000000000000000000000000000000000000011
hm http://fx.invalid/hm.git 0000000000000000000000000000000000000012
devops http://fx.invalid/devops.git 0000000000000000000000000000000000000020
`
	validSeedPins = "devops 0000000000000000000000000000000000000020\nbeads 0000000000000000000000000000000000000030\n"
	validSeedURLs = "devops http://fx.invalid/devops.git\nbeads http://fx.invalid/beads.git\n"
)

// TestRunRefusesAnUnreadableInput checks each input file a run reads before it
// builds: a file that is missing or does not parse is refused with the file named,
// and exits 1 with nothing built.
func TestRunRefusesAnUnreadableInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(dir string) (pins, seedPins, seedURLs string)
		want  string
	}{
		{"an image-pins file that is missing", func(dir string) (string, string, string) {
			return filepath.Join(dir, "missing"), write(t, dir, "seed-pins", validSeedPins), write(t, dir, "seed-urls", validSeedURLs)
		}, "cannot read"},
		{"an image-pins file that does not parse", func(dir string) (string, string, string) {
			return write(t, dir, "pins", "bd\n"), write(t, dir, "seed-pins", validSeedPins), write(t, dir, "seed-urls", validSeedURLs)
		}, "line 1"},
		{"a seed-pins file that is missing", func(dir string) (string, string, string) {
			return write(t, dir, "pins", validImagePins), filepath.Join(dir, "missing"), write(t, dir, "seed-urls", validSeedURLs)
		}, "cannot read"},
		{"a seed-urls file that does not parse", func(dir string) (string, string, string) {
			return write(t, dir, "pins", validImagePins), write(t, dir, "seed-pins", validSeedPins), write(t, dir, "seed-urls", "devops\n")
		}, "line 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			pins, seedPins, seedURLs := tc.setup(dir)
			var stdout, stderr bytes.Buffer
			args := []string{
				"-pins", pins, "-seed-pins", seedPins, "-seed-urls", seedURLs,
				"-rev", "0000000000000000000000000000000000000001", "-out", filepath.Join(dir, "out"),
			}
			if code := run(args, &stdout, &stderr); code != 1 {
				t.Fatalf("run exit = %d, want 1\n%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("a refused run printed to stdout: %q", stdout.String())
			}
		})
	}
}

// write writes content to a file named name in dir and returns its path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
