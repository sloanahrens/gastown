package formula

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Formulas live in internal/formula/formulas/ (source of truth).
// They are embedded into the binary and provisioned to .beads/formulas/ at install time.

//go:embed formulas/*.formula.toml
var formulasFS embed.FS

// InstalledRecord tracks which formulas were installed and their checksums.
// Stored in .beads/formulas/.installed.json
type InstalledRecord struct {
	Formulas map[string]string `json:"formulas"` // filename -> sha256 at install time
}

// FormulaStatus represents the status of a single formula during health check.
type FormulaStatus struct {
	Name          string
	Status        string // "ok", "outdated", "modified", "missing", "new", "untracked"
	EmbeddedHash  string // hash computed from embedded content
	InstalledHash string // hash we installed (from .installed.json)
	CurrentHash   string // hash of current file on disk
}

// HealthReport contains the results of checking formula health.
type HealthReport struct {
	Formulas []FormulaStatus
	// Counts
	OK        int
	Outdated  int // embedded changed, user hasn't modified
	Modified  int // user modified the file (tracked in .installed.json)
	Missing   int // file was deleted
	New       int // new formula not yet installed
	Untracked int // file exists but not in .installed.json (safe to update)
	Error     int // file could not be read (e.g. permission denied)
}

// ResolveFormulaContent resolves formula content using the three-tier precedence
// defined in docs/design/formula-resolution.md: rig > town > embedded.
//
// Tier 1 (rig): townRoot/rigName/.beads/formulas/<name>.formula.toml
// Tier 2 (town): townRoot/.beads/formulas/<name>.formula.toml
// Tier 3 (embedded): compiled into the binary
//
// Either townRoot or rigName may be empty; those tiers are skipped.
func ResolveFormulaContent(name, townRoot, rigName string) ([]byte, error) {
	filename := name
	if !hasFormulaSuffix(filename) {
		filename = filename + ".formula.toml"
	}

	// Tier 1: rig-level (most specific)
	if townRoot != "" && rigName != "" {
		path := filepath.Join(townRoot, rigName, ".beads", "formulas", filename)
		if content, err := os.ReadFile(path); err == nil {
			return content, nil
		}
	}

	// Tier 2: town-level
	if townRoot != "" {
		path := filepath.Join(townRoot, ".beads", "formulas", filename)
		if content, err := os.ReadFile(path); err == nil {
			return content, nil
		}
	}

	// Tier 3: embedded (system fallback)
	return GetEmbeddedFormulaContent(name)
}

// GetEmbeddedFormulaContent returns the raw content of an embedded formula by name.
// The name can be with or without the .formula.toml suffix.
// Returns the content bytes, or an error if the formula is not found.
func GetEmbeddedFormulaContent(name string) ([]byte, error) {
	// Normalize: ensure the filename has the correct suffix
	filename := name
	if !hasFormulaSuffix(filename) {
		filename = filename + ".formula.toml"
	}
	content, err := formulasFS.ReadFile("formulas/" + filename)
	if err != nil {
		return nil, fmt.Errorf("embedded formula %q not found: %w", name, err)
	}
	return content, nil
}

// hasFormulaSuffix checks if a name already has a formula file suffix.
func hasFormulaSuffix(name string) bool {
	return len(name) > len(".formula.toml") &&
		name[len(name)-len(".formula.toml"):] == ".formula.toml"
}

// computeHash computes SHA256 hash of data.
func computeHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// getEmbeddedFormulas returns a map of filename -> sha256 for all embedded formulas.
func getEmbeddedFormulas() (map[string]string, error) {
	entries, err := formulasFS.ReadDir("formulas")
	if err != nil {
		return nil, fmt.Errorf("reading formulas directory: %w", err)
	}

	result := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := formulasFS.ReadFile("formulas/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		result[entry.Name()] = computeHash(content)
	}
	return result, nil
}

// loadInstalledRecord loads the installed record from disk.
func loadInstalledRecord(formulasDir string) (*InstalledRecord, error) {
	path := filepath.Join(formulasDir, ".installed.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &InstalledRecord{Formulas: make(map[string]string)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading installed record: %w", err)
	}
	var r InstalledRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing installed record: %w", err)
	}
	if r.Formulas == nil {
		r.Formulas = make(map[string]string)
	}
	return &r, nil
}

// saveInstalledRecord saves the installed record to disk.
func saveInstalledRecord(formulasDir string, record *InstalledRecord) error {
	path := filepath.Join(formulasDir, ".installed.json")
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding installed record: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// computeFileHash computes SHA256 hash of a file.
func computeFileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return computeHash(data), nil
}

