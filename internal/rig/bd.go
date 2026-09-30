package rig

import (
	"bytes"
	"context"
	"os"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// environ is the base environment the manager builds bd's from.
func (m *Manager) environ() []string {
	if m.env != nil {
		return m.env
	}
	return os.Environ()
}

// runBD runs bd in dir with env, nil meaning the base environment, and
// returns what it wrote to stdout (unwrapped from the machine envelope on
// success, as beads.CommandWithEnv's Run leaves it) and stderr.
func (m *Manager) runBD(dir string, env []string, args ...string) (stdout, stderr []byte, err error) {
	if m.bd == nil {
		cmd := beads.CommandWithEnv(dir, env, args...)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.Bytes(), errOut.Bytes(), err
	}
	if env == nil {
		env = m.environ()
	}
	stdout, stderr, err = m.bd(context.Background(), beads.BDCall{Dir: dir, Env: env, Args: args})
	if err == nil {
		stdout = beads.LegacyPayload(args, stdout)
	}
	return stdout, stderr, err
}

// combinedBD is runBD's stdout followed by its stderr, as CombinedOutput
// reports them.
func (m *Manager) combinedBD(dir string, env []string, args ...string) ([]byte, error) {
	stdout, stderr, err := m.runBD(dir, env, args...)
	return append(stdout, stderr...), err
}

// beadsFor is the bd client for a rig's beads directory.
func (m *Manager) beadsFor(workDir, beadsDir string) *beads.Beads {
	return beads.NewWithBeadsDirAndRunner(workDir, beadsDir, m.bd)
}

func (m *Manager) checkBeads() (deps.BeadsStatus, string) {
	if m.bdVersion != nil {
		return m.bdVersion()
	}
	return deps.CheckBeads()
}

// doltDatabases is the Dolt server work rig add does outside bd: finding and
// dropping the orphan databases bd init leaves, and seeding issue_prefix.
type doltDatabases interface {
	Exists(name string) bool
	Remove(name string) error
	SetIssuePrefix(beadsDir, database, prefix string) error
}

// townDolt is doltDatabases on the town's Dolt server.
type townDolt struct{ townRoot string }

func (d townDolt) Exists(name string) bool { return doltserver.DatabaseExists(d.townRoot, name) }

func (d townDolt) Remove(name string) error { return doltserver.RemoveDatabase(d.townRoot, name, true) }

func (d townDolt) SetIssuePrefix(beadsDir, database, prefix string) error {
	return doltserver.SetRigIssuePrefix(d.townRoot, beadsDir, database, prefix)
}

func (m *Manager) doltDBs() doltDatabases {
	if m.dolt != nil {
		return m.dolt
	}
	return townDolt{m.townRoot}
}
