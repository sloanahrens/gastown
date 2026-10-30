// Package forgejocut is scripts/forgejo-cutover.sh as Go: the command line here,
// and the derived values the run reads from it. The run itself arrives in later
// beads; the shell script stays in place until the port's deletion bead removes
// it.
package forgejocut

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// The command's exit codes, as the script's die and usage_die settled them.
const (
	// ExitOK is a run that finished, and the help request.
	ExitOK = 0
	// ExitFailure is a run that started and failed, or a rig whose files are missing.
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

// The defaults a command line that names no value falls back to. The usage text
// reads them from here, so the two cannot drift apart.
const (
	// DefaultAPIURL is the API root when --api-url and $FORGEJO_API_URL name none.
	DefaultAPIURL = "http://127.0.0.1:3000/api/v1"
	// DefaultMainBranch is the protected landing target.
	DefaultMainBranch = "main"
	// DefaultGateWorkflow is the gate workflow file basename.
	DefaultGateWorkflow = "gate"
	// DefaultBotPrefix is the bot login prefix; each role gets one bot.
	DefaultBotPrefix = "bot-"
	// DefaultProbe is the probe run before the cutover, relative to the repository
	// the binary sits in. defaultProbe resolves it.
	DefaultProbe = "scripts/forgejo-probe.sh"
	// DefaultGT is the gt binary the daemon restart runs. The run resolves the name
	// with exec.LookPath; no environment variable names another binary.
	DefaultGT = "gt"
)

// botRoles are the bot roles the rig's merge_queue.forgejo block names, as
// config.ForgejoConfig reads them.
var botRoles = []string{"polecat", "landing", "registry"}

// plainPattern is what <rig>, --gate-workflow and --bot-prefix may hold: a name
// that lands in a path under the town root or in a workflow file name.
var plainPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// mainBranchPattern is what --main-branch may hold. The branch name lands in a
// URL path and on git's command line, so a name outside this class is refused.
// The class also keeps out the characters git's ref rules forbid (space, ~ ^ : ?
// * [ \, control bytes, and the { of @{), so branchNameProblem only adds the
// structural rules.
var mainBranchPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// repoPattern is what each side of OWNER/NAME may hold. Both sides land unescaped
// in URL and API paths, so a space, ?, # or % would address another resource.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// Options is one forgejo-cutover invocation: the rig, the flags as the script read
// them, and the values the script derived from them and from the rig's files.
type Options struct {
	Rig            string // <rig>: the registered rig being cut over
	TownRoot       string // --town-root, else GT_TOWN_ROOT, else the town the working directory is in, else $HOME/gt
	RigRoot        string // TownRoot/Rig: the rig's directory
	BareRepo       string // RigRoot/.repo.git: the rig's bare repository
	Repo           string // --repo OWNER/NAME: the rig's Forgejo repository
	GitHubURL      string // --github-url, else the bare repository's origin URL: the remote cut over from
	GitHubSlug     string // OWNER/NAME the GitHub URL names
	ForgejoURL     string // --forgejo-url, else AdminGitURL: the Forgejo git URL written to the rig
	AdminGitURL    string // WebURL/OWNER/NAME.git: the base the script's own git work rides
	PromoteTarget  string // the ssh form of GitHubURL, which the promotion pushes to
	PromoteKey     string // CONFIG_DIR/gt/promote-RIG.key: the promote key's private half
	AcceptDiverged bool   // --accept-diverged: skip a diverged ref instead of stopping the run
	MainBranch     string // --main-branch: protected landing target
	GateWorkflow   string // --gate-workflow: gate workflow file basename
	BotPrefix      string // --bot-prefix: bot login prefix
	APIURL         string // --api-url: API root, trailing slash trimmed
	WebURL         string // --web-url, else APIURL less /api/v1: the web root
	AdminTokenFile string // --admin-token-file: file holding FORGEJO_ADMIN_TOKEN=
	Probe          string // --probe: probe run before the cutover
	GT             string // --gt: gt binary for the daemon restart
	DryRun         bool   // --dry-run: read and print, never write
}

// BotLogins returns the login of each bot the rig names: the bot prefix plus the
// role, one bot per role.
func (o *Options) BotLogins() []string {
	logins := make([]string, 0, len(botRoles))
	for _, role := range botRoles {
		logins = append(logins, o.BotPrefix+role)
	}
	return logins
}

// Git is the git read the derived values need: the origin URL of the rig's
// bare repository. The command reads it through git; a test injects a fake.
type Git interface {
	OriginURL(bareRepo string) (string, error)
}

// Env is the world a command line is parsed against: the environment, the
// filesystem and git, each a seam so a test runs the parse without a real town.
type Env struct {
	Getenv func(string) string
	IsDir  func(path string) bool
	IsFile func(path string) bool
	// Town is the town root the working directory is in, "" when there is none. It
	// is read only when neither --town-root nor GT_TOWN_ROOT names one.
	Town func() string
	// Executable is the path of the running binary. The probe default is found
	// beside it.
	Executable func() (string, error)
	Git        Git
}

// valueFlags maps each flag that reads the next word to where the word goes, so
// a flag can neither take a value nowhere nor take none. A flag absent here takes
// no value: --town-root and the switches are handled in parseFlags, and anything
// else is an unknown option. --mirror, --mirror-target and --provision are not
// here: the dropped push mirror and the in-process provisioner have no flag.
var valueFlags = map[string]func(*Options, string){
	"--repo":             func(o *Options, v string) { o.Repo = v },
	"--github-url":       func(o *Options, v string) { o.GitHubURL = v },
	"--forgejo-url":      func(o *Options, v string) { o.ForgejoURL = v },
	"--main-branch":      func(o *Options, v string) { o.MainBranch = v },
	"--gate-workflow":    func(o *Options, v string) { o.GateWorkflow = v },
	"--bot-prefix":       func(o *Options, v string) { o.BotPrefix = v },
	"--api-url":          func(o *Options, v string) { o.APIURL = v },
	"--web-url":          func(o *Options, v string) { o.WebURL = v },
	"--admin-token-file": func(o *Options, v string) { o.AdminTokenFile = v },
	"--probe":            func(o *Options, v string) { o.Probe = v },
	"--gt":               func(o *Options, v string) { o.GT = v },
}

// Parse turns one command line into Options, resolving the defaults through env
// and refusing a bad value with a *UsageError. A rig whose files are missing is a
// plain error: the script died there, which is a failed run, not a refused
// command line. It returns ErrHelp for -h or --help, before any later argument is
// read.
func Parse(args []string, env Env) (*Options, error) {
	o := &Options{
		APIURL:         firstNonEmpty(env.Getenv("FORGEJO_API_URL"), DefaultAPIURL),
		WebURL:         env.Getenv("FORGEJO_URL"),
		AdminTokenFile: env.Getenv("FORGEJO_ADMIN_ENV"),
		MainBranch:     DefaultMainBranch,
		GateWorkflow:   DefaultGateWorkflow,
		BotPrefix:      DefaultBotPrefix,
		Probe:          defaultProbe(env),
		GT:             DefaultGT,
	}
	town, err := o.parseFlags(args)
	if err != nil {
		return nil, err
	}
	if err := o.checkValues(); err != nil {
		return nil, err
	}
	if err := o.resolveTown(town, env); err != nil {
		return nil, err
	}
	if err := o.checkRig(env); err != nil {
		return nil, err
	}
	if err := o.derive(env); err != nil {
		return nil, err
	}
	return o, nil
}

// parseFlags reads the arguments the script's while loop read. It returns the
// --town-root value when one was named, nil otherwise, so an empty value is still
// a value the directory check refuses. A flag and its value are separate words,
// so `--main-branch=main` stays an unknown option, as the shell case statement
// made it.
func (o *Options) parseFlags(args []string) (town *string, err error) {
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "-h", "--help":
			return nil, ErrHelp
		case "--dry-run":
			o.DryRun = true
			continue
		case "--accept-diverged":
			o.AcceptDiverged = true
			continue
		}
		if !strings.HasPrefix(flag, "-") {
			if o.Rig != "" {
				return nil, usagef("one rig only (already have '%s')", o.Rig)
			}
			o.Rig = flag
			continue
		}
		if i+1 == len(args) {
			if flag == "--town-root" || valueFlags[flag] != nil {
				return nil, usagef("%s needs a value", flag)
			}
			return nil, usagef("unknown option: %s", flag)
		}
		if flag == "--town-root" {
			v := args[i+1]
			town = &v
			i++
			continue
		}
		set, ok := valueFlags[flag]
		if !ok {
			return nil, usagef("unknown option: %s", flag)
		}
		i++
		set(o, args[i])
	}
	return town, nil
}

