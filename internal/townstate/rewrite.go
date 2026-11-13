package townstate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// RewriteBackupSuffix is appended to a rewritten file's path for the copy that
// holds its original bytes until Undo restores it.
const RewriteBackupSuffix = ".pre-rewrite"

// rewriteTempSuffix is appended to a rewritten file's path for the bytes built
// before the atomic rename over the original.
const rewriteTempSuffix = ".rewrite-tmp"

// rewriteMaxDepth bounds the walk: every listed class file and worktree pointer
// in the town layout sits five path segments or fewer below the town root, and a
// checkout's own tree is deeper than that.
const rewriteMaxDepth = 5

// rewriteForgePrefix is the host spelling the Forgejo owner is written behind.
const rewriteForgePrefix = "forgejo:3000/"

// rewriteOwnerPattern is a Forgejo account name: one lowercase path component,
// as internal/imagevalues holds it.
var rewriteOwnerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// townMayorDir is the mayor directory's name; the shared constant is not used here (gt-3lvcjz.26).
const townMayorDir = "mayor"

// RewriteClass is one class of file a rewrite may change: its name for the
// summary, and the rule that decides whether a town-root-relative path is in
// it. Match sees paths with forward slashes.
type RewriteClass struct {
	Name  string
	Match func(rel string) bool
}

// RewriteQuery is what one pass replaces: the old and new town root, and
// optionally the old and new Forgejo owner. An empty owner pair turns the owner
// replacement off.
type RewriteQuery struct {
	FromRoot  string
	ToRoot    string
	FromOwner string
	ToOwner   string
}

// Validate refuses a query that cannot name a rewrite, naming the flag that
// makes it unusable. Both roots are required, different, absolute and carry no
// trailing slash, so each matches as a whole path. The owners are both or
// neither, and each is one lowercase account name.
func (q RewriteQuery) Validate() error {
	if q.FromRoot == "" {
		return errors.New("rewrite: --from-root is required")
	}
	if q.ToRoot == "" {
		return errors.New("rewrite: --to-root is required")
	}
	if q.FromRoot == q.ToRoot {
		return fmt.Errorf("rewrite: --from-root and --to-root are the same (%s)", q.FromRoot)
	}
	if strings.HasPrefix(q.ToRoot, q.FromRoot+"/") {
		return fmt.Errorf("rewrite: --to-root %q is under --from-root; a second pass would rewrite it again", q.ToRoot)
	}
	for _, r := range []struct{ flag, root string }{{"--from-root", q.FromRoot}, {"--to-root", q.ToRoot}} {
		if !path.IsAbs(r.root) {
			return fmt.Errorf("rewrite: %s %q is not an absolute path", r.flag, r.root)
		}
		if strings.HasSuffix(r.root, "/") {
			return fmt.Errorf("rewrite: %s %q ends with a slash; name the directory itself", r.flag, r.root)
		}
	}
	if (q.FromOwner == "") != (q.ToOwner == "") {
		return errors.New("rewrite: --from-owner and --to-owner are given together or not at all")
	}
	for _, o := range []struct{ flag, owner string }{{"--from-owner", q.FromOwner}, {"--to-owner", q.ToOwner}} {
		if o.owner != "" && !rewriteOwnerPattern.MatchString(o.owner) {
			return fmt.Errorf("rewrite: %s %q is not one lowercase account name", o.flag, o.owner)
		}
	}
	return nil
}

