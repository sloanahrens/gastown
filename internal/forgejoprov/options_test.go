package forgejoprov

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// env turns a map into the getenv seam: a variable the map does not name reads
// empty, so no test sees the host's environment.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// TestParseDefaultsTable pins every default the script's header promises,
// against its literal value so a changed default fails here.
func TestParseDefaultsTable(t *testing.T) {
	t.Parallel()
	// base is the empty environment with no flags: HOME is empty too, so the
	// config directory is the script's "/.config/gt".
	base := Options{
		APIURL:         "http://127.0.0.1:3000/api/v1",
		WebURL:         "http://127.0.0.1:3000",
		BotPrefix:      "bot-",
		BotEmailDomain: "bots.invalid",
		Roles:          []string{"polecat", "landing", "registry"},
		ConfigDir:      "/.config/gt",
		TokenName:      "gt-provision",
		LandingBot:     "bot-landing",
		GateContexts:   []string{"ci / gate (push)", "om / review"},
		MainBranch:     "main",
		LandBranch:     "land/*",
	}
	cases := []struct {
		name   string
		args   []string
		vars   map[string]string
		change func(*Options)
	}{
		{name: "empty environment"},
		{
			name: "FORGEJO_API_URL sets the API root and the web root",
			vars: map[string]string{"FORGEJO_API_URL": "https://forgejo.test/api/v1"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
			},
		},
		{
			name:   "FORGEJO_URL sets the web root alone",
			vars:   map[string]string{"FORGEJO_URL": "https://web.test"},
			change: func(o *Options) { o.WebURL = "https://web.test" },
		},
		{
			name:   "FORGEJO_ADMIN_ENV names the admin token file",
			vars:   map[string]string{"FORGEJO_ADMIN_ENV": "/etc/gt/admin.env"},
			change: func(o *Options) { o.AdminTokenFile = "/etc/gt/admin.env" },
		},
		{
			name:   "XDG_CONFIG_HOME holds the token files",
			vars:   map[string]string{"XDG_CONFIG_HOME": "/xdg"},
			change: func(o *Options) { o.ConfigDir = "/xdg/gt" },
		},
		{
			name:   "HOME is the fallback for the token files",
			vars:   map[string]string{"HOME": "/home/gt"},
			change: func(o *Options) { o.ConfigDir = "/home/gt/.config/gt" },
		},
		{
			name:   "XDG_CONFIG_HOME wins over HOME",
			vars:   map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home/gt"},
			change: func(o *Options) { o.ConfigDir = "/xdg/gt" },
		},
		{
			name: "a trailing slash comes off the API root and its web root",
			args: []string{"--api-url", "https://forgejo.test/api/v1/"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
			},
		},
		{
			name: "an API root without /api/v1 keeps its whole self as the web root",
			args: []string{"--api-url", "http://127.0.0.1:8080/api/v1"},
			change: func(o *Options) {
				o.APIURL = "http://127.0.0.1:8080/api/v1"
				o.WebURL = "http://127.0.0.1:8080"
			},
		},
		{
			name:   "--web-url overrides the derived web root",
			args:   []string{"--web-url", "https://web.test"},
			change: func(o *Options) { o.WebURL = "https://web.test" },
		},
		{
			name:   "--role replaces the default roles",
			args:   []string{"--role", "viewer", "--role", "registry"},
			change: func(o *Options) { o.Roles = []string{"viewer", "registry"} },
		},
		{
			name:   "--gate-context replaces the default contexts",
			args:   []string{"--gate-context", "ci/gate"},
			change: func(o *Options) { o.GateContexts = []string{"ci/gate"} },
		},
		{
			name:   "--bot-prefix moves the landing bot with it",
			args:   []string{"--bot-prefix", "svc-"},
			change: func(o *Options) { o.BotPrefix = "svc-"; o.LandingBot = "svc-landing" },
		},
		{
			name:   "--landing-bot names its own login",
			args:   []string{"--landing-bot", "bot-merge"},
			change: func(o *Options) { o.LandingBot = "bot-merge" },
		},
		{
			name:   "--repo repeats",
			args:   []string{"--repo", "acme/gastown", "--repo", "acme/devops"},
			change: func(o *Options) { o.Repos = []string{"acme/gastown", "acme/devops"} },
		},
		{
			name: "--bot-email-domain, --token-name, --config-dir, --admin-token-file, --main-branch and --land-branch",
			args: []string{
				"--bot-email-domain", "bots.example", "--token-name", "gt-landing",
				"--config-dir", "/tmp/cfg", "--admin-token-file", "/tmp/admin.env",
				"--main-branch", "trunk", "--land-branch", "cand/*",
			},
			change: func(o *Options) {
				o.BotEmailDomain = "bots.example"
				o.TokenName = "gt-landing"
				o.ConfigDir = "/tmp/cfg"
				o.AdminTokenFile = "/tmp/admin.env"
				o.MainBranch = "trunk"
				o.LandBranch = "cand/*"
			},
		},
		{name: "--rotate", args: []string{"--rotate"}, change: func(o *Options) { o.Rotate = true }},
		{name: "--dry-run", args: []string{"--dry-run"}, change: func(o *Options) { o.DryRun = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := base
			if tc.change != nil {
				tc.change(&want)
			}
			got, err := Parse(tc.args, env(tc.vars))
			if err != nil {
				t.Fatalf("Parse(%v) errored: %v", tc.args, err)
			}
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("Parse(%v, %v) = %+v, want %+v", tc.args, tc.vars, *got, want)
			}
		})
	}
}