// checkValues refuses the values the run cannot use, in the order the script
// checked them, so the first bad argument named is the one an operator fixes. It
// also settles the API and web roots, which the later steps read.
func (o *Options) checkValues() error {
	if o.Rig == "" {
		return usagef("<rig> is required")
	}
	if !plainPattern.MatchString(o.Rig) || o.Rig == "." || o.Rig == ".." {
		return usagef("<rig> '%s' must match [A-Za-z0-9._-]+", o.Rig)
	}
	if o.Repo == "" {
		return usagef("--repo is required")
	}
	if !validRepo(o.Repo) {
		return usagef("--repo '%s' must be OWNER/NAME", o.Repo)
	}
	if !mainBranchPattern.MatchString(o.MainBranch) {
		return usagef("--main-branch '%s' must match [A-Za-z0-9._/-]+", o.MainBranch)
	}
	if problem := branchNameProblem(o.MainBranch); problem != "" {
		return usagef("--main-branch '%s' is not a branch name: it %s", o.MainBranch, problem)
	}
	if !plainPattern.MatchString(o.GateWorkflow) {
		return usagef("--gate-workflow '%s' must match [A-Za-z0-9._-]+", o.GateWorkflow)
	}
	if !plainPattern.MatchString(o.BotPrefix) {
		return usagef("--bot-prefix '%s' must match [A-Za-z0-9._-]+", o.BotPrefix)
	}
	if !isHTTPURL(o.APIURL) {
		return usagef("--api-url must be http:// or https:// (got '%s')", redactURL(o.APIURL))
	}
	// A credential in the URL would ride every request and show in the process list.
	if u, err := url.Parse(o.APIURL); err != nil || u.User != nil {
		return usagef("--api-url must be a URL with no credentials")
	}
	o.APIURL = strings.TrimSuffix(o.APIURL, "/")
	// An empty web root is derived from the validated API root.
	if o.WebURL == "" {
		o.WebURL = strings.TrimSuffix(o.APIURL, "/api/v1")
	}
	return nil
}

