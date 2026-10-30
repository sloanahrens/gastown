// Package forgejoresync is the command line of scripts/forgejo-resync.sh as a
// Go type: its flags, defaults and validation. The run that spends them arrives
// in a later bead, and the shell script stays in place, and green, until the
// port's last bead deletes it.
package forgejoresync

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// The command's exit codes, as the script's die and usage_die settled them.
const (
	// ExitOK is a run that finished, and the help request.
	ExitOK = 0
	// ExitFailure is a run that started and failed.
	ExitFailure = 1
	// ExitUsage is a command line the validation refused.
	ExitUsage = 2
)

// ErrHelp reports -h or --help: the caller prints the usage text and exits
// ExitOK.
var ErrHelp = errors.New("help requested")

// UsageError is a command line the validation refused: the caller prints the
// message, then the usage hint, and exits ExitUsage.
type UsageError struct {
	Message string
}

func (e *UsageError) Error() string { return e.Message }

// Options is one forgejo-resync invocation: the repository and the flags as the
// script read them, with the environment already resolved into defaults.
type Options struct {
	Repo           string // OWNER/NAME: the repository whose refs are resynced
	APIURL         string // --api-url: API root, trailing slash trimmed
	WebURL         string // --web-url: web root the Forgejo git remote is derived from
	GitHubURL      string // --github-url: git remote the refs are imported from
	ForgejoURL     string // --forgejo-url: git remote the refs are pushed to
	AdminTokenFile string // --admin-token-file: file holding FORGEJO_ADMIN_TOKEN=
	MainBranch     string // --main-branch: protected landing target to lift and restore
	BackupDir      string // --backup-dir: where the lifted rule's dated .bak- copy goes
	DryRun         bool   // --dry-run: read and print, never write
}

// The defaults a command line that names no value falls back to. The usage text
// reads them from here, so the two cannot drift apart.
const (
	// DefaultAPIURL is the API root when --api-url and $FORGEJO_API_URL name
	// none.
	DefaultAPIURL = "http://127.0.0.1:3000/api/v1"
	// DefaultMainBranch is the protected landing target.
	DefaultMainBranch = "main"
)

// mainBranchPattern is what --main-branch may hold. The branch name lands in a
// URL path and on git's command line, so a name outside this class is refused.
var mainBranchPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// repoPattern is what each side of OWNER/NAME may hold. Both sides land unescaped
// in URL and API paths, so a space, ?, # or % would address another resource.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// valueFlags maps each flag that reads the next word to where the word goes, so
// a flag can neither take a value nowhere nor take none. A flag absent here
// takes no value: either a switch in parseFlags or an unknown option.
var valueFlags = map[string]func(*Options, string){
	"--api-url":          func(o *Options, v string) { o.APIURL = v },
	"--web-url":          func(o *Options, v string) { o.WebURL = v },
	"--github-url":       func(o *Options, v string) { o.GitHubURL = v },
	"--forgejo-url":      func(o *Options, v string) { o.ForgejoURL = v },
	"--admin-token-file": func(o *Options, v string) { o.AdminTokenFile = v },
	"--main-branch":      func(o *Options, v string) { o.MainBranch = v },
	"--backup-dir":       func(o *Options, v string) { o.BackupDir = v },
}

// Parse turns one command line into Options, resolving defaults from getenv
// (nil reads the process environment) and refusing a bad value with a
// *UsageError. It returns ErrHelp for -h or --help, before any later argument
// is read.
func Parse(args []string, getenv func(string) string) (*Options, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	o := &Options{
		APIURL:         firstNonEmpty(getenv("FORGEJO_API_URL"), DefaultAPIURL),
		WebURL:         getenv("FORGEJO_URL"),
		AdminTokenFile: getenv("FORGEJO_ADMIN_ENV"),
		BackupDir:      firstNonEmpty(getenv("FORGEJO_BACKUP_DIR"), homeBackupDir(getenv("HOME"))),
		MainBranch:     DefaultMainBranch,
	}
	if err := o.parseFlags(args); err != nil {
		return nil, err
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	o.applyDefaults()
	return o, nil
}

// parseFlags reads the arguments the script's while loop read. A flag and its
// value are separate words, so `--main-branch=main` stays an unknown option, as
// the shell case statement made it.
func (o *Options) parseFlags(args []string) error {
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "-h", "--help":
			return ErrHelp
		case "--dry-run":
			o.DryRun = true
			continue
		}
		if !strings.HasPrefix(flag, "-") {
			if o.Repo != "" {
				return usagef("one repository only (already have '%s')", o.Repo)
			}
			o.Repo = flag
			continue
		}
		set, ok := valueFlags[flag]
		if !ok {
			return usagef("unknown option: %s", flag)
		}
		if i+1 == len(args) {
			return usagef("%s needs a value", flag)
		}
		i++
		set(o, args[i])
	}
	return nil
}

// validate refuses the values the run cannot use, in the order the script
// checked them, so the first bad argument named is the one an operator fixes.
func (o *Options) validate() error {
	if o.Repo == "" {
		return usagef("OWNER/NAME is required")
	}
	if !validRepo(o.Repo) {
		return usagef("'%s' must be OWNER/NAME", o.Repo)
	}
	if !isHTTPURL(o.APIURL) {
		return usagef("--api-url must be http:// or https:// (got '%s')", o.APIURL)
	}
	o.APIURL = strings.TrimSuffix(o.APIURL, "/")
	if !mainBranchPattern.MatchString(o.MainBranch) {
		return usagef("--main-branch '%s' must match [A-Za-z0-9._/-]+", o.MainBranch)
	}
	// An empty WebURL is derived from the validated API root, so only a value
	// the operator named can fail here.
	if o.WebURL != "" && !isHTTPURL(o.WebURL) {
		return usagef("the web root (--web-url or FORGEJO_URL) must be http:// or https:// (got '%s')", o.WebURL)
	}
	if o.BackupDir == "" {
		return usagef("--backup-dir is required: HOME is unset and FORGEJO_BACKUP_DIR is empty")
	}
	return nil
}

// applyDefaults fills the values no flag named. The git remotes depend on the
// repository and the web root, so they are derived once both are settled.
func (o *Options) applyDefaults() {
	if o.WebURL == "" {
		o.WebURL = strings.TrimSuffix(o.APIURL, "/api/v1")
	}
	if o.GitHubURL == "" {
		o.GitHubURL = "https://github.com/" + o.Repo + ".git"
	}
	if o.ForgejoURL == "" {
		o.ForgejoURL = strings.TrimSuffix(o.WebURL, "/") + "/" + o.Repo + ".git"
	}
}

// validRepo reports whether repo is OWNER/NAME over repoPattern, with neither
// side "." or "..": those would climb out of /repos/ in the API path.
func validRepo(repo string) bool {
	if !repoPattern.MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// isHTTPURL reports whether u names an http:// or https:// root.
func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// homeBackupDir is the backup directory under HOME, or "" when HOME is unset:
// joining an empty HOME would name the root-level /forgejo/backups.
func homeBackupDir(home string) string {
	if home == "" {
		return ""
	}
	return home + "/forgejo/backups"
}

// firstNonEmpty is the first argument that is not the empty string, else "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// usagef builds the *UsageError a refused command line gets.
func usagef(format string, args ...any) error {
	return &UsageError{Message: fmt.Sprintf(format, args...)}
}
