package forgejoprobe

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// env turns a map into the getenv seam: a variable the map does not name reads
// empty, so no test sees the host's environment.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// validArgs is the shortest command line the validation accepts.
var validArgs = []string{"rig", "--repo", "acme/rig"}

// TestDefaultsAndEnvironment pins every default, then each variable that moves
// one and each flag that overrides it, against the whole Options value so a
// default that stops being applied fails here.
func TestDefaultsAndEnvironment(t *testing.T) {
	t.Parallel()
	// base is validArgs in an empty environment: HOME is empty too, so the
	// town root is the script's "/gt" and the config directory "/.config/gt".
	base := Options{
		Rig:          "rig",
		Repo:         "acme/rig",
		Owner:        "acme",
		Name:         "rig",
		APIURL:       "http://127.0.0.1:3000/api/v1",
		WebURL:       "http://127.0.0.1:3000",
		TownRoot:     "/gt",
		GitHubURL:    "https://github.com/acme/rig.git",
		GitURL:       "http://127.0.0.1:3000/acme/rig.git",
		MainBranch:   "main",
		Timeout:      1200 * time.Second,
		PollInterval: 15 * time.Second,
		ConfigDir:    "/.config/gt",
	}
	cases := []struct {
		name   string
		args   []string
		vars   map[string]string
		change func(*Options)
	}{
		{name: "empty environment"},
		{
			name: "FORGEJO_API_URL sets the API root, its web root and the git URL",
			vars: map[string]string{"FORGEJO_API_URL": "https://forgejo.test/api/v1"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
				o.GitURL = "https://forgejo.test/acme/rig.git"
			},
		},
		{
			name: "a trailing slash comes off the API root from the environment",
			vars: map[string]string{"FORGEJO_API_URL": "https://forgejo.test/api/v1/"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
				o.GitURL = "https://forgejo.test/acme/rig.git"
			},
		},
		{
			name: "FORGEJO_URL sets the web root and the git URL it derives",
			vars: map[string]string{"FORGEJO_URL": "https://web.test"},
			change: func(o *Options) {
				o.WebURL = "https://web.test"
				o.GitURL = "https://web.test/acme/rig.git"
			},
		},
		{
			name: "FORGEJO_URL wins over the web root the API URL derives",
			vars: map[string]string{
				"FORGEJO_API_URL": "https://forgejo.test/api/v1",
				"FORGEJO_URL":     "https://web.test",
			},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://web.test"
				o.GitURL = "https://web.test/acme/rig.git"
			},
		},
		{
			name:   "FORGEJO_ADMIN_ENV names the admin token file",
			vars:   map[string]string{"FORGEJO_ADMIN_ENV": "/etc/gt/admin.env"},
			change: func(o *Options) { o.AdminTokenFile = "/etc/gt/admin.env" },
		},
		{
			name:   "GT_TOWN_ROOT names the town root",
			vars:   map[string]string{"GT_TOWN_ROOT": "/town"},
			change: func(o *Options) { o.TownRoot = "/town" },
		},
		{
			name: "GT_TOWN_ROOT wins over HOME",
			vars: map[string]string{"GT_TOWN_ROOT": "/town", "HOME": "/home/gt"},
			change: func(o *Options) {
				o.TownRoot = "/town"
				o.ConfigDir = "/home/gt/.config/gt"
			},
		},
		{
			name: "HOME is the fallback town root, as it is for the config directory",
			vars: map[string]string{"HOME": "/home/gt"},
			change: func(o *Options) {
				o.TownRoot = "/home/gt/gt"
				o.ConfigDir = "/home/gt/.config/gt"
			},
		},
		{
			// GT_ROOT is the bd alias, not a name this command reads through
			// (workspace.TownRootFromEnv): only GT_TOWN_ROOT names a town root.
			name: "GT_ROOT alone does not steer the town root",
			vars: map[string]string{"GT_ROOT": "/gtroot"},
		},
		{
			name:   "XDG_CONFIG_HOME holds the record",
			vars:   map[string]string{"XDG_CONFIG_HOME": "/xdg"},
			change: func(o *Options) { o.ConfigDir = "/xdg/gt" },
		},
		{
			name: "XDG_CONFIG_HOME wins over HOME",
			vars: map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home/gt"},
			change: func(o *Options) {
				o.ConfigDir = "/xdg/gt"
				o.TownRoot = "/home/gt/gt"
			},
		},
		{
			name: "the flags may precede the rig",
			args: []string{"--repo", "acme/rig", "rig"},
		},
		{
			name: "a trailing slash comes off the API root",
			args: []string{"rig", "--repo", "acme/rig", "--api-url", "https://forgejo.test/api/v1/"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
				o.GitURL = "https://forgejo.test/acme/rig.git"
			},
		},
		{
			name: "--web-url overrides the derived web root",
			args: []string{"rig", "--repo", "acme/rig", "--web-url", "https://web.test"},
			change: func(o *Options) {
				o.WebURL = "https://web.test"
				o.GitURL = "https://web.test/acme/rig.git"
			},
		},
		{
			name:   "--admin-token-file names the token file",
			args:   []string{"rig", "--repo", "acme/rig", "--admin-token-file", "/tmp/admin.env"},
			change: func(o *Options) { o.AdminTokenFile = "/tmp/admin.env" },
		},
		{
			name:   "--town-root names the town root",
			args:   []string{"rig", "--repo", "acme/rig", "--town-root", "/town"},
			change: func(o *Options) { o.TownRoot = "/town" },
		},
		{
			name:   "--main-branch names the copy's branch",
			args:   []string{"rig", "--repo", "acme/rig", "--main-branch", "trunk"},
			change: func(o *Options) { o.MainBranch = "trunk" },
		},
		{
			name:   "--github-url names the repository the copy is cloned from",
			args:   []string{"rig", "--repo", "acme/rig", "--github-url", "https://github.test/acme/rig.git"},
			change: func(o *Options) { o.GitHubURL = "https://github.test/acme/rig.git" },
		},
		{
			name:   "--git-url names the Forgejo remote the branch is pushed to",
			args:   []string{"rig", "--repo", "acme/rig", "--git-url", "https://push.test/acme/rig.git"},
			change: func(o *Options) { o.GitURL = "https://push.test/acme/rig.git" },
		},
		{
			name:   "--timeout reads its seconds",
			args:   []string{"rig", "--repo", "acme/rig", "--timeout", "60"},
			change: func(o *Options) { o.Timeout = 60 * time.Second },
		},
		{
			// 010 is ten, not eight: the shell read it with 10#.
			name:   "a leading zero in --timeout reads as decimal",
			args:   []string{"rig", "--repo", "acme/rig", "--timeout", "010"},
			change: func(o *Options) { o.Timeout = 10 * time.Second },
		},
		{
			name:   "a leading zero in --poll-interval reads as decimal",
			args:   []string{"rig", "--repo", "acme/rig", "--poll-interval", "030"},
			change: func(o *Options) { o.PollInterval = 30 * time.Second },
		},
		{
			name:   "--dry-run",
			args:   []string{"rig", "--repo", "acme/rig", "--dry-run"},
			change: func(o *Options) { o.DryRun = true },
		},
		{
			name: "--every flag at once",
			args: []string{
				"other", "--repo", "acme/tools", "--api-url", "https://forgejo.test/api/v1",
				"--web-url", "https://web.test", "--admin-token-file", "/tmp/admin.env",
				"--town-root", "/town", "--github-url", "https://github.test/acme/tools.git",
				"--git-url", "https://push.test/acme/tools.git", "--main-branch", "trunk",
				"--timeout", "90", "--poll-interval", "3", "--dry-run",
			},
			change: func(o *Options) {
				*o = Options{
					Rig:            "other",
					Repo:           "acme/tools",
					Owner:          "acme",
					Name:           "tools",
					APIURL:         "https://forgejo.test/api/v1",
					WebURL:         "https://web.test",
					AdminTokenFile: "/tmp/admin.env",
					TownRoot:       "/town",
					GitHubURL:      "https://github.test/acme/tools.git",
					GitURL:         "https://push.test/acme/tools.git",
					MainBranch:     "trunk",
					Timeout:        90 * time.Second,
					PollInterval:   3 * time.Second,
					DryRun:         true,
					ConfigDir:      "/.config/gt",
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := tc.args
			if args == nil {
				args = validArgs
			}
			want := base
			if tc.change != nil {
				tc.change(&want)
			}
			got, err := Parse(args, env(tc.vars))
			if err != nil {
				t.Fatalf("Parse(%v) errored: %v", args, err)
			}
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("Parse(%v, %v) = %+v, want %+v", args, tc.vars, *got, want)
			}
		})
	}
}

