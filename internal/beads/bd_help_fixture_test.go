package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bdHelpFixturePath is a scratch capture of fork build 0f9f640fe0f1, not the
// pod image, the text HelpOffersVerb is tested against (gt-dtibiv, gt-c2tetm).
const bdHelpFixturePath = "testdata/bd-help-0f9f640.txt"

// bdHelpBuild is the build hash the fixture's header names.
const bdHelpBuild = "0f9f640fe0f1"

// bdHelpSampleAddUsage is the sample verb's usage line the fixture pins.
const bdHelpSampleAddUsage = "bd sample add --kind <kind> --at <rfc3339> --json <file|->"

// bdHelpSyncHeading is the group the fixture lists the sample verb under.
const bdHelpSyncHeading = "Sync & Data:"

// readBdHelpFixture reads a bd help capture as lines, naming the path in the
// error so a moved or unreadable fixture says which file a test wanted.
func readBdHelpFixture(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bd help fixture %s: %w", path, err)
	}
	return strings.Split(string(data), "\n"), nil
}

// groupVerbRows returns the first word of every indented row under heading,
// scanning to the next unindented line. A blank line separates groups rather
// than ending one, matching the way bd prints them.
func groupVerbRows(lines []string, heading string) []string {
	var verbs []string
	inGroup := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if !inGroup {
			inGroup = !indented && strings.TrimSpace(line) == heading
			continue
		}
		if !indented {
			break
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			verbs = append(verbs, fields[0])
		}
	}
	return verbs
}

// TestBdHelpFixtureFacts pins the two facts the next bead's fix relies on:
// the sample verb sits under the "Sync & Data:" group of real bd help, and its
// usage line is the one the fixture holds (gt-c2tetm).
func TestBdHelpFixtureFacts(t *testing.T) {
	t.Run("sample under Sync & Data", func(t *testing.T) {
		lines, err := readBdHelpFixture(bdHelpFixturePath)
		if err != nil {
			t.Fatal(err)
		}
		verbs := groupVerbRows(lines, bdHelpSyncHeading)
		for _, verb := range verbs {
			if verb == "sample" {
				return
			}
		}
		t.Fatalf("%s: %q group offers %q, want sample", bdHelpFixturePath, bdHelpSyncHeading, verbs)
	})

	t.Run("sample add usage line", func(t *testing.T) {
		lines, err := readBdHelpFixture(bdHelpFixturePath)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			if strings.TrimSpace(line) == bdHelpSampleAddUsage {
				return
			}
		}
		t.Fatalf("%s: no line %q", bdHelpFixturePath, bdHelpSampleAddUsage)
	})

	t.Run("header names the build", func(t *testing.T) {
		lines, err := readBdHelpFixture(bdHelpFixturePath)
		if err != nil {
			t.Fatal(err)
		}
		var header []string
		for _, line := range lines {
			if !strings.HasPrefix(line, "#") {
				break
			}
			header = append(header, line)
		}
		if !strings.Contains(strings.Join(header, "\n"), bdHelpBuild) {
			t.Fatalf("%s: header %q does not name build %s", bdHelpFixturePath, header, bdHelpBuild)
		}
	})

	t.Run("unreadable path fails naming it", func(t *testing.T) {
		dir := t.TempDir()
		rows := []struct {
			name string
			path string
		}{
			{"missing file", filepath.Join(dir, "bd-help-missing.txt")},
			{"directory", dir},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				_, err := readBdHelpFixture(row.path)
				if err == nil {
					t.Fatalf("read %s: want an error, got none", row.path)
				}
				if !strings.Contains(err.Error(), row.path) {
					t.Fatalf("read %s: error %q does not name the path", row.path, err)
				}
			})
		}
	})
}
