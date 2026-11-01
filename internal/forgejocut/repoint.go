package forgejocut

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Files is the filesystem the cutover's config steps read and write. The command
// runs on the operating system's; a test injects an in-memory one, so no test
// touches a real town.
type Files interface {
	Stat(path string) (fs.FileInfo, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
	Rename(oldpath, newpath string) error
	Chmod(path string, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
}

// osFiles is Files on the operating system.
type osFiles struct{}

func (osFiles) Stat(path string) (fs.FileInfo, error)      { return os.Stat(path) }
func (osFiles) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
func (osFiles) ReadFile(path string) ([]byte, error)       { return os.ReadFile(path) }
func (osFiles) Rename(oldpath, newpath string) error       { return os.Rename(oldpath, newpath) }
func (osFiles) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (osFiles) Chmod(path string, perm fs.FileMode) error    { return os.Chmod(path, perm) }
func (osFiles) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

// files is the filesystem the run reads: the injected one, else the operating
// system's.
func (c *Cutover) files() Files {
	if c.Files != nil {
		return c.Files
	}
	return osFiles{}
}

// now is the clock the backup names are stamped from.
func (c *Cutover) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// RepointRemotes is step 5's git half: the rig's origin is set to the Forgejo URL
// in the bare repository the landing path pushes to, in the mayor's clone and in
// every crew clone that exists. A directory already at the URL is reported and
// left alone; a directory that is not a git repository is reported and skipped,
// since crew/ holds files beside the clones.
//
// A dry run prints each set-url it would run and runs none; it still makes the
// read-only calls that find each repository and its origin. This changes live
// checkouts, so the live run is a deliberate step after the promote key exists:
// the undo for any one directory is the printed command with the old URL, which
// the log names ("origin was <old>").
func (c *Cutover) RepointRemotes() (err error) {
	defer func() { err = c.redact(err) }()
	o := c.Opts
	dirs, err := c.repointDirs()
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if err := c.repointOne(d.path, o.ForgejoURL, d.crew); err != nil {
			return err
		}
	}
	return nil
}

// repointDir is one directory to repoint. A crew entry may be a directory that
// is not a clone and is then skipped; the bare repository and the mayor's clone
// must be repositories, or the step fails.
type repointDir struct {
	path string
	crew bool
}

// repointDirs lists the directories to repoint, in the script's order: the bare
// repository, the mayor's clone when it exists, then each crew directory by name.
// A hidden entry and a file in crew/ are not clones.
func (c *Cutover) repointDirs() ([]repointDir, error) {
	o := c.Opts
	dirs := []repointDir{{path: o.BareRepo}}
	if c.isDir(filepath.Join(o.RigRoot, "mayor", "rig")) {
		dirs = append(dirs, repointDir{path: filepath.Join(o.RigRoot, "mayor", "rig")})
	}
	crew := filepath.Join(o.RigRoot, "crew")
	if !c.isDir(crew) {
		return dirs, nil
	}
	entries, err := c.files().ReadDir(crew)
	if err != nil {
		return nil, fmt.Errorf("could not list the crew clones in %s: %w", crew, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if dir := filepath.Join(crew, e.Name()); c.isDir(dir) {
			dirs = append(dirs, repointDir{path: dir, crew: true})
		}
	}
	return dirs, nil
}

// isDir is a directory test that follows a symlink, as the script's [ -d ] did.
func (c *Cutover) isDir(path string) bool {
	info, err := c.files().Stat(path)
	return err == nil && info.IsDir()
}

// repointOne sets one directory's origin to url, or says why it did not. A crew
// directory that is not a repository of its own is logged and skipped; the bare
// repository and the mayor's clone that are not fail the step, since a step that
// reports success with origin still at GitHub would let the registry move on
// without them.
func (c *Cutover) repointOne(dir, url string, crew bool) error {
	if !c.isRepositoryRoot(dir) {
		if !crew {
			return fmt.Errorf("%s is not a git repository of its own, so its origin cannot be repointed", dir)
		}
		c.logf("skip %s (not a git repository of its own)", dir)
		return nil
	}
	// A repository with no origin reads as an empty URL and is set, as the script's
	// "|| true" did. Git exits 2 for a remote that is not defined; any other failure
	// is a read that went wrong and would blank the undo line.
	out, err := c.git()(context.Background(), []string{"git", "-C", dir, "remote", "get-url", "origin"})
	var coded interface{ ExitCode() int }
	if err != nil && !(errors.As(err, &coded) && coded.ExitCode() == noSuchRemoteExit) {
		return fmt.Errorf("could not read the origin of %s: %w", dir, err)
	}
	current := strings.TrimRight(string(out), "\n")
	if current == url {
		c.logf("%s: origin is already %s", dir, printedURL(url))
		return nil
	}
	c.logf("+ git -C %s remote set-url origin %s (origin was %s)", dir, printedURL(url), printedURL(current))
	if c.Opts.DryRun {
		return nil
	}
	if _, err := c.git()(context.Background(), []string{"git", "-C", dir, "remote", "set-url", "origin", url}); err != nil {
		return fmt.Errorf("could not repoint %s: %w", dir, err)
	}
	return nil
}

// isRepositoryRoot reports whether dir is itself a repository: a work tree whose
// top level is dir, or a bare repository whose git directory is dir. Git finds a
// repository by walking up, so a plain folder inside another repository answers
// rev-parse --git-dir; asking for the top level is what tells the two apart, and
// the origin that gets set is then the folder's own, never an ancestor's.
func (c *Cutover) isRepositoryRoot(dir string) bool {
	ctx := context.Background()
	if out, err := c.git()(ctx, []string{"git", "-C", dir, "rev-parse", "--show-toplevel"}); err == nil {
		return samePath(strings.TrimRight(string(out), "\n"), dir)
	}
	// A bare repository has no top level; it is the root when git says it is bare
	// and its git directory is the directory itself.
	out, err := c.git()(ctx, []string{"git", "-C", dir, "rev-parse", "--is-bare-repository", "--git-dir"})
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	return len(lines) == 2 && lines[0] == "true" && lines[1] == "."
}

// samePath reports whether a and b name the same directory. Git prints the
// resolved top level, so a clone reached through a symlink (a town root or a
// crew folder that is a link) reports a path that differs from the one given;
// comparing only the cleaned text would call that real repository "not of its
// own" and let the crew skip report success. Both are resolved when they exist;
// a path that does not resolve is compared as written.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// noSuchRemoteExit is the status git remote get-url exits with for a remote the
// repository does not define.
const noSuchRemoteExit = 2

// SetTownGitURL is step 5's registry half: registry.rigs.<rig>.git_url in the
// town's mayor/town.json is set to the Forgejo URL. The file is edited as JSON:
// only that one value changes, every other key keeps its place and its value text,
// the document is re-indented at two spaces as the script wrote it, and
// a value that already matches is neither rewritten nor backed up. Otherwise the
// file is copied to a dated .bak- sibling first, and the undo is to copy that
// backup back over town.json. A dry run logs the backup and the edit it would make
// and writes nothing.
func (c *Cutover) SetTownGitURL() error {
	o := c.Opts
	path := filepath.Join(o.TownRoot, "mayor", "town.json")
	data, err := c.files().ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not read %s: %w", path, err)
	}
	gitURLPath := []string{"registry", "rigs", o.Rig, "git_url"}
	current, _, err := jsonGet(data, gitURLPath)
	if err != nil {
		return fmt.Errorf("%s is not usable: %w", path, err)
	}
	if jsonStringValue(current) == o.ForgejoURL {
		c.logf("%s: registry.rigs.%s.git_url is already %s", path, o.Rig, printedURL(o.ForgejoURL))
		return nil
	}
	// The edit is made before anything is backed up or written, so a registry that
	// does not list the rig is refused with the file untouched.
	edited, err := jsonSet(data, gitURLPath, jsonString(o.ForgejoURL))
	if err != nil {
		return fmt.Errorf("%s has no registry.rigs.%s to set (%w)", path, o.Rig, err)
	}
	if err := c.BackupFile(path); err != nil {
		return err
	}
	if o.DryRun {
		c.logf("would set registry.rigs.%s.git_url to %s in %s", o.Rig, printedURL(o.ForgejoURL), path)
		return nil
	}
	if err := c.replaceFile(path, edited); err != nil {
		return err
	}
	c.logf("set registry.rigs.%s.git_url to %s in %s", o.Rig, printedURL(o.ForgejoURL), path)
	return nil
}

// BackupFile copies path to a sibling named path.bak-<yyyymmdd-hhmmss> and logs
// the copy; the copy keeps the file's permission bits. A file that does not exist
// has nothing to back up and is logged. A dry run logs the copy it would make and
// writes none.
func (c *Cutover) BackupFile(path string) error {
	info, err := c.files().Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not look at %s to back it up: %w", path, err)
	}
	if err != nil || !info.Mode().IsRegular() {
		c.logf("no %s to back up (it does not exist yet)", path)
		return nil
	}
	backup := path + ".bak-" + c.now().Format("20060102-150405")
	if _, err := c.files().Stat(backup); err == nil {
		return fmt.Errorf("the backup %s already exists; a second run in the same second would replace it, so run again in a second", backup)
	}
	if c.Opts.DryRun {
		c.logf("would back up %s to %s", path, backup)
		return nil
	}
	data, err := c.files().ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not read %s to back it up: %w", path, err)
	}
	if err := c.files().WriteFile(backup, data, info.Mode().Perm()); err != nil {
		return fmt.Errorf("could not write the backup %s: %w", backup, err)
	}
	c.logf("backed up %s to %s", path, backup)
	return nil
}

