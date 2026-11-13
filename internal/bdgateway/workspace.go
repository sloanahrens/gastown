package bdgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The workspace is the small tree of bd's own files the gateway owns and gt
// pushes: the town's .beads/routes.jsonl, each rig's .beads/redirect, and each
// rig's .beads/{metadata.json,config.yaml,dolt-server.port}. The design is
// docs/design/bd-container.md ("The workspace"); the tree layout it pushes is
// the one the Phase 0 proof (workspace_proof_integration_test.go) showed a real
// bd reading. G6 mounts the volume and routes PUT /v1/workspace to the store
// below; G8 builds the document from the town copy.

const (
	// WorkspaceHashHeader carries the live tree's manifest hash on every gateway
	// response, so the forwarder can compare it with the files it believes
	// current and the doctor can report a mismatch as drift.
	WorkspaceHashHeader = "X-Bd-Workspace"

	// MaxWorkspaceFileBytes bounds one workspace file. Every file here is bd
	// configuration: a few lines, or for routes.jsonl one short line per rig.
	MaxWorkspaceFileBytes = 256 << 10

	// MaxWorkspaceTotalBytes bounds the whole tree. A document over it would mean
	// a caller that is not pushing bd's five files.
	MaxWorkspaceTotalBytes = 1 << 20

	// MaxWorkspaceDocumentBytes bounds the JSON body of a push. JSON escaping can
	// expand one byte to six ("\u00XX"), so a tree at the total cap can arrive as
	// a body several times the cap; this leaves room for that and the manifest.
	MaxWorkspaceDocumentBytes = 8 << 20
)

// The workspace store's named refusals. Put and OpenWorkspaceStore wrap one of
// these, so a caller and the HTTP layer match on the error rather than on
// message text. Every refusal leaves the live tree as it was: validation
// happens before the store writes anything.
var (
	// ErrWorkspacePath is a file path that is not a clean relative path inside
	// the workspace: absolute, a backslash, an empty, "." or ".." component, or
	// a spelling path.Clean would change.
	ErrWorkspacePath = errors.New("bdgateway: workspace path refused")
	// ErrWorkspaceFileName is a file name outside the five names the gateway
	// serves, or one not directly under a .beads directory.
	ErrWorkspaceFileName = errors.New("bdgateway: workspace file name refused")
	// ErrWorkspaceFileTooLarge is one file over MaxWorkspaceFileBytes.
	ErrWorkspaceFileTooLarge = errors.New("bdgateway: workspace file over the size cap")
	// ErrWorkspaceTooLarge is the whole document over MaxWorkspaceTotalBytes.
	ErrWorkspaceTooLarge = errors.New("bdgateway: workspace over the size cap")
	// ErrWorkspaceNoFiles is a document that pushes no file. bd reads no empty
	// workspace, and an empty push is a caller bug rather than a clean tree.
	ErrWorkspaceNoFiles = errors.New("bdgateway: workspace document has no files")
	// ErrWorkspaceManifestMismatch is a document whose manifest_sha is absent or
	// differs from the hash of the files it carries: the pusher sent content
	// other than what it hashed.
	ErrWorkspaceManifestMismatch = errors.New("bdgateway: workspace manifest does not match the files")
	// ErrWorkspaceRoot is a store root the gateway will not own: empty, not a
	// directory, or a tree holding an entry the store did not write.
	ErrWorkspaceRoot = errors.New("bdgateway: bad workspace root")
	// ErrWorkspaceSwap is a push that failed while building or publishing the
	// temp tree. The previous tree is restored where the filesystem allows.
	ErrWorkspaceSwap = errors.New("bdgateway: workspace swap failed")
)

// workspaceFileNames is every file the workspace holds, by base name. bd reads
// no other file from it, so a document naming another name is refused rather
// than written where nothing reads it.
var workspaceFileNames = map[string]bool{
	"metadata.json":    true,
	"config.yaml":      true,
	"redirect":         true,
	"dolt-server.port": true,
	"routes.jsonl":     true,
}

// WorkspaceDocument is the body of PUT /v1/workspace: every file of the tree,
// keyed by its slash-separated path relative to the workspace root, and the
// hash the pusher computed for those files with WorkspaceManifest.
type WorkspaceDocument struct {
	ManifestSHA string            `json:"manifest_sha"`
	Files       map[string]string `json:"files"`
}

