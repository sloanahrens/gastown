package plugin

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// SyncResult records the outcome of a plugin sync operation.
type SyncResult struct {
	Copied  []string // plugin names that were copied/updated
	Removed []string // plugin names that were removed (clean mode)
	Skipped []string // plugin names that were already up-to-date
	Errors  []string // errors encountered
	// Protected maps a plugin name to the runtime files a sync would have
	// destroyed (content the source repo never held); the plugin was left
	// untouched. Empty under SyncOptions.Force.
	Protected map[string][]string
}

// SyncOptions controls SyncPluginsWithOptions.
type SyncOptions struct {
	Clean bool // remove target plugins that are not in the source
	Force bool // overwrite/remove even plugins holding runtime edits
}

// SyncPlugins copies plugin directories from source to target.
// If clean is true, removes plugins from target that don't exist in source.
// Plugins whose runtime copy holds edits the source repo never had are left
// untouched and reported in Protected (gt-o848l).
func SyncPlugins(sourceDir, targetDir string, clean bool) (*SyncResult, error) {
	return SyncPluginsWithOptions(sourceDir, targetDir, SyncOptions{Clean: clean})
}

// SyncPluginsWithOptions is SyncPlugins with explicit options.
func SyncPluginsWithOptions(sourceDir, targetDir string, opts SyncOptions) (*SyncResult, error) {
	result := &SyncResult{}
	clean := opts.Clean
	var hist blobHistory
	var prefix string
	if !opts.Force {
		hist, prefix = sourceHistory(sourceDir)
	}
	// guard reports whether dstPluginDir may be replaced or removed; when it
	// may not, it records the runtime edits in result.Protected.
	guard := func(name, srcPluginDir, dstPluginDir string) bool {
		if opts.Force {
			return true
		}
		edits, err := runtimeEdits(name, srcPluginDir, dstPluginDir, hist, prefix)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: checking for runtime edits: %v", name, err))
			return false
		}
		if len(edits) == 0 {
			return true
		}
		if result.Protected == nil {
			result.Protected = map[string][]string{}
		}
		result.Protected[name] = edits
		return false
	}

	srcInfo, err := os.Stat(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("source directory %s: %w", sourceDir, err)
	}
	if !srcInfo.IsDir() {
		return nil, fmt.Errorf("source is not a directory: %s", sourceDir)
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("creating target directory: %w", err)
	}

	srcEntries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("reading source directory: %w", err)
	}

	srcPlugins := make(map[string]bool)
	for _, entry := range srcEntries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		pluginMD := filepath.Join(sourceDir, entry.Name(), "plugin.md")
		if _, err := os.Stat(pluginMD); err != nil {
			continue // Not a plugin directory
		}
		srcPlugins[entry.Name()] = true

		srcPluginDir := filepath.Join(sourceDir, entry.Name())
		dstPluginDir := filepath.Join(targetDir, entry.Name())

		if dirsMatch(srcPluginDir, dstPluginDir) {
			result.Skipped = append(result.Skipped, entry.Name())
			continue
		}
		if _, err := os.Stat(dstPluginDir); err == nil && !guard(entry.Name(), srcPluginDir, dstPluginDir) {
			continue
		}

		if err := copyDir(srcPluginDir, dstPluginDir); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		result.Copied = append(result.Copied, entry.Name())
	}

	if clean {
		dstEntries, err := os.ReadDir(targetDir)
		if err == nil {
			for _, entry := range dstEntries {
				if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
					continue
				}
				if !srcPlugins[entry.Name()] {
					dstPath := filepath.Join(targetDir, entry.Name())
					if !guard(entry.Name(), filepath.Join(sourceDir, entry.Name()), dstPath) {
						continue
					}
					if err := os.RemoveAll(dstPath); err != nil {
						result.Errors = append(result.Errors, fmt.Sprintf("removing %s: %v", entry.Name(), err))
					} else {
						result.Removed = append(result.Removed, entry.Name())
					}
				}
			}
		}
	}

	return result, nil
}

// dirsMatch checks if two plugin directories have identical contents.
func dirsMatch(src, dst string) bool {
	srcHash := DirHash(src)
	dstHash := DirHash(dst)
	return srcHash != "" && srcHash == dstHash
}

// DirHash computes a content hash of all files in a directory.
func DirHash(dir string) string {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: walking trusted plugin directory
		if err != nil {
			return err
		}
		h.Write(data)
		return nil
	})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// copyDir recursively copies a directory, replacing the destination atomically.
