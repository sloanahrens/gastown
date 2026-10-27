package plugin

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Scanner discovers plugins in town and rig directories.
type Scanner struct {
	townRoot string
	rigNames []string
}

// NewScanner creates a new plugin scanner.
func NewScanner(townRoot string, rigNames []string) *Scanner {
	return &Scanner{
		townRoot: townRoot,
		rigNames: rigNames,
	}
}

// DiscoverAll scans all plugin locations and returns discovered plugins.
// Town-level plugins are scanned first, then rig-level plugins.
// Plugins are deduplicated by name (rig-level overrides town-level).
func (s *Scanner) DiscoverAll() ([]*Plugin, error) {
	pluginMap := make(map[string]*Plugin)

	// Scan town-level plugins first
	townPlugins, err := s.scanTownPlugins()
	if err != nil {
		return nil, fmt.Errorf("scanning town plugins: %w", err)
	}
	for _, p := range townPlugins {
		pluginMap[p.Name] = p
	}

	// Scan rig-level plugins (override town-level by name)
	for _, rigName := range s.rigNames {
		rigPlugins, err := s.scanRigPlugins(rigName)
		if err != nil {
			// Log warning but continue with other rigs
			fmt.Fprintf(os.Stderr, "Warning: scanning plugins for rig %q: %v\n", rigName, err)
			continue
		}
		for _, p := range rigPlugins {
			pluginMap[p.Name] = p
		}
	}

	// Convert map to slice
	plugins := make([]*Plugin, 0, len(pluginMap))
	for _, p := range pluginMap {
		plugins = append(plugins, p)
	}

	return plugins, nil
}

// scanTownPlugins scans the town-level plugins directory.
func (s *Scanner) scanTownPlugins() ([]*Plugin, error) {
	pluginsDir := filepath.Join(s.townRoot, "plugins")
	return s.scanDirectory(pluginsDir, LocationTown, "")
}

// scanRigPlugins scans a rig's plugins directory.
func (s *Scanner) scanRigPlugins(rigName string) ([]*Plugin, error) {
	pluginsDir := filepath.Join(s.townRoot, rigName, "plugins")
	return s.scanDirectory(pluginsDir, LocationRig, rigName)
}

// scanDirectory scans a plugins directory for plugin definitions.
func (s *Scanner) scanDirectory(dir string, location Location, rigName string) ([]*Plugin, error) {
	// Check if directory exists
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil, nil // No plugins directory is fine
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	// List plugin directories
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var plugins []*Plugin
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		pluginDir := filepath.Join(dir, entry.Name())
		plugin, err := s.loadPlugin(pluginDir, location, rigName)
		if err != nil {
			// Log warning but continue with other plugins
			fmt.Fprintf(os.Stderr, "Warning: loading plugin %q: %v\n", entry.Name(), err)
			continue
		}
		if plugin != nil {
			plugins = append(plugins, plugin)
		}
	}

	return plugins, nil
}

// loadPlugin loads a plugin from its directory.
func (s *Scanner) loadPlugin(pluginDir string, location Location, rigName string) (*Plugin, error) {
	// Look for plugin.md
	pluginFile := filepath.Join(pluginDir, "plugin.md")
	if _, err := os.Stat(pluginFile); os.IsNotExist(err) {
		return nil, nil // No plugin.md, skip
	}

	// Read and parse plugin.md
	content, err := os.ReadFile(pluginFile) //nolint:gosec // G304: path is from trusted plugin directory
	if err != nil {
		return nil, fmt.Errorf("reading plugin.md: %w", err)
	}

	plugin, err := parsePluginMD(content, pluginDir, location, rigName)
	if err != nil {
		return nil, err
	}

	// Check for run.sh alongside plugin.md
	runScriptPath := filepath.Join(pluginDir, "run.sh")
	if info, statErr := os.Stat(runScriptPath); statErr == nil && !info.IsDir() {
		plugin.HasRunScript = true
	}

	return plugin, nil
}

// parsePluginMD parses a plugin.md file with TOML frontmatter.
// Format:
//
//	+++
//	name = "plugin-name"
//	...
//	+++
//	# Instructions
//	...
func parsePluginMD(content []byte, pluginDir string, location Location, rigName string) (*Plugin, error) {
	fm, body, err := pluginFrontmatter(content)
	if err != nil {
		return nil, err
	}

	// Refuse an escaping binary path here, where the scanner's warning names
	// the plugin, instead of at the run that would exec it.
	if fm.Execution != nil && fm.Execution.Binary != "" {
		if _, err := resolveBinary(pluginDir, fm.Execution.Binary); err != nil {
			return nil, err
		}
	}

	plugin := &Plugin{
		Name:         fm.Name,
		Description:  fm.Description,
		Version:      fm.Version,
		Location:     location,
		Path:         pluginDir,
		RigName:      rigName,
		Gate:         fm.Gate,
		Tracking:     fm.Tracking,
		Execution:    fm.Execution,
		Instructions: body,
	}

	return plugin, nil
}

