// Package townstate holds the town root's ownership split as data: which paths
// the daemon owns, which belong to sessions, and which are shared through a
// command. The one manifest is the source for the volume seed and rollback
// (gt-gg9hv.68) and for the guard test that keeps daemon writes out of
// session directories (gt-gg9hv.67).
package townstate

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/bird"
	"github.com/steveyegge/gastown/internal/constants"
)

// Owner names who a town-root path belongs to.
type Owner string

const (
	// OwnerDaemon marks a path the daemon writes; it moves with the town-state volume.
	OwnerDaemon Owner = "daemon"
	// OwnerSession marks a path a session writes; it stays on the host.
	OwnerSession Owner = "session"
	// OwnerShared marks a path both sides reach through a command, not a mount.
	OwnerShared Owner = "shared"
)

// Placeholders stand for the parts of a town the manifest cannot name once:
// the rigs the town registers and the bird names the pool hands out.
const (
	RigPlaceholder  = "<rig>"
	BirdPlaceholder = "<bird>"
)

// Entry is one row of the split: a town-root-relative path, its owner and why.
type Entry struct {
	Path  string `yaml:"path" json:"path"`
	Owner Owner  `yaml:"owner" json:"owner"`
	Why   string `yaml:"why" json:"why"`
	// Rollback marks the daemon-owned rows that hold one-way state: what the
	// daemon writes while it owns the town and the Mac tree cannot rebuild.
	// `gt town state export --rollback` of this tagged set, imported with
	// --merge-newer, is the replay half of a rollback (gt-gg9hv.68). Only a
	// daemon row can carry it, because only the daemon-owned set travels to
	// the volume.
	Rollback bool `yaml:"rollback,omitempty" json:"rollback,omitempty"`
	// Private marks a daemon-owned row whose files carry modes that must
	// survive a round trip (a kubeconfig, a config an agent may hold): import
	// keeps the archive's mode instead of masking it to 0755/0644.
	Private bool `yaml:"private,omitempty" json:"private,omitempty"`
}

// Manifest is the ordered list of split rows.
type Manifest struct {
	Entries []Entry `yaml:"entries"`
}