// RewriteClasses is the one list of file classes a rewrite changes. rigs are
// the town's rig names; only their settings, config and beads files are listed,
// so a file inside a rig's checkout is not one of them.
//
// Logs, events, Dolt data, git object stores and every file outside this list
// are left alone; RewriteSkipped names them for the summary.
func RewriteClasses(rigs []string) []RewriteClass {
	isRig := func(name string) bool { return slices.Contains(rigs, name) }
	return []RewriteClass{
		{Name: "town settings (settings/*.json, *.yaml, *.yml)", Match: topConfigMatch("settings")},
		{Name: "mayor files (mayor/*.json, *.yaml, *.yml)", Match: topConfigMatch(townMayorDir)},
		{Name: "daemon configuration (daemon/*.json, *.yaml, *.yml)", Match: topConfigMatch("daemon")},
		{Name: "rig settings and config (<rig>/settings/*.json|yaml|yml, <rig>/config.json)", Match: func(rel string) bool {
			seg := strings.Split(rel, "/")
			if len(seg) < 2 || !isRig(seg[0]) {
				return false
			}
			switch {
			case len(seg) == 2:
				return seg[1] == "config.json"
			case len(seg) == 3:
				return seg[1] == "settings" && hasConfigExt(seg[2])
			}
			return false
		}},
		{Name: "beads files (.beads and <rig>/.beads: redirect, routes.jsonl, metadata.json)", Match: func(rel string) bool {
			seg := strings.Split(rel, "/")
			switch len(seg) {
			case 2:
				return seg[0] == ".beads" && isBeadsFile(seg[1])
			case 3:
				return isRig(seg[0]) && seg[1] == ".beads" && isBeadsFile(seg[2])
			}
			return false
		}},
		{Name: "git pointer files (worktree .git files and .repo.git/worktrees/*/gitdir)", Match: func(rel string) bool {
			seg := strings.Split(rel, "/")
			n := len(seg)
			if n >= 2 && seg[n-1] == ".git" {
				return true
			}
			return n >= 4 && seg[n-1] == "gitdir" && seg[n-3] == "worktrees" && seg[n-4] == constants.DirRepoGit
		}},
	}
}

// RewriteSkipped names what a rewrite leaves alone, in the order the command's
// summary prints it.
var RewriteSkipped = []string{
	"logs and daemon subsystem records under logs/, events/ and .runtime/",
	"the town event feed (.events.jsonl) and the feed (.feed.jsonl)",
	"Dolt data (.dolt-data*, dolt/)",
	"git object stores (.git directories, and .repo.git except its worktrees/*/gitdir pointers)",
	"every file outside the listed classes",
}

// rewriteKeptSkip names the directories the kept-copy search does not enter. A
// name ending in * matches any name with that prefix. .git and node_modules hold
// a checkout's history and dependencies, not town state; dolt, .dolt-archive and
// .dolt-data* are Dolt's own files. Every other directory is searched, at any
// depth, so a kept copy in logs, events or .runtime still refuses the run.
var rewriteKeptSkip = []string{".git", "node_modules", "dolt", ".dolt-archive", ".dolt-data*"}

// RewriteFile is one file a pass changed or would change.
type RewriteFile struct {
	// Path is relative to the town root, with forward slashes.
	Path string
	// Replacements is how many old values the pass replaces in it.
	Replacements int
}

// RewriteReport is what one pass reports, sorted by path.
type RewriteReport struct {
	// Classes names the file classes the pass walked, in the order
	// RewriteClasses lists them.
	Classes []string
	// Files are the listed-class files that hold an old value.
	Files []RewriteFile
	// Undone are the files an undo restored.
	Undone []RewriteRestored
}

// RewriteRestored is one file an undo put back.
type RewriteRestored struct {
	// Path is relative to the town root, with forward slashes.
	Path string
	// Recreated is true when the rewritten file was missing, and the undo created
	// it again from its kept copy.
	Recreated bool
}

// rewriteFiles is the write seam an apply or an undo writes through. Reads go
// through the fs.FS the walk uses. Production uses hostRewriteFiles; a test
// replaces one operation to drive its failure without an unwritable directory on
// the host.
type rewriteFiles struct {
	lstat    func(string) (fs.FileInfo, error)
	openFile func(string, int, fs.FileMode) (io.WriteCloser, error)
	rename   func(string, string) error
	remove   func(string) error
}

// hostRewriteFiles is the seam backed by the host filesystem.
func hostRewriteFiles() rewriteFiles {
	return rewriteFiles{
		lstat: os.Lstat,
		openFile: func(p string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
			return os.OpenFile(p, flag, perm) //nolint:gosec // G304: the path is a listed file's own path plus a fixed suffix
		},
		rename: os.Rename,
		remove: os.Remove,
	}
}

// Rewrite replaces the query's old values in the town's listed classes. It
// walks the town under root, reports each listed-class file that holds one, and
// with apply writes the result: the original bytes are kept as
// <file>.pre-rewrite and the new bytes replace the file atomically. A file with
// no old value is not written.
//
// After it reads the listed-class files, and before anything is written, it
// refuses, in a dry run as well as an apply, when any file under root named
// <file>.pre-rewrite exists outside the trees in rewriteKeptSkip: a previous run
// was neither undone nor cleaned, and that copy is the only original. Nothing is
// written in either case.
func Rewrite(root string, q RewriteQuery, apply bool) (*RewriteReport, error) {
	return rewriteWith(os.DirFS(root), root, q, apply, hostRewriteFiles())
}

