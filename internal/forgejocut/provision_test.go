package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
)

// The repository, API root, token file and config home a provision step's tests
// run against. The token is the fake's placeholder: the only value the fake
// accepts, and the value every assertion reports by name rather than printing.
const (
	provisionRepoName = "acme/widget"
	provisionAPI      = forgejotest.APIRoot + forgejotest.APIPath
	provisionTokenKey = forgejotest.AdminToken
	provisionTokenEnv = "FORGEJO_ADMIN_TOKEN=" + provisionTokenKey + "\n"
	provisionToken    = "/fixture/admin.env"
	provisionConfig   = "/fixture/config"
)

// provisionOptions is the cutover's options a provision step reads: the repository
// it provisions, the protected branch and the API root it names.
func provisionOptions(dryRun bool) *Options {
	return &Options{
		Rig:            "gastown",
		Repo:           provisionRepoName,
		MainBranch:     "main",
		APIURL:         provisionAPI,
		AdminTokenFile: provisionToken,
		DryRun:         dryRun,
	}
}

// provisionGetenv is the environment seam a provision step's tests read: the config
// home the token files go under, and nothing else.
func provisionGetenv(name string) string {
	if name == "XDG_CONFIG_HOME" {
		return provisionConfig
	}
	return ""
}

// provisionReadFile is the token file reader a provision step's tests read: the
// stand-in admin token file, and an absent file for any other path.
func provisionReadFile(path string) ([]byte, error) {
	if path == provisionToken {
		return []byte(provisionTokenEnv), nil
	}
	return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

// provisionCall is one recorded call of the provisioning seam.
type provisionCall struct {
	ctx  context.Context
	opts *forgejoprov.Options
	env  forgejoprov.Env
}

// newProvisionCutover is a cutover whose provisioning run is recorded instead of
// run, with the environment seam the admin token resolves through.
func newProvisionCutover(dryRun bool) (*Cutover, *[]string, *[]provisionCall) {
	logs := &[]string{}
	calls := &[]provisionCall{}
	return &Cutover{
		Opts: provisionOptions(dryRun),
		Log:  func(line string) { *logs = append(*logs, line) },
		ProvisionRun: func(ctx context.Context, opts *forgejoprov.Options, env forgejoprov.Env) int {
			*calls = append(*calls, provisionCall{ctx: ctx, opts: opts, env: env})
			return forgejoprov.ExitOK
		},
		ProvisionEnv: forgejoprov.Env{Getenv: provisionGetenv, ReadFile: provisionReadFile},
	}, logs, calls
}

// TestProvisionCallsTheProvisionerOnce pins the step's whole contract with the run
// counted: one call, with the repository, the protected branch, the API root, the
// cutover's token file and its dry run, over the cutover's own environment seam.
// Calling the run twice, dropping a value or dropping a seam turns it red.
func TestProvisionCallsTheProvisionerOnce(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		dryRun bool
	}{
		{name: "run"},
		{name: "dry run", dryRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, logs, calls := newProvisionCutover(tt.dryRun)
			if err := c.Provision(); err != nil {
				t.Fatalf("Provision() = %v, want nil", err)
			}
			if len(*calls) != 1 {
				t.Fatalf("the provisioner ran %d times, want once", len(*calls))
			}
			call := (*calls)[0]
			if call.ctx == nil {
				t.Error("the provisioning run got no context")
			}
			got := call.opts
			if !slices.Equal(got.Repos, []string{provisionRepoName}) {
				t.Errorf("the provisioner's repositories = %q, want %q", got.Repos, []string{provisionRepoName})
			}
			values := []struct {
				field, got, want string
			}{
				{"main branch", got.MainBranch, "main"},
				{"API root", got.APIURL, provisionAPI},
				{"admin token file", got.AdminTokenFile, provisionToken},
			}
			for _, v := range values {
				if v.got != v.want {
					t.Errorf("the provisioner's %s = %q, want %q", v.field, v.got, v.want)
				}
			}
			if got.DryRun != tt.dryRun {
				t.Errorf("the provisioner's dry run = %v, want %v", got.DryRun, tt.dryRun)
			}
			// The cutover names no roles, so the provisioner's own defaults are what
			// the script's run provisioned.
			if !slices.Equal(got.Roles, forgejoprov.DefaultRoles()) {
				t.Errorf("the provisioner's roles = %q, want its defaults %q", got.Roles, forgejoprov.DefaultRoles())
			}
			// The cutover's environment seam is the run's: the token file and the
			// config dir are read through it, never through the process environment.
			if dir := call.env.Getenv("XDG_CONFIG_HOME"); dir != provisionConfig {
				t.Errorf("the provisioner's config home = %q, want the cutover's seam %q", dir, provisionConfig)
			}
			data, err := call.env.ReadFile(provisionToken)
			if err != nil || string(data) != provisionTokenEnv {
				t.Errorf("the provisioner's token file read = %q, %v, want the cutover's own seam", data, err)
			}
			// The provisioner's progress lines join the cutover's log, one line each.
			if call.env.Log == nil {
				t.Fatal("the provisioning run got no log writer")
			}
			if _, err := call.env.Log.Write([]byte("forgejo-provision: progress\n")); err != nil {
				t.Fatalf("writing a progress line = %v, want nil", err)
			}
			want := []string{
				"provisioning " + provisionRepoName + ": bots, access and branch protection",
				"forgejo-provision: progress",
			}
			if !slices.Equal(*logs, want) {
				t.Fatalf("the run logged %q, want %q", *logs, want)
			}
		})
	}
}

