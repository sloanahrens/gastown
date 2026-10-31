package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// productionDoltPort is the port a town's Dolt server listens on by default
// (doltserver.DefaultPort; the constant is repeated here with this pointer because importing
// doltserver pulls the daemon's packages into this command). No recording names it (gt-p85pw.115).
const productionDoltPort = 3307

// endpointPortVars are the names bd reads the Dolt server's port from that cross into the container:
// internal/config's DoltEndpointEnvKeys names the same server for the bd gt starts, and only the
// BEADS_DOLT_ names reach a container (gt-p85pw.115, gt-p85pw.117).
var endpointPortVars = []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// workspaceMarkers are what a directory holds when a beads workspace sits in it: the .beads
// directory itself, or the metadata.json bd keeps its store's settings in.
var workspaceMarkers = []string{".beads", "metadata.json"}

// Resolver reports the beads workspace bd resolves for a run in dir carrying env, in the container's
// own paths. The recorder answers it with bd where --json, the one read-only call that names the
// active .beads location; an empty workspace with no error is bd's answer that it resolved none, and
// a run that did not answer is an error.
type Resolver func(dir string, env []string) (string, error)

// guardRun refuses a recording that could write a workspace or a Dolt server the recorder did not
// make. Every bd child runs in a container with only its own directory mounted, so bd init cannot
// walk up to an ancestor workspace; the guard proves the container is the one the recorder meant
// before anything is written: the container's bd must be the host's build, and bd must resolve no
// workspace outside workDir. Only then does the scratch's init run, and the workspace it makes must
// be on disk in the scratch (gt-p85pw.115, gt-p85pw.117).
//
// The environment the child gets is refused when it aims the scratch's runs at the town's Dolt
// server, whose database the init would then create as cruft no cleanup owns (gt-p85pw.115).
//
// The recorder's own working directory is refused only when it is a workspace root itself; a
// directory that merely sits under one passes, since work lives under the town (gt-p85pw.116).
// Every refusal comes before anything is recorded.
func guardRun(getwd func() (string, error), scratch string, child childBD) error {
	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("name the working directory: %w", err)
	}
	real, err := realPath(cwd)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", cwd, err)
	}
	root, err := workspaceRootAt(real)
	if err != nil {
		return err
	}
	if root {
		where := cwd
		if real != cwd {
			where = fmt.Sprintf("%s (that is %s)", cwd, real)
		}
		return fmt.Errorf("%s is itself a beads workspace root, where bd init would rewrite that workspace's store; run the recorder from a directory that holds no .beads of its own", where)
	}
	// The build is proved first: it reads only, so it needs no directory, and every mode records
	// from the one bd it makes sure the container runs.
	if err := proveBDBuild(child); err != nil {
		return err
	}
	if scratch == "" {
		return nil // a flags recording makes no copies, so it mounts no workspace to prove
	}
	if !filepath.IsAbs(scratch) {
		scratch = filepath.Join(cwd, scratch)
	}
	scratch = filepath.Clean(scratch)
	if link, found, err := symlinkOnPath(scratch); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%s is behind the symlink %s; name a scratch directory whose path holds no symlink, since the directory the recorder mounts is then not the one it named", scratch, link)
	}
	if err := refuseTownServer(child.Env(scratch), townDoltPort(cwd)); err != nil {
		return err
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return fmt.Errorf("make the scratch directory %s: %w", scratch, err)
	}
	if err := proveWorkspace(scratch, child); err != nil {
		return err
	}
	name := newDatabaseName()
	if err := initDatabase(child.Run, scratch, name); err != nil {
		return fmt.Errorf("give the scratch directory %s a workspace of its own: %w", scratch, err)
	}
	if err := scratchMadeWorkspace(scratch); err != nil {
		return err
	}
	// The workspace existed for the check alone. Both parts of its cleanup run, so a failed drop
	// still removes the directory, and either failure is reported rather than left behind.
	if err := errors.Join(dropDatabase(child.Run, scratch, name), os.RemoveAll(beadsDirIn(scratch))); err != nil {
		return fmt.Errorf("clean up the scratch workspace %s: %w", scratch, err)
	}
	return nil
}

// proveWorkspace refuses a container that resolves a beads workspace anywhere but under workDir.
// bd walks up from its working directory, so an answer from outside workDir means the mount let an
// ancestor workspace reach the container and the recording would read and write that store; pinning
// BEADS_DIR changes nothing, since bd honors the pin only when that workspace exists (gt-p85pw.117).
// No workspace at all passes: the container is fresh and bd init makes one under workDir. A bd that
// cannot answer is refused, so the proof is never decided by an error or by an empty answer.
func proveWorkspace(scratch string, child childBD) error {
	resolved, err := child.Where(scratch, child.Env(scratch))
	if err != nil {
		return fmt.Errorf("ask bd which workspace it resolves in the container at %s: %w", workDir, err)
	}
	if resolved == "" {
		return nil
	}
	if !isWithin(workDir, resolved) {
		return fmt.Errorf("bd resolves the workspace %s in the container, which is outside %s; only the scratch directory is mounted there, so the recording would run against a workspace it did not make", resolved, workDir)
	}
	return nil
}