// resolveTown settles the town root and checks the files a parse can see: the
// town directory and the probe. The town root is the --town-root flag, then
// GT_TOWN_ROOT, then the town the working directory is in, then $HOME/gt. GT_ROOT
// and GT_BIN are not read.
func (o *Options) resolveTown(town *string, env Env) error {
	switch {
	case town != nil:
		o.TownRoot = *town
	case env.Getenv("GT_TOWN_ROOT") != "":
		o.TownRoot = env.Getenv("GT_TOWN_ROOT")
	case env.Town() != "":
		o.TownRoot = env.Town()
	default:
		o.TownRoot = homeTown(env.Getenv("HOME"))
	}
	if !env.IsDir(o.TownRoot) {
		return usagef("--town-root '%s' is not a directory", o.TownRoot)
	}
	if !env.IsFile(o.Probe) {
		return usagef("--probe '%s' is not a file", o.Probe)
	}
	return nil
}

// checkRig refuses a rig whose directory, bare repository or town marker is
// missing. The script died on each, a failed run rather than a refused command
// line, so these are plain errors.
func (o *Options) checkRig(env Env) error {
	o.RigRoot = filepath.Join(o.TownRoot, o.Rig)
	if !env.IsDir(o.RigRoot) {
		return fmt.Errorf("no rig directory %s (is '%s' a registered rig?)", o.RigRoot, o.Rig)
	}
	o.BareRepo = filepath.Join(o.RigRoot, ".repo.git")
	if !env.IsDir(o.BareRepo) {
		return fmt.Errorf("no bare repository %s", o.BareRepo)
	}
	if townJSON := filepath.Join(o.TownRoot, "mayor", "town.json"); !env.IsFile(townJSON) {
		return fmt.Errorf("no %s", townJSON)
	}
	return nil
}

// derive fills the values no flag named, and checks the ones the derivation
// produces: the git remotes, the promote target and key, and the GitHub slug.
func (o *Options) derive(env Env) error {
	webBase := strings.TrimSuffix(o.WebURL, "/")
	o.AdminGitURL = webBase + "/" + o.Repo + ".git"
	if o.ForgejoURL == "" {
		o.ForgejoURL = o.AdminGitURL
	}
	if err := requireJSONSafe("--forgejo-url", o.ForgejoURL); err != nil {
		return err
	}

	if o.GitHubURL == "" {
		origin, err := env.Git.OriginURL(o.BareRepo)
		if err != nil || origin == "" {
			return fmt.Errorf("no --github-url, and %s has no origin URL to read one from", o.BareRepo)
		}
		o.GitHubURL = origin
	}
	if err := requireJSONSafe("--github-url", o.GitHubURL); err != nil {
		return err
	}

	// sshForm adds no quote, backslash or newline, so the GitHub URL check above
	// covers the promote target too.
	o.PromoteTarget = sshForm(o.GitHubURL)
	dir, err := configDir(env)
	if err != nil {
		return err
	}
	o.PromoteKey = filepath.Join(dir, "promote-"+o.Rig+".key")
	if err := requireJSONSafe("the promote key path", o.PromoteKey); err != nil {
		return err
	}

	o.GitHubSlug = slug(o.GitHubURL)
	if !strings.Contains(o.GitHubSlug, "/") {
		// The URL is left out of the message: a remote can carry a credential.
		return usagef("--github-url does not name OWNER/NAME")
	}
	return nil
}

