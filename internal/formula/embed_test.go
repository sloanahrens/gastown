package formula

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGetEmbeddedFormulas verifies embedded formulas can be read and hashed.
func TestGetEmbeddedFormulas(t *testing.T) {
	t.Parallel()
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		t.Fatalf("getEmbeddedFormulas() error: %v", err)
	}
	if len(embedded) == 0 {
		t.Error("should have embedded formulas")
	}

	// Verify at least one known formula exists
	if _, ok := embedded["mol-polecat-work.formula.toml"]; !ok {
		t.Error("should contain mol-polecat-work.formula.toml")
	}

	// Verify hashes are valid hex strings
	for name, hash := range embedded {
		if len(hash) != 64 {
			t.Errorf("%s hash has wrong length: %d", name, len(hash))
		}
	}
}

// TestResolveFormulaContent verifies resolution order: rig > town > embedded.
func TestResolveFormulaContent(t *testing.T) {
	t.Parallel()
	t.Run("returns embedded formula when no disk overrides exist", func(t *testing.T) {
		content, err := ResolveFormulaContent("mol-polecat-work", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(content) == 0 {
			t.Error("expected non-empty content")
		}
	})

	t.Run("town-level formula shadows embedded", func(t *testing.T) {
		tmpDir := t.TempDir()
		formulasDir := filepath.Join(tmpDir, ".beads", "formulas")
		if err := os.MkdirAll(formulasDir, 0755); err != nil {
			t.Fatal(err)
		}
		townContent := []byte("formula = \"mol-polecat-work\"\nversion = 99\n")
		if err := os.WriteFile(filepath.Join(formulasDir, "mol-polecat-work.formula.toml"), townContent, 0644); err != nil {
			t.Fatal(err)
		}

		content, err := ResolveFormulaContent("mol-polecat-work", tmpDir, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(content) != string(townContent) {
			t.Error("expected town-level content to shadow embedded")
		}
	})

	t.Run("rig-level formula shadows town and embedded", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Write town-level formula
		townFormulasDir := filepath.Join(tmpDir, ".beads", "formulas")
		if err := os.MkdirAll(townFormulasDir, 0755); err != nil {
			t.Fatal(err)
		}
		townContent := []byte("formula = \"mol-polecat-work\"\nversion = 99\n")
		if err := os.WriteFile(filepath.Join(townFormulasDir, "mol-polecat-work.formula.toml"), townContent, 0644); err != nil {
			t.Fatal(err)
		}

		// Write rig-level formula (takes priority)
		rigFormulasDir := filepath.Join(tmpDir, "myrig", ".beads", "formulas")
		if err := os.MkdirAll(rigFormulasDir, 0755); err != nil {
			t.Fatal(err)
		}
		rigContent := []byte("formula = \"mol-polecat-work\"\nversion = 100\n")
		if err := os.WriteFile(filepath.Join(rigFormulasDir, "mol-polecat-work.formula.toml"), rigContent, 0644); err != nil {
			t.Fatal(err)
		}

		content, err := ResolveFormulaContent("mol-polecat-work", tmpDir, "myrig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(content) != string(rigContent) {
			t.Error("expected rig-level content to shadow town and embedded")
		}
	})

	t.Run("falls back to town when rig has no override", func(t *testing.T) {
		tmpDir := t.TempDir()
		townFormulasDir := filepath.Join(tmpDir, ".beads", "formulas")
		if err := os.MkdirAll(townFormulasDir, 0755); err != nil {
			t.Fatal(err)
		}
		townContent := []byte("formula = \"mol-custom-test\"\nversion = 1\n")
		if err := os.WriteFile(filepath.Join(townFormulasDir, "mol-custom-test.formula.toml"), townContent, 0644); err != nil {
			t.Fatal(err)
		}

		content, err := ResolveFormulaContent("mol-custom-test", tmpDir, "myrig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(content) != string(townContent) {
			t.Error("expected town-level content when rig has no override")
		}
	})

	t.Run("returns error when not found anywhere", func(t *testing.T) {
		_, err := ResolveFormulaContent("mol-does-not-exist", "", "")
		if err == nil {
			t.Error("expected error for non-existent formula")
		}
	})
}

// TestGetEmbeddedFormulaContent verifies extraction of individual embedded formulas.
func TestGetEmbeddedFormulaContent(t *testing.T) {
	t.Parallel()
	// Known embedded formula should succeed
	content, err := GetEmbeddedFormulaContent("mol-polecat-work")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-polecat-work) error: %v", err)
	}
	if len(content) == 0 {
		t.Error("expected non-empty content")
	}

	// With suffix should also work
	content2, err := GetEmbeddedFormulaContent("mol-polecat-work.formula.toml")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent with suffix error: %v", err)
	}
	if string(content) != string(content2) {
		t.Error("content should be identical with or without suffix")
	}

	// Non-existent formula should fail
	_, err = GetEmbeddedFormulaContent("nonexistent-formula")
	if err == nil {
		t.Error("expected error for non-existent formula")
	}
}

