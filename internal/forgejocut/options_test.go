package forgejocut

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// testTowns are the towns the fake filesystem holds. Each has the rig gastown, its
// bare repository and its town marker, so every one of them parses.
var testTowns = []string{"/srv/town", "/srv/helper", "/home/test/gt"}

// fakeGit is the Git seam: it reports one origin URL, or the error it is given.
type fakeGit struct {
	origin string
	err    error
}

func (g fakeGit) OriginURL(string) (string, error) { return g.origin, g.err }

// isTownDir reports whether p is one of the test towns, its rig or its bare
// repository.
func isTownDir(p string) bool {
	for _, t := range testTowns {
		if p == t || p == t+"/gastown" || p == t+"/gastown/.repo.git" {
			return true
		}
	}
	return false
}

// testBinary is the path of the running binary the test seams report: at the root
// of testRepo, as a build without bin/ puts it.
const (
	testBinary = "/repo/forgejo-cutover"
	testProbe  = "/repo/scripts/forgejo-probe.sh"
)

// isTownFile reports whether p is a test town's marker, the test repository's
// probe, or a probe under scripts/.
func isTownFile(p string) bool {
	if strings.HasPrefix(p, "scripts/") || p == testProbe {
		return true
	}
	for _, t := range testTowns {
		if p == t+"/mayor/town.json" {
			return true
		}
	}
	return false
}

// testEnv is the seam set a parse sees: vars as the environment, the test towns
// as the filesystem, and an origin of the GitHub repository acme/gastown. Town
// reads as "" unless a test sets it.
func testEnv(vars map[string]string) Env {
	return Env{
		Getenv:     func(key string) string { return vars[key] },
		IsDir:      isTownDir,
		IsFile:     isTownFile,
		Town:       func() string { return "" },
		Executable: func() (string, error) { return testBinary, nil },
		Git:        fakeGit{origin: "https://github.com/acme/gastown.git"},
	}
}