// pluginFrontmatter splits a plugin.md into its decoded frontmatter and the
// instruction body that follows it.
func pluginFrontmatter(content []byte) (*PluginFrontmatter, string, error) {
	str := string(content)

	// Find TOML frontmatter delimiters
	const delimiter = "+++"
	start := strings.Index(str, delimiter)
	if start == -1 {
		return nil, "", fmt.Errorf("missing TOML frontmatter (no opening +++)")
	}

	// Find closing delimiter
	end := strings.Index(str[start+len(delimiter):], delimiter)
	if end == -1 {
		return nil, "", fmt.Errorf("missing TOML frontmatter (no closing +++)")
	}
	end += start + len(delimiter)

	// Extract frontmatter and body
	frontmatter := str[start+len(delimiter) : end]
	body := strings.TrimSpace(str[end+len(delimiter):])

	// Parse TOML frontmatter
	var fm PluginFrontmatter
	if _, err := toml.Decode(frontmatter, &fm); err != nil {
		return nil, "", fmt.Errorf("parsing TOML frontmatter: %w", err)
	}

	// Validate required fields
	if fm.Name == "" {
		return nil, "", fmt.Errorf("missing required field: name")
	}

	return &fm, body, nil
}

// declaredBinaryOf returns the binary path a plugin directory's plugin.md
// declares, relative to that directory, or "" when it declares none or cannot
// be read. The sync guard reads it to tell a rebuilt build artifact from an
// operator edit (gt-tpg0p.3).
func declaredBinaryOf(pluginDir string) string {
	content, err := os.ReadFile(filepath.Join(pluginDir, "plugin.md"))
	if err != nil {
		return ""
	}
	fm, _, err := pluginFrontmatter(content)
	if err != nil || fm.Execution == nil || fm.Execution.Binary == "" {
		return ""
	}
	return filepath.ToSlash(path.Clean(fm.Execution.Binary))
}

// GetPlugin returns the plugin whose frontmatter name is name, deduplicated the
// way DiscoverAll dedupes it, and reports the scan's error rather than "plugin
// not found" when discovery itself fails.
func (s *Scanner) GetPlugin(name string) (*Plugin, error) {
	// A plugin is identified by frontmatter name, not directory name (gt-v19m3),
	// so finding one costs a full scan; each caller is a one-shot CLI command, so
	// that scan is the command's cost (gt-ontk4).
	plugins, err := s.DiscoverAll()
	if err != nil {
		return nil, err
	}
	for _, plugin := range plugins {
		if plugin.Name == name {
			return plugin, nil
		}
	}
	return nil, fmt.Errorf("plugin not found: %s", name)
}

// DiscoverExecWrappers returns all exec-wrapper plugins.
// These are plugins with [execution] type = "exec-wrapper" that wrap session startup.
func (s *Scanner) DiscoverExecWrappers() ([]*Plugin, error) {
	all, err := s.DiscoverAll()
	if err != nil {
		return nil, err
	}

	var wrappers []*Plugin
	for _, p := range all {
		if p.IsExecWrapper() {
			wrappers = append(wrappers, p)
		}
	}
	return wrappers, nil
}

// GetExecWrapper returns a specific exec-wrapper plugin by name.
// Returns an error if the plugin is not found or is not an exec-wrapper type.
func (s *Scanner) GetExecWrapper(name string) (*Plugin, error) {
	p, err := s.GetPlugin(name)
	if err != nil {
		return nil, err
	}
	if !p.IsExecWrapper() {
		execType := ExecutionType("none")
		if p.Execution != nil {
			execType = p.Execution.Type
		}
		return nil, fmt.Errorf("plugin %q is not an exec-wrapper (type=%s)", name, execType)
	}
	return p, nil
}

// ListPluginDirs returns the directories where plugins are stored.
func (s *Scanner) ListPluginDirs() []string {
	dirs := []string{filepath.Join(s.townRoot, "plugins")}
	for _, rigName := range s.rigNames {
		dirs = append(dirs, filepath.Join(s.townRoot, rigName, "plugins"))
	}
	return dirs
}
