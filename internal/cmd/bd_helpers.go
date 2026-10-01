package cmd

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
)

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

// bdErrOutput is what bd printed for a failed plain-wrapper call (see
// beads.CLIError.Output), or err's text when bd never ran.
func bdErrOutput(err error) string {
	var cliErr *beads.CLIError
	if errors.As(err, &cliErr) {
		return cliErr.Output()
	}
	return err.Error()
}

// townBeadsClient runs bd against the town's .beads with the caller's
// environment.
func townBeadsClient(townRoot string) beads.Client {
	return beads.NewPlain("", append(os.Environ(), "BEADS_DIR="+filepath.Join(townRoot, ".beads")))
}

// pinnedBd runs bd against dir's resolved .beads database and nowhere else,
// as BdCmd(...).Dir(dir) without routing did.
func pinnedBd(dir string) *beads.Beads {
	return beads.NewPinned(beads.ResolveBeadsDir(dir))
}
