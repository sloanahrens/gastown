package cmd

import "github.com/steveyegge/gastown/internal/hooks"

// gtDataDir returns the directory used for GT's runtime data files
// (logs, command usage, cost records, etc.): hooks.GTDir, $GT_HOME/.gt when
// GT_HOME is set, else ~/.gt.
func gtDataDir() string {
	return hooks.GTDir()
}