// homeVars is the environment with HOME set and nothing else.
func homeVars(extra map[string]string) map[string]string {
	vars := map[string]string{"HOME": "/home/test"}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// withRig appends the rig and repository every valid command line names.
func withRig(args ...string) []string {
	return append(append([]string{}, args...), "gastown", "--repo", "acme/gastown")
}

// baseOptions is the Options a command line with no flag and an empty
// environment (HOME=/home/test) parses to.
func baseOptions() Options {
	return Options{
		Rig:           "gastown",
		TownRoot:      "/home/test/gt",
		RigRoot:       "/home/test/gt/gastown",
		BareRepo:      "/home/test/gt/gastown/.repo.git",
		Repo:          "acme/gastown",
		GitHubURL:     "https://github.com/acme/gastown.git",
		GitHubSlug:    "acme/gastown",
		ForgejoURL:    "http://127.0.0.1:3000/acme/gastown.git",
		AdminGitURL:   "http://127.0.0.1:3000/acme/gastown.git",
		PromoteTarget: "git@github.com:acme/gastown.git",
		PromoteKey:    "/home/test/.config/gt/promote-gastown.key",
		MainBranch:    "main",
		GateWorkflow:  "gate",
		BotPrefix:     "bot-",
		APIURL:        "http://127.0.0.1:3000/api/v1",
		WebURL:        "http://127.0.0.1:3000",
		Probe:         testProbe,
		GT:            "gt",
	}
}

// inTown moves the expected town, and so the rig and bare repository under it.
func inTown(town string) func(*Options) {
	return func(o *Options) {
		o.TownRoot = town
		o.RigRoot = town + "/gastown"
		o.BareRepo = town + "/gastown/.repo.git"
	}
}

// TestParseDefaultsTable pins every default the script's header promises, against
// its literal value, so a changed default fails here.
func TestParseDefaultsTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		args   []string
		vars   map[string]string
		change func(*Options)
	}{
		{name: "empty environment"},
		{
			name:   "GT_TOWN_ROOT names the town",
			vars:   map[string]string{"GT_TOWN_ROOT": "/srv/town"},
			change: inTown("/srv/town"),
		},
		{
			name:   "--town-root beats GT_TOWN_ROOT",
			args:   []string{"--town-root", "/srv/helper"},
			vars:   map[string]string{"GT_TOWN_ROOT": "/srv/town"},
			change: inTown("/srv/helper"),
		},
		{
			name: "GT_ROOT is not read: the town stays HOME's gt",
			vars: map[string]string{"GT_ROOT": "/srv/town"},
		},
		{
			name: "GT_BIN is not read: the gt binary stays the name gt",
			vars: map[string]string{"GT_BIN": "/opt/gt/bin/gt"},
		},
		{
			name: "FORGEJO_API_URL sets the API root, and through it the web root and both git URLs",
			vars: map[string]string{"FORGEJO_API_URL": "https://forgejo.test/api/v1"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
				o.ForgejoURL = "https://forgejo.test/acme/gastown.git"
				o.AdminGitURL = "https://forgejo.test/acme/gastown.git"
			},
		},
		{
			name: "FORGEJO_URL sets the web root and both git URLs",
			vars: map[string]string{"FORGEJO_URL": "https://web.test"},
			change: func(o *Options) {
				o.WebURL = "https://web.test"
				o.ForgejoURL = "https://web.test/acme/gastown.git"
				o.AdminGitURL = "https://web.test/acme/gastown.git"
			},
		},
		{
			name: "--web-url beats FORGEJO_URL",
			args: []string{"--web-url", "https://flag.test"},
			vars: map[string]string{"FORGEJO_URL": "https://env.test"},
			change: func(o *Options) {
				o.WebURL = "https://flag.test"
				o.ForgejoURL = "https://flag.test/acme/gastown.git"
				o.AdminGitURL = "https://flag.test/acme/gastown.git"
			},
		},
		{
			name: "a trailing slash on the web root is trimmed from the git URLs, not from the flag's value",
			args: []string{"--web-url", "https://web.test/"},
			change: func(o *Options) {
				o.WebURL = "https://web.test/"
				o.ForgejoURL = "https://web.test/acme/gastown.git"
				o.AdminGitURL = "https://web.test/acme/gastown.git"
			},
		},
		{
			name: "a trailing slash comes off the API root, and the web root is derived from the trimmed root",
			args: []string{"--api-url", "https://forgejo.test/api/v1/"},
			change: func(o *Options) {
				o.APIURL = "https://forgejo.test/api/v1"
				o.WebURL = "https://forgejo.test"
				o.ForgejoURL = "https://forgejo.test/acme/gastown.git"
				o.AdminGitURL = "https://forgejo.test/acme/gastown.git"
			},
		},
		{
			name: "an API root without /api/v1 keeps its whole self as the web root",
			args: []string{"--api-url", "http://127.0.0.1:8080"},
			change: func(o *Options) {
				o.APIURL = "http://127.0.0.1:8080"
				o.WebURL = "http://127.0.0.1:8080"
				o.ForgejoURL = "http://127.0.0.1:8080/acme/gastown.git"
				o.AdminGitURL = "http://127.0.0.1:8080/acme/gastown.git"
			},
		},
		{
			name: "--forgejo-url replaces the Forgejo remote alone, never the admin base",
			args: []string{"--forgejo-url", "ssh://forge.test/acme/gastown.git"},
			change: func(o *Options) {
				o.ForgejoURL = "ssh://forge.test/acme/gastown.git"
			},
		},
		{
			name: "--github-url replaces the GitHub remote and the promote target built on it",
			args: []string{"--github-url", "https://mirror.test/acme/gastown.git"},
			change: func(o *Options) {
				o.GitHubURL = "https://mirror.test/acme/gastown.git"
				o.PromoteTarget = "git@mirror.test:acme/gastown.git"
			},
		},
		{
			name:   "--main-branch names the protected branch",
			args:   []string{"--main-branch", "trunk/v2"},
			change: func(o *Options) { o.MainBranch = "trunk/v2" },
		},
		{
			name: "--gate-workflow and --bot-prefix name the gate and the bots",
			args: []string{"--gate-workflow", "ci-gate", "--bot-prefix", "svc-"},
			change: func(o *Options) {
				o.GateWorkflow = "ci-gate"
				o.BotPrefix = "svc-"
			},
		},
		{
			name:   "--accept-diverged",
			args:   []string{"--accept-diverged"},
			change: func(o *Options) { o.AcceptDiverged = true },
		},
		{
			name:   "--dry-run",
			args:   []string{"--dry-run"},
			change: func(o *Options) { o.DryRun = true },
		},
		{
			name:   "--main-branch takes a slashed name",
			args:   []string{"--main-branch", "release/1"},
			change: func(o *Options) { o.MainBranch = "release/1" },
		},
		{
			name:   "--probe names the probe",
			args:   []string{"--probe", "scripts/other-probe.sh"},
			change: func(o *Options) { o.Probe = "scripts/other-probe.sh" },
		},
		{
			name:   "--gt names the gt binary",
			args:   []string{"--gt", "/opt/gt/bin/gt"},
			change: func(o *Options) { o.GT = "/opt/gt/bin/gt" },
		},
		{
			name:   "FORGEJO_ADMIN_ENV names the admin token file",
			vars:   map[string]string{"FORGEJO_ADMIN_ENV": "/etc/gt/admin.env"},
			change: func(o *Options) { o.AdminTokenFile = "/etc/gt/admin.env" },
		},
		{
			name:   "--admin-token-file beats FORGEJO_ADMIN_ENV",
			args:   []string{"--admin-token-file", "/flag/admin.env"},
			vars:   map[string]string{"FORGEJO_ADMIN_ENV": "/env/admin.env"},
			change: func(o *Options) { o.AdminTokenFile = "/flag/admin.env" },
		},
		{
			name: "XDG_CONFIG_HOME holds the promote key's directory",
			vars: map[string]string{"XDG_CONFIG_HOME": "/xdg"},
			change: func(o *Options) {
				o.PromoteKey = "/xdg/gt/promote-gastown.key"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := baseOptions()
			if tc.change != nil {
				tc.change(&want)
			}
			got, err := Parse(withRig(tc.args...), testEnv(homeVars(tc.vars)))
			if err != nil {
				t.Fatalf("Parse(%v) error = %v, want none", tc.args, err)
			}
			if *got != want {
				t.Errorf("Parse(%v) = %+v, want %+v", tc.args, *got, want)
			}
		})
	}
}

