package forgejoresync

import (
	"errors"
	"os"
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
	// base is the empty environment with no flags and the repository acme/gastown:
	// HOME is empty too, so the backup directory is the script's "/forgejo/backups".
	base := Options{
		Repo:       "acme/gastown",
		APIURL:     "http://127.0.0.1:3000/api/v1",
		WebURL:     "http://127.0.0.1:3000",
		GitHubURL:  "https://github.com/acme/gastown.git",
		ForgejoURL: "http://127.0.0.1:3000/acme/gastown.git",
		MainBranch: "main",
		BackupDir:  "/forgejo/backups",
	}
	cases := []struct {
		name   string
		args   []string
		vars   map[string]string
		change func(*Options)
	}{
		{name: "empty environment"},
		{
			name: "FORGEJO_API_URL sets the API root and, through it, the web root and the remote",
			vars: map[string]string{"FORGEJO_API_URL": "https://forgejo.test/api/v1"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
				o.ForgejoURL = "https://forgejo.test/acme/gastown.git"
			},
		},
		{
			name: "FORGEJO_URL sets the web root and the remote alone",
			vars: map[string]string{"FORGEJO_URL": "https://web.test"},
			change: func(o *Options) {
				o.WebURL = "https://web.test"
				o.ForgejoURL = "https://web.test/acme/gastown.git"
			},
		},
		{
			name:   "FORGEJO_ADMIN_ENV names the admin token file",
			vars:   map[string]string{"FORGEJO_ADMIN_ENV": "/etc/gt/admin.env"},
			change: func(o *Options) { o.AdminTokenFile = "/etc/gt/admin.env" },
		},
		{
			name:   "FORGEJO_BACKUP_DIR holds the backup copy",
			vars:   map[string]string{"FORGEJO_BACKUP_DIR": "/var/backups"},
			change: func(o *Options) { o.BackupDir = "/var/backups" },
		},
		{
			name:   "HOME is the fallback for the backup directory",
			vars:   map[string]string{"HOME": "/home/gt"},
			change: func(o *Options) { o.BackupDir = "/home/gt/forgejo/backups" },
		},
		{
			name:   "FORGEJO_BACKUP_DIR wins over HOME",
			vars:   map[string]string{"FORGEJO_BACKUP_DIR": "/var/backups", "HOME": "/home/gt"},
			change: func(o *Options) { o.BackupDir = "/var/backups" },
		},
		{
			name: "a trailing slash comes off the API root, and the web root is derived from the trimmed root",
			args: []string{"--api-url", "https://forgejo.test/api/v1/"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
				o.ForgejoURL = "https://forgejo.test/acme/gastown.git"
			},
		},
		{
			name: "an API root without /api/v1 keeps its whole self as the web root",
			args: []string{"--api-url", "http://127.0.0.1:8080"},
			change: func(o *Options) {
				o.APIURL = "http://127.0.0.1:8080"
				o.WebURL = "http://127.0.0.1:8080"
				o.ForgejoURL = "http://127.0.0.1:8080/acme/gastown.git"
			},
		},
		{
			name: "--web-url overrides the derived web root and the remote built on it",
			args: []string{"--web-url", "https://web.test"},
			change: func(o *Options) {
				o.WebURL = "https://web.test"
				o.ForgejoURL = "https://web.test/acme/gastown.git"
			},
		},
		{
			name: "--web-url beats FORGEJO_URL",
			args: []string{"--web-url", "https://flag.test"},
			vars: map[string]string{"FORGEJO_URL": "https://env.test"},
			change: func(o *Options) {
				o.WebURL = "https://flag.test"
				o.ForgejoURL = "https://flag.test/acme/gastown.git"
			},
		},
		{
			name:   "--github-url replaces the GitHub remote",
			args:   []string{"--github-url", "https://mirror.test/acme/gastown.git"},
			change: func(o *Options) { o.GitHubURL = "https://mirror.test/acme/gastown.git" },
		},
		{
			name:   "--forgejo-url replaces the Forgejo remote alone",
			args:   []string{"--forgejo-url", "ssh://forge.test/acme/gastown.git"},
			change: func(o *Options) { o.ForgejoURL = "ssh://forge.test/acme/gastown.git" },
		},
		{
			name:   "--main-branch names the protected branch",
			args:   []string{"--main-branch", "trunk/v2"},
			change: func(o *Options) { o.MainBranch = "trunk/v2" },
		},
		{
			name:   "--backup-dir beats FORGEJO_BACKUP_DIR",
			args:   []string{"--backup-dir", "/flag/backups"},
			vars:   map[string]string{"FORGEJO_BACKUP_DIR": "/var/backups"},
			change: func(o *Options) { o.BackupDir = "/flag/backups" },
		},
		{
			name:   "--admin-token-file beats FORGEJO_ADMIN_ENV",
			args:   []string{"--admin-token-file", "/flag/admin.env"},
			vars:   map[string]string{"FORGEJO_ADMIN_ENV": "/env/admin.env"},
			change: func(o *Options) { o.AdminTokenFile = "/flag/admin.env" },
		},
		{name: "--dry-run", args: []string{"--dry-run"}, change: func(o *Options) { o.DryRun = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := base
			if tc.change != nil {
				tc.change(&want)
			}
			args := append([]string{}, tc.args...)
			args = append(args, "acme/gastown")
			got, err := Parse(args, env(tc.vars))
			if err != nil {
				t.Fatalf("Parse(%v) error = %v, want none", args, err)
			}
			if *got != want {
				t.Errorf("Parse(%v) = %+v, want %+v", args, *got, want)
			}
		})
	}
}

