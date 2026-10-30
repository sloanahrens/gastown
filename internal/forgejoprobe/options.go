// Package forgejoprobe is the command line of the Forgejo gate probe as a Go
// type: its flags, defaults, validation and the names it derives from them. The
// run that spends them arrives in later beads of the port (gt-tpg0p.85).
package forgejoprobe

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/workspace"
)

// The command's exit codes.
const (
	// ExitOK is a command line that asked for the help text.
	ExitOK = 0
	// ExitFailure is a run that could not start, or a probe that failed.
	ExitFailure = 1
	// ExitUsage is a command line the validation refused.
	ExitUsage = 2
	// ExitHooksNotExercised is a green run whose push did not run the rig's
	// hooks. It writes no record, so a script cannot take the run for the proof.
	ExitHooksNotExercised = 3
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

// Options is one forgejo-probe invocation: the flags as they were given, with
// the environment already resolved into defaults.
type Options struct {
	Rig            string        // <rig>: the rig whose gate is probed
	Repo           string        // --repo: OWNER/NAME of the rig's Forgejo copy
	Owner          string        // the OWNER half of --repo
	Name           string        // the NAME half of --repo
	APIURL         string        // --api-url: API root, one trailing slash trimmed
	WebURL         string        // --web-url: Forgejo web root, the base of the git URL
	AdminTokenFile string        // --admin-token-file: file holding FORGEJO_ADMIN_TOKEN=
	TownRoot       string        // --town-root: town holding <rig>/<rig>/.repo.git
	GitHubURL      string        // --github-url: repository the copy is cloned from
	GitURL         string        // --git-url: Forgejo remote the probe branch is pushed to
	MainBranch     string        // --main-branch: branch the copy is taken from
	Timeout        time.Duration // --timeout: how long to wait for the gate
	PollInterval   time.Duration // --poll-interval: wait between status reads
	DryRun         bool          // --dry-run: read but never write
	ConfigDir      string        // where the green record goes
}

// The defaults a command line that names no value falls back to. The usage text
// reads them from here, so the two cannot drift apart.
const (
	// DefaultAPIURL is the API root when --api-url and $FORGEJO_API_URL name
	// none.
	DefaultAPIURL = "http://127.0.0.1:3000/api/v1"
	// DefaultMainBranch is the branch the copy is taken from.
	DefaultMainBranch = "main"
	// DefaultTimeout is how long a run waits for the gate when --timeout names
	// nothing.
	DefaultTimeout = 1200 * time.Second
	// DefaultPollInterval is the wait between status reads when
	// --poll-interval names nothing.
	DefaultPollInterval = 15 * time.Second
)

// Bare is the rig's own repository, which the probe pushes its branch from:
// <town-root>/<rig>/.repo.git.
func (o *Options) Bare() string {
	return filepath.Join(o.TownRoot, o.Rig, ".repo.git")
}

// Record is the file a green run leaves for the cutover to read:
// <config-dir>/probe-<rig>.record.
func (o *Options) Record() string {
	return recordPath(o.ConfigDir, o.Rig)
}

// Branch is the probe branch pushed to the rig's Forgejo copy.
func (o *Options) Branch() string {
	return "land/probe-" + o.Rig
}

// BranchEncoded is Branch with its slash escaped, the form Forgejo resolves a
// branch name through a URL path (go-gitea#21093).
func (o *Options) BranchEncoded() string {
	return strings.ReplaceAll(o.Branch(), "/", "%2F")
}

// valueFlags maps each flag that reads the next word to where the word goes,
// so a flag can neither take a value nowhere nor take none.
var valueFlags = map[string]func(*Options, string){
	"--api-url":          func(o *Options, v string) { o.APIURL = v },
	"--web-url":          func(o *Options, v string) { o.WebURL = v },
	"--admin-token-file": func(o *Options, v string) { o.AdminTokenFile = v },
	"--repo":             func(o *Options, v string) { o.Repo = v },
	"--town-root":        func(o *Options, v string) { o.TownRoot = v },
	"--github-url":       func(o *Options, v string) { o.GitHubURL = v },
	"--git-url":          func(o *Options, v string) { o.GitURL = v },
	"--main-branch":      func(o *Options, v string) { o.MainBranch = v },
}

// Parse turns one command line into Options, resolving defaults from getenv
// (nil reads the process environment) and refusing a bad value with a
// *UsageError. It returns ErrHelp for -h or --help.
func Parse(args []string, getenv func(string) string) (*Options, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	o := &Options{
		APIURL:         firstNonEmpty(getenv("FORGEJO_API_URL"), DefaultAPIURL),
		WebURL:         getenv("FORGEJO_URL"),
		AdminTokenFile: getenv("FORGEJO_ADMIN_ENV"),
		TownRoot:       defaultTownRoot(getenv),
		MainBranch:     DefaultMainBranch,
		Timeout:        DefaultTimeout,
		PollInterval:   DefaultPollInterval,
		ConfigDir:      defaultConfigDir(getenv),
	}
	// The two waits stay words until validate reads them, so a bad value is
	// refused after the flags that precede it, as the shell kept them.
	timeout, poll, err := o.parseFlags(args)
	if err != nil {
		return nil, err
	}
	if err := o.validate(timeout, poll); err != nil {
		return nil, err
	}
	return o, nil
}

// parseFlags reads the arguments and returns the two waits as digits. A flag
// and its value are separate words, so `--repo=acme/rig` stays an unknown
// option.
func (o *Options) parseFlags(args []string) (timeout, poll string, err error) {
	timeout = strconv.FormatInt(int64(DefaultTimeout/time.Second), 10)
	poll = strconv.FormatInt(int64(DefaultPollInterval/time.Second), 10)
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "-h", "--help":
			return "", "", ErrHelp
		case "--dry-run":
			o.DryRun = true
			continue
		case "--timeout", "--poll-interval":
			if i+1 == len(args) {
				return "", "", usagef("%s needs a value", flag)
			}
			i++
			if flag == "--timeout" {
				timeout = args[i]
			} else {
				poll = args[i]
			}
			continue
		}
		if set, ok := valueFlags[flag]; ok {
			if i+1 == len(args) {
				return "", "", usagef("%s needs a value", flag)
			}
			i++
			set(o, args[i])
			continue
		}
		if strings.HasPrefix(flag, "-") {
			return "", "", usagef("unknown option: %s", flag)
		}
		if o.Rig != "" {
			return "", "", usagef("unexpected argument: %s", flag)
		}
		o.Rig = flag
	}
	return timeout, poll, nil
}

