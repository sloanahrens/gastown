package formula

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// rejectionWriteRE matches a "MERGE REJECTION ..." note write and captures
// what carried it: `gt bead note <id>`, or a bd update note flag.
var rejectionWriteRE = regexp.MustCompile(`(gt bead note \S+|bd update[^\n]*?(?:--append-notes|--notes))\s+"?MERGE REJECTION`)

// findingsWriteRE matches the polecat's own progress write on a resumed bead.
var findingsWriteRE = regexp.MustCompile(`(gt bead note \S+|bd update[^\n]*?(?:--append-notes|--notes))\s+"Findings so far`)

// appends reports whether a matched note write appends: gt bead note always
// does (gt-7iwy0.8); a bd update only with --append-notes.
func appends(writer string) bool {
	return strings.HasPrefix(writer, "gt bead note ") || strings.HasSuffix(writer, "--append-notes")
}

// gt-nxvg: `--notes` REPLACES a bead's notes. A rejection write through it
// wiped the polecat's implementation notes, and the resume path
// (mol-polecat-work's load-context) greps notes for the MERGE REJECTION
// marker and the Branch: line, so a resumed bead needs both blocks intact.
func TestFormulaNotesWritesAppend(t *testing.T) {
	t.Parallel()
	files, err := fs.Glob(formulasFS, "formulas/*.formula.toml")
	if err != nil {
		t.Fatalf("globbing embedded formulas: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded formulas found; the glob or the embed directive changed")
	}

	cases := []struct {
		file     string
		re       *regexp.Regexp
		minMatch int
	}{
		{"formulas/mol-polecat-work.formula.toml", findingsWriteRE, 1},
		{"formulas/mol-polecat-work-monorepo.formula.toml", findingsWriteRE, 1},
	}

	for _, tc := range cases {
		raw, err := formulasFS.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("reading %s: %v", tc.file, err)
		}
		matches := tc.re.FindAllStringSubmatch(string(raw), -1)
		if len(matches) < tc.minMatch {
			t.Fatalf("%s: found %d matching note writes, want at least %d — the vocabulary moved, so this test no longer guards it",
				tc.file, len(matches), tc.minMatch)
		}
		for _, m := range matches {
			if !appends(m[1]) {
				t.Errorf("%s: note write %q replaces; it must append so prior notes survive", tc.file, m[1])
			}
		}
	}

	for _, file := range files {
		raw, err := formulasFS.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, m := range rejectionWriteRE.FindAllStringSubmatch(string(raw), -1) {
			if !appends(m[1]) {
				t.Errorf("%s: MERGE REJECTION note write %q replaces; it must append so the polecat's own notes survive", file, m[1])
			}
		}
	}
}