// TestParseDerivesBotLogins: one bot per role the rig block names, each login the
// bot prefix plus the role.
func TestParseDerivesBotLogins(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "default prefix", want: []string{"bot-polecat", "bot-landing", "bot-registry"}},
		{name: "named prefix", args: []string{"--bot-prefix", "svc-"}, want: []string{"svc-polecat", "svc-landing", "svc-registry"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(withRig(tc.args...), testEnv(homeVars(nil)))
			if err != nil {
				t.Fatalf("Parse error = %v, want none", err)
			}
			if logins := got.BotLogins(); strings.Join(logins, ",") != strings.Join(tc.want, ",") {
				t.Errorf("BotLogins() = %v, want %v", logins, tc.want)
			}
		})
	}
}

// TestParseTownRootChain: the town root is the flag, then GT_TOWN_ROOT, then the
// town the working directory is in, then HOME's gt.
func TestParseTownRootChain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		vars map[string]string
		town string
		want string
	}{
		{name: "the working directory's town beats HOME's gt", town: "/srv/helper", want: "/srv/helper"},
		{name: "GT_TOWN_ROOT beats the working directory's town", vars: map[string]string{"GT_TOWN_ROOT": "/srv/town"}, town: "/srv/helper", want: "/srv/town"},
		{name: "the flag beats the working directory's town", args: []string{"--town-root", "/srv/town"}, town: "/srv/helper", want: "/srv/town"},
		{name: "HOME's gt is the last fallback", want: "/home/test/gt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(homeVars(tc.vars))
			env.Town = func() string { return tc.town }
			got, err := Parse(withRig(tc.args...), env)
			if err != nil {
				t.Fatalf("Parse error = %v, want none", err)
			}
			if got.TownRoot != tc.want {
				t.Errorf("TownRoot = %q, want %q", got.TownRoot, tc.want)
			}
		})
	}
}