// TestProvisionPropagatesFailure pins that a run which does not finish stops the
// cutover with an error that names the step and the exit status, while the run's own
// failure line — the one an operator acts on — is already in the log. The admin
// token appears in neither. Ignoring the exit code turns it red.
func TestProvisionPropagatesFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		dryRun bool
	}{
		{name: "run"},
		{name: "dry run", dryRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, logs, calls := newProvisionCutover(tt.dryRun)
			c.ProvisionRun = func(_ context.Context, _ *forgejoprov.Options, env forgejoprov.Env) int {
				*calls = append(*calls, provisionCall{})
				// The provisioner reports its own failure before it returns.
				fmt.Fprint(env.Log, "forgejo-provision: the token was refused\n")
				return forgejoprov.ExitFailure
			}
			err := c.Provision()
			if err == nil {
				t.Fatal("Provision() = nil, want an error when the provisioning run fails")
			}
			if len(*calls) != 1 {
				t.Errorf("the provisioner ran %d times, want once", len(*calls))
			}
			wantErr := "the provisioning run of " + provisionRepoName + " did not finish (exit 1)"
			if err.Error() != wantErr {
				t.Errorf("Provision() error = %q, want %q", err, wantErr)
			}
			want := []string{
				"provisioning " + provisionRepoName + ": bots, access and branch protection",
				"forgejo-provision: the token was refused",
			}
			if !slices.Equal(*logs, want) {
				t.Fatalf("the run logged %q, want %q", *logs, want)
			}
			// The secret rule over the captured output: failure messages name the
			// surface, never a value, and the bead forbids the token in a test's
			// output.
			if strings.Contains(err.Error(), provisionTokenKey) {
				t.Fatal("the returned error carries the admin token")
			}
			if strings.Contains(strings.Join(*logs, "\n"), provisionTokenKey) {
				t.Fatal("a log line carries the admin token")
			}
		})
	}
}