// TestValidateTable holds one case per refusal the validation makes, in the
// order it makes them, so dropping a check leaves a case that no longer
// refuses its value.
func TestValidateTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing rig", []string{"--repo", "acme/rig"}, "<rig> is required"},
		{"rig with a slash", []string{"bad/rig", "--repo", "acme/rig"}, "<rig> 'bad/rig' must match [A-Za-z0-9._-]+"},
		{"rig with a space", []string{"bad rig", "--repo", "acme/rig"}, "<rig> 'bad rig' must match [A-Za-z0-9._-]+"},
		{"rig that walks up", []string{"..", "--repo", "acme/rig"}, "<rig> must not be '..'"},
		{"rig that is the current directory", []string{".", "--repo", "acme/rig"}, "<rig> must not be '.'"},
		{"empty town root", []string{"rig", "--repo", "acme/rig", "--town-root", ""}, "--town-root must not be empty"},
		{"missing repo", []string{"rig"}, "--repo is required"},
		{"repo that is not OWNER/NAME", []string{"rig", "--repo", "not-a-repo"}, "--repo 'not-a-repo' must be OWNER/NAME"},
		{"main branch with a slash", []string{"rig", "--repo", "acme/rig", "--main-branch", "ma/in"}, "--main-branch 'ma/in' must match [A-Za-z0-9._-]+"},
		{"main branch with a leading dash", []string{"rig", "--repo", "acme/rig", "--main-branch", "-x"}, "--main-branch '-x' must not start with '-'"},
		{"main branch that is only a dash", []string{"rig", "--repo", "acme/rig", "--main-branch", "-"}, "--main-branch '-' must not start with '-'"},
		{"main branch with a colon", []string{"rig", "--repo", "acme/rig", "--main-branch", "ma:in"}, "--main-branch 'ma:in' must match [A-Za-z0-9._-]+"},
		{"zero timeout", []string{"rig", "--repo", "acme/rig", "--timeout", "0"}, "--timeout '0' must be a positive integer"},
		{"zero timeout written with zeros", []string{"rig", "--repo", "acme/rig", "--timeout", "000"}, "--timeout '000' must be a positive integer"},
		{"negative timeout", []string{"rig", "--repo", "acme/rig", "--timeout", "-5"}, "--timeout '-5' must be a positive integer"},
		{"non-numeric timeout", []string{"rig", "--repo", "acme/rig", "--timeout", "soon"}, "--timeout 'soon' must be a positive integer"},
		{"fractional timeout", []string{"rig", "--repo", "acme/rig", "--timeout", "1.5"}, "--timeout '1.5' must be a positive integer"},
		{"timeout too large for a duration", []string{"rig", "--repo", "acme/rig", "--timeout", "99999999999999999999"}, "--timeout '99999999999999999999' is too large to wait"},
		{"zero poll interval", []string{"rig", "--repo", "acme/rig", "--poll-interval", "0"}, "--poll-interval '0' must be a positive integer"},
		{"non-numeric poll interval", []string{"rig", "--repo", "acme/rig", "--poll-interval", "often"}, "--poll-interval 'often' must be a positive integer"},
		{"poll interval too large for a duration", []string{"rig", "--repo", "acme/rig", "--poll-interval", "99999999999999999999"}, "--poll-interval '99999999999999999999' is too large to wait"},
		{"api-url without a scheme", []string{"rig", "--repo", "acme/rig", "--api-url", "127.0.0.1:3000/api/v1"}, "--api-url must be http:// or https:// (got '127.0.0.1:3000/api/v1')"},
		{"api-url over ftp", []string{"rig", "--repo", "acme/rig", "--api-url", "ftp://forgejo.test/api/v1"}, "--api-url must be http:// or https:// (got 'ftp://forgejo.test/api/v1')"},
		{"api-url that is a bare scheme", []string{"rig", "--repo", "acme/rig", "--api-url", "http://"}, "--api-url must be http:// or https:// (got 'http://')"},
		{"api-url with a scheme and no host", []string{"rig", "--repo", "acme/rig", "--api-url", "http:///api/v1"}, "--api-url must be http:// or https://"},
		{"unknown option", []string{"rig", "--repo", "acme/rig", "--nope"}, "unknown option: --nope"},
		{"an option written with =", []string{"rig", "--repo=acme/rig"}, "unknown option: --repo=acme/rig"},
		{"an option with no value left", []string{"rig", "--repo", "acme/rig", "--timeout"}, "--timeout needs a value"},
		{"a value flag with no value left", []string{"rig", "--repo", "acme/rig", "--main-branch"}, "--main-branch needs a value"},
		{"a second positional", []string{"rig", "rig2", "--repo", "acme/rig"}, "unexpected argument: rig2"},
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