// TestParseProbeDefaultIsBesideTheBinary: with no --probe, the probe is the one
// under the repository the binary sits in, the binary's directory or its parent.
// Where the binary's location names no probe, the path under its directory is
// named, so the refusal shows what was looked at.
func TestParseProbeDefaultIsBesideTheBinary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		executable string
		err        error
		probes     []string // files beside the binary, besides the test repository's probe
		want       string   // the probe path the command names
		refused    bool     // the probe is missing, so the parse refuses it
	}{
		{name: "the binary at the repository root", executable: "/repo/forgejo-cutover", want: "/repo/scripts/forgejo-probe.sh"},
		{name: "the binary in bin/", executable: "/repo/bin/forgejo-cutover", want: "/repo/scripts/forgejo-probe.sh"},
		{name: "the binary's own directory wins", executable: "/repo/bin/forgejo-cutover", probes: []string{"/repo/bin/scripts/forgejo-probe.sh"}, want: "/repo/bin/scripts/forgejo-probe.sh"},
		{name: "no probe beside the binary names the binary's directory", executable: "/usr/local/libexec/forgejo-cutover", want: "/usr/local/libexec/scripts/forgejo-probe.sh", refused: true},
		{name: "no executable path falls back to the relative default", err: errors.New("no executable"), want: "scripts/forgejo-probe.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(homeVars(nil))
			env.Executable = func() (string, error) { return tc.executable, tc.err }
			env.IsFile = func(p string) bool {
				return isTownFile(p) || slices.Contains(tc.probes, p)
			}
			got, err := Parse(withRig(), env)
			if tc.refused {
				var refused *UsageError
				if !errors.As(err, &refused) || !strings.Contains(refused.Message, "--probe '"+tc.want+"' is not a file") {
					t.Fatalf("Parse error = %v, want a usage error naming probe %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse error = %v, want none", err)
			}
			if got.Probe != tc.want {
				t.Errorf("Probe = %q, want %q", got.Probe, tc.want)
			}
		})
	}
}

// TestParseNeverEchoesCredentials: a URL that carries a password is refused, and
// the password is in neither the message nor any other part of the error.
func TestParseNeverEchoesCredentials(t *testing.T) {
	t.Parallel()
	const secret = "hunter2"
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "an https API root with a password", args: withRig("--api-url", "https://user:"+secret+"@forgejo.test/api/v1")},
		{name: "an http API root with a password", args: withRig("--api-url", "http://user:"+secret+"@forgejo.test/api/v1")},
		{name: "a non-http API root with a password", args: withRig("--api-url", "ftp://user:"+secret+"@forgejo.test")},
		{name: "a malformed non-http API root with an @", args: withRig("--api-url", "ftp://user:"+secret+"@forgejo test")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.args, testEnv(homeVars(nil)))
			var refused *UsageError
			if !errors.As(err, &refused) {
				t.Fatalf("Parse error = %v, want a usage error", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Parse error = %q, want the password absent", err)
			}
		})
	}
}

