package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

func TestNewFormulaCheck(t *testing.T) {
	t.Parallel()
	check := NewFormulaCheck()
	if check.Name() != "formulas" {
		t.Errorf("Name() = %q, want %q", check.Name(), "formulas")
	}
	if !check.CanFix() {
		t.Error("FormulaCheck should be fixable")
	}
}

func TestFormulaCheck_Run_AllOK(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Provision formulas fresh
	_, err := formula.ProvisionFormulas(tmpDir)
	if err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want %v", result.Status, StatusOK)
	}
}

func TestFormulaCheck_Run_Missing(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Provision formulas
	_, err := formula.ProvisionFormulas(tmpDir)
	if err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	// Delete a formula
	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	formulaPath := filepath.Join(formulasDir, "mol-convoy-feed.formula.toml")
	if err := os.Remove(formulaPath); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want %v", result.Status, StatusWarning)
	}
	if result.FixHint == "" {
		t.Error("should have FixHint")
	}
}

func TestFormulaCheck_Fix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Provision formulas
	_, err := formula.ProvisionFormulas(tmpDir)
	if err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	// Delete a formula
	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	formulaPath := filepath.Join(formulasDir, "mol-convoy-feed.formula.toml")
	if err := os.Remove(formulaPath); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	// Run fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}

	// Verify formula was restored
	if _, err := os.Stat(formulaPath); os.IsNotExist(err) {
		t.Error("formula should have been restored")
	}

	// Re-run check - should be OK now
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("after fix, Status = %v, want %v", result.Status, StatusOK)
	}
}