// TestRepoShapeTable holds the --repo shapes a Forgejo path can and cannot
// take: exactly two plain parts, neither of them a dot segment.
func TestRepoShapeTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		repo string
		ok   bool
	}{
		{"acme/rig", true},
		{"a/b", true},
		{"a.b/c-d", true},
		{"Acme_1/Rig.2", true},
		{"a/b/c", false},
		{"/a/b", false},
		{"a/b/", false},
		{"a//b", false},
		{"", false},
		{"abc", false},
		{"acme/.", false},
		{"acme/..", false},
		{"./rig", false},
		{"a c/rig", false},
	}
	for _, tc := range cases {
		t.Run(tc.repo, func(t *testing.T) {
			t.Parallel()
			opts, err := Parse([]string{"rig", "--repo", tc.repo}, env(nil))
			if tc.ok {
				if err != nil {
					t.Fatalf("Parse(--repo %q) errored: %v", tc.repo, err)
				}
				owner, name, _ := strings.Cut(tc.repo, "/")
				if opts.Owner != owner || opts.Name != name {
					t.Errorf("Parse(--repo %q) derived %q/%q, want %q/%q", tc.repo, opts.Owner, opts.Name, owner, name)
				}
				return
			}
			if err == nil {
				t.Fatalf("Parse(--repo %q) = %+v, want a refusal", tc.repo, *opts)
			}
			var refused *UsageError
			if !errors.As(err, &refused) {
				t.Fatalf("Parse(--repo %q) error = %v (%T), want *UsageError", tc.repo, err, err)
			}
			want := "--repo '" + tc.repo + "' must be OWNER/NAME"
			if tc.repo == "" {
				want = "--repo is required"
			}
			if !strings.Contains(refused.Message, want) {
				t.Errorf("Parse(--repo %q) error = %q, want it to hold %q", tc.repo, refused.Message, want)
			}
		})
	}
}

