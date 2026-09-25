package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

// TestFormulaTierCheck_SameVersionDifferentBytes is the regression test for
// the 2026-09-19 stale-tier incident: ~/gt/.beads/formulas/mol-witness-
// patrol.formula.toml sat at version 18 (30,870 bytes, missing the gt-xb27
// Stall Judgement block) while main's embedded copy was version 18
// (36,443 bytes). A version bump is what every drift check keys on, so a
// town-tier override that holds the SAME version but different bytes passes
// them all — and the override shadows the system tier (tier 2 > tier 3), so
// every town agent read pre-fix instructions until a human refreshed it by
// hand. mol-convoy-feed (missing the gt-yg24 text) and mol-polecat-work
// (missing the gt-pxlg filtered-test text) hit the same hole the same day.
//
// This check must warn, name the file, and give both sizes.
func TestFormulaTierCheck_SameVersionDifferentBytes(t *testing.T) {
	tmpDir := t.TempDir()

	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	name := "mol-witness-patrol.formula.toml"
	path := filepath.Join(formulasDir, name)

	// Take the installed copy and append a block of the same age: the version
	// is unchanged, the bytes differ. That is exactly what "stale at the same
	// version" means.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	originalSize := len(data)
	data = append(data, []byte("\n# a block the embedded copy also carries\n")...)
	data = append(data, []byte("# but worded from before it landed\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaTierCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want %v: same-version different-bytes override is a warning", result.Status, StatusWarning)
	}

	var found bool
	for _, d := range result.Details {
		if strings.Contains(d, name) {
			found = true
			// The remedy is unambiguous: refresh from the embedded copy.
			if !strings.Contains(d, "version") {
				t.Errorf("detail for %s = %q, want the version named", name, d)
			}
			// Both sizes, so a human can eyeball the divergence.
			if !strings.Contains(d, formatByteSize(originalSize)) {
				t.Errorf("detail for %s = %q, want the embedded size %d", name, d, originalSize)
			}
			if !strings.Contains(d, formatByteSize(len(data))) {
				t.Errorf("detail for %s = %q, want the town size %d", name, d, len(data))
			}
		}
	}
	if !found {
		t.Fatalf("Details do not name %s: %v", name, result.Details)
	}

	// The check is advisory: --fix must not run, and there is nothing
	// auto-fixable about a user's tier choice.
	if check.CanFix() {
		t.Error("FormulaTierCheck should not be fixable: overwriting the town tier discards the user's copy")
	}
}

// TestFormulaTierCheck_IdenticalBytesNoWarning pins the other side of the
// comparison: a town-tier copy that is byte-identical to the embedded copy is
// a refresh, not an edit — no warning, whatever the version says.
func TestFormulaTierCheck_IdenticalBytesNoWarning(t *testing.T) {
	tmpDir := t.TempDir()

	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	check := NewFormulaTierCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want %v: fresh provisioned copies are identical to the embedded tier", result.Status, StatusOK)
	}
}

// TestFormulaTierCheck_NewerVersionOverrideNoWarning: a town-tier copy whose
// version is AHEAD of the embedded one is a local advance, not a stale
// shadow — the check is about same-version drift, not disagreement.
func TestFormulaTierCheck_NewerVersionOverrideNoWarning(t *testing.T) {
	tmpDir := t.TempDir()

	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	name := "mol-witness-patrol.formula.toml"
	path := filepath.Join(formulasDir, name)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Bump the version and add content: a deliberate local advance.
	data = []byte(strings.Replace(string(data), "version = 21", "version = 22", 1))
	data = append(data, []byte("\n# a local advance\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaTierCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want %v: a newer-version override is a local advance, not a stale shadow", result.Status, StatusOK)
	}
}

// TestFormulaTierCheck_OlderVersionOverrideNoWarning: a same-version check
// that finds an OLDER version on the town tier is the pre-existing "outdated"
// story owned by FormulaCheck, not this check's job.
func TestFormulaTierCheck_OlderVersionOverrideNoWarning(t *testing.T) {
	tmpDir := t.TempDir()

	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	name := "mol-witness-patrol.formula.toml"
	path := filepath.Join(formulasDir, name)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "version = 21", "version = 20", 1))
	data = append(data, []byte("\n# older wording\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaTierCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want %v: an older-version override is FormulaCheck's outdated story", result.Status, StatusOK)
	}
}