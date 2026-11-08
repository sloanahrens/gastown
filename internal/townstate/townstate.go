// Package townstate holds the town root's ownership split as data: which paths
// the daemon owns, which belong to sessions, and which are shared through a
// command. The one manifest is the source for the volume seed and rollback
// (gt-gg9hv.68) and for the guard test that keeps daemon writes out of
// session directories (gt-gg9hv.67). The types and parsers live in
// internal/manifestdata, which the cluster renderer imports without a process
// or a socket; this package adds the town's own names to them.
package townstate

import (
	"github.com/steveyegge/gastown/internal/bird"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/manifestdata"
)

// Owner names who a town-root path belongs to.
type Owner = manifestdata.Owner

const (
	// OwnerDaemon marks a path the daemon writes; it moves with the town-state volume.
	OwnerDaemon = manifestdata.OwnerDaemon
	// OwnerSession marks a path a session writes; it stays on the host.
	OwnerSession = manifestdata.OwnerSession
	// OwnerShared marks a path both sides reach through a command, not a mount.
	OwnerShared = manifestdata.OwnerShared

	// RigPlaceholder stands for a rig the town registers; BirdPlaceholder for a
	// bird name the pool hands out.
	RigPlaceholder  = manifestdata.RigPlaceholder
	BirdPlaceholder = manifestdata.BirdPlaceholder
)

// Entry is one row of the split: a town-root-relative path, its owner and why.
type Entry = manifestdata.Entry

// Manifest is the ordered list of split rows.
type Manifest manifestdata.Manifest

// Default returns the town root's split as this town defines it, with the rig
// rows spelled from the town's directory names.
func Default() *Manifest {
	return (*Manifest)(manifestdata.Default(manifestdata.DirNames{
		RepoGit: constants.DirRepoGit,
		Mayor:   constants.DirMayor,
		Rig:     constants.DirRig,
	}))
}

// DaemonRuntimeNames returns the daemon-owned .runtime directory names, sorted.
func (m *Manifest) DaemonRuntimeNames() []string {
	return (*manifestdata.Manifest)(m).DaemonRuntimeNames()
}

// Validate refuses every entry that cannot name a town-root path or owner.
func (m *Manifest) Validate() error {
	return (*manifestdata.Manifest)(m).Validate()
}

// Rollback returns the rows tagged rollback: true, in manifest order.
func (m *Manifest) Rollback() []Entry {
	return (*manifestdata.Manifest)(m).Rollback()
}

// Expand replaces the rig and bird placeholders with the names a town has.
func (m *Manifest) Expand(rigs, birds []string) []Entry {
	return (*manifestdata.Manifest)(m).Expand(rigs, birds)
}

// SessionOwned returns the session-owned entry covering rel, and whether one
// covers it.
func (m *Manifest) SessionOwned(rel string) (Entry, bool) {
	return (*manifestdata.Manifest)(m).SessionOwned(rel)
}

// names reports whether an entry path is exactly p.
func (m *Manifest) names(p string) bool {
	return (*manifestdata.Manifest)(m).Names(p)
}

// Parse reads a manifest from its YAML mirror and refuses any row that cannot
// name a town-root path.
func Parse(data []byte) (*Manifest, error) {
	m, err := manifestdata.Parse(data)
	if err != nil {
		return nil, err
	}
	return (*Manifest)(m), nil
}

// BirdRuntimeNames returns the bird names the embedded pool hands out, whose
// .runtime directories the manifest classes session.
func BirdRuntimeNames() []string {
	return bird.Pool()
}

// Lookup returns the deepest entry covering p, and whether one covers it.
func Lookup(entries []Entry, p string) (Entry, bool) {
	return manifestdata.Lookup(entries, p)
}

// IsContainer reports whether p is a directory that only holds entries.
func IsContainer(entries []Entry, p string) bool {
	return manifestdata.IsContainer(entries, p)
}