// TestValidateURLTable holds the three URLs a later bead hands to git: each is
// addressed (a scheme and a host), never reads as an option, and loses exactly
// one trailing slash.
func TestValidateURLTable(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--web-url", "--git-url", "--github-url"} {
		cases := []struct {
			name string
			url  string
			want string
		}{
			{"a dash", "-x", flag + " '-x' must not start with '-'"},
			{"a git option", "--upload-pack=x", flag + " '--upload-pack=x' must not start with '-'"},
			{"no scheme", "x", flag + " must be http:// or https:// (got 'x')"},
			{"a bare scheme", "http://", flag + " must be http:// or https:// (got 'http://')"},
			{"over ftp", "ftp://h", flag + " must be http:// or https:// (got 'ftp://h')"},
		}
		for _, tc := range cases {
			t.Run(flag+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := Parse([]string{"rig", "--repo", "acme/rig", flag, tc.url}, env(nil))
				var refused *UsageError
				if !errors.As(err, &refused) {
					t.Fatalf("Parse(%s %q) error = %v, want a *UsageError", flag, tc.url, err)
				}
				if !strings.Contains(refused.Message, tc.want) {
					t.Errorf("Parse(%s %q) error = %q, want it to hold %q", flag, tc.url, refused.Message, tc.want)
				}
			})
		}
	}

	// One trailing slash comes off, and only one: "http://h//" keeps the
	// second, as the shell's ${var%/} left it (TestDerivedNames pins where the
	// trimmed value lands).
	t.Run("one trailing slash is trimmed", func(t *testing.T) {
		t.Parallel()
		got, err := Parse([]string{"rig", "--repo", "acme/rig", "--web-url", "http://h//"}, env(nil))
		if err != nil {
			t.Fatalf("Parse(--web-url http://h//) errored: %v", err)
		}
		if got.WebURL != "http://h/" {
			t.Errorf("WebURL = %q, want http://h/", got.WebURL)
		}
	})
	t.Run("a URL with no trailing slash is unchanged", func(t *testing.T) {
		t.Parallel()
		got, err := Parse([]string{"rig", "--repo", "acme/rig", "--web-url", "http://h"}, env(nil))
		if err != nil {
			t.Fatalf("Parse(--web-url http://h) errored: %v", err)
		}
		if got.WebURL != "http://h" {
			t.Errorf("WebURL = %q, want http://h", got.WebURL)
		}
	})
}

