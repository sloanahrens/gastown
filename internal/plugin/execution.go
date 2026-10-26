package plugin

import (
	"fmt"
	"path/filepath"
	"strings"
)

// BinaryPath returns the absolute path of the binary [execution] binary names,
// an empty string when the plugin names none, or an error when the path leaves
// the plugin directory.
func (p *Plugin) BinaryPath() (string, error) {
	if p.Execution == nil || p.Execution.Binary == "" {
		return "", nil
	}
	return resolveBinary(p.Path, p.Execution.Binary)
}

// resolveBinary joins rel onto dir and refuses a result that steps outside it:
// an absolute path, or one whose .. reaches above the plugin directory. The
// daemon execs the result under the plugin's name and the operator's
// permissions, so a path that leaves the directory would run a program the
// plugin never shipped.
func resolveBinary(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("binary %q must be relative to the plugin directory", rel)
	}
	full := filepath.Join(dir, rel)
	inside, err := filepath.Rel(dir, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("binary %q escapes the plugin directory", rel)
	}
	return full, nil
}