// TestParseRejectsBadValues holds each refusal the script's usage_die raised: the
// usage error names the flag or the value it refused, so the operator can fix
// the right word. Each row is a command line that must not parse.
func TestParseRejectsBadValues(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args    []string
		message string
	}{
		"api-url without a scheme":         {args: []string{"--api-url", "forgejo.test/api/v1", "acme/gastown"}, message: "--api-url must be http:// or https://"},
		"api-url with ftp":                 {args: []string{"--api-url", "ftp://forgejo.test", "acme/gastown"}, message: "--api-url must be http:// or https://"},
		"api-url with no value":            {args: []string{"acme/gastown", "--api-url"}, message: "--api-url needs a value"},
		"main-branch with a space":         {args: []string{"--main-branch", "ma in", "acme/gastown"}, message: "--main-branch 'ma in' must match"},
		"main-branch with a quote":         {args: []string{"--main-branch", `"main"`, "acme/gastown"}, message: "--main-branch '\"main\"' must match"},
		"main-branch empty":                {args: []string{"--main-branch", "", "acme/gastown"}, message: "--main-branch '' must match"},
		"repo with no slash":               {args: []string{"gastown"}, message: "'gastown' must be OWNER/NAME"},
		"repo with two slashes":            {args: []string{"acme/gastown/extra"}, message: "'acme/gastown/extra' must be OWNER/NAME"},
		"repo with a leading slash":        {args: []string{"/gastown"}, message: "'/gastown' must be OWNER/NAME"},
		"repo with a trailing slash":       {args: []string{"acme/"}, message: "'acme/' must be OWNER/NAME"},
		"no repo at all":                   {args: []string{"--dry-run"}, message: "OWNER/NAME is required"},
		"two repositories":                 {args: []string{"acme/gastown", "acme/devops"}, message: "one repository only"},
		"unknown option":                   {args: []string{"--nope", "acme/gastown"}, message: "unknown option: --nope"},
		"the removed --provision flag":     {args: []string{"--provision", "p.sh", "acme/gastown"}, message: "unknown option: --provision"},
		"a flag written with its value":    {args: []string{"--main-branch=main", "acme/gastown"}, message: "unknown option: --main-branch=main"},
		"a bare dash is an unknown option": {args: []string{"-", "acme/gastown"}, message: "unknown option: -"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.args, env(nil))
			if got != nil {
				t.Errorf("Parse(%v) = %+v, want no options", tc.args, *got)
			}
			var refused *UsageError
			if !errors.As(err, &refused) {
				t.Fatalf("Parse(%v) error = %v, want a *UsageError", tc.args, err)
			}
			if !strings.Contains(refused.Message, tc.message) {
				t.Errorf("Parse(%v) message = %q, want it to hold %q", tc.args, refused.Message, tc.message)
			}
		})
	}
}

// TestParseHelpStopsBeforeLaterArguments: -h is answered where it stands, so a
// bad value after it is never read, as the script exited at -h.
func TestParseHelpStopsBeforeLaterArguments(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"--help", "--api-url", "ftp://nope", "acme/gastown"},
		{"--help", "acme/gastown", "acme/devops"},
	} {
		if _, err := Parse(args, env(nil)); !errors.Is(err, ErrHelp) {
			t.Errorf("Parse(%v) error = %v, want ErrHelp", args, err)
		}
	}
}
