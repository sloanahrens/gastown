package beads

import (
	"bytes"
	"fmt"
	"strings"
)

// BdJSONOptions tunes RunBdJSONWith.
type BdJSONOptions struct {
	// Env is the base environment for bd; nil is os.Environ().
	Env []string
	// AllowStale asks for bd's stale-read bypass.
	AllowStale bool
	// AutoCommit turns bd's Dolt auto-commit on, for sequential calls that
	// must see one another's writes.
	AutoCommit bool
}

// RunBdJSON runs bd in dir and returns its stdout. A failure carries bd's
// stderr instead of a bare "exit status 1". An inherited BEADS_DIR is
// stripped so bd discovers dir's own database.
func RunBdJSON(dir string, args ...string) ([]byte, error) {
	return RunBdJSONWith(BdJSONOptions{}, dir, args...)
}

// RunBdJSONAllowStale is RunBdJSON with bd's stale-read bypass.
func RunBdJSONAllowStale(dir string, args ...string) ([]byte, error) {
	return RunBdJSONWith(BdJSONOptions{AllowStale: true}, dir, args...)
}

// RunBdJSONWithAutoCommit is RunBdJSON with bd's Dolt auto-commit on.
func RunBdJSONWithAutoCommit(dir string, args ...string) ([]byte, error) {
	return RunBdJSONWith(BdJSONOptions{AutoCommit: true}, dir, args...)
}

// RunBdJSONWith is RunBdJSON with options.
func RunBdJSONWith(opts BdJSONOptions, dir string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	bdc := NewBdCmd(args...)
	if opts.Env != nil {
		bdc.WithEnv(opts.Env)
	}
	if opts.AllowStale {
		bdc.AllowStale()
	}
	if opts.AutoCommit {
		bdc.WithAutoCommit()
	}
	bdc.Dir(dir).StripBeadsDir().Stderr(&stderr)
	cmd := bdc.Build()
	cmd.Dir = dir
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if errMsg := strings.TrimSpace(stderr.String()); errMsg != "" {
			return nil, fmt.Errorf("bd %s: %s", args[0], errMsg)
		}
		return nil, fmt.Errorf("bd %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}