// rewriteWith is Rewrite over an injected read filesystem and write seam.
func rewriteWith(fsys fs.FS, root string, q RewriteQuery, apply bool, files rewriteFiles) (*RewriteReport, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := checkRewriteTown(root); err != nil {
		return nil, err
	}
	classes, err := rewriteClassesFor(fsys)
	if err != nil {
		return nil, err
	}
	var pending []rewriteTarget
	err = walkRewrite(fsys, root, classes, func(t rewriteTarget) error {
		n, err := rewriteFileContent(fsys, &t, q)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		t.replacements = n
		pending = append(pending, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	kept, err := keptCopies(fsys)
	if err != nil {
		return nil, err
	}
	if len(kept) > 0 {
		return nil, keptCopiesError(kept)
	}
	report := &RewriteReport{Classes: classNames(classes)}
	for _, t := range pending {
		report.Files = append(report.Files, RewriteFile{Path: t.rel, Replacements: t.replacements})
	}
	if !apply {
		return report, nil
	}
	for _, t := range pending {
		if err := keepAndReplace(files, t.rel, t.full, t.orig, t.new); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// Undo restores every kept copy of a listed-class file, byte for byte, and
// removes it. A kept copy whose rewritten file is missing is created again from
// the copy, with its mode. A kept copy outside the listed classes is left where
// it is, and so is a listed-class file with no kept copy, so the undo of a town
// with nothing to undo changes nothing.
func Undo(root string) (*RewriteReport, error) {
	return undoWith(os.DirFS(root), root, hostRewriteFiles())
}

// undoWith is Undo over an injected read filesystem and write seam.
func undoWith(fsys fs.FS, root string, files rewriteFiles) (*RewriteReport, error) {
	if err := checkRewriteTown(root); err != nil {
		return nil, err
	}
	classes, err := rewriteClassesFor(fsys)
	if err != nil {
		return nil, err
	}
	kept, err := keptCopies(fsys)
	if err != nil {
		return nil, err
	}
	report := &RewriteReport{Classes: classNames(classes)}
	for _, copyRel := range kept {
		rel := strings.TrimSuffix(copyRel, RewriteBackupSuffix)
		if !matchesClass(classes, rel) {
			continue
		}
		if err := undoKeptCopy(fsys, files, root, rel, report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// undoKeptCopy puts the kept copy of rel back over rel, or creates rel from the
// copy when it is missing, and records what it did in report.
func undoKeptCopy(fsys fs.FS, files rewriteFiles, root, rel string, report *RewriteReport) error {
	full := filepath.Join(root, filepath.FromSlash(rel))
	backup := full + RewriteBackupSuffix
	info, err := files.lstat(backup)
	if err != nil {
		return fmt.Errorf("rewrite: checking for a kept copy of %s: %w", rel, err)
	}
	// A link or other non-file at the kept name would be renamed over the class
	// file, so it is refused rather than restored.
	if !info.Mode().IsRegular() {
		return fmt.Errorf("rewrite: %s is not a regular file; not restoring it over %s", rel+RewriteBackupSuffix, rel)
	}
	_, err = files.lstat(full)
	switch {
	case err == nil:
		if err := files.rename(backup, full); err != nil {
			return fmt.Errorf("rewrite: restoring %s: %w", rel, err)
		}
		report.Undone = append(report.Undone, RewriteRestored{Path: rel})
		return nil
	case errors.Is(err, fs.ErrNotExist):
		data, err := fs.ReadFile(fsys, rel+RewriteBackupSuffix)
		if err != nil {
			return fmt.Errorf("rewrite: reading the kept copy of %s: %w", rel, err)
		}
		if err := writeNewFile(files, full, data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("rewrite: restoring %s: %w", rel, err)
		}
		if err := files.remove(backup); err != nil {
			return fmt.Errorf("rewrite: removing the kept copy of %s after restoring it: %w", rel, err)
		}
		report.Undone = append(report.Undone, RewriteRestored{Path: rel, Recreated: true})
		return nil
	default:
		return fmt.Errorf("rewrite: checking %s: %w", rel, err)
	}
}

// classNames returns the class names in list order.
func classNames(classes []RewriteClass) []string {
	names := make([]string, len(classes))
	for i, c := range classes {
		names[i] = c.Name
	}
	return names
}

// Scan returns the listed-class files under root that still hold an old value in
// a form a rewrite replaces, sorted by path. nil classes means the pass's own
// list. The command runs it after apply to report any file the pass left
// holding one.
func Scan(root string, q RewriteQuery, classes []RewriteClass) ([]string, error) {
	return scanWith(os.DirFS(root), root, q, classes)
}

// scanWith is Scan over an injected read filesystem.
func scanWith(fsys fs.FS, root string, q RewriteQuery, classes []RewriteClass) ([]string, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := checkRewriteTown(root); err != nil {
		return nil, err
	}
	if classes == nil {
		var err error
		if classes, err = rewriteClassesFor(fsys); err != nil {
			return nil, err
		}
	}
	var remaining []string
	err := walkRewrite(fsys, root, classes, func(t rewriteTarget) error {
		n, err := rewriteFileContent(fsys, &t, q)
		if err != nil {
			return err
		}
		if n > 0 {
			remaining = append(remaining, t.rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return remaining, nil
}

// rewriteTarget is one file the walk may change.
type rewriteTarget struct {
	rel  string
	full string
	// orig is the file's bytes as the pass read them; new is what apply writes,
	// and replacements is how many old values new holds.
	orig         []byte
	new          []byte
	replacements int
}

// rewriteClassesFor returns the pass's own class list for the town in fsys, with
// its rigs read from the layout.
func rewriteClassesFor(fsys fs.FS) ([]RewriteClass, error) {
	rigs, err := townRigs(fsys)
	if err != nil {
		return nil, err
	}
	return RewriteClasses(rigs), nil
}

// checkRewriteTown refuses a town root the pass cannot read as a directory.
func checkRewriteTown(root string) error {
	if root == "" {
		return errors.New("rewrite: --town is required")
	}
	if !path.IsAbs(root) {
		return fmt.Errorf("rewrite: --town %q is not an absolute path", root)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("rewrite: reading the town root %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("rewrite: --town %s is not a directory", root)
	}
	return nil
}

// townRigs returns the town's rig names: its top-level directories that the
// split manifest does not name and that are not hidden, since the manifest names
// every other top-level entry.
func townRigs(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("rewrite: reading the town root: %w", err)
	}
	m := Default()
	var rigs []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || m.names(name) {
			continue
		}
		rigs = append(rigs, name)
	}
	return rigs, nil
}

// walkRewrite visits every listed-class regular file in fsys in path order. It
// descends only where a class file or a worktree pointer can be: heavy
// directories are skipped whole, and so is anything below rewriteMaxDepth. The
// kept-copy search is a separate walk with its own skip set (keptCopies).
func walkRewrite(fsys fs.FS, root string, classes []RewriteClass, visit func(rewriteTarget) error) error {
	return fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("rewrite: walking %s: %w", rel, err)
		}
		if rel == "." {
			return nil
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		if d.IsDir() {
			if d.Name() == constants.DirRepoGit {
				if err := visitRepoGitPointers(fsys, rel, root, visit); err != nil {
					return err
				}
				return fs.SkipDir
			}
			if skipRewriteDir(rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !matchesClass(classes, rel) {
			return nil
		}
		return visit(rewriteTarget{rel: rel, full: full})
	})
}

// matchesClass reports whether rel, a town-root-relative slash path, is in one of
// classes.
func matchesClass(classes []RewriteClass, rel string) bool {
	return slices.ContainsFunc(classes, func(c RewriteClass) bool { return c.Match(rel) })
}

// keptCopies returns the slash paths of every file under fsys named
// <file>.pre-rewrite, whatever its type, in path order. It enters every directory
// except the rewriteKeptSkip names, at any depth. fs.WalkDir does not follow a
// symlinked directory, so the search stays inside the town.
func keptCopies(fsys fs.FS) ([]string, error) {
	var kept []string
	err := fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("rewrite: walking %s: %w", rel, err)
		}
		if d.IsDir() {
			if keptSkipped(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, RewriteBackupSuffix) {
			kept = append(kept, rel)
		}
		return nil
	})
	return kept, err
}

// keptSkipped reports whether the kept-copy search leaves the directory name whole.
func keptSkipped(name string) bool {
	for _, p := range rewriteKeptSkip {
		if prefix, wild := strings.CutSuffix(p, "*"); wild {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if name == p {
			return true
		}
	}
	return false
}

// skipRewriteDir reports whether the walk leaves rel whole. The heavy
// directories hold no class file: logs and events are named skipped, Dolt data
// and git object stores are data, and no class names a checkout's own files.
func skipRewriteDir(rel, name string) bool {
	if strings.Count(rel, "/")+1 >= rewriteMaxDepth {
		return true
	}
	if name == ".git" || name == "dolt" || name == "node_modules" || strings.HasPrefix(name, ".dolt-data") {
		return true
	}
	if !strings.Contains(rel, "/") {
		switch rel {
		case "logs", "events", ".runtime", ".dolt-archive":
			return true
		}
	}
	return false
}

// visitRepoGitPointers visits the worktree pointers of the bare repository at
// rel/.repo.git: <rig>/.repo.git/worktrees/<name>/gitdir, whose entry is the
// worktree's .git file. A bare repository without worktrees has none. The rest
// of the object store is never entered.
func visitRepoGitPointers(fsys fs.FS, rel, root string, visit func(rewriteTarget) error) error {
	worktrees := path.Join(rel, "worktrees")
	entries, err := fs.ReadDir(fsys, worktrees)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rewrite: reading %s: %w", worktrees, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entry := path.Join(worktrees, e.Name(), "gitdir")
		info, err := fs.Lstat(fsys, entry)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("rewrite: reading %s: %w", entry, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := visit(rewriteTarget{rel: entry, full: filepath.Join(root, filepath.FromSlash(entry))}); err != nil {
			return err
		}
	}
	return nil
}

// rewriteFileContent reads t from fsys into t.orig and t.new, and returns how
// many old values t.new replaces. A .git file that is not a worktree pointer,
// which starts with "gitdir: ", is left as it is. A file holding a NUL byte is
// refused: a rewrite is text, and one that guesses would corrupt it.
func rewriteFileContent(fsys fs.FS, t *rewriteTarget, q RewriteQuery) (int, error) {
	data, err := fs.ReadFile(fsys, t.rel)
	if err != nil {
		return 0, fmt.Errorf("rewrite: reading %s: %w", t.rel, err)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return 0, fmt.Errorf("rewrite: %s holds a NUL byte; it is not a text file", t.rel)
	}
	if path.Base(t.rel) == ".git" && !strings.HasPrefix(string(data), "gitdir: ") {
		return 0, nil
	}
	t.orig = data
	out, n := rewriteContent(data, q)
	t.new = out
	return n, nil
}

// rewriteContent replaces the query's old values in data and reports how many it
// replaced.
func rewriteContent(data []byte, q RewriteQuery) ([]byte, int) {
	text, n := replaceOldRoot(string(data), q.FromRoot, q.ToRoot)
	if q.FromOwner != "" {
		var owner int
		text, owner = replaceOldOwner(text, q.FromOwner, q.ToOwner)
		n += owner
	}
	return []byte(text), n
}

// replaceOldRoot replaces each old root that sits on a path boundary and
// reports how many it replaced. The byte before it and the byte after it must
// not be part of a file name, so /town never matches inside /township and
// /srv/town matches before a slash, a quote, a line end or the end of the file.
func replaceOldRoot(s, from, to string) (string, int) {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], from) && (i == 0 || pathBoundaryBefore(s[i-1])) && rootBoundaryAfter(s, i+len(from)) {
			b.WriteString(to)
			i += len(from)
			n++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), n
}

// replaceOldOwner replaces each old owner that is the first path component of
// one of the two repository forms and reports how many it replaced:
// forgejo:3000/<owner>/ and "<owner>/. The form must start at a token boundary,
// and the owner must sit between the form's marker and its slash, so a longer
// name that contains the owner, or a host ending in the form's text, is left
// alone.
func replaceOldOwner(s, from, to string) (string, int) {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for i := 0; i < len(s); {
		rest := s[i:]
		if ownerBoundaryBefore(s, i) && strings.HasPrefix(rest, rewriteForgePrefix+from+"/") {
			b.WriteString(rewriteForgePrefix + to + "/")
			i += len(rewriteForgePrefix) + len(from) + 1
			n++
			continue
		}
		if ownerBoundaryBefore(s, i) && strings.HasPrefix(rest, `"`+from+"/") {
			b.WriteString(`"` + to + "/")
			i += len(from) + 2
			n++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), n
}

// ownerBoundaryBefore reports whether a repository form may start at s[i]: the
// byte before it is not part of an account or host name, so myforgejo:3000/ and
// x"owner/ are not forms. The ssh form git@forgejo:3000/ keeps its @.
func ownerBoundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	c := s[i-1]
	return !(c == '_' || c == '-' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))
}

// pathBoundaryBefore reports whether c can precede a whole path: it ends no
// file name. An unknown byte is not a boundary, so a form this list does not
// name is left as it is rather than rewritten on a guess.
func pathBoundaryBefore(c byte) bool {
	switch c {
	case '/', '\\', '=', ':', '"', '\'', '(', '[', '{', ',', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// rootBoundaryAfter reports whether the byte at i ends the path or separates it
// from what follows, as the same list does for the byte before.
func rootBoundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	switch s[i] {
	case '/', '\\', '=', ':', '"', '\'', ')', ']', '}', ',', ';', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// keepAndReplace writes out over full, keeping the original bytes in
// <full>.pre-rewrite first. The new bytes are built beside the file and renamed
// over it, so a reader sees the old file or the new one, never a partial write.
// Both beside-files are created exclusively: a link or file already at either
// name is refused, and never written through.
func keepAndReplace(files rewriteFiles, rel, full string, orig, out []byte) error {
	info, err := files.lstat(full)
	if err != nil {
		return fmt.Errorf("rewrite: reading %s: %w", rel, err)
	}
	perm := info.Mode().Perm()
	backup := full + RewriteBackupSuffix
	if err := writeNewFile(files, backup, orig, perm); err != nil {
		return fmt.Errorf("rewrite: keeping a copy of %s as %s: %w", rel, filepath.Base(backup), err)
	}
	tmp := full + rewriteTempSuffix
	if err := writeNewFile(files, tmp, out, perm); err != nil {
		return fmt.Errorf("rewrite: writing %s: %w", rel, err)
	}
	if err := files.rename(tmp, full); err != nil {
		return removeCreated(files, tmp, fmt.Errorf("rewrite: replacing %s: %w", rel, err))
	}
	return nil
}

// writeNewFile creates p exclusively and writes data to it. A file that already
// exists is refused and left alone; a file this call created that does not hold
// all of data is removed, so no partial copy is left to refuse the next run.
func writeNewFile(files rewriteFiles, p string, data []byte, perm fs.FileMode) error {
	b, err := files.openFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = b.Write(data)
	if closeErr := b.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return removeCreated(files, p, err)
	}
	return nil
}

// removeCreated removes the file a failed write left at p and returns writeErr,
// joined with the removal error when a file stays behind that the operator has
// to delete. Only a file the caller created may be passed here.
func removeCreated(files rewriteFiles, p string, writeErr error) error {
	if err := files.remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w (removing %s also failed: %w)", writeErr, filepath.Base(p), err)
	}
	return writeErr
}

// rewriteListedKeptCopies is how many kept-copy paths keptCopiesError names.
const rewriteListedKeptCopies = 10

// keptCopiesError refuses a run because the town holds kept copies, naming at
// most rewriteListedKeptCopies of them in the order keptCopies found them.
func keptCopiesError(kept []string) error {
	shown, more := kept, ""
	if len(kept) > rewriteListedKeptCopies {
		shown = kept[:rewriteListedKeptCopies]
		more = fmt.Sprintf(" and %d more", len(kept)-rewriteListedKeptCopies)
	}
	return fmt.Errorf("rewrite: a previous rewrite was not undone or cleaned: run --undo or delete them (%d kept copies: %s%s)",
		len(kept), strings.Join(shown, ", "), more)
}

// topConfigMatch matches a town-level directory's JSON and YAML files, one
// segment below it.
func topConfigMatch(dir string) func(string) bool {
	return func(rel string) bool {
		first, rest, ok := strings.Cut(rel, "/")
		return ok && first == dir && !strings.Contains(rest, "/") && hasConfigExt(rest)
	}
}

// hasConfigExt reports whether name carries a configuration file extension.
func hasConfigExt(name string) bool {
	switch path.Ext(name) {
	case ".json", ".yaml", ".yml":
		return true
	}
	return false
}

// isBeadsFile reports whether name is one of the beads files a rewrite changes.
func isBeadsFile(name string) bool {
	return name == "redirect" || name == "routes.jsonl" || name == "metadata.json"
}
