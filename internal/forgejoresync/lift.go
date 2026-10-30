package forgejoresync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// backupStamp is the layout of the dated suffix on a lifted rule's backup: year,
// month, day, then hour, minute and second.
const backupStamp = "20060102-150405"

// maxBackups bounds how many names one lift tries for its backup. A dated name that is
// already taken gets a counter suffix; a run that finds no free name stops before the
// DELETE.
const maxBackups = 100

// Files is the filesystem the lift writes its backup through, so a test can make
// a step fail and read back what was written.
type Files interface {
	MkdirAll(path string, perm fs.FileMode) error
	// CreateNew writes data to path only if nothing exists there. A taken path fails
	// with an error matching fs.ErrExist, so a backup is never overwritten.
	CreateNew(path string, data []byte, perm fs.FileMode) error
}

// osFiles is the real filesystem.
type osFiles struct{}

// MkdirAll creates path and its parents.
func (osFiles) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

// CreateNew writes data to a new file at path. O_EXCL makes the create fail when the
// file exists, so two lifts can never write over each other's backup.
func (osFiles) CreateNew(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	return errors.Join(werr, f.Close())
}

// Resync is one run of the lift: the options it was given, the Forgejo client
// the rule is read and deleted through, and the seams the clock, the disk and
// the progress log go through. A nil seam is the real one.
type Resync struct {
	Opts *Options
	// Client is the Forgejo API the rule is read and deleted through; nil makes Run build
	// one from the admin token, through HTTP.
	Client *forgejoprov.Client
	// HTTP sends the requests of the client Run builds; nil means the client's default.
	HTTP  *http.Client
	Files Files
	Now   func() time.Time
	Log   func(string)
	// Getenv and ReadFile resolve the admin token; nil means the process's own.
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	// Steps are the run's ordered stages; nil runs the production stages. Provision re-runs
	// the provisioner; nil runs forgejoprov.Run.
	Steps     []Step
	Provision func(context.Context, *forgejoprov.Options, forgejoprov.Env) int
	// RestoreTimeout bounds the protection restore; zero means the default.
	RestoreTimeout time.Duration
	// Git runs the git calls the mirror makes; nil runs the git binary.
	Git Git
	// Context is the run's context: a signal cancels it in the command, and the
	// git calls derive theirs from it. nil means context.Background().
	Context context.Context
	// MkdirTemp and RemoveAll make and remove the scratch directory the mirror is built in;
	// nil means os.MkdirTemp and os.RemoveAll.
	MkdirTemp func(dir, pattern string) (string, error)
	RemoveAll func(path string) error
	// work is the scratch directory the fetch stage made; Run removes it on every path.
	work string
}

// LiftProtection removes the protection rule on MainBranch so the admin push
// the mirror needs is accepted. The rule is written to a dated backup before the
// DELETE, so an operator can restore it by hand: a failed backup sends no
// DELETE. A rule already gone is reported, not an error. With DryRun it logs the
// backup it would write and sends no DELETE.
func (r *Resync) LiftProtection() error {
	repo, branch := r.Opts.Repo, r.Opts.MainBranch
	base := "/repos/" + routeRepo(repo) + "/branch_protections"
	// The rule name is a branch glob and the per-rule route takes it in the path:
	// chi routes on the raw path, so a "/" must arrive as %2F (go-gitea#21093).
	rulePath := base + "/" + url.PathEscape(branch)
	status, resp, err := r.Client.Do(http.MethodGet, base, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("GET %s returned 404: the repository is not on %s, or the admin token cannot see it%s", base, r.Client.API, r.Client.BodyTail(resp))
	default:
		return r.Client.Unexpected(http.MethodGet, base, status, resp)
	}
	// A reply that is not a list of rules is an error, so no rule is judged
	// absent on a guess.
	var rules []json.RawMessage
	if err := json.Unmarshal(resp, &rules); err != nil || rules == nil {
		return fmt.Errorf("GET %s: reply is not a list of rules", base)
	}
	rule, ok, err := findRawRule(rules, branch)
	if err != nil {
		return fmt.Errorf("GET %s: reply is not a list of rules: %w", base, err)
	}
	if !ok {
		r.logf("no %s protection on %s: nothing to lift", branch, repo)
		return nil
	}
	if r.Opts.DryRun {
		r.logf("+ write the %s rule to %s", branch, r.backupPath())
		r.logf("+ DELETE %s (dry run: not sent)", rulePath)
		return nil
	}
	backup, err := r.saveRule(append(slices.Clone(rule), '\n'))
	if err != nil {
		return err
	}
	r.logf("saved the %s rule to %s", branch, backup)
	status, resp, err = r.Client.Do(http.MethodDelete, rulePath, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		r.logf("lifted %s protection on %s", branch, repo)
	case http.StatusNotFound:
		r.logf("lifted %s protection on %s (already absent)", branch, repo)
	default:
		return r.Client.Unexpected(http.MethodDelete, rulePath, status, resp)
	}
	return nil
}

// routeRepo escapes OWNER and NAME as the two segments of a repository route.
func routeRepo(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// findRawRule returns the reply's bytes for the rule named name, and false when no rule
// has that name. The bytes are kept as Forgejo sent them, so the backup is the rule as
// it stood. A rule that is not a JSON object is an error, so a reply the run cannot
// read is never taken for a set of rules without the one it looks for.
func findRawRule(rules []json.RawMessage, name string) (json.RawMessage, bool, error) {
	for _, raw := range rules {
		var head struct {
			RuleName string `json:"rule_name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, false, err
		}
		if head.RuleName == name {
			return raw, true, nil
		}
	}
	return nil, false, nil
}

// backupPath is the file the lifted rule is saved to first: BackupDir/OWNER__NAME__BRANCH
// with the branch's slashes written as %2F so the name stays one path element, and the
// lift's start time as the suffix.
func (r *Resync) backupPath() string {
	name := fmt.Sprintf("%s__%s.json.bak-%s",
		strings.ReplaceAll(r.Opts.Repo, "/", "__"),
		strings.ReplaceAll(r.Opts.MainBranch, "/", "%2F"),
		r.now().Format(backupStamp))
	return filepath.Join(r.Opts.BackupDir, name)
}

// saveRule writes the lifted rule to a backup that did not exist before, creating the
// backup directory when it is missing, and returns the path it wrote. The first name is
// the dated one; a name already taken gets a counter (.1, .2, ...), so a second lift in
// the same second keeps both copies. An error here stops the lift before any DELETE.
func (r *Resync) saveRule(data []byte) (string, error) {
	if err := r.files().MkdirAll(r.Opts.BackupDir, 0o700); err != nil {
		return "", fmt.Errorf("create backup dir %s: %w", r.Opts.BackupDir, err)
	}
	first := r.backupPath()
	for i := range maxBackups {
		path := first
		if i > 0 {
			path = fmt.Sprintf("%s.%d", first, i)
		}
		err := r.files().CreateNew(path, data, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("write backup %s: %w", path, err)
		}
		return path, nil
	}
	return "", fmt.Errorf("no free backup name for %s in %d tries", first, maxBackups)
}

// files returns the filesystem the backup goes through.
func (r *Resync) files() Files {
	if r.Files != nil {
		return r.Files
	}
	return osFiles{}
}

// now returns the time the backup's stamp is taken from.
func (r *Resync) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// logf sends one progress line to Log, when it has one.
func (r *Resync) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(fmt.Sprintf(format, args...))
	}
}