// TestProvisionRefusesACommandLineTheProvisionerRejects pins the guard on a command
// line the cutover itself would never have produced: a cutover built without its
// parse (an API root the provisioner cannot address) stops before the run, with an
// error naming the refused command line, and the provisioner is never called.
// Dropping the parse error check turns it red.
func TestProvisionRefusesACommandLineTheProvisionerRejects(t *testing.T) {
	t.Parallel()
	c, logs, calls := newProvisionCutover(false)
	c.Opts.APIURL = "not-a-url"
	err := c.Provision()
	if err == nil {
		t.Fatal("Provision() = nil, want the provisioner's refusal")
	}
	if !strings.HasPrefix(err.Error(), "the provisioning command line was refused: ") {
		t.Fatalf("Provision() error = %q, want the refused command line named", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("the provisioner ran %d times over a refused command line, want none", len(*calls))
	}
	if len(*logs) != 0 {
		t.Fatalf("the run logged %q over a refused command line, want nothing", *logs)
	}
}

// TestProvisionRefusesAnUnwiredEnvironment pins that a cutover whose ProvisionEnv has
// no Getenv stops before the run and before any read of the process environment:
// the admin token must resolve through the cutover's seam, never through a fallback.
// Dropping the nil check turns it red, because the run is then called.
func TestProvisionRefusesAnUnwiredEnvironment(t *testing.T) {
	t.Parallel()
	c, logs, calls := newProvisionCutover(false)
	c.ProvisionEnv = forgejoprov.Env{}
	err := c.Provision()
	if !errors.Is(err, errProvisionEnvUnwired) {
		t.Fatalf("Provision() = %v, want the unwired environment refused", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("the provisioner ran %d times with no environment seam, want none", len(*calls))
	}
	if len(*logs) != 0 {
		t.Fatalf("the run logged %q with no environment seam, want nothing", *logs)
	}
}

// provisionFiles is the in-memory filesystem a provisioning run writes its token
// files through: each path written, with the data and the mode it was given.
type provisionFiles struct {
	written map[string][]byte
	modes   map[string]fs.FileMode
}

func newProvisionFiles() *provisionFiles {
	return &provisionFiles{written: map[string][]byte{}, modes: map[string]fs.FileMode{}}
}

func (f *provisionFiles) WriteFileAtomic(path string, data []byte, mode fs.FileMode) error {
	f.written[path] = append([]byte(nil), data...)
	f.modes[path] = mode
	return nil
}

func (f *provisionFiles) Chmod(path string, mode fs.FileMode) error {
	if _, ok := f.written[path]; !ok {
		return &fs.PathError{Op: "chmod", Path: path, Err: fs.ErrNotExist}
	}
	f.modes[path] = mode
	return nil
}

func (f *provisionFiles) Stat(path string) (fs.FileInfo, error) {
	if _, ok := f.written[path]; !ok {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return provisionInfo{mode: f.modes[path]}, nil
}

// provisionInfo is the fs.FileInfo of a token file the in-memory filesystem holds.
type provisionInfo struct{ mode fs.FileMode }

func (provisionInfo) Name() string        { return "" }
func (provisionInfo) Size() int64         { return 0 }
func (i provisionInfo) Mode() fs.FileMode { return i.mode }
func (provisionInfo) ModTime() time.Time  { return time.Time{} }
func (provisionInfo) IsDir() bool         { return false }
func (provisionInfo) Sys() any            { return nil }

// provisionFakeCutover is a cutover over an in-process Forgejo: the real
// forgejoprov.Run, the fake's transport, and an in-memory filesystem for the token
// files, with the config home at provisionConfig. The repository is one the fake
// holds, and the viewer bot exists, so the dashboard's read grant is made.
func provisionFakeCutover(t *testing.T, dryRun bool) (*Cutover, *forgejotest.Fake, *[]string, *provisionFiles) {
	t.Helper()
	fake := forgejotest.New()
	fake.AddRepo(provisionRepoName)
	fake.ViewerExists = true
	files := newProvisionFiles()
	logs := &[]string{}
	return &Cutover{
		Opts: provisionOptions(dryRun),
		Log:  func(line string) { *logs = append(*logs, line) },
		ProvisionEnv: forgejoprov.Env{
			Getenv:   provisionGetenv,
			ReadFile: provisionReadFile,
			Files:    files,
			HTTP:     &http.Client{Transport: fake},
		},
	}, fake, logs, files
}

// TestProvisionInProcessAgainstTheFake runs the real provisioner against the
// in-process Forgejo: no child process, no socket and no argv. The observable
// result is the state the fake is left in — each bot's access, the viewer's read
// grant, the protected main branch, and a mode-600 token file per role in the
// cutover's config home — and, over the captured output, that no token value is in
// any log line.
func TestProvisionInProcessAgainstTheFake(t *testing.T) {
	t.Parallel()
	c, fake, logs, files := provisionFakeCutover(t, false)
	if err := c.Provision(); err != nil {
		t.Fatalf("Provision() = %v, want the fake provisioned and nil", err)
	}

	for _, tt := range []struct{ bot, want string }{
		{"bot-polecat", "write"},
		{"bot-landing", "write"},
		{"bot-viewer", "read"},
	} {
		if got := fake.Permission(provisionRepoName, tt.bot); got != tt.want {
			t.Errorf("%s holds %q access to the repository after the run, want %q", tt.bot, got, tt.want)
		}
	}
	rule := fake.Rule(provisionRepoName, "main")
	if rule == nil {
		t.Fatal("the repository's main branch is not protected after the run")
	}
	if got := fmt.Sprint(rule["merge_whitelist_usernames"]); got != "[bot-landing]" {
		t.Errorf("the main rule's merge whitelist = %s, want the landing bot alone", got)
	}
	if violations := fake.Violations(); len(violations) != 0 {
		t.Errorf("%d request(s) reached the fake without the admin token", len(violations))
	}
	for _, role := range []string{"polecat", "landing", "registry"} {
		path := filepath.Join(provisionConfig, "gt", "forgejo-"+role+".env")
		data, ok := files.written[path]
		if !ok {
			t.Fatalf("the %s token file was not written", role)
		}
		if !strings.Contains(string(data), "FORGEJO_TOKEN=") {
			t.Errorf("the %s token file holds no FORGEJO_TOKEN line", role)
		}
		if mode := files.modes[path]; mode != 0o600 {
			t.Errorf("the %s token file mode = %v, want 0600", role, mode)
		}
	}

	// The secret rule over the captured output: no admin token and no minted bot
	// token reaches a log line.
	joined := strings.Join(*logs, "\n")
	if strings.Contains(joined, provisionTokenKey) {
		t.Fatal("a log line carries the admin token")
	}
	for _, minted := range fake.MintedValues() {
		if minted != "" && strings.Contains(joined, minted) {
			t.Fatal("a log line carries a minted bot token")
		}
	}
}

// TestProvisionDryRunSendsNoWrite pins that the cutover's dry run reaches the
// provisioner: every request the fake sees is a read, no bot gains access, no token
// file is written, and the run still reports what it would have done. Dropping
// --dry-run from the provisioner's command line turns it red.
func TestProvisionDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	c, fake, logs, files := provisionFakeCutover(t, true)
	if err := c.Provision(); err != nil {
		t.Fatalf("Provision() = %v, want a dry run that reads and returns nil", err)
	}
	if writes := fake.Writes(); len(writes) != 0 {
		t.Fatalf("the dry run sent %d write(s), want none", len(writes))
	}
	if got := fake.Permission(provisionRepoName, "bot-polecat"); got != "" {
		t.Errorf("the dry run left the repository access at %q, want no access", got)
	}
	if len(files.written) != 0 {
		t.Errorf("the dry run wrote %d token file(s), want none", len(files.written))
	}
	// The provisioner reports what it would have done on its own progress lines,
	// which carry its documented prefix; the cutover's line is not one of them.
	var provisionerLines int
	for _, line := range *logs {
		if strings.HasPrefix(line, "forgejo-provision: ") {
			provisionerLines++
		}
	}
	if provisionerLines == 0 {
		t.Fatalf("the dry run reported nothing on the provisioner's lines:\n%s", strings.Join(*logs, "\n"))
	}
}
