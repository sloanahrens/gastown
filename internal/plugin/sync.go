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
	return syncPlugins(sourceDir, targetDir, opts, sourceHistory)
}

// ProtectedEdits returns what SyncPluginsWithOptions(sourceDir, targetDir,
// opts) would report in Protected, without writing anything. report is
// DetectDrift's result for the same directories: its drifted plugins are
// checked, and its extras too under opts.Clean, the two cases a sync replaces
// or removes. Under opts.Force nothing is protected.
func ProtectedEdits(sourceDir, targetDir string, report *DriftReport, opts SyncOptions) (map[string][]string, error) {
	return protectedEdits(sourceDir, targetDir, report, opts, sourceHistory)
}

// protectedEdits is ProtectedEdits reading the source's blob history through
// history, which unit tests script instead of running git.
func protectedEdits(sourceDir, targetDir string, report *DriftReport, opts SyncOptions, history func(sourceDir string) (blobHistory, string)) (map[string][]string, error) {
	g := newEditGuard(sourceDir, opts, history)
	names := make([]string, 0, len(report.Drifted)+len(report.Extra))
	for _, d := range report.Drifted {
		names = append(names, d.Name)
	}
	if opts.Clean {
		names = append(names, report.Extra...)
	}
	for _, name := range names {
		if _, err := g.allows(name, filepath.Join(sourceDir, name), filepath.Join(targetDir, name)); err != nil {
			return nil, err
		}
	}
	return g.protected, nil
}

// editGuard decides whether a sync may replace or remove a runtime plugin
// directory and records each refusal. The real sync and the dry-run preview
// (ProtectedEdits) both ask it, so the preview cannot drift from the run.
type editGuard struct {
	force     bool
	hist      blobHistory
	prefix    string
	protected map[string][]string
}

func newEditGuard(sourceDir string, opts SyncOptions, history func(sourceDir string) (blobHistory, string)) *editGuard {
	g := &editGuard{force: opts.Force}
	if !g.force {
		g.hist, g.prefix = history(sourceDir)
	}
	return g
}

// allows reports whether dstPluginDir may be replaced or removed. When it may
// not, the runtime edits are recorded in g.protected. An error means the check
// itself failed and the plugin must be left alone.
func (g *editGuard) allows(name, srcPluginDir, dstPluginDir string) (bool, error) {
	if g.force {
		return true, nil
	}
	edits, err := runtimeEdits(name, srcPluginDir, dstPluginDir, g.hist, g.prefix)
	if err != nil {
		return false, fmt.Errorf("%s: checking for runtime edits: %w", name, err)
	}
	if len(edits) == 0 {
		return true, nil
	}
	if g.protected == nil {
		g.protected = map[string][]string{}
	}
	g.protected[name] = edits
	return false, nil
}

// syncPlugins is SyncPluginsWithOptions reading the source's blob history
// through history, which unit tests script instead of running git.
func syncPlugins(sourceDir, targetDir string, opts SyncOptions, history func(sourceDir string) (blobHistory, string)) (*SyncResult, error) {
	result := &SyncResult{}
	clean := opts.Clean
	g := newEditGuard(sourceDir, opts, history)
	// guard reports whether dstPluginDir may be replaced or removed; a failed
	// check lands in result.Errors, a refusal in g.protected.
	guard := func(name, srcPluginDir, dstPluginDir string) bool {
		ok, err := g.allows(name, srcPluginDir, dstPluginDir)
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
		}
		return ok
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

	result.Protected = g.protected
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
// The town's own checkout answers first, then the LocalRepo clone named in
// mayor/rigs.json, and the working directory never does: a stale copy in
// either of those places once decided which plugins the town got (gt-2l1lr,
// gt-nc7q).
func FindGastownSource(townRoot string) (GastownSource, error) {
	townGastown := rigCheckoutRoot(townRoot, "gastown")
	probed := []string{townGastown}
	if local := localRepoOverride(townRoot, "gastown"); local != "" && local != townGastown {
		probed = append(probed, local)
	}

	for _, root := range probed {
		if source, ok := findPluginsUnder(root); ok {
			return source, nil
		}
	}

	return GastownSource{}, fmt.Errorf("no plugin source under %s; use --source to specify", strings.Join(probed, " or "))
}

// findPluginsUnder probes one gastown checkout root for a plugins directory,
// in layout preference order.
func findPluginsUnder(root string) (GastownSource, bool) {
	candidates := []GastownSource{
		{filepath.Join(root, "mayor", "rig", "plugins"), "mayor rig checkout"},
		{filepath.Join(root, "crew", "den", "plugins"), "legacy crew/den layout"},
		{filepath.Join(root, "plugins"), "legacy gastown/plugins layout"},
	}
	for _, candidate := range candidates {
		if hasPlugins(candidate.Dir) {
			return candidate, true
		}
	}
	return GastownSource{}, false
}

// rigCheckoutRoot resolves the on-disk root of a registered rig's checkout
// inside the town: <townRoot>/<rigName>. A LocalRepo override in
// mayor/rigs.json is deliberately not honored here; see localRepoOverride.
func rigCheckoutRoot(townRoot, rigName string) string {
	return filepath.Join(townRoot, rigName)
}

// localRepoOverride returns the LocalRepo reference clone that mayor/rigs.json
// names for rigName, or "" when rigs.json is absent or has no override.
func localRepoOverride(townRoot, rigName string) string {
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if cfg, err := config.LoadRigsConfig(rigsPath); err == nil {
		if entry, ok := cfg.Rigs[rigName]; ok {
			return entry.LocalRepo
		}
	}
	return ""
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

// HasDrift reports whether the runtime copy differs at all from source,
// counting a plugin source no longer has (gt-bw1wo).
func (r *DriftReport) HasDrift() bool {
	return len(r.Drifted) > 0 || len(r.Missing) > 0 || len(r.Extra) > 0
}
