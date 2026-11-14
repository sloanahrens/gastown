package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// directiveCeilingPath holds the rendered polecat directive size the ratchet
// allows, in bytes. It moves only down: a commit that cuts a rule or merges two
// lowers it in the same commit, and no commit raises it.
const directiveCeilingPath = "testdata/directive_ceiling.txt"

// directiveSectionWant is one source the report must print, and the bytes it
// must count.
type directiveSectionWant struct {
	name  string
	bytes int
}

// TestDirectiveSize checks the report counts every source of a role's
// directive the way the seat prompt renders it, town layer and rig layer alike,
// with the sections adding up to the printed total.
func TestDirectiveSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		files    map[string]string // slash paths under the temp town, with their text
		role     string
		rig      string
		want     []directiveSectionWant
		wantNone bool
	}{
		{
			name:  "one role file counts its heading and text",
			files: map[string]string{"directives/polecat.md": "## Rule\n\nBody\n"},
			role:  "polecat",
			want: []directiveSectionWant{
				{"town polecat", len("\n## Directive: town polecat\n\n## Rule\n\nBody\n")},
			},
		},
		{
			name: "town and rig layers are both counted, broadest first",
			files: map[string]string{
				"directives/_common.md":                "shared town\n",
				"directives/polecat.md":                "role town\n",
				"myrig/.gastown/directives/_common.md": "shared rig\n",
				"myrig/.gastown/directives/polecat.md": "role rig\n",
			},
			role: "polecat",
			rig:  "myrig",
			want: []directiveSectionWant{
				{"town _common", len("\n## Directive: town _common\n\nshared town\n")},
				{"myrig _common", len("\n## Directive: myrig _common\n\nshared rig\n")},
				{"town polecat", len("\n## Directive: town polecat\n\nrole town\n")},
				{"myrig polecat", len("\n## Directive: myrig polecat\n\nrole rig\n")},
			},
		},
		{
			name:     "a role with no directive files reports zero",
			role:     "polecat",
			wantNone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			for path, text := range tc.files {
				stageDirectiveFile(t, filepath.Join(town, filepath.FromSlash(path)), text)
			}

			var buf bytes.Buffer
			if err := runDirectiveSizeIn(&buf, tc.role, town, tc.rig); err != nil {
				t.Fatalf("runDirectiveSizeIn: %v", err)
			}
			out := buf.String()

			total := 0
			for _, want := range tc.want {
				total += want.bytes
			}
			if !strings.Contains(out, fmt.Sprintf("%s directive: %d bytes", tc.role, total)) {
				t.Fatalf("report does not print the total %d bytes for %s:\n%s", total, tc.role, out)
			}
			if !strings.Contains(out, fmt.Sprintf("target %d", directiveSizeTarget)) {
				t.Errorf("report does not name the %d-byte target:\n%s", directiveSizeTarget, out)
			}
			if got := strings.Count(out, " bytes\n"); got != len(tc.want) {
				t.Fatalf("report prints %d source lines, want %d:\n%s", got, len(tc.want), out)
			}
			if tc.wantNone {
				if !strings.Contains(out, "no directive files") {
					t.Errorf("empty directive does not say so:\n%s", out)
				}
				return
			}
			for _, want := range tc.want {
				line := directiveSizeLine(t, out, want.name)
				if !strings.Contains(line, fmt.Sprintf("%d bytes", want.bytes)) {
					t.Errorf("source %q does not count %d bytes: %q", want.name, want.bytes, line)
				}
			}
		})
	}
}

// directiveSizeLine returns the report's one line naming the source.
func directiveSizeLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, name+" ") && strings.HasSuffix(strings.TrimSpace(line), "bytes") {
			return line
		}
	}
	t.Fatalf("no source line for %q in:\n%s", name, out)
	return ""
}

// TestDirectiveSizeCommandRegistered checks the report is reachable as
// 'gt directive size': deleting the registration leaves the helper tested and
// the command gone, so the operator loses the answer the budget asks for.
func TestDirectiveSizeCommandRegistered(t *testing.T) {
	t.Parallel()

	for _, c := range directiveCmd.Commands() {
		if c.Name() != "size" {
			continue
		}
		if c.Flags().Lookup("rig") == nil {
			t.Error("gt directive size has no --rig flag")
		}
		return
	}
	t.Fatal("gt directive size is not registered on gt directive")
}

// TestDirectiveSizeNoTown checks a town that cannot be found fails the report
// instead of measuring the directives of whatever directory the command ran in.
func TestDirectiveSizeNoTown(t *testing.T) {
	t.Parallel()

	notInTown := errors.New("not in a Gas Town workspace")
	var buf bytes.Buffer
	err := runDirectiveSizeWith(&buf, nil, "", func(string) (string, string, error) {
		return "", "", notInTown
	})
	if !errors.Is(err, notInTown) {
		t.Fatalf("runDirectiveSizeWith = %v, want the resolver's error", err)
	}
	if buf.Len() != 0 {
		t.Errorf("printed a report with no town: %q", buf.String())
	}
}