// Default returns the town root's split as this town defines it.
//
// The rows tagged rollback: true are the one-way state the pod writes while it
// owns the town: the red-main statuses, the landing records, the spec dispatcher
// state, the events and feeds, and the maintenance and patrol state under
// daemon/. The rollback replays them onto the Mac tree with gt town state export
// --rollback and import --merge-newer. The tag sits on whole rows, so the replay
// carries all of daemon/, not only the two files named above (gt-gg9hv.68).
func Default() *Manifest {
	return &Manifest{Entries: []Entry{
		// Town-level state the daemon writes.
		{Path: "daemon", Owner: OwnerDaemon, Rollback: true, Why: "the daemon's own state, settings and patrol records; rollback replays it"},
		{Path: "logs", Owner: OwnerDaemon, Why: "daemon and session logs the daemon rotates"},
		{Path: "events", Owner: OwnerDaemon, Rollback: true, Why: "the town event store the daemon appends; rollback replays it"},
		{Path: ".events.jsonl", Owner: OwnerDaemon, Rollback: true, Why: "the town event feed the daemon appends; rollback replays it"},
		{Path: ".feed.jsonl", Owner: OwnerDaemon, Rollback: true, Why: "the feed the daemon appends; rollback replays it"},
		{Path: "mayor", Owner: OwnerDaemon, Why: "town config and the pool and roster records it writes"},
		{Path: ".beads", Owner: OwnerDaemon, Why: "workspace metadata the daemon reads and writes"},
		{Path: "plugins", Owner: OwnerDaemon, Why: "installed plugins the daemon loads"},
		{Path: ".claude-town", Owner: OwnerDaemon, Why: "the daemon's liveness probe reads session transcripts under it (internal/liveness)"},
		{Path: "settings", Owner: OwnerShared, Why: "edited by gt config daemon, read by the daemon (gt-yjzepk.4)"},

		// Rig repositories the landing worker and promoter run git in.
		{Path: "<rig>/config.json", Owner: OwnerDaemon, Why: "the rig's registry record the daemon and landing worker read"},
		{Path: "<rig>/" + constants.DirRepoGit, Owner: OwnerDaemon, Why: "the rig's bare repository the landing worker fetches and merges into; the name is constants.DirRepoGit, which internal/rig and the daemon build the path from"},
		{Path: "<rig>/" + constants.DirMayor + "/" + constants.DirRig, Owner: OwnerDaemon, Why: "the rig's canonical clone the landing worker and promoter land on; the name is constants.DirMayor and constants.DirRig, which rig.RepoPath resolves (all nine rigs of this town have it, read 2026-11-06)"},

		// The daemon's subsystem directories under .runtime.
		{Path: ".runtime/jev", Owner: OwnerDaemon, Why: "jev's dataset the daemon writes"},
		{Path: ".runtime/polecat-spans", Owner: OwnerDaemon, Why: "polecat span records the daemon writes"},
		{Path: ".runtime/agents", Owner: OwnerDaemon, Why: "agent intent records and the bird registry. Every writer is the daemon or a gt command (gt done, gt sling, the bird commands); after the wrapper cutover every gt command runs in the container, so the volume holds the only copy. Until then gt commands run on the Mac also write it directly, which is a cutover prerequisite (gt-yjzepk.10)"},
		{Path: ".runtime/landings", Owner: OwnerDaemon, Rollback: true, Why: "landing ledgers the daemon writes; rollback replays the landing records"},
		{Path: ".runtime/usage", Owner: OwnerDaemon, Why: "usage records the daemon writes"},
		{Path: ".runtime/sweep-runs", Owner: OwnerDaemon, Why: "tier sweep output the daemon writes"},
		{Path: ".runtime/overrun-guard", Owner: OwnerDaemon, Why: "overrun guard state the daemon writes"},
		{Path: ".runtime/metrics", Owner: OwnerDaemon, Why: "resource samples the daemon writes"},
		{Path: ".runtime/landing-om", Owner: OwnerDaemon, Why: "landing om evidence the daemon writes"},
		{Path: ".runtime/spec_dispatch", Owner: OwnerDaemon, Rollback: true, Why: "spec dispatcher state the daemon writes; rollback replays it"},
		{Path: ".runtime/heartbeats", Owner: OwnerDaemon, Why: "daemon heartbeats"},
		{Path: ".runtime/sandboxes", Owner: OwnerDaemon, Why: "sandbox records the daemon writes"},
		{Path: ".runtime/landing-logs", Owner: OwnerDaemon, Why: "landing stage logs the daemon writes"},
		{Path: ".runtime/attention", Owner: OwnerDaemon, Why: "the attention queue the daemon writes"},
		{Path: ".runtime/red-main", Owner: OwnerDaemon, Rollback: true, Why: "red-main status the landing worker writes; rollback replays the statuses"},
		{Path: ".runtime/locks", Owner: OwnerDaemon, Why: "town lock files the daemon takes; export skips the files themselves and keeps the directory"},
		{Path: ".runtime/container", Owner: OwnerDaemon, Private: true, Why: "the container's kubeconfig and state the daemon writes; the kubeconfig and the 0600 configs beside it must keep their modes through a copy"},
		{Path: ".runtime/daemon-image", Owner: OwnerDaemon, Why: "the daemon image build record"},
		{Path: ".runtime/reports", Owner: OwnerDaemon, Why: "the published bird reports gt report publish writes, one directory per bird (gt-yjzepk.3)"},

		// Session-owned trees.
		{Path: "<rig>/crew", Owner: OwnerSession, Why: "each crew worker's persistent workspace"},
		{Path: "<rig>/polecats", Owner: OwnerSession, Why: "each polecat's worktree"},
		{Path: "<rig>/witness", Owner: OwnerSession, Why: "the witness session's own tree"},
		{Path: "<rig>/refinery", Owner: OwnerSession, Why: "the refinery session's own tree"},
		{Path: ".runtime/<bird>", Owner: OwnerSession, Why: "each bird session's own directory; gt report publish writes its daily report under .runtime/reports instead (gt-yjzepk.3)"},
	}}
}