// ProvisionFormulas creates the .beads/formulas/ directory with embedded formulas.
// This is called during gt install for fresh installations.
// If a formula already exists, it is skipped (no overwrite).
// Returns the number of formulas provisioned.
func ProvisionFormulas(beadsPath string) (int, error) {
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		return 0, err
	}

	entries, err := formulasFS.ReadDir("formulas")
	if err != nil {
		return 0, fmt.Errorf("reading formulas directory: %w", err)
	}

	// Create .beads/formulas/ directory
	formulasDir := filepath.Join(beadsPath, ".beads", "formulas")
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		return 0, fmt.Errorf("creating formulas directory: %w", err)
	}

	// Load existing installed record (or create new)
	installed, err := loadInstalledRecord(formulasDir)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		destPath := filepath.Join(formulasDir, entry.Name())

		// Skip if formula already exists (don't overwrite user customizations)
		if _, err := os.Stat(destPath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return count, fmt.Errorf("checking %s: %w", entry.Name(), err)
		}

		content, err := formulasFS.ReadFile("formulas/" + entry.Name())
		if err != nil {
			return count, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}

		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return count, fmt.Errorf("writing %s: %w", entry.Name(), err)
		}

		// Record the hash we installed
		if hash, ok := embedded[entry.Name()]; ok {
			installed.Formulas[entry.Name()] = hash
		}
		count++
	}

	// Save updated installed record
	if err := saveInstalledRecord(formulasDir, installed); err != nil {
		return count, fmt.Errorf("saving installed record: %w", err)
	}

	return count, nil
}

// CheckFormulaHealth checks the status of all formulas.
// Returns a report of which formulas are ok, outdated, modified, or missing.
func CheckFormulaHealth(beadsPath string) (*HealthReport, error) {
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		return nil, err
	}

	formulasDir := filepath.Join(beadsPath, ".beads", "formulas")
	installed, err := loadInstalledRecord(formulasDir)
	if err != nil {
		return nil, err
	}

	report := &HealthReport{}

	for filename, embeddedHash := range embedded {
		status := FormulaStatus{
			Name:         filename,
			EmbeddedHash: embeddedHash,
		}

		installedHash, wasInstalled := installed.Formulas[filename]
		status.InstalledHash = installedHash

		destPath := filepath.Join(formulasDir, filename)
		currentHash, err := computeFileHash(destPath)

		if os.IsNotExist(err) {
			// File doesn't exist
			if wasInstalled {
				// We installed it before, user deleted it
				status.Status = "missing"
				report.Missing++
			} else {
				// New formula, never installed
				status.Status = "new"
				report.New++
			}
		} else if err != nil {
			// Some other error reading file (e.g. permission denied)
			status.Status = "error"
			report.Error++
		} else {
			status.CurrentHash = currentHash

			if currentHash == embeddedHash {
				// File matches embedded - all good
				status.Status = "ok"
				report.OK++
			} else if wasInstalled && currentHash == installedHash {
				// File matches what we installed, but embedded has changed
				// User hasn't modified, safe to update
				status.Status = "outdated"
				report.Outdated++
			} else if wasInstalled {
				// File was tracked and user modified it - don't overwrite
				status.Status = "modified"
				report.Modified++
			} else {
				// File exists but not tracked (e.g., from older gt version)
				// Safe to update since we have no record of user modification
				status.Status = "untracked"
				report.Untracked++
			}
		}

		report.Formulas = append(report.Formulas, status)
	}

	return report, nil
}

// SyncAction is what a sync does with one formula's town copy.
type SyncAction string

const (
	// SyncUpToDate: the town copy already matches the embedded content.
	SyncUpToDate SyncAction = "up-to-date"
	// SyncUpdate: the town copy is unmodified but older than the embedded content.
	SyncUpdate SyncAction = "update"
	// SyncReinstall: the town copy was deleted; the embedded content is written back.
	SyncReinstall SyncAction = "reinstall"
	// SyncInstall: no town copy ever existed; this is the first write of it.
	SyncInstall SyncAction = "install"
	// SyncSkipModified: the town copy was hand-edited, so sync will not overwrite
	// it and the embedded content goes undelivered until that is resolved.
	SyncSkipModified SyncAction = "skip-modified"
	// SyncForceOverwrite: the town copy was hand-edited and opts.Force replaced
	// it with the embedded content, after backing the edit up.
	SyncForceOverwrite SyncAction = "force-overwrite"
)

// SyncEntry is one formula's disposition in a SyncPlan.
type SyncEntry struct {
	Name   string
	Action SyncAction
	// Superseded is true when the embedded content also moved past what the last
	// install recorded, so the blocked copy is hiding a newer formula rather than
	// only preserving a local edit.
	Superseded bool
}

