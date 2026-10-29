// Package forgejoprov is the command line of scripts/forgejo-provision.sh as a
// Go type: its flags, defaults, validation and role table. The run that spends
// them arrives in later beads of the port, so the shell script stays the
// working implementation until the last one (gt-tpg0p.26).
package forgejoprov

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// The command's exit codes, as the script's die and usage_die settled them.
const (
	// ExitOK is a run that finished.
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

// Options is one forgejo-provision invocation: the flags as the script read
// them, with the environment already resolved into defaults.
type Options struct {
	APIURL         string   // --api-url: API root, trailing slash trimmed
	WebURL         string   // --web-url: web root written to the token files
	AdminTokenFile string   // --admin-token-file: file holding FORGEJO_ADMIN_TOKEN=
	Roles          []string // --role: bot roles to provision (default polecat landing registry)
	BotPrefix      string   // --bot-prefix: username prefix of every bot
	BotEmailDomain string   // --bot-email-domain: domain of a new bot's email
	ConfigDir      string   // --config-dir: where the role token files go
	TokenName      string   // --token-name: name of each minted token
	Repos          []string // --repo: OWNER/NAME repositories to protect
	LandingBot     string   // --landing-bot: login on the merge whitelist
	GateContexts   []string // --gate-context: required commit-status contexts
	MainBranch     string   // --main-branch: protected landing target
	LandBranch     string   // --land-branch: candidate branch pattern kept unprotected
	Rotate         bool     // --rotate: mint a new token where one exists, revoking the old
	DryRun         bool     // --dry-run: read and report without writing
}

// The defaults a command line that names no value falls back to. The usage text
// reads them from here, so the two cannot drift apart.
const (
	// DefaultAPIURL is the API root when --api-url and $FORGEJO_API_URL name
	// none.
	DefaultAPIURL = "http://127.0.0.1:3000/api/v1"
	// DefaultBotPrefix prefixes every bot login.
	DefaultBotPrefix = "bot-"
	// DefaultBotEmailDomain is the domain of a new bot's email.
	DefaultBotEmailDomain = "bots.invalid"
	// DefaultTokenName names each minted token.
	DefaultTokenName = "gt-provision"
	// DefaultMainBranch is the protected landing target.
	DefaultMainBranch = "main"
	// DefaultLandBranch is the candidate branch pattern kept unprotected.
	DefaultLandBranch = "land/*"
)

// roleScopes is one row of the role table: the token scopes that role's bot is
// minted with (gt-fn9e6.15).
type roleScopes struct {
	name   string
	scopes []string
}

// roleTable is the role → scopes mapping, in the order the unknown-role
// message names the roles. A registry bot pushes packages with write:package
// and no repository scope at all, so it cannot touch a rig repo; the landing
// bot reads its own login; the viewer only reads.
func roleTable() []roleScopes {
	return []roleScopes{
		{"polecat", []string{"write:repository"}},
		{"landing", []string{"write:repository", "read:user"}},
		{"registry", []string{"write:package", "read:package"}},
		{"viewer", []string{"read:repository", "read:user"}},
	}
}

// RoleScopes lists the token scopes a role's bot is minted with, and reports
// false for a name no role uses.
func RoleScopes(role string) ([]string, bool) {
	for _, r := range roleTable() {
		if r.name == role {
			return r.scopes, true
		}
	}
	return nil, false
}

// DefaultRoles is the roles a run provisions when --role names none. Each call
// builds its own slice, so a caller cannot move the default through it.
func DefaultRoles() []string { return []string{"polecat", "landing", "registry"} }

// DefaultGateContexts is the commit-status pair the protection rules require
// when --gate-context names none.
func DefaultGateContexts() []string { return []string{"ci / gate (push)", "om / review"} }

// valueFlags maps each flag that reads the next word to where the word goes,
// so a flag can neither take a value nowhere nor take none. A flag absent here
// takes no value: either a switch in parseFlags or an unknown option.
var valueFlags = map[string]func(*Options, string){
	"--api-url":          func(o *Options, v string) { o.APIURL = v },
	"--web-url":          func(o *Options, v string) { o.WebURL = v },
	"--admin-token-file": func(o *Options, v string) { o.AdminTokenFile = v },
	"--role":             func(o *Options, v string) { o.Roles = append(o.Roles, v) },
	"--bot-prefix":       func(o *Options, v string) { o.BotPrefix = v },
	"--bot-email-domain": func(o *Options, v string) { o.BotEmailDomain = v },
	"--config-dir":       func(o *Options, v string) { o.ConfigDir = v },
	"--token-name":       func(o *Options, v string) { o.TokenName = v },
	"--repo":             func(o *Options, v string) { o.Repos = append(o.Repos, v) },
	"--landing-bot":      func(o *Options, v string) { o.LandingBot = v },
	"--gate-context":     func(o *Options, v string) { o.GateContexts = append(o.GateContexts, v) },
	"--main-branch":      func(o *Options, v string) { o.MainBranch = v },
	"--land-branch":      func(o *Options, v string) { o.LandBranch = v },
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
		ConfigDir:      defaultConfigDir(getenv),
		BotPrefix:      DefaultBotPrefix,
		BotEmailDomain: DefaultBotEmailDomain,
		TokenName:      DefaultTokenName,
		MainBranch:     DefaultMainBranch,
		LandBranch:     DefaultLandBranch,
	}
	if err := o.parseFlags(args); err != nil {
		return nil, err
	}
	o.applyDefaults()
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// parseFlags reads the arguments the script's while loop read. A flag and its
// value are separate words, so `--role=polecat` stays an unknown option, as
// the shell case statement made it.
func (o *Options) parseFlags(args []string) error {
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "-h", "--help":
			return ErrHelp
		case "--rotate":
			o.Rotate = true
			continue
		case "--dry-run":
			o.DryRun = true
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

// applyDefaults fills the values no flag named: the script's ROLES,
// GATE_CONTEXTS and LANDING_BOT assignments after its parse loop.
func (o *Options) applyDefaults() {
	if len(o.Roles) == 0 {
		o.Roles = DefaultRoles()
	}
	if len(o.GateContexts) == 0 {
		o.GateContexts = DefaultGateContexts()
	}
	// The landing bot is the one login the protection rules name, so it is the
	// landing role's bot unless the operator names another account.
	if o.LandingBot == "" {
		o.LandingBot = o.BotPrefix + "landing"
	}
}

// validate refuses the values the run cannot use, in the order the script
// checked them, so the first bad flag named is the one an operator fixes.
func (o *Options) validate() error {
	if !validAPIURL(o.APIURL) {
		return usagef("--api-url must be http:// or https:// (got '%s')", o.APIURL)
	}
	o.APIURL = strings.TrimSuffix(o.APIURL, "/")
	if o.WebURL == "" {
		o.WebURL = strings.TrimSuffix(o.APIURL, "/api/v1")
	}
	for _, role := range o.Roles {
		if err := requirePlain("--role", role); err != nil {
			return err
		}
		if _, ok := RoleScopes(role); !ok {
			return usagef("unknown role '%s' (known roles: %s)", role, KnownRoles())
		}
	}
	for _, name := range []struct{ flag, value string }{
		{"--bot-prefix", o.BotPrefix},
		{"--token-name", o.TokenName},
		{"--landing-bot", o.LandingBot},
	} {
		if err := requirePlain(name.flag, name.value); err != nil {
			return err
		}
	}
	for _, text := range []struct{ flag, value string }{
		{"--bot-email-domain", o.BotEmailDomain},
		{"--main-branch", o.MainBranch},
		{"--land-branch", o.LandBranch},
	} {
		if err := requireJSONSafe(text.flag, text.value); err != nil {
			return err
		}
	}
	for _, ctx := range o.GateContexts {
		if err := requireJSONSafe("--gate-context", ctx); err != nil {
			return err
		}
	}
	for _, repo := range o.Repos {
		if !validRepo(repo) {
			return usagef("--repo '%s' must be OWNER/NAME, each part matching [A-Za-z0-9._-]+ and neither '.' nor '..'", repo)
		}
	}
	return nil
}

// validAPIURL reports whether value is the API root: the scheme exactly as the
// script spelled it, and a host after it. A bare scheme would leave a root with
// nothing to address, and every request would be built against nothing.
func validAPIURL(value string) bool {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && u.Host != ""
}

// KnownRoles names every role in the table, space-separated: the message a
// mistyped --role gets, and the list the usage text shows.
func KnownRoles() string {
	table := roleTable()
	names := make([]string, 0, len(table))
	for _, r := range table {
		names = append(names, r.name)
	}
	return strings.Join(names, " ")
}

// plainName is what a Forgejo login, a role or a token name may hold: the
// characters that need no escaping in a URL path or a JSON string.
var plainName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// requirePlain refuses a value that lands in a URL path or a JSON string with a
// character outside plainName.
func requirePlain(flag, value string) error {
	if !plainName.MatchString(value) {
		return usagef("%s '%s' must match [A-Za-z0-9._-]+", flag, value)
	}
	return nil
}

// requireJSONSafe refuses a value that would need escaping inside the JSON this
// tool writes, which escapes nothing. The shell could name only the newline it
// hit; every control character breaks a JSON string the same way, so a tab or a
// carriage return is refused too.
func requireJSONSafe(flag, value string) error {
	if !jsonSafe(value) {
		return usagef("%s '%s' may not be empty or hold a control character, quote or backslash", flag, value)
	}
	return nil
}

// jsonSafe reports whether value fits in a JSON string unescaped: non-empty,
// with no control character, quote or backslash.
func jsonSafe(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < ' ' || r == 0x7f || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// validRepo reports whether repo is OWNER/NAME with both parts plain names.
// The value is pasted into a URL path, so a part that walks (or carries a
// character the path reads as a delimiter) is refused here rather than being
// sent to the server.
func validRepo(repo string) bool {
	owner, name, found := strings.Cut(repo, "/")
	return found && repoPart(owner) && repoPart(name)
}

// repoPart reports whether one side of the slash may name a repository: a plain
// name, and not a path segment that walks to another directory.
func repoPart(value string) bool {
	return plainName.MatchString(value) && value != "." && value != ".."
}

// defaultConfigDir is where the role token files go: $XDG_CONFIG_HOME/gt, else
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