// DaemonRuntimeNames returns the names of the daemon-owned directories
// directly under .runtime — jev, landings, usage, agents and the rest — sorted.
// A bird's published name may not equal one: the split under .runtime is read
// by name, so a session directory carrying a daemon name would be owned by two
// (gt-yjzepk.3).
func (m *Manifest) DaemonRuntimeNames() []string {
	names := daemonSubsystemNames(m.Entries)
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Parse reads a manifest from its YAML mirror and refuses any row that cannot
// name a town-root path.
func Parse(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("reading the town-state manifest: %w", err)
	}
	for _, e := range m.Entries {
		if err := e.Validate(); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// Validate refuses an entry that cannot name a town-root path or owner.
func (e Entry) Validate() error {
	switch e.Owner {
	case OwnerDaemon, OwnerSession, OwnerShared:
	default:
		return fmt.Errorf("town-state entry %q: unknown owner %q (want %s, %s or %s)",
			e.Path, e.Owner, OwnerDaemon, OwnerSession, OwnerShared)
	}
	if e.Path == "" {
		return fmt.Errorf("town-state entry with owner %s has an empty path", e.Owner)
	}
	if path.IsAbs(e.Path) {
		return fmt.Errorf("town-state entry %q: the path must be relative to the town root", e.Path)
	}
	if strings.TrimSpace(e.Why) == "" {
		return fmt.Errorf("town-state entry %q: why is empty; say what the path holds", e.Path)
	}
	if e.Rollback && e.Owner != OwnerDaemon {
		return fmt.Errorf("town-state entry %q: rollback: true needs a daemon owner; only the daemon-owned set travels to the volume and back", e.Path)
	}
	if e.Private && e.Owner != OwnerDaemon {
		return fmt.Errorf("town-state entry %q: private: true needs a daemon owner; only the daemon-owned set is copied", e.Path)
	}
	for _, seg := range strings.Split(e.Path, "/") {
		switch seg {
		case "":
			return fmt.Errorf("town-state entry %q: empty path segment", e.Path)
		case ".", "..":
			return fmt.Errorf("town-state entry %q: %q is not a town-root path segment", e.Path, seg)
		}
		if strings.ContainsAny(seg, "<>") && seg != RigPlaceholder && seg != BirdPlaceholder {
			return fmt.Errorf("town-state entry %q: unknown placeholder %q (want %s or %s)",
				e.Path, seg, RigPlaceholder, BirdPlaceholder)
		}
	}
	return nil
}

// Validate refuses every entry that cannot name a town-root path or owner.
func (m *Manifest) Validate() error {
	for _, e := range m.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Rollback returns the rows tagged rollback: true, in manifest order. It is the
// one-way state list a rollback replays from the volume onto the Mac tree;
// `gt town state export --rollback` takes it from here so seed, rollback and
// the guard cannot disagree about what it is.
func (m *Manifest) Rollback() []Entry {
	var out []Entry
	for _, e := range m.Entries {
		if e.Rollback {
			out = append(out, e)
		}
	}
	return out
}

// Expand replaces the rig and bird placeholders with the names a town has.
func (m *Manifest) Expand(rigs, birds []string) []Entry {
	out := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		segs := strings.Split(e.Path, "/")
		var expanded []Entry
		switch {
		case slices.Contains(segs, RigPlaceholder):
			for _, rig := range rigs {
				expanded = append(expanded, e.withPath(replaceSeg(segs, RigPlaceholder, rig)))
			}
		case slices.Contains(segs, BirdPlaceholder):
			for _, name := range birds {
				expanded = append(expanded, e.withPath(replaceSeg(segs, BirdPlaceholder, name)))
			}
		default:
			expanded = append(expanded, e)
		}
		out = append(out, expanded...)
	}
	return out
}

// withPath returns the entry with a different path.
func (e Entry) withPath(p string) Entry {
	e.Path = p
	return e
}

// replaceSeg returns segs with every occurrence of old swapped for name.
func replaceSeg(segs []string, old, name string) string {
	out := slices.Clone(segs)
	for i, s := range out {
		if s == old {
			out[i] = name
		}
	}
	return strings.Join(out, "/")
}

// BirdRuntimeNames returns the bird names the embedded pool hands out (bird.Pool,
// not a list derived from the manifest), whose .runtime directories the manifest
// classes session. Verify also adds any other .runtime directory that holds a
// reports folder.
func BirdRuntimeNames() []string {
	return bird.Pool()
}

// Lookup returns the deepest entry covering p, and whether one covers it.
func Lookup(entries []Entry, p string) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range entries {
		if p != e.Path && !strings.HasPrefix(p, e.Path+"/") {
			continue
		}
		if !found || strings.Count(e.Path, "/") > strings.Count(best.Path, "/") {
			best, found = e, true
		}
	}
	return best, found
}

// SessionOwned returns the session-owned entry covering rel, a town-root-relative
// path, and whether one covers it. The rig and bird placeholders stand for any
// one non-empty segment, so a caller that does not know the town's rigs still
// finds a path under any rig's session tree. The deepest entry wins, and an
// entry with no placeholder wins over a placeholder at the same depth, so
// .runtime/usage stays the daemon's even though .runtime/<bird> also matches it.
// Letters are folded as the default macOS volume folds them, so the same path in
// another case is the same directory.
func (m *Manifest) SessionOwned(rel string) (Entry, bool) {
	rel = strings.Trim(path.Clean("/"+rel), "/")
	if rel == "" || rel == "." {
		return Entry{}, false
	}
	var best Entry
	found := false
	for _, e := range m.Entries {
		if !coversEntry(e.Path, rel) {
			continue
		}
		switch {
		case !found:
		case entryDepth(e.Path) > entryDepth(best.Path):
		case entryDepth(e.Path) == entryDepth(best.Path) && hasPlaceholder(best.Path) && !hasPlaceholder(e.Path):
		default:
			continue
		}
		best, found = e, true
	}
	if !found || best.Owner != OwnerSession {
		return Entry{}, false
	}
	return best, true
}

// coversEntry reports whether rel is pattern or lies under it, with the rig and
// bird placeholders matching any one non-empty segment.
func coversEntry(pattern, rel string) bool {
	ps := strings.Split(strings.ToLower(pattern), "/")
	rs := strings.Split(strings.ToLower(rel), "/")
	if len(rs) < len(ps) {
		return false
	}
	for i, seg := range ps {
		if seg == RigPlaceholder || seg == BirdPlaceholder {
			continue
		}
		if rs[i] != seg {
			return false
		}
	}
	return true
}

// entryDepth is how many segments an entry path has.
func entryDepth(p string) int { return strings.Count(p, "/") }

// hasPlaceholder reports whether an entry path holds the rig or bird placeholder.
func hasPlaceholder(p string) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), func(seg string) bool {
		return seg == RigPlaceholder || seg == BirdPlaceholder
	})
}

// names reports whether an entry path is exactly p.
func (m *Manifest) names(p string) bool {
	for _, e := range m.Entries {
		if e.Path == p {
			return true
		}
	}
	return false
}

// IsContainer reports whether p is a directory that only holds entries.
func IsContainer(entries []Entry, p string) bool {
	for _, e := range entries {
		if strings.HasPrefix(e.Path, p+"/") {
			return true
		}
	}
	return false
}