// WorkspaceManifest returns the manifest hash of a workspace's files: sha256
// over the sorted paths and their contents, domain-separated so it cannot equal
// a hash of the same bytes in another protocol. The path, its length and its
// content are each delimited, so no two different trees hash alike. It is
// independent of map order and of the tree's on-disk layout, which is what
// makes a restart recompute the same value. G8 must call this function rather
// than its own hash: the gateway refuses a document whose manifest_sha differs.
func WorkspaceManifest(files map[string]string) string {
	h := sha256.New()
	h.Write([]byte("gt-workspace-v1\x00"))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := files[name]
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(content))))
		h.Write([]byte{0})
		h.Write([]byte(content))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// validatedManifest checks a document against every rule the workspace has and
// returns the manifest hash of its files. Nothing here touches the live tree,
// so a refusal leaves it unchanged.
func validatedManifest(doc WorkspaceDocument) (string, error) {
	if len(doc.Files) == 0 {
		return "", ErrWorkspaceNoFiles
	}
	total := 0
	for _, name := range slices.Sorted(maps.Keys(doc.Files)) {
		if err := validateWorkspacePath(name); err != nil {
			return "", err
		}
		size := len(doc.Files[name])
		if size > MaxWorkspaceFileBytes {
			return "", fmt.Errorf("%w: %q is %d bytes, cap %d", ErrWorkspaceFileTooLarge, shown(name), size, MaxWorkspaceFileBytes)
		}
		total += size
		if total > MaxWorkspaceTotalBytes {
			return "", fmt.Errorf("%w: over %d bytes at %q", ErrWorkspaceTooLarge, MaxWorkspaceTotalBytes, shown(name))
		}
	}
	manifest := WorkspaceManifest(doc.Files)
	if doc.ManifestSHA != manifest {
		return "", fmt.Errorf("%w: sent %q, files hash to %s", ErrWorkspaceManifestMismatch, shown(doc.ManifestSHA), manifest)
	}
	return manifest, nil
}