// TestParseRejectsBadValues holds each refusal the script's usage_die raised: the
// usage error names the flag or the value it refused, so the operator can fix the
// right word. Each row is a command line that must not parse.
func TestParseRejectsBadValues(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args    []string
		vars    map[string]string
		message string
	}{
		"no rig":                              {args: []string{"--repo", "acme/gastown"}, message: "<rig> is required"},
		"rig with a slash":                    {args: []string{"gastown/x", "--repo", "acme/gastown"}, message: "<rig> 'gastown/x' must match [A-Za-z0-9._-]+"},
		"rig that is a dot-dot":               {args: []string{"..", "--repo", "acme/gastown"}, message: "<rig> '..' must match"},
		"two rigs":                            {args: []string{"gastown", "devops", "--repo", "acme/gastown"}, message: "one rig only (already have 'gastown')"},
		"no repo":                             {args: []string{"gastown"}, message: "--repo is required"},
		"repo with no slash":                  {args: []string{"gastown", "--repo", "gastown"}, message: "--repo 'gastown' must be OWNER/NAME"},
		"repo with two slashes":               {args: []string{"gastown", "--repo", "a/b/c"}, message: "--repo 'a/b/c' must be OWNER/NAME"},
		"repo with a dot-dot owner":           {args: []string{"gastown", "--repo", "../gastown"}, message: "--repo '../gastown' must be OWNER/NAME"},
		"repo with a space":                   {args: []string{"gastown", "--repo", "acme/git town"}, message: "--repo 'acme/git town' must be OWNER/NAME"},
		"repo with a question mark":           {args: []string{"gastown", "--repo", "acme/gast?own"}, message: "--repo 'acme/gast?own' must be OWNER/NAME"},
		"main-branch with a space":            {args: withRig("--main-branch", "ma in"), message: "--main-branch 'ma in' must match [A-Za-z0-9._/-]+"},
		"main-branch empty":                   {args: withRig("--main-branch", ""), message: "--main-branch '' must match"},
		"main-branch starts with a dash":      {args: withRig("--main-branch", "-x"), message: "--main-branch '-x' is not a branch name: it starts with '-'"},
		"main-branch is an option with value": {args: withRig("--main-branch", "--upload-pack=x"), message: "--main-branch '--upload-pack=x' must match"},
		"main-branch has a dot-dot":           {args: withRig("--main-branch", "a..b"), message: "--main-branch 'a..b' is not a branch name: it contains '..'"},
		"main-branch has a double slash":      {args: withRig("--main-branch", "a//b"), message: "--main-branch 'a//b' is not a branch name: it contains '//'"},
		"main-branch starts with a slash":     {args: withRig("--main-branch", "/a"), message: "--main-branch '/a' is not a branch name: it starts or ends with '/'"},
		"main-branch ends with a slash":       {args: withRig("--main-branch", "a/"), message: "--main-branch 'a/' is not a branch name: it starts or ends with '/'"},
		"main-branch ends with .lock":         {args: withRig("--main-branch", "a.lock"), message: "--main-branch 'a.lock' is not a branch name: it ends with '.' or '.lock'"},
		"main-branch ends with a dot":         {args: withRig("--main-branch", "a."), message: "--main-branch 'a.' is not a branch name: it ends with '.' or '.lock'"},
		"main-branch component starts a dot":  {args: withRig("--main-branch", "a/.b"), message: "--main-branch 'a/.b' is not a branch name: it has a component that starts with '.'"},
		"main-branch is the single @":         {args: withRig("--main-branch", "@"), message: "--main-branch '@' must match [A-Za-z0-9._/-]+"},
		"main-branch has a tilde":             {args: withRig("--main-branch", "a~1"), message: "--main-branch 'a~1' must match"},
		"main-branch has an @{":               {args: withRig("--main-branch", "a@{1}"), message: "--main-branch 'a@{1}' must match"},
		"gate-workflow with a slash":          {args: withRig("--gate-workflow", "ci/gate"), message: "--gate-workflow 'ci/gate' must match [A-Za-z0-9._-]+"},
		"bot-prefix with a space":             {args: withRig("--bot-prefix", "bot x"), message: "--bot-prefix 'bot x' must match [A-Za-z0-9._-]+"},
		"api-url without a scheme":            {args: withRig("--api-url", "forgejo.test/api/v1"), message: "--api-url must be http:// or https:// (got 'forgejo.test/api/v1')"},
		"api-url with ftp":                    {args: withRig("--api-url", "ftp://forgejo.test"), message: "--api-url must be http:// or https://"},
		"api-url with no value":               {args: []string{"gastown", "--repo", "acme/gastown", "--api-url"}, message: "--api-url needs a value"},
		"town-root that is not a directory":   {args: withRig("--town-root", "/nowhere"), message: "--town-root '/nowhere' is not a directory"},
		"town-root empty":                     {args: withRig("--town-root", ""), message: "--town-root '' is not a directory"},
		"no HOME and no town root":            {args: withRig(), vars: map[string]string{"HOME": ""}, message: "--town-root '' is not a directory"},
		"town-root with no value":             {args: []string{"gastown", "--repo", "acme/gastown", "--town-root"}, message: "--town-root needs a value"},
		"probe that is not a file":            {args: withRig("--probe", "nope.sh"), message: "--probe 'nope.sh' is not a file"},
		"probe with no value":                 {args: []string{"gastown", "--repo", "acme/gastown", "--probe"}, message: "--probe needs a value"},
		"unknown option":                      {args: withRig("--nope"), message: "unknown option: --nope"},
		"the dropped --mirror flag":           {args: withRig("--mirror"), message: "unknown option: --mirror"},
		"the dropped --mirror-target flag":    {args: withRig("--mirror-target", "git@x:y/z.git"), message: "unknown option: --mirror-target"},
		"the dropped --provision flag":        {args: withRig("--provision", "p.sh"), message: "unknown option: --provision"},
		"a flag written with its value":       {args: withRig("--main-branch=main"), message: "unknown option: --main-branch=main"},
		"a bare dash is an unknown option":    {args: []string{"-", "gastown", "--repo", "acme/gastown"}, message: "unknown option: -"},
		"a value flag at the end":             {args: []string{"gastown", "--repo"}, message: "--repo needs a value"},
		"forgejo-url with a quote":            {args: withRig("--forgejo-url", `https://x"y/acme/gastown.git`), message: "--forgejo-url may not be empty or hold a newline, quote or backslash"},
		"github-url with a backslash":         {args: withRig("--github-url", `https://github.com\acme/gastown.git`), message: "--github-url may not be empty or hold a newline, quote or backslash"},
		"github-url that names no OWNER/NAME": {args: withRig("--github-url", "https://github.com/gastown.git"), message: "--github-url does not name OWNER/NAME"},
		"XDG_CONFIG_HOME with a quote":        {args: withRig(), vars: map[string]string{"XDG_CONFIG_HOME": `/x"y`}, message: "the promote key path may not be empty or hold a newline, quote or backslash"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.args, testEnv(homeVars(tc.vars)))
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

// TestParseFailedRunIsNotAUsageError: a rig whose directory, bare repository or
// town marker is missing, and a host with no config directory, are failed runs,
// which the script reported with die. Each error is not a usage error, so the
// command exits 1.
func TestParseFailedRunIsNotAUsageError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		args   []string
		vars   map[string]string
		change func(*Env)
		want   string
	}{
		{
			name: "no rig directory",
			args: []string{"absent", "--repo", "acme/gastown"},
			want: "no rig directory /home/test/gt/absent",
		},
		{
			name:   "no bare repository",
			args:   withRig(),
			change: func(e *Env) { e.IsDir = withoutPath(e.IsDir, ".repo.git") },
			want:   "no bare repository /home/test/gt/gastown/.repo.git",
		},
		{
			name:   "no town marker",
			args:   withRig(),
			change: func(e *Env) { e.IsFile = withoutPath(e.IsFile, "town.json") },
			want:   "no /home/test/gt/mayor/town.json",
		},
		{
			name: "no XDG_CONFIG_HOME and no HOME: the promote key has no directory",
			args: withRig(),
			vars: map[string]string{"GT_TOWN_ROOT": "/srv/town", "HOME": ""},
			want: "no XDG_CONFIG_HOME or HOME",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(homeVars(tc.vars))
			if tc.change != nil {
				tc.change(&env)
			}
			got, err := Parse(tc.args, env)
			if got != nil {
				t.Fatalf("Parse(%v) = %+v, want no options", tc.args, *got)
			}
			var refused *UsageError
			if errors.As(err, &refused) {
				t.Fatalf("Parse(%v) error = %v, want a failed run, not a usage error", tc.args, err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%v) error = %v, want it to hold %q", tc.args, err, tc.want)
			}
		})
	}
}

