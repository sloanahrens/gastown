package forgejoprov

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
)

// Env is what Run reads and writes beside its options: the environment the admin
// token and the defaults come from, the file the admin token is read through, the
// progress lines, and the seams a test replaces. Run itself applies the defaults for
// Getenv (os.Getenv), ReadFile (os.ReadFile) and Log (discarded). HTTP, Files and Random
// are passed through unchanged, and the type that uses each applies its own default when
// it is nil: the client's default HTTP client with a request timeout, the real
// filesystem, and crypto/rand.
type Env struct {
	// Getenv reads the environment the admin token resolves from; nil means os.Getenv.
	Getenv func(string) string
	// ReadFile reads the admin token file; nil means os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// Log receives the progress lines and the failure, each prefixed "forgejo-provision:";
	// nil discards them.
	Log io.Writer
	// HTTP sends the requests; nil is left to the client, which uses its default client.
	HTTP *http.Client
	// Files is the filesystem the token files are written through; nil is left to the
	// provisioner, which uses the real one.
	Files Files
	// Random is the source of a new bot's password; nil is left to the provisioner, which
	// uses crypto/rand.
	Random io.Reader
}

// Run provisions what opts names, in the order the shell script did: each role's bot
// and token, then for each repository its access, the viewer grant, the main rule and
// the removal of the candidate-branch rule. It returns ExitOK when the run finished (a
// dry run included, which sends no write) and ExitFailure when a step failed. Progress
// and failures go to env.Log; no admin or minted token reaches it.
func Run(ctx context.Context, opts *Options, env Env) int {
	logOut := env.Log
	if logOut == nil {
		logOut = io.Discard
	}
	getenv := env.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	readFile := env.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(logOut, "forgejo-provision: "+format+"\n", args...)
	}

	secret, err := ResolveAdminToken(getenv, readFile, opts.AdminTokenFile)
	if err != nil {
		logf("%v", err)
		return ExitFailure
	}
	client := &Client{
		API:    opts.APIURL,
		HTTP:   env.HTTP,
		Ctx:    ctx,
		Token:  secret,
		DryRun: opts.DryRun,
		Log:    func(line string) { logf("%s", line) },
	}
	p := &Provisioner{
		Client:          client,
		BotPrefix:       opts.BotPrefix,
		BotEmailDomain:  opts.BotEmailDomain,
		Random:          env.Random,
		TokenName:       opts.TokenName,
		WebURL:          opts.WebURL,
		ConfigDir:       opts.ConfigDir,
		Rotate:          opts.Rotate,
		ViewerRequested: slices.Contains(opts.Roles, "viewer"),
		Files:           env.Files,
	}
	if err := provision(p, opts); err != nil {
		logf("%v", err)
		return ExitFailure
	}
	if opts.DryRun {
		logf("dry run: no write was sent")
	} else {
		logf("provisioning complete")
	}
	return ExitOK
}

// provision runs the bot and token steps for every role, then the access and
// protection steps for every repository. A failure stops the run where it is: each
// step converges on its own, so a repeat picks up where this one stopped.
func provision(p *Provisioner, opts *Options) error {
	for _, role := range opts.Roles {
		if err := p.EnsureUser(role); err != nil {
			return err
		}
		if err := p.EnsureToken(role); err != nil {
			return err
		}
	}
	for _, repo := range opts.Repos {
		if err := provisionRepo(p, repo, opts); err != nil {
			return err
		}
	}
	return nil
}

// provisionRepo grants the repository access before it writes the protection: Forgejo
// drops a merge whitelist entry for a login that is not yet a collaborator, so the
// access has to come first (gt-fn9e6.23). The viewer grant is made here only when the
// role loop does not make it, so the access is never granted twice (gt-fn9e6.49).
func provisionRepo(p *Provisioner, repo string, opts *Options) error {
	for _, role := range opts.Roles {
		perm, ok, err := CollaboratorPermission(role)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := p.EnsureCollaborator(role, repo, perm); err != nil {
			return err
		}
	}
	if !p.ViewerRequested {
		if _, err := p.EnsureViewerAccess(repo); err != nil {
			return err
		}
	}
	if err := p.EnsureRule(repo, *opts); err != nil {
		return err
	}
	return p.EnsureNoRule(repo, opts.LandBranch)
}