// syncedTown returns a temp town whose formulas dir gt has just synced.
func syncedTown(t *testing.T) (townRoot, formulasDir string) {
	t.Helper()
	townRoot = t.TempDir()
	if _, err := SyncFormulas(townRoot, SyncOptions{}); err != nil {
		t.Fatalf("SyncFormulas() error: %v", err)
	}
	return townRoot, filepath.Join(townRoot, ".beads", "formulas")
}

func writeTownFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func actionOf(plan *SyncPlan, name string) SyncAction {
	for _, e := range plan.Entries {
		if e.Name == name {
			return e.Action
		}
	}
	return ""
}

const workFormula = "mol-polecat-work.formula.toml"

// TestSyncFormulas_FreshTownWritesEveryFormulaWithItsHash verifies the write
// path: every embedded formula lands on disk and its content hash is recorded.
func TestSyncFormulas_FreshTownWritesEveryFormulaWithItsHash(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	count, err := ProvisionFormulas(townRoot)
	if err != nil {
		t.Fatalf("ProvisionFormulas() error: %v", err)
	}
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		t.Fatal(err)
	}
	if count != len(embedded) {
		t.Errorf("provisioned %d formulas, want %d", count, len(embedded))
	}

	formulasDir := filepath.Join(townRoot, ".beads", "formulas")
	record, err := loadInstalledRecord(formulasDir)
	if err != nil {
		t.Fatal(err)
	}
	for name, hash := range embedded {
		if record.Formulas[name] != hash {
			t.Errorf("record[%s] = %q, want embedded hash %q", name, record.Formulas[name], hash)
		}
		if got, _ := computeFileHash(filepath.Join(formulasDir, name)); got != hash {
			t.Errorf("%s on disk hashes %q, want %q", name, got, hash)
		}
	}

	again, err := SyncFormulas(townRoot, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed() != 0 || again.UpToDate() != len(embedded) {
		t.Errorf("second sync changed %d, up-to-date %d; want 0 and %d", again.Changed(), again.UpToDate(), len(embedded))
	}
}