// replaceFile writes data to path by writing a dated sibling and renaming it over
// path, so a failed write leaves the old file whole. A failed rename leaves the
// sibling in place, and the error names it. The file keeps its permission bits.
func (c *Cutover) replaceFile(path string, data []byte) error {
	perm := fs.FileMode(0o644)
	if info, err := c.files().Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	tmp := path + ".new-" + c.now().Format("20060102-150405")
	if _, err := c.files().Stat(tmp); err == nil {
		return fmt.Errorf("%s already exists, left by an earlier run in the same second; remove it or run again in a second", tmp)
	}
	if err := c.files().WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("could not write %s: %w", tmp, err)
	}
	if err := c.files().Rename(tmp, path); err != nil {
		return fmt.Errorf("could not replace %s (the new text is left in %s): %w", path, tmp, err)
	}
	return nil
}

// errEmptyPath is the refusal of a lookup or edit that names no key.
var errEmptyPath = errors.New("the JSON path names no key")

// member is one key of a JSON object with its value's text untouched.
type member struct {
	key string
	val json.RawMessage
}

// parseObject reads one JSON object into its members, in document order.
func parseObject(raw []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("a value on the path is not an object")
	}
	var members []member
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		members = append(members, member{key: key, val: val})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("text follows the JSON document")
	}
	return members, nil
}

