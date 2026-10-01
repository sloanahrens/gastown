package rig

import (
	"errors"
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

// rigBD is the bd surface rig setup runs: init, config and the
// repository-fingerprint migration. A *beads.Beads from beads.NewPlain
// provides it, sending exactly the argv, directory and environment given.
type rigBD interface {
	InitDatabase(opts beads.InitOptions) error
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
	MigrateRepoID() error
}

// bdIn is the bd client that runs in dir with env, nil meaning the base
// environment.
func (m *Manager) bdIn(dir string, env []string) rigBD {
	if env == nil {
		env = m.environ()
	}
	if m.openBD != nil {
		return m.openBD(dir, env)
	}
	return beads.NewPlain(dir, env)
}

// rigLocalBD is the bd client pinned to dir's own database.
func (m *Manager) rigLocalBD(dir string) configSetter {
	if m.openLocal != nil {
		return m.openLocal(dir)
	}
	return beads.NewRigLocal(dir)
}

// bdErrOutput is what a failed bd call printed (stdout then stderr), or the
// error's text when bd never ran.
func bdErrOutput(err error) string {
	var cliErr *beads.CLIError
	if errors.As(err, &cliErr) {
		return cliErr.Output()
	}
	return err.Error()
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