// SyncPlan is what a sync did, or would do, to the town's formula copies.
type SyncPlan struct {
	Entries []SyncEntry
}

// names returns the names of every entry with the given action.
func (p *SyncPlan) names(action SyncAction) []string {
	var out []string
	for _, e := range p.Entries {
		if e.Action == action {
			out = append(out, e.Name)
		}
	}
	return out
}

// Updated returns the formulas rewritten because the embedded content moved ahead.
func (p *SyncPlan) Updated() []string { return p.names(SyncUpdate) }

// Reinstalled returns the formulas rewritten because the town copy was deleted.
func (p *SyncPlan) Reinstalled() []string { return p.names(SyncReinstall) }

// Installed returns the formulas the town had no copy of at all.
func (p *SyncPlan) Installed() []string { return p.names(SyncInstall) }

// SkippedModified returns the formulas sync refused to overwrite. Their embedded
// content is not on disk, so any fix merged into it is undelivered.
func (p *SyncPlan) SkippedModified() []string { return p.names(SyncSkipModified) }

// Superseded returns the hand-edited formulas whose embedded content also moved
// past the last install, i.e. copies hiding a newer formula, not just a local edit.
func (p *SyncPlan) Superseded() []string {
	var out []string
	for _, e := range p.Entries {
		if e.Superseded && (e.Action == SyncSkipModified || e.Action == SyncForceOverwrite) {
			out = append(out, e.Name)
		}
	}
	return out
}

// ForceOverwritten returns the hand-edited formulas --force replaced.
func (p *SyncPlan) ForceOverwritten() []string { return p.names(SyncForceOverwrite) }

// UpToDate returns the count of formulas already matching the embedded content.
func (p *SyncPlan) UpToDate() int {
	return len(p.names(SyncUpToDate))
}

// Changed returns the count of formulas written (installed + updated + reinstalled).
func (p *SyncPlan) Changed() int {
	return len(p.Installed()) + len(p.Updated()) + len(p.Reinstalled())
}

// SyncOptions tunes what a sync is allowed to do.
type SyncOptions struct {
	// DryRun classifies every formula without writing anything.
	DryRun bool
	// Force overwrites hand-edited copies too, backing each one up first.
	Force bool
}

// PlanFormulaSync classifies every embedded formula against the town's copy
// without writing anything, so a caller can inspect the sync before running it.
func PlanFormulaSync(beadsPath string) (*SyncPlan, error) {
	return SyncFormulas(beadsPath, SyncOptions{DryRun: true})
}

// SyncFormulas updates town formula copies from the embedded set and returns the
// plan it executed. Copies the user has edited since the last install are left
// alone unless opts.Force is set; see SyncSkipModified.
func SyncFormulas(beadsPath string, opts SyncOptions) (*SyncPlan, error) {
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		return nil, err
	}

	formulasDir := filepath.Join(beadsPath, ".beads", "formulas")
	if !opts.DryRun {
		if err := os.MkdirAll(formulasDir, 0755); err != nil {
			return nil, fmt.Errorf("creating formulas directory: %w", err)
		}
	}

	installed, err := loadInstalledRecord(formulasDir)
	if err != nil {
		return nil, err
	}

	plan := &SyncPlan{}
	var backups []BackupRecord

	// Sorted iteration: a sync writes files and rewrites .installed.json, so a
	// stable order keeps the record diffable across runs.
	for _, filename := range sortedNames(embedded) {
		embeddedHash := embedded[filename]
		installedHash, wasInstalled := installed.Formulas[filename]
		destPath := filepath.Join(formulasDir, filename)
		currentHash, fileErr := computeFileHash(destPath)

		// superseded marks the cases where the embedded content also moved past
		// what the last install recorded, so the copy on disk is hiding a newer
		// formula rather than only preserving a local edit.
		superseded := embeddedHash != installedHash

		var action SyncAction
		switch {
		case os.IsNotExist(fileErr) && wasInstalled:
			action = SyncReinstall
		case os.IsNotExist(fileErr):
			action = SyncInstall
		case fileErr != nil:
			// Unreadable copy: sync has nothing safe to do with it.
			continue
		case currentHash == embeddedHash:
			action = SyncUpToDate
		case wasInstalled && currentHash == installedHash:
			// Unmodified since install, safe to update.
			action = SyncUpdate
		case wasInstalled && opts.Force:
			// Hand-edited since install, overwritten on request.
			action = SyncForceOverwrite
		case wasInstalled:
			// Hand-edited since install: refuse.
			action = SyncSkipModified
		default:
			// Exists but untracked (e.g. from an older gt): safe to update.
			action = SyncUpdate
		}

		if action == SyncSkipModified {
			plan.Entries = append(plan.Entries, SyncEntry{
				Name:       filename,
				Action:     action,
				Superseded: superseded,
			})
			continue
		}

		if action == SyncUpToDate || opts.DryRun {
			plan.Entries = append(plan.Entries, SyncEntry{Name: filename, Action: action})
			continue
		}

		content, err := formulasFS.ReadFile("formulas/" + filename)
		if err != nil {
			return plan, fmt.Errorf("reading %s: %w", filename, err)
		}

		if action == SyncForceOverwrite {
			backup, err := backupFormulaFile(formulasDir, destPath, filename)
			if err != nil {
				return plan, err
			}
			backups = append(backups, backup)
		}

		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return plan, fmt.Errorf("writing %s: %w", filename, err)
		}
		installed.Formulas[filename] = embeddedHash
		plan.Entries = append(plan.Entries, SyncEntry{Name: filename, Action: action, Superseded: superseded})
	}

	if opts.DryRun {
		return plan, nil
	}

	if err := saveInstalledRecord(formulasDir, installed); err != nil {
		return plan, fmt.Errorf("saving installed record: %w", err)
	}
	if err := saveBackupManifest(formulasDir, backups); err != nil {
		return plan, err
	}

	return plan, nil
}