// validateWorkspacePath checks one relative path. The rules are the design's
// ("relative, no .., only the five file names above") plus the `.beads` parent
// every one of the five files sits under, which keeps a path from landing in a
// directory bd does not read or beside the tree's root.
func validateWorkspacePath(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty path", ErrWorkspacePath)
	}
	if strings.Contains(name, `\`) {
		return fmt.Errorf("%w: %q has a backslash", ErrWorkspacePath, shown(name))
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: %q has a NUL byte", ErrWorkspacePath, shown(name))
	}
	if path.IsAbs(name) {
		return fmt.Errorf("%w: %q is absolute", ErrWorkspacePath, shown(name))
	}
	if path.Clean(name) != name {
		return fmt.Errorf("%w: %q is not a clean relative path", ErrWorkspacePath, shown(name))
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: %q has a %q component", ErrWorkspacePath, shown(name), part)
		}
	}
	if len(parts) < 2 || parts[len(parts)-2] != ".beads" {
		return fmt.Errorf("%w: %q is not directly under a .beads directory", ErrWorkspaceFileName, shown(name))
	}
	if base := parts[len(parts)-1]; !workspaceFileNames[base] {
		return fmt.Errorf("%w: %q is not one of the workspace's files", ErrWorkspaceFileName, base)
	}
	return nil
}

// shown bounds text the caller sent before it reaches an error message, so a
// path or hash of any length cannot bloat a response.
func shown(text string) string {
	const max = 128
	if len(text) <= max {
		return text
	}
	return text[:max] + "..."
}

// workspaceFS is the store's filesystem seam. The real one is osWorkspaceFS;
// tests replace one call to drive an error branch that a healthy disk cannot.
type workspaceFS struct {
	lstat     func(path string) (os.FileInfo, error)
	readFile  func(path string) ([]byte, error)
	readDir   func(path string) ([]os.DirEntry, error)
	mkdirTemp func(dir, pattern string) (string, error)
	mkdirAll  func(path string, perm os.FileMode) error
	removeAll func(path string) error
	writeFile func(path string, data []byte, perm os.FileMode) error
	rename    func(oldpath, newpath string) error
}

// osWorkspaceFS is the seam's production value.
func osWorkspaceFS() workspaceFS {
	return workspaceFS{
		lstat:     os.Lstat,
		readFile:  os.ReadFile,
		readDir:   os.ReadDir,
		mkdirTemp: os.MkdirTemp,
		mkdirAll:  os.MkdirAll,
		removeAll: os.RemoveAll,
		writeFile: os.WriteFile,
		rename:    os.Rename,
	}
}

// WorkspaceStore owns one workspace tree on disk and the manifest hash of the
// tree it last published. It is safe for concurrent use: every push runs under
// one mutex, so two pushes racing never interleave their temp trees. A second
// gateway process on the same volume is out of scope, as the design gives the
// volume to the one container.
type WorkspaceStore struct {
	root     string
	mu       sync.Mutex
	manifest string
	fs       workspaceFS
}

// OpenWorkspaceStore takes ownership of the tree at root, creating nothing
// until the first Put. A missing root is an empty workspace; an existing one
// must hold only entries the store itself would write, so a foreign tree is
// refused rather than hashed and served. The store swaps
// the root directory itself, so root's parent must exist and be on the writable
// volume: a mount point cannot be renamed, and a push against one fails with the
// old tree still live.
func OpenWorkspaceStore(root string) (*WorkspaceStore, error) {
	return openWorkspaceStore(root, osWorkspaceFS())
}

// openWorkspaceStore is OpenWorkspaceStore with the filesystem seam exposed, so
// tests can drive the read failures a healthy disk does not produce.
func openWorkspaceStore(root string, fsys workspaceFS) (*WorkspaceStore, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: empty path", ErrWorkspaceRoot)
	}
	s := &WorkspaceStore{root: root, fs: fsys}
	files, err := s.readTree()
	if err != nil {
		return nil, err
	}
	s.manifest = WorkspaceManifest(files)
	return s, nil
}

// Manifest returns the manifest hash of the live tree. Every gateway response
// carries it in WorkspaceHashHeader (AddWorkspaceHeader).
func (s *WorkspaceStore) Manifest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.manifest
}

// readTree loads and checks the live tree. The checks are the push rules, so a
// tree the store would not accept from a pusher is not one it serves either.
func (s *WorkspaceStore) readTree() (map[string]string, error) {
	info, err := s.fs.lstat(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkspaceRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrWorkspaceRoot, s.root)
	}
	files := map[string]string{}
	total := 0
	if err := s.readTreeDir(s.root, "", files, &total); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkspaceRoot, err)
	}
	return files, nil
}

// readTreeDir adds every file under dir to files, keyed by its path from the
// root, and checks it against the push rules. prefix is that path so far, in
// slash form. Directories are walked through the filesystem seam rather than
// filepath.WalkDir, so every read failure is one a test can drive.
func (s *WorkspaceStore) readTreeDir(dir, prefix string, files map[string]string, total *int) error {
	entries, err := s.fs.readDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := path.Join(prefix, entry.Name())
		if entry.IsDir() {
			if err := s.readTreeDir(filepath.Join(dir, entry.Name()), name, files, total); err != nil {
				return err
			}
			continue
		}
		if err := validateWorkspacePath(name); err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: %q is not a regular file", ErrWorkspacePath, name)
		}
		content, err := s.fs.readFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		if len(content) > MaxWorkspaceFileBytes {
			return fmt.Errorf("%w: %q is %d bytes, cap %d", ErrWorkspaceFileTooLarge, name, len(content), MaxWorkspaceFileBytes)
		}
		*total += len(content)
		if *total > MaxWorkspaceTotalBytes {
			return fmt.Errorf("%w: over %d bytes at %q", ErrWorkspaceTooLarge, MaxWorkspaceTotalBytes, name)
		}
		files[name] = string(content)
	}
	return nil
}

// Put validates a document, builds its tree beside the live one, and publishes
// it, returning the manifest hash now served. Every refusal happens before the
// tree is touched; a failure while building or publishing leaves the previous
// tree live (see swap).
func (s *WorkspaceStore) Put(doc WorkspaceDocument) (string, error) {
	manifest, err := validatedManifest(doc)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.sweep(); err != nil {
		return "", err
	}
	tmp, err := s.fs.mkdirTemp(s.parent(), s.base()+".new.")
	if err != nil {
		return "", fmt.Errorf("%w: creating the temp tree: %v", ErrWorkspaceSwap, err)
	}
	if err := s.writeTree(tmp, doc.Files); err != nil {
		return "", errors.Join(err, s.removeTempTree(tmp))
	}
	if err := s.swap(tmp); err != nil {
		return "", err
	}
	s.manifest = manifest
	return manifest, nil
}

// writeTree writes every file of the document under root, in sorted path order
// so a failure is reproducible. Parent directories are created as needed; the
// paths were validated, so no write can leave the temp tree.
func (s *WorkspaceStore) writeTree(root string, files map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(files)) {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := s.fs.mkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("%w: creating %q: %v", ErrWorkspaceSwap, shown(name), err)
		}
		if err := s.fs.writeFile(target, []byte(files[name]), 0o600); err != nil {
			return fmt.Errorf("%w: writing %q: %v", ErrWorkspaceSwap, shown(name), err)
		}
	}
	return nil
}

// swap publishes the temp tree at the root. A first push is one rename, which
// is atomic. A replacement moves the live tree aside, publishes the new one and
// leaves the old for the next sweep: if the publish fails, the old tree is
// moved back, and the caller's error says so when even that fails. Between the
// two renames the root is briefly absent, never half-written; a reader in that
// window sees a missing tree, not a mixed one. A rename that cannot run at all
// (the root is a mount point, say) fails before the live tree moves.
func (s *WorkspaceStore) swap(tmp string) error {
	old := s.oldPath()
	_, err := s.fs.lstat(s.root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := s.fs.rename(tmp, s.root); err != nil {
			return errors.Join(fmt.Errorf("%w: publishing the first tree: %v", ErrWorkspaceSwap, err), s.removeTempTree(tmp))
		}
		return nil
	case err != nil:
		return errors.Join(fmt.Errorf("%w: reading the live tree: %v", ErrWorkspaceSwap, err), s.removeTempTree(tmp))
	}
	if err := s.fs.rename(s.root, old); err != nil {
		return errors.Join(fmt.Errorf("%w: moving the live tree aside: %v", ErrWorkspaceSwap, err), s.removeTempTree(tmp))
	}
	if err := s.fs.rename(tmp, s.root); err != nil {
		joined := []error{fmt.Errorf("%w: publishing the new tree: %v", ErrWorkspaceSwap, err)}
		if restoreErr := s.fs.rename(old, s.root); restoreErr != nil {
			joined = append(joined, fmt.Errorf("restoring the previous tree: %v", restoreErr))
		}
		joined = append(joined, s.removeTempTree(tmp))
		return errors.Join(joined...)
	}
	return nil
}

// sweep removes the trees a finished or interrupted push left beside the live
// one. It runs before a push writes anything, so a sweep failure refuses the
// push with the live tree untouched; it never runs on Open, where a leftover
// from a crash must not keep the gateway from booting. A failed removal after a
// successful publish is the same leftover, cleaned by the next push.
func (s *WorkspaceStore) sweep() error {
	entries, err := s.fs.readDir(s.parent())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: reading the store directory: %v", ErrWorkspaceSwap, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != s.base()+".old" && !strings.HasPrefix(name, s.base()+".new.") {
			continue
		}
		if err := s.fs.removeAll(filepath.Join(s.parent(), name)); err != nil {
			return fmt.Errorf("%w: removing the leftover tree %q: %v", ErrWorkspaceSwap, name, err)
		}
	}
	return nil
}

// removeTempTree removes an abandoned temp tree. Callers join the error onto
// the one that already failed the push.
func (s *WorkspaceStore) removeTempTree(path string) error {
	if err := s.fs.removeAll(path); err != nil {
		return fmt.Errorf("removing the temp tree: %v", err)
	}
	return nil
}

// parent is the directory holding the live tree and its temp siblings: one
// filesystem, so a rename is atomic.
func (s *WorkspaceStore) parent() string { return filepath.Dir(s.root) }

// base is the live tree's name under parent, and the prefix its temp siblings
// share.
func (s *WorkspaceStore) base() string { return filepath.Base(s.root) }

// oldPath is where a replacement moves the live tree before publishing the new
// one. The fixed name is safe because Put holds the store's one mutex.
func (s *WorkspaceStore) oldPath() string {
	return filepath.Join(s.parent(), s.base()+".old")
}
