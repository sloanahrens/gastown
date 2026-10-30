package forgejoresync

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// backupStamp is the dated suffix of a lifted rule's backup: the script's
// date +%Y%m%d-%H%M%S.
const backupStamp = "20060102-150405"

// Files is the filesystem the lift writes its backup through, so a test can make
// a step fail and read back what was written.
type Files interface {
	MkdirAll(path string, perm fs.FileMode) error
	WriteFile(path string, data []byte, perm fs.FileMode) error
}

// osFiles is the real filesystem.
type osFiles struct{}

// MkdirAll creates path and its parents.
func (osFiles) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

// WriteFile writes data to path.
func (osFiles) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// Resync is one run of the lift: the options it was given, the Forgejo client
// the rule is read and deleted through, and the seams the clock, the disk and
// the progress log go through. A nil seam is the real one.
type Resync struct {
	Opts   *Options
	Client *forgejoprov.Client
	Files  Files
	Now    func() time.Time
	Log    func(string)
}

// LiftProtection removes the protection rule on MainBranch so the admin push
// the mirror needs is accepted. The rule is written to a dated backup before the
// DELETE, so an operator can restore it by hand: a failed backup sends no
// DELETE. A rule already gone is reported, not an error. With DryRun it logs the
// backup it would write and sends no DELETE.
func (r *Resync) LiftProtection() error {
	repo, branch := r.Opts.Repo, r.Opts.MainBranch
	base := "/repos/" + repo + "/branch_protections"
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
		return fmt.Errorf("GET %s: reply is not a list of rules", base)
	}
	if !ok {
		r.logf("no %s protection on %s: nothing to lift", branch, repo)
		return nil
	}
	backup := r.backupPath()
	if r.Opts.DryRun {
		r.logf("+ write the %s rule to %s", branch, backup)
		r.logf("+ DELETE %s (dry run: not sent)", base+"/"+escapeBranch(branch))
		return nil
	}
	if err := r.saveRule(backup, append(slices.Clone(rule), '\n')); err != nil {
		return err
	}
	r.logf("saved the %s rule to %s", branch, backup)
	path := base + "/" + escapeBranch(branch)
	status, resp, err = r.Client.Do(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		r.logf("lifted %s protection on %s", branch, repo)
	case http.StatusNotFound:
		r.logf("lifted %s protection on %s (already absent)", branch, repo)
	default:
		return r.Client.Unexpected(http.MethodDelete, path, status, resp)
	}
	return nil
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

// backupPath is the dated file the lifted rule is saved to: the script's
// $BACKUP_DIR/OWNER__NAME__BRANCH.json.bak-STAMP, with the branch's slashes
// written as %2F so the name stays one path element.
func (r *Resync) backupPath() string {
	name := fmt.Sprintf("%s__%s.json.bak-%s",
		strings.ReplaceAll(r.Opts.Repo, "/", "__"),
		escapeBranch(r.Opts.MainBranch),
		r.now().Format(backupStamp))
	return filepath.Join(r.Opts.BackupDir, name)
}

// saveRule writes the lifted rule to path, creating the backup directory when
// it is missing. An error here stops the lift before any DELETE.
func (r *Resync) saveRule(path string, data []byte) error {
	if err := r.files().MkdirAll(r.Opts.BackupDir, 0o700); err != nil {
		return fmt.Errorf("create backup dir %s: %w", r.Opts.BackupDir, err)
	}
	if err := r.files().WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write backup %s: %w", path, err)
	}
	return nil
}

// escapeBranch writes the branch's slashes as %2F. The per-rule route takes the
// rule name in the path, and chi routes on the raw path, so a bare "/" would
// address another route (go-gitea#21093).
func escapeBranch(branch string) string {
	return strings.ReplaceAll(branch, "/", "%2F")
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