// withoutPath wraps a seam so the path ending in suffix reads as absent, and every
// other path reads as the test towns does.
func withoutPath(seam func(string) bool, suffix string) func(string) bool {
	return func(p string) bool {
		return !strings.HasSuffix(p, suffix) && seam(p)
	}
}

// TestParseNoOriginIsAFailedRun: with no --github-url, the origin of the bare
// repository is the GitHub remote. When git reads none, the run fails, and a
// --github-url given by the operator needs no origin at all.
func TestParseNoOriginIsAFailedRun(t *testing.T) {
	t.Parallel()
	env := testEnv(homeVars(nil))
	env.Git = fakeGit{err: errors.New("no such remote")}
	_, err := Parse(withRig(), env)
	var refused *UsageError
	if errors.As(err, &refused) || err == nil || !strings.Contains(err.Error(), "has no origin URL") {
		t.Errorf("Parse with no origin error = %v, want a failed run naming the missing origin", err)
	}

	got, err := Parse(withRig("--github-url", "https://mirror.test/acme/gastown.git"), env)
	if err != nil {
		t.Fatalf("Parse with --github-url error = %v, want none", err)
	}
	if got.GitHubURL != "https://mirror.test/acme/gastown.git" {
		t.Errorf("GitHubURL = %q, want the flag's value", got.GitHubURL)
	}
}