// TestDerivedNames reads the names the run is built from, each for the
// inputs that can move it.
func TestDerivedNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		vars    map[string]string
		bare    string
		record  string
		branch  string
		encoded string
		github  string
		git     string
		web     string
	}{
		{
			name:    "empty environment",
			bare:    "/gt/rig/.repo.git",
			record:  "/.config/gt/probe-rig.record",
			branch:  "land/probe-rig",
			encoded: "land%2Fprobe-rig",
			github:  "https://github.com/acme/rig.git",
			git:     "http://127.0.0.1:3000/acme/rig.git",
			web:     "http://127.0.0.1:3000",
		},
		{
			name:    "the town root and the rig move the bare repository",
			args:    []string{"r.1", "--repo", "acme/rig", "--town-root", "/town"},
			bare:    "/town/r.1/.repo.git",
			record:  "/.config/gt/probe-r.1.record",
			branch:  "land/probe-r.1",
			encoded: "land%2Fprobe-r.1",
			github:  "https://github.com/acme/rig.git",
			git:     "http://127.0.0.1:3000/acme/rig.git",
			web:     "http://127.0.0.1:3000",
		},
		{
			name:    "XDG_CONFIG_HOME moves the record",
			vars:    map[string]string{"XDG_CONFIG_HOME": "/xdg"},
			bare:    "/gt/rig/.repo.git",
			record:  "/xdg/gt/probe-rig.record",
			branch:  "land/probe-rig",
			encoded: "land%2Fprobe-rig",
			github:  "https://github.com/acme/rig.git",
			git:     "http://127.0.0.1:3000/acme/rig.git",
			web:     "http://127.0.0.1:3000",
		},
		{
			name:    "the repository names both URLs",
			args:    []string{"rig", "--repo", "acme/tools"},
			bare:    "/gt/rig/.repo.git",
			record:  "/.config/gt/probe-rig.record",
			branch:  "land/probe-rig",
			encoded: "land%2Fprobe-rig",
			github:  "https://github.com/acme/tools.git",
			git:     "http://127.0.0.1:3000/acme/tools.git",
			web:     "http://127.0.0.1:3000",
		},
		{
			name: "a web URL with a trailing slash adds no second slash",
			args: []string{"rig", "--repo", "acme/rig", "--web-url", "https://web.test/"},
			bare: "/gt/rig/.repo.git", record: "/.config/gt/probe-rig.record",
			branch: "land/probe-rig", encoded: "land%2Fprobe-rig",
			github: "https://github.com/acme/rig.git",
			git:    "https://web.test/acme/rig.git",
			web:    "https://web.test",
		},
		{
			name: "the URL flags name their own remotes",
			args: []string{
				"rig", "--repo", "acme/rig",
				"--github-url", "https://github.test/copy/rig.git/",
				"--git-url", "https://push.test/acme/rig.git/",
			},
			bare: "/gt/rig/.repo.git", record: "/.config/gt/probe-rig.record",
			branch: "land/probe-rig", encoded: "land%2Fprobe-rig",
			github: "https://github.test/copy/rig.git",
			git:    "https://push.test/acme/rig.git",
			web:    "http://127.0.0.1:3000",
		},
		{
			name: "an API URL without /api/v1 keeps its whole self as the web root",
			args: []string{"rig", "--repo", "acme/rig", "--api-url", "http://127.0.0.1:8080/custom"},
			bare: "/gt/rig/.repo.git", record: "/.config/gt/probe-rig.record",
			branch: "land/probe-rig", encoded: "land%2Fprobe-rig",
			github: "https://github.com/acme/rig.git",
			git:    "http://127.0.0.1:8080/custom/acme/rig.git",
			web:    "http://127.0.0.1:8080/custom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := tc.args
			if args == nil {
				args = validArgs
			}
			o, err := Parse(args, env(tc.vars))
			if err != nil {
				t.Fatalf("Parse(%v) errored: %v", args, err)
			}
			if got := o.Bare(); got != tc.bare {
				t.Errorf("Bare = %q, want %q", got, tc.bare)
			}
			if got := o.Record(); got != tc.record {
				t.Errorf("Record = %q, want %q", got, tc.record)
			}
			if got := o.Branch(); got != tc.branch {
				t.Errorf("Branch = %q, want %q", got, tc.branch)
			}
			if got := o.BranchEncoded(); got != tc.encoded {
				t.Errorf("BranchEncoded = %q, want %q", got, tc.encoded)
			}
			if got := o.GitHubURL; got != tc.github {
				t.Errorf("GitHubURL = %q, want %q", got, tc.github)
			}
			if got := o.GitURL; got != tc.git {
				t.Errorf("GitURL = %q, want %q", got, tc.git)
			}
			if got := o.WebURL; got != tc.web {
				t.Errorf("WebURL = %q, want %q", got, tc.web)
			}
		})
	}
}