// TestParseRejectsBadValues holds one case per check the validation makes, so
// dropping a check leaves a case that no longer refuses its value.
func TestParseRejectsBadValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"api-url without a scheme", []string{"--api-url", "127.0.0.1:3000/api/v1"}, "--api-url must be http:// or https://"},
		{"api-url over ftp", []string{"--api-url", "ftp://forgejo.test/api/v1"}, "--api-url must be http:// or https://"},
		{"role with a slash", []string{"--role", "pola/cat"}, "--role 'pola/cat' must match [A-Za-z0-9._-]+"},
		{"empty role", []string{"--role", ""}, "--role '' must match [A-Za-z0-9._-]+"},
		{"unknown role", []string{"--role", "admin"}, "unknown role 'admin' (known roles: polecat landing registry viewer)"},
		{"bot prefix with a space", []string{"--bot-prefix", "bot "}, "--bot-prefix 'bot ' must match [A-Za-z0-9._-]+"},
		{"token name with a space", []string{"--token-name", "gt provision"}, "--token-name 'gt provision' must match [A-Za-z0-9._-]+"},
		{"landing bot with a colon", []string{"--landing-bot", "bot:landing"}, "--landing-bot 'bot:landing' must match [A-Za-z0-9._-]+"},
		{"email domain with a quote", []string{"--bot-email-domain", `bots"invalid`}, `--bot-email-domain 'bots"invalid' may not be empty or hold a newline, quote or backslash`},
		{"email domain with a backslash", []string{"--bot-email-domain", `bots\invalid`}, "--bot-email-domain"},
		{"main branch with a newline", []string{"--main-branch", "ma\nin"}, "--main-branch 'ma\nin' may not be empty or hold a newline, quote or backslash"},
		{"empty land branch", []string{"--land-branch", ""}, "--land-branch '' may not be empty or hold a newline, quote or backslash"},
		{"gate context with a quote", []string{"--gate-context", `ci "gate"`}, `--gate-context 'ci "gate"' may not be empty or hold a newline, quote or backslash`},
		{"repo without a slash", []string{"--repo", "gastown"}, "--repo 'gastown' must be OWNER/NAME"},
		{"repo with two slashes", []string{"--repo", "acme/gastown/extra"}, "--repo 'acme/gastown/extra' must be OWNER/NAME"},
		{"repo with a leading slash", []string{"--repo", "/gastown"}, "--repo '/gastown' must be OWNER/NAME"},
		{"repo with a trailing slash", []string{"--repo", "acme/"}, "--repo 'acme/' must be OWNER/NAME"},
		{"empty repo", []string{"--repo", ""}, "--repo '' must be OWNER/NAME"},
		{"a flag with no value left", []string{"--role"}, "--role needs a value"},
		{"unknown option", []string{"--bogus"}, "unknown option: --bogus"},
		{"a flag written with =", []string{"--role=polecat"}, "unknown option: --role=polecat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, err := Parse(tc.args, env(nil))
			if err == nil {
				t.Fatalf("Parse(%v) = %+v, want a usage error", tc.args, *opts)
			}
			var refused *UsageError
			if !errors.As(err, &refused) {
				t.Fatalf("Parse(%v) error = %v (%T), want *UsageError", tc.args, err, err)
			}
			if !strings.Contains(refused.Message, tc.want) {
				t.Errorf("Parse(%v) error = %q, want it to hold %q", tc.args, refused.Message, tc.want)
			}
			if strings.Contains(refused.Message, "usage:") {
				t.Errorf("Parse(%v) error = %q, want the message alone; the caller adds the usage hint", tc.args, refused.Message)
			}
		})
	}
}

// TestParseHelp holds both spellings of the help request: parsing stops and
// returns no options, and the command prints the usage text and exits 0.
func TestParseHelp(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			opts, err := Parse([]string{flag}, env(nil))
			if !errors.Is(err, ErrHelp) {
				t.Fatalf("Parse(%s) = %+v, %v, want ErrHelp", flag, opts, err)
			}
			if opts != nil {
				t.Errorf("Parse(%s) = %+v, want no options with the help request", flag, *opts)
			}
		})
	}
}

// TestRoleScopes pins the four roles' scope lists, which are what a bot can
// reach, and the unknown role that provisions nothing.
func TestRoleScopes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role  string
		want  []string
		known bool
	}{
		{"polecat", []string{"write:repository"}, true},
		{"landing", []string{"write:repository", "read:user"}, true},
		{"registry", []string{"write:package", "read:package"}, true},
		{"viewer", []string{"read:repository", "read:user"}, true},
		{"admin", nil, false},
		{"", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			t.Parallel()
			got, known := RoleScopes(tc.role)
			if known != tc.known {
				t.Errorf("RoleScopes(%q) known = %v, want %v", tc.role, known, tc.known)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RoleScopes(%q) = %v, want %v", tc.role, got, tc.want)
			}
		})
	}
}