// TestSyncFormulas_ClassifiesEveryFileByHash covers each state a town copy can
// be in, through a dry run (writes nothing) and then a real sync.
func TestSyncFormulas_ClassifiesEveryFileByHash(t *testing.T) {
	t.Parallel()
	townRoot, formulasDir := syncedTown(t)
	embeddedWork, err := GetEmbeddedFormulaContent(workFormula)
	if err != nil {
		t.Fatal(err)
	}

	// update: disk is what gt last wrote; the binary has moved on.
	const outdated = "mol-polecat-code-review.formula.toml"
	oldContent := []byte("formula = \"older\"\n")
	writeTownFile(t, filepath.Join(formulasDir, outdated), oldContent)
	// replace-drift: hand-edited after gt wrote it.
	handEdit := append(append([]byte{}, embeddedWork...), []byte("\n# hand edit\n")...)
	writeTownFile(t, filepath.Join(formulasDir, workFormula), handEdit)
	// reinstall: gt wrote it, someone deleted it.
	const deleted = "code-review.formula.toml"
	if err := os.Remove(filepath.Join(formulasDir, deleted)); err != nil {
		t.Fatal(err)
	}
	// orphaned: gt wrote it, the binary no longer embeds it.
	const orphan = "mol-witness-patrol.formula.toml"
	writeTownFile(t, filepath.Join(formulasDir, orphan), []byte("formula = \"retired\"\n"))
	// unowned: hand-written formula, a .bak copy, a backup dir.
	const handWritten = "my-own.formula.toml"
	const bak = workFormula + ".bak-20260921-resync"
	writeTownFile(t, filepath.Join(formulasDir, handWritten), []byte("formula = \"mine\"\n"))
	writeTownFile(t, filepath.Join(formulasDir, bak), []byte("old"))
	if err := os.Mkdir(filepath.Join(formulasDir, ".bak-20260922-drift"), 0755); err != nil {
		t.Fatal(err)
	}

	record, err := loadInstalledRecord(formulasDir)
	if err != nil {
		t.Fatal(err)
	}
	record.Formulas[outdated] = computeHash(oldContent)
	record.Formulas[orphan] = "sha-from-an-older-binary"
	if err := saveInstalledRecord(formulasDir, record); err != nil {
		t.Fatal(err)
	}

	want := map[string]SyncAction{
		outdated:               SyncUpdate,
		workFormula:            SyncReplaceDrift,
		deleted:                SyncReinstall,
		orphan:                 SyncOrphaned,
		handWritten:            SyncUnowned,
		bak:                    SyncUnowned,
		".bak-20260922-drift/": SyncUnowned,
		"shiny.formula.toml":   SyncUpToDate,
	}

	dry, err := PlanFormulaSync(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	for name, action := range want {
		if got := actionOf(dry, name); got != action {
			t.Errorf("dry run: %s = %q, want %q", name, got, action)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(formulasDir, workFormula)); string(data) != string(handEdit) {
		t.Error("dry run rewrote a drifted copy")
	}

	plan, err := SyncFormulas(townRoot, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, action := range want {
		if got := actionOf(plan, name); got != action {
			t.Errorf("sync: %s = %q, want %q", name, got, action)
		}
	}
	if plan.Changed() != 3 {
		t.Errorf("Changed() = %d, want 3 (update, replace-drift, reinstall)", plan.Changed())
	}

	// The binary is canonical: drifted, outdated and deleted copies now carry
	// the embedded content and its recorded hash.
	after, err := loadInstalledRecord(formulasDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{outdated, workFormula, deleted} {
		embedded, err := GetEmbeddedFormulaContent(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(formulasDir, name))
		if err != nil || string(got) != string(embedded) {
			t.Errorf("%s not rewritten to the embedded content (err=%v)", name, err)
		}
		if after.Formulas[name] != computeHash(embedded) {
			t.Errorf("record[%s] not the embedded hash", name)
		}
	}
	// Files the binary does not embed are reported, never touched.
	for _, name := range []string{orphan, handWritten, bak} {
		if _, err := os.Stat(filepath.Join(formulasDir, name)); err != nil {
			t.Errorf("sync touched %s: %v", name, err)
		}
	}
	if _, ok := after.Formulas[orphan]; !ok {
		t.Error("sync dropped the orphan's record, so the next sync would call it unowned")
	}
}

// TestSyncFormulas_UnrecordedCopyThatDiffersIsDrift verifies a copy gt has no
// record of (a hand copy, or an older gt) is replaced unless it already matches.
func TestSyncFormulas_UnrecordedCopyThatDiffersIsDrift(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	formulasDir := filepath.Join(townRoot, ".beads", "formulas")
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		t.Fatal(err)
	}
	embeddedWork, err := GetEmbeddedFormulaContent(workFormula)
	if err != nil {
		t.Fatal(err)
	}
	writeTownFile(t, filepath.Join(formulasDir, workFormula), []byte("formula = \"hand copy\"\n"))
	writeTownFile(t, filepath.Join(formulasDir, "shiny.formula.toml"), mustEmbedded(t, "shiny"))

	plan, err := PlanFormulaSync(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(plan, workFormula); got != SyncReplaceDrift {
		t.Errorf("%s = %q, want %q", workFormula, got, SyncReplaceDrift)
	}
	if got := actionOf(plan, "shiny.formula.toml"); got != SyncUpToDate {
		t.Errorf("matching unrecorded copy = %q, want %q", got, SyncUpToDate)
	}

	if _, err := ProvisionFormulas(townRoot); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(formulasDir, workFormula)); string(got) != string(embeddedWork) {
		t.Error("gt install kept a hand copy instead of writing the embedded formula")
	}
}

// TestPlanFormulaSync_FreshTownWritesNothing verifies the dry run creates no
// directory and no record.
func TestPlanFormulaSync_FreshTownWritesNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	plan, err := PlanFormulaSync(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Installed()) == 0 {
		t.Error("dry run on an empty town should plan installs")
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".beads")); !os.IsNotExist(err) {
		t.Errorf("dry run created .beads: %v", err)
	}
}

func mustEmbedded(t *testing.T, name string) []byte {
	t.Helper()
	data, err := GetEmbeddedFormulaContent(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
