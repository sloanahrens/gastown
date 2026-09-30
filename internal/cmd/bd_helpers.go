package cmd

import "github.com/steveyegge/gastown/internal/beads"

// The bd command builder lives in internal/beads so the daemon, deacon and
// convoy packages run bd through it without importing package cmd.
type bdCmd = beads.BdCmd

// BdCmd creates a bd command builder; see beads.NewBdCmd.
func BdCmd(args ...string) *bdCmd {
	return beads.NewBdCmd(args...)
}

// filterEnvKey removes every entry for key from env.
func filterEnvKey(env []string, key string) []string {
	return beads.StripEnvKey(env, key)
}
