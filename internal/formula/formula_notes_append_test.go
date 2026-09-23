package formula

import (
	"io/fs"
	"regexp"
	"testing"
)

// rejectionWriteRE matches a `bd update ... <note-flag> "MERGE REJECTION ...`
// write and captures the flag that carried the note.
var rejectionWriteRE = regexp.MustCompile(`bd update[^\n]*?(--append-notes|--notes)\s+"?MERGE REJECTION`)

// findingsWriteRE matches the polecat's own progress write on a resumed bead.
var findingsWriteRE = regexp.MustCompile(`bd update[^\n]*?(--append-notes|--notes)\s+"Findings so far`)

// rejectFindingsRE matches the reject call that records an om verdict's
// findings onto the source bead.
var rejectFindingsRE = regexp.MustCompile(`gt mq reject[^\n]*--findings-json`)

// findingLineRE matches a finding line written out in a formula: an id, then
// a severity. Both bullets, so a hand-write under either one is caught.
var findingLineRE = regexp.MustCompile(`[-•]\s*id:[0-9a-f]{6,}\s+sev:`)

// The '- id:<hex> sev:<sev> <path>:<line> — <title>' lines are a format
// contract with editorial.BuildPriorFindings, so a formula must not hand-write
// them: `gt mq reject --findings-json` formats them from the om verdict
// (gt-3mp1, gt-s4f6).
func TestRefineryRejectionRecordsFindingsViaReject(t *testing.T) {
	const patrol = "formulas/mol-refinery-patrol.formula.toml"
	raw, err := formulasFS.ReadFile(patrol)
	if err != nil {
		t.Fatalf("reading %s: %v", patrol, err)
	}
	if !rejectFindingsRE.MatchString(string(raw)) {
		t.Fatalf("%s: no `gt mq reject ... --findings-json` — an editorial rejection then reaches the next attempt with no durable findings", patrol)
	}

	files, err := fs.Glob(formulasFS, "formulas/*.formula.toml")
	if err != nil {
		t.Fatalf("globbing embedded formulas: %v", err)
	}
	for _, file := range files {
		raw, err := formulasFS.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		if m := findingLineRE.FindString(string(raw)); m != "" {
			t.Errorf("%s: hand-formatted finding line %q — compose it with `gt mq reject --findings-json` instead, so the writer and the parser cannot disagree", file, m)
		}
	}
}

// gt-nxvg: `--notes` REPLACES a bead's notes. A rejection write through it
// wiped the polecat's implementation notes, and the resume path
// (mol-polecat-work's load-context) greps notes for the MERGE REJECTION
// marker and the Branch: line, so a resumed bead needs both blocks intact.
func TestFormulaNotesWritesAppend(t *testing.T) {
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
		{"formulas/mol-refinery-patrol.formula.toml", rejectionWriteRE, 1},
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
			if m[1] != "--append-notes" {
				t.Errorf("%s: note write uses %s; it must append so prior notes survive", tc.file, m[1])
			}
		}
	}

	for _, file := range files {
		raw, err := formulasFS.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, m := range rejectionWriteRE.FindAllStringSubmatch(string(raw), -1) {
			if m[1] != "--append-notes" {
				t.Errorf("%s: MERGE REJECTION note write uses %s; it must append so the polecat's own notes survive", file, m[1])
			}
		}
	}
}