// TestValidateUnderTownRoot drives the containment check directly. requireRig
// refuses every rig name Parse could hand it that walks, so this is the second
// of the two checks the derived path gets, and no command line can reach it:
// the rows below are the only way to see it refuse.
func TestValidateUnderTownRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		townRoot string
		rig      string
		ok       bool
	}{
		{"a plain rig", "/town", "rig", true},
		{"a rig with dots", "/town", "r.1", true},
		{"a rig that walks up", "/town", "..", false},
		{"a rig that walks up and across", "/town", "../other", false},
		{"a nested rig name", "/town", "a/b", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireUnderTownRoot(tc.townRoot, tc.rig)
			if tc.ok && err != nil {
				t.Fatalf("requireUnderTownRoot(%q, %q) = %v, want it accepted", tc.townRoot, tc.rig, err)
			}
			if !tc.ok {
				var refused *UsageError
				if !errors.As(err, &refused) {
					t.Fatalf("requireUnderTownRoot(%q, %q) = %v, want a *UsageError", tc.townRoot, tc.rig, err)
				}
			}
		})
	}
}

// TestParseOptionsHelp holds both spellings of the help request: parsing stops
// and returns no options, and the command prints the usage text and exits 0.
func TestParseOptionsHelp(t *testing.T) {
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