// TestParseHelpStopsBeforeLaterArguments: -h is answered where it stands, so a bad
// value after it is never read, as the script exited at -h.
func TestParseHelpStopsBeforeLaterArguments(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"--help", "--api-url", "ftp://nope", "gastown", "--repo", "acme/gastown"},
		{"--help", "gastown", "devops"},
	} {
		if _, err := Parse(args, testEnv(homeVars(nil))); !errors.Is(err, ErrHelp) {
			t.Errorf("Parse(%v) error = %v, want ErrHelp", args, err)
		}
	}
}

// TestSSHFormTable pins ssh_form on the URL shapes the script's own comment names:
// an http(s) git URL becomes its ssh form, an ssh URL and a scp-like one pass
// through in ssh form.
func TestSSHFormTable(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		url  string
		want string
	}{
		"https URL":                 {url: "https://github.com/acme/gastown.git", want: "git@github.com:acme/gastown.git"},
		"http URL without .git":     {url: "http://github.com/acme/gastown", want: "git@github.com:acme/gastown"},
		"ssh URL with a user":       {url: "ssh://git@github.com/acme/gastown.git", want: "git@github.com:acme/gastown.git"},
		"ssh URL without a user":    {url: "ssh://github.com/acme/gastown.git", want: "git@github.com:acme/gastown.git"},
		"scp-like URL is unchanged": {url: "git@github.com:acme/gastown.git", want: "git@github.com:acme/gastown.git"},
		"a local path is unchanged": {url: "/srv/repos/gastown.git", want: "/srv/repos/gastown.git"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := sshForm(tc.url); got != tc.want {
				t.Errorf("sshForm(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestSlugTable pins slug on the URL shapes a GitHub remote takes: the OWNER/NAME
// is the same whichever scheme or user the URL carries, and a port is dropped.
func TestSlugTable(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		url  string
		want string
	}{
		"https URL":              {url: "https://github.com/acme/gastown.git", want: "acme/gastown"},
		"https URL without .git": {url: "https://github.com/acme/gastown", want: "acme/gastown"},
		"scp-like URL":           {url: "git@github.com:acme/gastown.git", want: "acme/gastown"},
		"ssh URL with a user":    {url: "ssh://git@github.com/acme/gastown.git", want: "acme/gastown"},
		"ssh URL with a port":    {url: "ssh://git@github.com:2222/acme/gastown", want: "acme/gastown"},
		"a bare OWNER/NAME":      {url: "acme/gastown", want: "acme/gastown"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := slug(tc.url); got != tc.want {
				t.Errorf("slug(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