// It copies to a temp directory in the same parent, then swaps via rename.
func copyDir(src, dst string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(dst), ".plugin-sync-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	// Clean up temp dir on failure; on success it's been renamed away.
	defer os.RemoveAll(tmpDir)

	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		tmpPath := filepath.Join(tmpDir, rel)
		if d.IsDir() {
			return os.MkdirAll(tmpPath, 0755)
		}
		return copyFile(path, tmpPath)
	}); err != nil {
		return err
	}

	// Atomic swap: remove old dst, rename temp into place.
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("removing old destination: %w", err)
	}
	return os.Rename(tmpDir, dst)
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src) //nolint:gosec // G304: path is from trusted plugin directory
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode()) //nolint:gosec // G304: path is from trusted plugin directory
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// GastownSource is a resolved plugin source directory, plus the rule that
// chose it so a caller can say where a sync's plugins came from.
type GastownSource struct {
	Dir string
	// Rule names the layout that matched, phrased for a sync log.
	Rule string
}

// FindGastownSource locates the gastown source repo's plugins directory and
// reports which rule chose it.
//
// Candidates lie only inside the town: <gastown rig>/mayor/rig/plugins — the
// canonical checkout maintained by the mayor, resolved via mayor/rigs.json so
// a LocalRepo override is honored — then the legacy
// <gastown rig>/crew/den/plugins and <gastown rig>/plugins layouts.
//
// The working directory is never a candidate. Resolving from it let a stale
// clone's checkout decide which plugin files the town got: a dog running from
// a Sep-17 checkout pushed that commit's destructive compactor-dog default
// over ~/gt/plugins (gt-nc7q). Pass --source to name a directory explicitly.
func FindGastownSource(townRoot string) (GastownSource, error) {
	gastownRoot := rigCheckoutRoot(townRoot, "gastown")
	candidates := []GastownSource{
		{filepath.Join(gastownRoot, "mayor", "rig", "plugins"), "mayor rig checkout"},
		{filepath.Join(gastownRoot, "crew", "den", "plugins"), "legacy crew/den layout"},
		{filepath.Join(gastownRoot, "plugins"), "legacy gastown/plugins layout"},
	}
	for _, candidate := range candidates {
		if hasPlugins(candidate.Dir) {
			return candidate, nil
		}
	}

	return GastownSource{}, fmt.Errorf("no plugin source under %s; use --source to specify", gastownRoot)
}

// rigCheckoutRoot resolves the on-disk root of a registered rig's checkout.
// It honors an explicit LocalRepo override in mayor/rigs.json, falling back
// to the conventional <townRoot>/<rigName> layout when rigs.json is absent
// or has no override for rigName.
func rigCheckoutRoot(townRoot, rigName string) string {
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if cfg, err := config.LoadRigsConfig(rigsPath); err == nil {
		if entry, ok := cfg.Rigs[rigName]; ok && entry.LocalRepo != "" {
			return entry.LocalRepo
		}
	}
	return filepath.Join(townRoot, rigName)
}

func hasPlugins(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			if _, err := os.Stat(filepath.Join(dir, entry.Name(), "plugin.md")); err == nil {
				return true
			}
		}
	}
	return false
}

// DriftReport describes differences between source and runtime plugins.
type DriftReport struct {
	Source  string       `json:"source"`
	Target  string       `json:"target"`
	Drifted []DriftEntry `json:"drifted,omitempty"`
	Missing []string     `json:"missing,omitempty"` // in source but not target
	Extra   []string     `json:"extra,omitempty"`   // in target but not source
}

// DriftEntry describes a single plugin that differs between source and runtime.
type DriftEntry struct {
	Name       string `json:"name"`
	SourceHash string `json:"source_hash"`
	TargetHash string `json:"target_hash"`
}

// DetectDrift compares plugin directories between source and target.
func DetectDrift(sourceDir, targetDir string) (*DriftReport, error) {
	report := &DriftReport{
		Source: sourceDir,
		Target: targetDir,
	}

	srcEntries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("reading source: %w", err)
	}

	tgtPlugins := make(map[string]bool)
	if tgtEntries, err := os.ReadDir(targetDir); err == nil {
		for _, entry := range tgtEntries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				tgtPlugins[entry.Name()] = true
			}
		}
	}

	for _, entry := range srcEntries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(sourceDir, entry.Name(), "plugin.md")); err != nil {
			continue
		}

		srcDir := filepath.Join(sourceDir, entry.Name())
		dstDir := filepath.Join(targetDir, entry.Name())

		if !tgtPlugins[entry.Name()] {
			report.Missing = append(report.Missing, entry.Name())
			continue
		}
		delete(tgtPlugins, entry.Name())

		srcHash := DirHash(srcDir)
		dstHash := DirHash(dstDir)
		if srcHash != dstHash {
			report.Drifted = append(report.Drifted, DriftEntry{
				Name:       entry.Name(),
				SourceHash: srcHash,
				TargetHash: dstHash,
			})
		}
	}

	for name := range tgtPlugins {
		if _, err := os.Stat(filepath.Join(targetDir, name, "plugin.md")); err == nil {
			report.Extra = append(report.Extra, name)
		}
	}

	return report, nil
}

// HasDrift returns true if the report indicates any differences.
func (r *DriftReport) HasDrift() bool {
	return len(r.Drifted) > 0 || len(r.Missing) > 0
}
