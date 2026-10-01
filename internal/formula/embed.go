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
)

// Formulas live in internal/formula/formulas/ (source of truth).
// They are embedded into the binary and provisioned to .beads/formulas/ at install time.

//go:embed formulas/*.formula.toml
var formulasFS embed.FS

// InstalledRecord is the content hash gt wrote for each town formula file; a
// file whose hash is neither this nor the embedded hash has drifted.
// Stored in .beads/formulas/.installed.json
type InstalledRecord struct {
	Formulas map[string]string `json:"formulas"` // filename -> sha256 at install time
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
	path := filepath.Join(formulasDir, installedRecordName)
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
	path := filepath.Join(formulasDir, installedRecordName)
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

// installedRecordName is gt's record of what it wrote into the formulas dir.
const installedRecordName = ".installed.json"

// ProvisionFormulas writes the embedded formulas into <beadsPath>/.beads/formulas
// at gt install. It is a sync: the binary is canonical, so a copy already on
// disk that differs from the embedded content is replaced, not kept.
// Returns the number of formula files written.
func ProvisionFormulas(beadsPath string) (int, error) {
	plan, err := SyncFormulas(beadsPath, SyncOptions{})
	if err != nil {
		return 0, err
	}
	return plan.Changed(), nil
}

// SyncAction is what a sync does with one file in the town formulas dir.
type SyncAction string

const (
	// SyncUpToDate: the town copy already matches the embedded content.
	SyncUpToDate SyncAction = "up-to-date"
	// SyncUpdate: the town copy is what gt last wrote, and the embedded content
	// has moved past it.
	SyncUpdate SyncAction = "update"
	// SyncReinstall: gt wrote this copy once and it was deleted.
	SyncReinstall SyncAction = "reinstall"
	// SyncInstall: no town copy ever existed; this is the first write of it.
	SyncInstall SyncAction = "install"
	// SyncReplaceDrift: the town copy matches neither the embedded content nor
	// the hash gt recorded when it last wrote it, so someone edited or copied it
	// by hand. The binary is canonical: sync replaces it. A change worth keeping
	// belongs in gastown source or an overlay.
	SyncReplaceDrift SyncAction = "replace-drift"
	// SyncOrphaned: gt wrote this town copy, but the binary no longer embeds it
	// (its formula was deleted from source). Sync leaves it for an operator to
	// delete.
	SyncOrphaned SyncAction = "orphaned"
	// SyncUnowned: a file in the formulas dir that gt never wrote and the binary
	// does not embed: a hand-written formula, a *.bak copy, a backup directory.
	// Every formula the town uses lives in gastown source, so sync reports it for
	// an operator to promote into source or delete.
	SyncUnowned SyncAction = "unowned"
)

// SyncEntry is one file's disposition in a SyncPlan.
type SyncEntry struct {
	Name   string
	Action SyncAction
	// DiskHash is the sha256 of the file on disk before the sync ("" when absent
	// or a directory).
	DiskHash string
	// EmbeddedHash is the sha256 of the binary's content ("" when not embedded).
	EmbeddedHash string
}

// SyncPlan is what a sync did, or would do, to the town formulas dir.
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

// ReplacedDrift returns the formulas whose town copy was hand-edited or
// hand-copied (its hash is not one gt wrote) and is replaced by the embedded content.
func (p *SyncPlan) ReplacedDrift() []string { return p.names(SyncReplaceDrift) }

// Orphaned returns the town copies gt wrote that this binary no longer
// embeds, such as the patrol formulas of a deleted agent (gt-zggoh).
func (p *SyncPlan) Orphaned() []string { return p.names(SyncOrphaned) }

// Unowned returns the files in the formulas dir that gt never wrote and the
// binary does not embed (gt-fd2cu.3).
func (p *SyncPlan) Unowned() []string { return p.names(SyncUnowned) }

// UpToDate returns the count of formulas already matching the embedded content.
func (p *SyncPlan) UpToDate() int {
	return len(p.names(SyncUpToDate))
}

// Changed returns the count of formula files written (or, in a dry run, that
// would be written).
func (p *SyncPlan) Changed() int {
	return len(p.Installed()) + len(p.Updated()) + len(p.Reinstalled()) + len(p.ReplacedDrift())
}

// SyncOptions tunes what a sync is allowed to do.
type SyncOptions struct {
	// DryRun classifies every file without writing anything.
	DryRun bool
}

// PlanFormulaSync classifies every file in the town formulas dir against the
// embedded set without writing anything. Doctor reports from it.
func PlanFormulaSync(beadsPath string) (*SyncPlan, error) {
	return SyncFormulas(beadsPath, SyncOptions{DryRun: true})
}

// SyncFormulas makes the town formulas dir match the binary: every embedded
// formula is written whose disk hash differs from the embedded hash, and the
// written hash is recorded in .installed.json. The binary is canonical
// (gt-y3pgh.6), so a hand-edited copy is replaced too. Files the binary does not
// embed are never touched, only reported (SyncOrphaned, SyncUnowned).
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

	// Sorted iteration: a sync writes files and rewrites .installed.json, so a
	// stable order keeps the record diffable across runs.
	for _, filename := range sortedNames(embedded) {
		embeddedHash := embedded[filename]
		installedHash, wasInstalled := installed.Formulas[filename]
		destPath := filepath.Join(formulasDir, filename)
		currentHash, fileErr := computeFileHash(destPath)

		var action SyncAction
		switch {
		case os.IsNotExist(fileErr) && wasInstalled:
			action = SyncReinstall
		case os.IsNotExist(fileErr):
			action = SyncInstall
		case fileErr != nil:
			return plan, fmt.Errorf("reading %s: %w", filename, fileErr)
		case currentHash == embeddedHash:
			action = SyncUpToDate
		case wasInstalled && currentHash == installedHash:
			action = SyncUpdate
		default:
			// Not a hash gt wrote: a hand edit, or a copy made by hand.
			action = SyncReplaceDrift
		}

		plan.Entries = append(plan.Entries, SyncEntry{
			Name:         filename,
			Action:       action,
			DiskHash:     currentHash,
			EmbeddedHash: embeddedHash,
		})

		if action == SyncUpToDate {
			// Record the hash even when nothing is written, so a copy that already
			// matched is drift-checked against it from now on.
			installed.Formulas[filename] = embeddedHash
			continue
		}
		if opts.DryRun {
			continue
		}

		content, err := formulasFS.ReadFile("formulas/" + filename)
		if err != nil {
			return plan, fmt.Errorf("reading %s: %w", filename, err)
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return plan, fmt.Errorf("writing %s: %w", filename, err)
		}
		installed.Formulas[filename] = embeddedHash
	}

	others, err := classifyNonEmbedded(formulasDir, embedded, installed)
	if err != nil {
		return plan, err
	}
	plan.Entries = append(plan.Entries, others...)

	if opts.DryRun {
		return plan, nil
	}

	if err := saveInstalledRecord(formulasDir, installed); err != nil {
		return plan, fmt.Errorf("saving installed record: %w", err)
	}
	return plan, nil
}

// classifyNonEmbedded reports every entry in the formulas dir the binary does
// not embed: a copy gt once wrote is orphaned, anything else is unowned. The
// record of an orphan is kept so the report survives later syncs.
func classifyNonEmbedded(formulasDir string, embedded map[string]string, installed *InstalledRecord) ([]SyncEntry, error) {
	dirEntries, err := os.ReadDir(formulasDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading formulas directory: %w", err)
	}

	var out []SyncEntry
	for _, de := range dirEntries {
		name := de.Name()
		if name == installedRecordName {
			continue
		}
		if _, ok := embedded[name]; ok {
			continue
		}
		entry := SyncEntry{Name: name, Action: SyncUnowned}
		if !de.IsDir() {
			hash, err := computeFileHash(filepath.Join(formulasDir, name))
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", name, err)
			}
			entry.DiskHash = hash
			if _, ok := installed.Formulas[name]; ok {
				entry.Action = SyncOrphaned
			}
		} else {
			entry.Name = name + "/"
		}
		out = append(out, entry)
	}
	return out, nil
}

// UpdateFormulas syncs the town formulas dir from the binary.
func UpdateFormulas(beadsPath string) (*SyncPlan, error) {
	return SyncFormulas(beadsPath, SyncOptions{})
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