// UpdateFormulas syncs with default options: hand-edited copies are preserved.
func UpdateFormulas(beadsPath string) (*SyncPlan, error) {
	return SyncFormulas(beadsPath, SyncOptions{})
}

// BackupRecord names a hand-edited formula copy that --force displaced.
type BackupRecord struct {
	Formula string
	Path    string
}

// backupDirName is where --force parks displaced copies, alongside the live ones.
const backupDirName = ".bak"

// ForceBackupPath returns where a displacing --force sync parks the town copy of
// formula. It is a pure function of the beads path and the name, so a dry run
// can name the destination a real run would write without reading a manifest.
func ForceBackupPath(beadsPath, formula string) string {
	return backupPathFor(filepath.Join(beadsPath, ".beads", "formulas"), formula)
}

// backupPathFor returns where a displaced town copy of filename is parked.
func backupPathFor(formulasDir, filename string) string {
	return filepath.Join(formulasDir, backupDirName, filename)
}

// backupFormulaFile copies a hand-edited town formula aside before --force
// overwrites it, so an overwrite is recoverable rather than destructive.
func backupFormulaFile(formulasDir, destPath, filename string) (BackupRecord, error) {
	backupPath := backupPathFor(formulasDir, filename)
	if err := os.MkdirAll(filepath.Dir(backupPath), 0755); err != nil {
		return BackupRecord{}, fmt.Errorf("creating backup directory: %w", err)
	}
	content, err := os.ReadFile(destPath) //nolint:gosec // G304: path is inside the town formulas dir
	if err != nil {
		return BackupRecord{}, fmt.Errorf("reading %s to back it up: %w", filename, err)
	}
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		return BackupRecord{}, fmt.Errorf("backing up %s: %w", filename, err)
	}
	return BackupRecord{Formula: filename, Path: backupPath}, nil
}

// backupManifestName records what the last displacing --force sync overwrote, so
// a later reader can find a hand edit that is no longer in the live copy.
const backupManifestName = "last-force-sync.txt"

// ReadForceBackupManifest returns the copies the last displacing --force sync
// moved aside. An empty result means no force sync has displaced anything.
func ReadForceBackupManifest(beadsPath string) ([]BackupRecord, error) {
	path := filepath.Join(beadsPath, ".beads", "formulas", backupDirName, backupManifestName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: fixed path under the given root
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading backup manifest: %w", err)
	}

	var records []BackupRecord
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		formula, backupPath, ok := strings.Cut(line, "\t")
		if ok && formula != "" {
			records = append(records, BackupRecord{Formula: formula, Path: backupPath})
		}
	}
	return records, nil
}

// saveBackupManifest records the displaced-copy list. A sync that displaced
// nothing leaves the previous manifest alone: its backups are still on disk, and
// dropping the pointer would strand them.
func saveBackupManifest(formulasDir string, backups []BackupRecord) error {
	if len(backups) == 0 {
		return nil
	}

	dir := filepath.Join(formulasDir, backupDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating backup directory: %w", err)
	}

	var b strings.Builder
	for _, rec := range backups {
		fmt.Fprintf(&b, "%s\t%s\n", rec.Formula, rec.Path)
	}
	if err := os.WriteFile(filepath.Join(dir, backupManifestName), []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("writing backup manifest: %w", err)
	}
	return nil
}

// sortedNames returns the map's keys in lexical order.
func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