// refuseTownServer refuses the environment a scratch's bd runs would get when it aims them at the
// town's own Dolt server instead of a throwaway one. The guard's init creates the scratch's database,
// so a run on the town's server leaves a database no cleanup owns, the orphan-test-database problem
// (gt-p85pw.115). Each of these fails closed:
//
//   - a server named by the port the town serves Dolt on, or by the production port on any host;
//   - a host that is not this machine, since the throwaway server a recording runs against is a
//     container mapped onto this host's loopback;
//   - no port named at all, since bd then falls back to its own default, the town's server here.
func refuseTownServer(env []string, townPort int) error {
	if host, ok := envValue(env, doltHostVar); ok && !isLocalHost(host) {
		return fmt.Errorf("%s names a host that is not this machine; record against the throwaway Dolt server on this host, never a server the town runs", doltHostVar)
	}
	named := ""
	for _, key := range endpointPortVars {
		value, ok := envValue(env, key)
		if !ok {
			continue
		}
		named = key
		port, err := strconv.Atoi(value)
		if err != nil || port <= 0 || port > 65535 {
			return fmt.Errorf("%s does not name a port, so the recorder cannot tell that the scratch's database would be made on a throwaway server", key)
		}
		if port == productionDoltPort || port == townPort {
			return fmt.Errorf("%s names port %d, the town's Dolt server; the scratch's database must be made on a throwaway server, never the town's", key, port)
		}
	}
	if named == "" {
		return fmt.Errorf("no Dolt server is named in BEADS_DOLT_SERVER_PORT or BEADS_DOLT_PORT, so bd would take its own default, the town's server on this machine; name the throwaway server the recording runs against")
	}
	return nil
}

// townDoltPort is the port the Dolt server of the town the recorder runs in listens on: the endpoint
// mayor/town.json names, and the production port when the recorder runs outside a town or the town
// names no endpoint. It is the address a scratch's init must never reach (gt-p85pw.115).
func townDoltPort(cwd string) int {
	if root := beads.FindTownRoot(cwd); root != "" {
		if endpoint, ok := agentconfig.ResolveDoltEndpoint(root); ok && endpoint.Port > 0 {
			return endpoint.Port
		}
	}
	return productionDoltPort
}

// isLocalHost reports whether host names this machine, where a scratch Dolt container is reached
// (testutil's DoltContainerAddr is 127.0.0.1 with the container's port). The guard resolves no
// names, so one it does not recognize as local is not this machine.
func isLocalHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// envValue returns the value env gives key, as a child process would read it: the last entry wins.
func envValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, entry := range env {
		name, val, ok := strings.Cut(entry, "=")
		if ok && name == key {
			value, found = val, true
		}
	}
	return value, found
}

// scratchMadeWorkspace refuses a scratch that does not hold the workspace the guard's own bd init
// made in it: the init is the only write, and an init that wrote somewhere else leaves the scratch
// without a marker, while the mounted directory is the only place bd can write (gt-p85pw.115,
// gt-p85pw.117). A marker above the scratch is the town's workspace, which the scratch does not own.
func scratchMadeWorkspace(scratch string) error {
	workspace, found, err := enclosingWorkspace(scratch)
	if err != nil {
		return fmt.Errorf("check for the workspace bd init made in %s: %w", scratch, err)
	}
	if found && isWithin(scratch, workspace) {
		return nil
	}
	if found {
		return fmt.Errorf("bd init made no workspace in %s; the nearest one is %s, which the scratch does not own", scratch, workspace)
	}
	return fmt.Errorf("bd init made no workspace in %s, so the recorder cannot prove the scratch owns the store its bd runs would write", scratch)
}

// isWithin reports whether path is dir or sits under it. A pair it cannot compare is not proof of a
// workspace inside the directory, so it reads as outside.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// symlinkOnPath returns the first symlink at path or among its parents. bd follows symlinks, so a
// scratch behind one is a directory the recorder cannot name with any confidence: the workspace it
// makes could be made somewhere else entirely (gt-p85pw.115). The walk starts at the root, since a
// symlink above the scratch moves it just as a symlink at it does.
func symlinkOnPath(path string) (string, bool, error) {
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	at := volume + string(filepath.Separator)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		switch {
		case err == nil && info.Mode()&fs.ModeSymlink != 0:
			return at, true, nil
		case err == nil:
			// A directory in the path, so the next component is checked on its own.
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			// Nothing at or below this component can hold a symlink, and the mkdir that follows
			// names the directory that cannot be made.
			return "", false, nil
		default:
			return "", false, fmt.Errorf("check %s for a symlink: %w", at, err)
		}
	}
	return "", false, nil
}

// realPath returns path with every symlink on it resolved, since bd follows them too: a -scratch
// that is a symlink into a workspace aims bd init at that workspace even though the lexical path
// never passes it (gt-p85pw.103). The part of the path that is not there yet has no symlink to
// resolve, so the deepest existing directory is resolved and the rest is joined on.
func realPath(path string) (string, error) {
	missing := ""
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(real, missing), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path { // the root is not there, so nothing above it can be either
			return filepath.Join(path, missing), nil
		}
		missing = filepath.Join(filepath.Base(path), missing)
		path = parent
	}
}

// workspaceRootAt reports whether dir is a beads workspace root itself, holding .beads or
// metadata.json directly. A directory that only sits under one is not a root: work lives under the
// town, so refusing every directory below a workspace leaves the recorder nowhere to run
// (gt-p85pw.116). A marker it cannot read is an error rather than an absent one.
func workspaceRootAt(dir string) (bool, error) {
	for _, marker := range workspaceMarkers {
		path := filepath.Join(dir, marker)
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("check for a beads workspace at %s: %w", path, err)
		}
	}
	return false, nil
}

// enclosingWorkspace returns the nearest directory at or above dir that holds a workspace marker.
// A marker it cannot read is an error rather than an absent one, so an unreadable tree is never
// taken for a safe one.
func enclosingWorkspace(dir string) (string, bool, error) {
	for {
		for _, marker := range workspaceMarkers {
			path := filepath.Join(dir, marker)
			_, err := os.Stat(path)
			if err == nil {
				return dir, true, nil
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return "", false, fmt.Errorf("check for a beads workspace at %s: %w", path, err)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}