// TestFormulaCheck_Run_DriftIsAnError: the binary is canonical (gt-fd2cu.3),
// so a town copy whose hash gt never wrote fails doctor, and --fix replaces it.
// While it stands, the formula this binary carries reaches nobody (gt-dt7r).
func TestFormulaCheck_Run_DriftIsAnError(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	edited := "mol-polecat-work.formula.toml"
	editedPath := filepath.Join(formulasDir, edited)
	if err := os.WriteFile(editedPath, []byte("# hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Errorf("Status = %v, want %v: a drifted formula fails doctor", result.Status, StatusError)
	}
	if !strings.Contains(result.Message, "1 drifted") {
		t.Errorf("Message = %q, want the drift counted", result.Message)
	}
	if d := detailFor(result, edited); !strings.Contains(d, "not one gt wrote") {
		t.Errorf("detail for %s = %q, want the hash finding", edited, d)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	embedded, err := formula.GetEmbeddedFormulaContent(edited)
	if err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(editedPath); string(content) != string(embedded) {
		t.Error("Fix() left the drifted copy in place")
	}
	if after := check.Run(ctx); after.Status != StatusOK {
		t.Errorf("after fix, Status = %v (%s), want OK", after.Status, after.Message)
	}
}

// TestFormulaCheck_Run_FilesGtDoesNotOwnAreErrorsFixNeverDeletes: an orphaned
// copy of a deleted formula and a hand-written file both fail doctor, and --fix
// leaves them for an operator to promote or delete.
func TestFormulaCheck_Run_FilesGtDoesNotOwnAreErrorsFixNeverDeletes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}
	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	orphan := "mol-deacon-patrol.formula.toml"
	handWritten := "mol-polecat-work.formula.toml.bak-20260921-resync"
	for _, name := range []string{orphan, handWritten} {
		if err := os.WriteFile(filepath.Join(formulasDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recordPath := filepath.Join(formulasDir, ".installed.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Formulas map[string]string `json:"formulas"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Formulas[orphan] = "sha-from-an-older-binary"
	out, _ := json.Marshal(record)
	if err := os.WriteFile(recordPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewFormulaCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Errorf("Status = %v, want %v", result.Status, StatusError)
	}
	if d := detailFor(result, orphan); !strings.Contains(d, "no longer embeds") {
		t.Errorf("orphan detail = %q", d)
	}
	if d := detailFor(result, handWritten); !strings.Contains(d, "promote it into source or delete it") {
		t.Errorf("hand-written detail = %q", d)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	for _, name := range []string{orphan, handWritten} {
		if _, err := os.Stat(filepath.Join(formulasDir, name)); err != nil {
			t.Errorf("Fix() removed %s: %v", name, err)
		}
	}
	if after := check.Run(ctx); after.Status != StatusError {
		t.Errorf("after fix, Status = %v, want still %v until an operator deletes them", after.Status, StatusError)
	}
}

// stageStaleCopy writes a town-tier copy of mol-polecat-work that keeps its
// version and grows by staleBytes, then records it as installed so the copy
// reads as "outdated" rather than hand-edited. Callers that want the
// hand-edited reading pass recordAsInstalled=false.
func stageStaleCopy(t *testing.T, tmpDir string, staleBytes []byte, recordAsInstalled bool) (townSize, embeddedSize int) {
	t.Helper()

	formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
	name := "mol-polecat-work.formula.toml"
	path := filepath.Join(formulasDir, name)

	embedded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := append(append([]byte{}, embedded...), staleBytes...)
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if !recordAsInstalled {
		return len(stale), len(embedded)
	}

	recordPath := filepath.Join(formulasDir, ".installed.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Formulas map[string]string `json:"formulas"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(stale)
	record.Formulas[name] = hex.EncodeToString(sum[:])
	updated, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	return len(stale), len(embedded)
}

// detailFor returns the detail line naming the given formula.
func detailFor(result *CheckResult, name string) string {
	for _, d := range result.Details {
		if strings.Contains(d, name) {
			return d
		}
	}
	return ""
}

// TestFormulaCheck_Run_SameVersionDivergenceNamesBothSizes is the regression
// test for gt-aydo: a town-tier copy that keeps the embedded version while its
// bytes diverge is announced by no version bump, so the warning has to carry
// both sizes for a reader to see the drift at all.
func TestFormulaCheck_Run_SameVersionDivergenceNamesBothSizes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	townSize, embeddedSize := stageStaleCopy(t, tmpDir, []byte("\n# a block main has since rewritten\n"), false)

	result := NewFormulaCheck().Run(&CheckContext{TownRoot: tmpDir})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want %v", result.Status, StatusError)
	}
	detail := detailFor(result, "mol-polecat-work.formula.toml")
	if detail == "" {
		t.Fatalf("Details do not name the stale copy: %v", result.Details)
	}
	if !strings.Contains(detail, "same version as embedded") {
		t.Errorf("detail = %q, want the same-version divergence named", detail)
	}
	if !strings.Contains(detail, exactBytes(townSize)) {
		t.Errorf("detail = %q, want the town size %d", detail, townSize)
	}
	if !strings.Contains(detail, exactBytes(embeddedSize)) {
		t.Errorf("detail = %q, want the embedded size %d", detail, embeddedSize)
	}
}

// TestFormulaCheck_Run_OutdatedAtSameVersionNamesBothSizes drives the same
// disclosure on the "outdated" reading: bytes this town recorded as installed,
// shadowed by embedded content at the same version.
func TestFormulaCheck_Run_OutdatedAtSameVersionNamesBothSizes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	townSize, embeddedSize := stageStaleCopy(t, tmpDir, []byte("\n# an older reading of the same block\n"), true)

	result := NewFormulaCheck().Run(&CheckContext{TownRoot: tmpDir})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want %v", result.Status, StatusWarning)
	}
	detail := detailFor(result, "mol-polecat-work.formula.toml")
	if !strings.Contains(detail, "update available") {
		t.Fatalf("detail = %q, want the outdated reading", detail)
	}
	if !strings.Contains(detail, exactBytes(townSize)) || !strings.Contains(detail, exactBytes(embeddedSize)) {
		t.Errorf("detail = %q, want both sizes %d and %d", detail, townSize, embeddedSize)
	}
}

// TestFormulaCheck_Run_BumpedVersionOverrideOmitsSizes pins the other side: a
// copy whose version is ahead of the embedded one is a local advance, and the
// version already says so.
func TestFormulaCheck_Run_BumpedVersionOverrideOmitsSizes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if _, err := formula.ProvisionFormulas(tmpDir); err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}

	path := filepath.Join(tmpDir, ".beads", "formulas", "mol-polecat-work.formula.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	version, err := formulaVersion(data)
	if err != nil {
		t.Fatal(err)
	}
	from, to := fmt.Sprintf("version = %d", version), fmt.Sprintf("version = %d", version+1)
	edited := strings.Replace(string(data), from, to, 1)
	if !strings.Contains(edited, to) {
		t.Fatalf("version bump %q did not apply; the formula's version line moved", from)
	}
	if err := os.WriteFile(path, append([]byte(edited), []byte("\n# a local advance\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	result := NewFormulaCheck().Run(&CheckContext{TownRoot: tmpDir})

	if detail := detailFor(result, "mol-polecat-work.formula.toml"); strings.Contains(detail, "same version as embedded") {
		t.Errorf("detail = %q, want no same-version clause for a bumped copy", detail)
	}
}

// TestExactBytes pins the detail-line rendering: a size a reader can compare
// at a glance.
func TestExactBytes(t *testing.T) {
	t.Parallel()
	cases := map[int]string{
		0:       "0 B",
		999:     "999 B",
		1000:    "1,000 B",
		30870:   "30,870 B",
		1234567: "1,234,567 B",
	}
	for n, want := range cases {
		if got := exactBytes(n); got != want {
			t.Errorf("exactBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