// validate refuses the values the run cannot use, in the order the script
// checked them, so the first bad flag named is the one an operator fixes.
func (o *Options) validate(timeout, poll string) error {
	if o.Rig == "" {
		return usagef("<rig> is required")
	}
	if err := requireRig(o.Rig); err != nil {
		return err
	}
	if o.TownRoot == "" {
		return usagef("--town-root must not be empty")
	}
	if o.Repo == "" {
		return usagef("--repo is required")
	}
	if !validRepo(o.Repo) {
		return usagef("--repo '%s' must be OWNER/NAME", o.Repo)
	}
	if err := requirePlain("--main-branch", o.MainBranch); err != nil {
		return err
	}
	// The branch reaches git argv in a later bead, so a leading dash would read as
	// an option, the same class the URL flags refuse.
	if strings.HasPrefix(o.MainBranch, "-") {
		return usagef("--main-branch '%s' must not start with '-'", o.MainBranch)
	}
	var err error
	if o.Timeout, err = parseSeconds("--timeout", timeout); err != nil {
		return err
	}
	if o.PollInterval, err = parseSeconds("--poll-interval", poll); err != nil {
		return err
	}
	if err := requireURL("--api-url", o.APIURL); err != nil {
		return err
	}
	o.APIURL = trimOneSlash(o.APIURL)
	if o.WebURL == "" {
		o.WebURL = strings.TrimSuffix(o.APIURL, "/api/v1")
	}
	// The web root is trimmed before the git URL is derived from it, so a
	// trailing slash cannot leave a double slash in the remote.
	if err := requireGitURL("--web-url", o.WebURL); err != nil {
		return err
	}
	o.WebURL = trimOneSlash(o.WebURL)
	o.Owner, o.Name, _ = strings.Cut(o.Repo, "/")
	if o.GitHubURL == "" {
		o.GitHubURL = fmt.Sprintf("https://github.com/%s/%s.git", o.Owner, o.Name)
	}
	if o.GitURL == "" {
		o.GitURL = fmt.Sprintf("%s/%s/%s.git", o.WebURL, o.Owner, o.Name)
	}
	for _, u := range []struct{ flag, value string }{
		{"--github-url", o.GitHubURL},
		{"--git-url", o.GitURL},
	} {
		if err := requireGitURL(u.flag, u.value); err != nil {
			return err
		}
	}
	o.GitHubURL = trimOneSlash(o.GitHubURL)
	o.GitURL = trimOneSlash(o.GitURL)
	return requireUnderTownRoot(o.TownRoot, o.Rig)
}