// defaultProbe is the probe --probe names when it names none: scripts/forgejo-probe.sh
// in the repository the binary sits in. A build puts the binary at the repository
// root or in bin/, so the binary's directory and its parent are the candidates. The
// shell script found the probe beside itself; a binary has no such file, so when no
// candidate holds one, the path under the binary's directory is named, and the
// refusal shows the operator the path that was looked at.
func defaultProbe(env Env) string {
	exe, err := env.Executable()
	if err != nil {
		return DefaultProbe
	}
	dir := filepath.Dir(exe)
	for _, root := range []string{dir, filepath.Dir(dir)} {
		if probe := filepath.Join(root, DefaultProbe); env.IsFile(probe) {
			return probe
		}
	}
	return filepath.Join(dir, DefaultProbe)
}

// branchNameProblem names the first rule of git check-ref-format --branch the name
// breaks, after the character class has passed it. It returns "" for a name git
// accepts.
func branchNameProblem(name string) string {
	switch {
	case strings.HasPrefix(name, "-"):
		return "starts with '-', which git reads as an option"
	case strings.Contains(name, ".."):
		return "contains '..'"
	case strings.Contains(name, "//"):
		return "contains '//'"
	case strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/"):
		return "starts or ends with '/'"
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock"):
		return "ends with '.' or '.lock'"
	}
	for _, component := range strings.Split(name, "/") {
		if strings.HasPrefix(component, ".") {
			return "has a component that starts with '.'"
		}
	}
	return ""
}

// redactURL is raw as a message may show it: the credentials taken out. A value
// that will not parse and holds an @ may carry one, so it is not shown at all.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "@") {
			return "(unparseable URL)"
		}
		return raw
	}
	u.User = nil
	return u.String()
}

// configDir is the gt config directory the promote key lives in: $XDG_CONFIG_HOME/gt,
// else $HOME/.config/gt, the directory the Forgejo token files use.
func configDir(env Env) (string, error) {
	if xdg := env.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gt"), nil
	}
	home := env.Getenv("HOME")
	if home == "" {
		return "", errors.New("no XDG_CONFIG_HOME or HOME: the promote key has no config directory")
	}
	return filepath.Join(home, ".config", "gt"), nil
}

// sshForm returns the ssh form of an http(s) git URL, and an ssh URL unchanged.
// The promotion pushes with `ssh -i`, so its target has to be a URL ssh reads.
func sshForm(url string) string {
	switch {
	case strings.HasPrefix(url, "git@") && strings.Contains(url[len("git@"):], ":"):
		return url
	case strings.HasPrefix(url, "ssh://"):
		host, path := splitHostPath(afterAt(url[len("ssh://"):]))
		return "git@" + host + ":" + path
	case strings.Contains(url, "://"):
		host, path := splitHostPath(after(url, "://"))
		return "git@" + host + ":" + path
	}
	return url
}

// slug returns the OWNER/NAME a git URL points at, so a one-line gh command can
// name the GitHub repository.
func slug(url string) string {
	var rest string
	switch {
	case strings.HasPrefix(url, "git@") && strings.Contains(url[len("git@"):], ":"):
		rest = url[strings.IndexByte(url, ':')+1:]
	case strings.HasPrefix(url, "ssh://"):
		rest = afterSlash(afterAt(url[len("ssh://"):]))
	case strings.Contains(url, "://"):
		rest = afterSlash(after(url, "://"))
	default:
		rest = url
	}
	return strings.TrimSuffix(rest, ".git")
}

// splitHostPath splits rest at its first slash: the host before it, the path
// after it. A rest with no slash is its own host and path, as the shell's
// parameter expansions read it.
func splitHostPath(rest string) (host, path string) {
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return rest, rest
	}
	return rest[:i], rest[i+1:]
}

// afterAt is rest past its first @, or rest unchanged when it holds none.
func afterAt(rest string) string {
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		return rest[i+1:]
	}
	return rest
}

// afterSlash is rest past its first slash, or rest unchanged when it holds none.
func afterSlash(rest string) string {
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i+1:]
	}
	return rest
}

// after is url past the first sep.
func after(url, sep string) string {
	return url[strings.Index(url, sep)+len(sep):]
}

// validRepo reports whether repo is OWNER/NAME over repoPattern, with neither side
// "." or "..": those would climb out of /repos/ in the API path.
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

// requireJSONSafe refuses a value that would need escaping in a JSON string the
// script does not escape: empty, or holding a newline, quote or backslash. The
// value is left out of the message, since a remote can carry a credential.
func requireJSONSafe(label, value string) error {
	if value == "" || strings.ContainsAny(value, "\n\"\\") {
		return usagef("%s may not be empty or hold a newline, quote or backslash", label)
	}
	return nil
}

// isHTTPURL reports whether u names an http:// or https:// root.
func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// homeTown is the town under HOME, or "" when HOME is unset: joining an empty HOME
// would name the root-level /gt.
func homeTown(home string) string {
	if home == "" {
		return ""
	}
	return home + "/gt"
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