// TestDirectiveSizeUnknownRole checks a role that names no directive file is
// refused before any read: measuring it would count only the shared file and
// report a healthy size for a directive that does not exist.
func TestDirectiveSizeUnknownRole(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := runDirectiveSizeIn(&buf, "wizard", t.TempDir(), "")
	if err == nil {
		t.Fatal("measured a directive for an unknown role")
	}
	if !strings.Contains(err.Error(), `"wizard"`) {
		t.Errorf("error does not name the role: %v", err)
	}
	if !strings.Contains(err.Error(), config.SharedDirectiveName) {
		t.Errorf("error does not list the valid names: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("printed a report for an unknown role: %q", buf.String())
	}
}

// TestDirectiveSizeUnreadableSource checks fail-closed: a source that exists
// and cannot be read fails the measurement instead of counting the rest, which
// would print a size smaller than the one installed.
func TestDirectiveSizeUnreadableSource(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "directives", "polecat.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runDirectiveSizeIn(&buf, "polecat", town, ""); err == nil {
		t.Fatal("measured a directive with an unreadable source")
	}
	if buf.Len() != 0 {
		t.Errorf("printed a report without the unreadable source: %q", buf.String())
	}
}

// TestDirectiveCeiling checks the ratchet's rule: a render above the ceiling
// fails with a message naming both numbers and the fix, and a render at the
// ceiling passes. It then holds the repo's committed directive to the committed
// ceiling, rendered the way the seat prompt renders it.
func TestDirectiveCeiling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		size     int
		ceiling  int
		wantFail bool
	}{
		{name: "below the ceiling passes", size: 100, ceiling: 120},
		{name: "at the ceiling passes", size: 120, ceiling: 120},
		{name: "above the ceiling fails", size: 121, ceiling: 120, wantFail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := checkDirectiveCeiling(tc.size, tc.ceiling)
			if !tc.wantFail {
				if msg != "" {
					t.Fatalf("a size of %d failed a %d-byte ceiling: %s", tc.size, tc.ceiling, msg)
				}
				return
			}
			for _, want := range []string{strconv.Itoa(tc.size), strconv.Itoa(tc.ceiling), "cut a rule or merge two", "lower the ceiling"} {
				if !strings.Contains(msg, want) {
					t.Errorf("failure message does not name %q: %s", want, msg)
				}
			}
		})
	}

	total := committedPolecatDirectiveBytes(t)
	ceiling := readDirectiveCeiling(t)
	if msg := checkDirectiveCeiling(total, ceiling); msg != "" {
		t.Fatal(msg)
	}
}

// checkDirectiveCeiling returns the failure message for a rendered polecat
// directive of size bytes against ceiling, or "" when it fits. The ceiling only
// moves down, so the message sends the editor back to cut first and lower the
// ceiling to the size they got.
func checkDirectiveCeiling(size, ceiling int) string {
	if size <= ceiling {
		return ""
	}
	return fmt.Sprintf("the polecat directive renders %d bytes, above the %d-byte ceiling: cut a rule or merge two, then lower the ceiling to the size you got", size, ceiling)
}

// committedPolecatDirectiveBytes measures the repo's committed directives as a
// polecat seat receives them: staged into a temp town's central layer, then
// read and rendered by the same functions that write the seat prompt.
func committedPolecatDirectiveBytes(t *testing.T) int {
	t.Helper()
	town := t.TempDir()
	for _, name := range []string{config.SharedDirectiveName, "polecat"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "directives", name+".md"))
		if err != nil {
			t.Fatalf("read the committed %s.md: %v", name, err)
		}
		stageDirectiveFile(t, filepath.Join(config.CentralDirectiveDir(town), name+".md"), string(data))
	}
	size, err := readDirectiveSize("polecat", town, "")
	if err != nil {
		t.Fatalf("measure the committed polecat directive: %v", err)
	}
	return size.Bytes
}

// readDirectiveCeiling reads the committed ceiling, refusing a file that holds
// anything but one positive byte count.
func readDirectiveCeiling(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(directiveCeilingPath)
	if err != nil {
		t.Fatalf("read the ceiling file: %v", err)
	}
	ceiling, err := parseDirectiveCeiling(data)
	if err != nil {
		t.Fatalf("%s %v", directiveCeilingPath, err)
	}
	return ceiling
}

// parseDirectiveCeiling reads the ceiling file's text. Blank lines and '#'
// comment lines are skipped, and exactly one other line must hold a positive
// byte count, so a stray edit cannot move the ceiling unnoticed.
func parseDirectiveCeiling(data []byte) (int, error) {
	var values []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		values = append(values, line)
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("holds %d value lines, want exactly one byte count", len(values))
	}
	ceiling, err := strconv.Atoi(values[0])
	if err != nil {
		return 0, fmt.Errorf("holds %q, not a byte count: %v", values[0], err)
	}
	if ceiling <= 0 {
		return 0, fmt.Errorf("holds %d; a ceiling must be a positive byte count", ceiling)
	}
	return ceiling, nil
}

// TestParseDirectiveCeiling checks the ceiling file reads its one number past
// the comment header, and refuses every file that would hide or move the
// ceiling: no number, two numbers, a zero, a negative, or text.
func TestParseDirectiveCeiling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		data    string
		want    int
		wantErr bool
	}{
		{name: "a bare number", data: "15384\n", want: 15384},
		{name: "a comment header then the number", data: "# the ceiling\n# moves down\n15384\n", want: 15384},
		{name: "only comments", data: "# the ceiling\n", wantErr: true},
		{name: "two numbers", data: "15384\n12000\n", wantErr: true},
		{name: "a zero", data: "0\n", wantErr: true},
		{name: "a negative", data: "-5\n", wantErr: true},
		{name: "text that is not a number", data: "fifteen\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDirectiveCeiling([]byte(tc.data))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsed %q as %d, want an error", tc.data, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tc.data, err)
			}
			if got != tc.want {
				t.Errorf("parse %q = %d, want %d", tc.data, got, tc.want)
			}
		})
	}
}