// plainName is what a rig, a branch or a repository part may hold: the
// characters that need no escaping in a URL path or a git refname.
var plainName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// requireRig refuses a rig name that could not name a directory under the town
// root, and the two dot segments that are not names of their own.
func requireRig(rig string) error {
	if !plainName.MatchString(rig) {
		return usagef("<rig> '%s' must match [A-Za-z0-9._-]+", rig)
	}
	if rig == "." || rig == ".." {
		return usagef("<rig> must not be '%s': it names a path segment, not a rig", rig)
	}
	return nil
}

// requirePlain refuses a value that lands in a URL path or a git refname with a
// character outside plainName.
func requirePlain(flag, value string) error {
	if !plainName.MatchString(value) {
		return usagef("%s '%s' must match [A-Za-z0-9._-]+", flag, value)
	}
	return nil
}

// validRepo reports whether repo is OWNER/NAME with both parts plain names.
func validRepo(repo string) bool {
	owner, name, found := strings.Cut(repo, "/")
	return found && repoPart(owner) && repoPart(name)
}

// repoPart reports whether one side of the slash may name a repository: a plain
// name, and not a path segment that walks to another directory.
func repoPart(value string) bool {
	return plainName.MatchString(value) && value != "." && value != ".."
}

// parseSeconds reads a positive whole number of seconds: digits only, base 10
// so 010 is ten, and neither zero nor a value a time.Duration cannot hold.
func parseSeconds(flag, text string) (time.Duration, error) {
	if text == "" || strings.Trim(text, "0123456789") != "" {
		return 0, usagef("%s '%s' must be a positive integer", flag, text)
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		// Digits only was settled above, so the only failure left is a value
		// past the range a uint64 holds.
		return 0, usagef("%s '%s' is too large to wait", flag, text)
	}
	if n == 0 {
		return 0, usagef("%s '%s' must be a positive integer", flag, text)
	}
	if n > math.MaxInt64/uint64(time.Second) {
		return 0, usagef("%s '%s' is too large to wait", flag, text)
	}
	return time.Duration(n) * time.Second, nil
}

// requireURL refuses a value that is not an http:// or https:// URL with a host
// to address: a bare scheme leaves every request built against nothing.
func requireURL(flag, value string) error {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return usagef("%s must be http:// or https:// (got '%s')", flag, value)
	}
	if u, err := url.Parse(value); err != nil || u.Host == "" {
		return usagef("%s must be http:// or https:// (got '%s')", flag, value)
	}
	return nil
}

// requireGitURL is requireURL for the three URLs a later bead hands to git. A
// value starting with a dash is refused first: git would read it as an option
// rather than as the remote it names.
func requireGitURL(flag, value string) error {
	if strings.HasPrefix(value, "-") {
		return usagef("%s '%s' must not start with '-'", flag, value)
	}
	return requireURL(flag, value)
}

// trimOneSlash removes one trailing slash, as the shell's ${var%/} did: a root
// written with two keeps the second, so the removal cannot be mistaken for
// cleanup of the value itself.
func trimOneSlash(value string) string {
	return strings.TrimSuffix(value, "/")
}

// requireUnderTownRoot refuses a derived bare-repository path that leaves the
// town root. requireRig already refuses the rig names that could walk, so this
// is the second of the two checks the path gets before git sees it.
func requireUnderTownRoot(townRoot, rig string) error {
	bare := filepath.Join(townRoot, rig, ".repo.git")
	rel, err := filepath.Rel(townRoot, bare)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return usagef("<rig> '%s' resolves outside the town root", rig)
	}
	return nil
}

// defaultTownRoot is the town holding the rig's repository: $GT_TOWN_ROOT, else
// $HOME/gt. $GT_ROOT is deliberately not read — workspace.TownRootFromEnv is
// the one reader of the town root, and it reads no alias (gt-tpg0p.85).
func defaultTownRoot(getenv func(string) string) string {
	return firstNonEmpty(workspace.TownRootFromEnv(getenv), getenv("HOME")+"/gt")
}

// defaultConfigDir is where the green record goes: $XDG_CONFIG_HOME/gt, else
// $HOME/.config/gt.
func defaultConfigDir(getenv func(string) string) string {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = getenv("HOME") + "/.config"
	}
	return base + "/gt"
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