// lastMember is the index of the last member named key, the one a JSON reader that
// keeps the final duplicate sees, or -1.
func lastMember(members []member, key string) int {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].key == key {
			return i
		}
	}
	return -1
}

// jsonGet returns the value at path in the object doc, and whether every key on
// the path exists.
func jsonGet(doc []byte, path []string) (json.RawMessage, bool, error) {
	if len(path) == 0 {
		return nil, false, errEmptyPath
	}
	members, err := parseObject(doc)
	if err != nil {
		return nil, false, err
	}
	i := lastMember(members, path[0])
	if i < 0 {
		return nil, false, nil
	}
	if len(path) == 1 {
		return members[i].val, true, nil
	}
	return jsonGet(members[i].val, path[1:])
}

// jsonSet returns doc with the value at path replaced by value, indented two
// spaces with a final newline. Every key on the path but the last must exist; the
// last is added at the end of its object when it does not. No other key moves and
// no other value is re-encoded.
func jsonSet(doc []byte, path []string, value json.RawMessage) ([]byte, error) {
	out, err := setPath(doc, path, value)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, out, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func setPath(raw []byte, path []string, value json.RawMessage) ([]byte, error) {
	if len(path) == 0 {
		return nil, errEmptyPath
	}
	members, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	i := lastMember(members, path[0])
	switch {
	case i < 0 && len(path) == 1:
		members = append(members, member{key: path[0], val: value})
	case i < 0:
		return nil, fmt.Errorf("no key %q on the path", path[0])
	case len(path) == 1:
		members[i].val = value
	default:
		if members[i].val, err = setPath(members[i].val, path[1:], value); err != nil {
			return nil, err
		}
	}
	return renderMembers(members), nil
}

// renderMembers is members as one compact JSON object, in the order given, each
// value's text untouched.
func renderMembers(members []member) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for n, m := range members {
		if n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonString(m.key))
		buf.WriteByte(':')
		buf.Write(m.val)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// ensureKeys returns the object doc with each default added at its end when the
// object has no such key. A key that is present is not touched, whatever it holds.
func ensureKeys(doc []byte, defaults []member) ([]byte, error) {
	members, err := parseObject(doc)
	if err != nil {
		return nil, err
	}
	for _, d := range defaults {
		if lastMember(members, d.key) < 0 {
			members = append(members, d)
		}
	}
	return renderMembers(members), nil
}

// jsonString is s as a JSON string, with no HTML escaping: a URL keeps its & and <.
func jsonString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// jsonStringValue is the string a JSON value holds, or "" for a value that is
// absent, null or not a string.
func jsonStringValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
