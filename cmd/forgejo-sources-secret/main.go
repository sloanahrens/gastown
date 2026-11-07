// Command forgejo-sources-secret stores the SOURCES_TOKEN Actions secret on
// sloan/gastown without any person seeing the token. It mints a read:repository
// token for bot-viewer, checks that the token reads every repository the image
// build fetches, and stores it only when all of them answer 200. With --check it
// mints, revokes and stores nothing: it reads whether SOURCES_TOKEN is listed,
// whether bot-viewer is a collaborator on each read repository, and the names of its
// sources-token- tokens; checkState says what a check reads, prints and judges.
// The exit codes are 0 when the secret is stored, a dry run finishes or a check
// reads everything and every read repository is readable; 1 when a step fails, a
// read fails or a required repository is unreadable; and 2 when the command line
// is refused. An unlisted secret is a state a check reports, not a failure,
// because the check runs before the first real run, when the secret is absent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// The secret the command stores, the repository it lands on, and the account and
// scope of the token it mints.
const (
	secretOwner     = "sloan"
	secretRepo      = "gastown"
	secretName      = "SOURCES_TOKEN"
	viewerAccount   = forgejoprov.DefaultBotPrefix + "viewer"
	tokenScope      = "read:repository"
	tokenNamePrefix = "sources-token-"
	tokenNameLayout = "20060102-150405"
)

// readRepos are the repositories of the owner that the minted token must read
// before the secret is stored: the names Forgejo gives the sources the image build
// fetches with it. The first four are the URL names in deploy/daemon/image-pins and
// deploy/polecat/seed-urls; mango is the MANGO_URL repository of
// deploy/ci/images.workflow.yml, which no pin file names. A test holds this list
// to those files.
var readRepos = []string{"beads", "organic-mechanic", "history-man", "devops", "mango"}

// noReply is the status a read records when the request got no HTTP reply at all. No
// HTTP status is 0, so it marks a transport failure and is never printed as a status.
const noReply = 0

// revokeTimeout bounds the revoke a refusal or an interrupt makes after the mint.
const revokeTimeout = 30 * time.Second

// stillLiveLine is the one fixed line printed when a revoke fails and a read token stays
// live: the token's name and the account, never its value. It is printed last.
const stillLiveLine = "token still live: revoke token %s for %s by hand"

// env is what the command reads and writes through: the clock the token's name is
// taken from, the file the admin token is read from, and the HTTP client Forgejo is
// reached through (nil means the client's default, with a timeout).
type env struct {
	Now      func() time.Time
	ReadFile func(string) ([]byte, error)
	HTTP     *http.Client
}

// options is the parsed command line.
type options struct {
	APIURL         string
	AdminTokenFile string
	DryRun         bool
	Check          bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], env{Now: time.Now, ReadFile: os.ReadFile}, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the process minus os.Exit: a test hands it a command line, the env seam and
// the streams, and reads the code the process would have exited with.
func run(ctx context.Context, args []string, env env, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return forgejoprov.ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: %v\n", err)
		return forgejoprov.ExitUsage
	}
	secret, err := readAdminToken(env.ReadFile, opts.AdminTokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: %v\n", err)
		return forgejoprov.ExitFailure
	}
	admin := &forgejoprov.Client{API: opts.APIURL, HTTP: env.HTTP, Token: secret, Ctx: ctx}
	if opts.Check {
		return checkState(admin, stdout, stderr)
	}
	name := tokenName(env.Now())
	if opts.DryRun {
		printPlan(stdout, name)
		return forgejoprov.ExitOK
	}

	value, err := admin.MintToken(viewerAccount, name, []string{tokenScope})
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: could not mint %s for %s: %v\n", name, viewerAccount, err)
		// A mint that got no reply may have created the token, so it is revoked by name.
		// A refused mint (an HTTP status) created nothing and is not revoked.
		if errors.Is(err, forgejoprov.ErrNotDone) {
			revokeMinted(ctx, admin, name, stderr)
		}
		return forgejoprov.ExitFailure
	}

	// Every repository is read before any answer is judged, so the operator sees
	// each one that needs a collaborator grant, not only the first.
	reader := admin.WithToken(value)
	unreadable := map[string]int{}
	for _, repo := range readRepos {
		status, err := readRepoStatus(reader, repo)
		if err != nil {
			fmt.Fprintf(stdout, "%s/%s: request failed (no HTTP status)\n", secretOwner, repo)
			unreadable[repo] = noReply
			continue
		}
		fmt.Fprintf(stdout, "%s/%s: HTTP %d\n", secretOwner, repo, status)
		if status != http.StatusOK {
			unreadable[repo] = status
		}
	}
	if ctx.Err() != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: interrupted; nothing is stored on %s/%s (%s)\n", secretOwner, secretRepo, secretName)
		revokeMinted(ctx, admin, name, stderr)
		return forgejoprov.ExitFailure
	}
	if len(unreadable) > 0 {
		for _, repo := range readRepos {
			status, bad := unreadable[repo]
			if !bad {
				continue
			}
			switch {
			case status == noReply:
				fmt.Fprintf(stderr, "forgejo-sources-secret: %s/%s was not read (no HTTP reply, a transport failure); nothing is stored for it\n", secretOwner, repo)
			case status == http.StatusNotFound || status == http.StatusForbidden:
				fmt.Fprintf(stderr, "forgejo-sources-secret: %s/%s is not readable by %s; the operator should add %s as a read collaborator on %s/%s in the Forgejo UI, then run this again\n",
					secretOwner, repo, viewerAccount, viewerAccount, secretOwner, repo)
			default:
				fmt.Fprintf(stderr, "forgejo-sources-secret: %s/%s was not read (HTTP %d); nothing is stored for it\n", secretOwner, repo, status)
			}
		}
		fmt.Fprintf(stderr, "forgejo-sources-secret: nothing stored on %s/%s (%s)\n", secretOwner, secretRepo, secretName)
		revokeMinted(ctx, admin, name, stderr)
		return forgejoprov.ExitFailure
	}

	if err := admin.SetRepoSecret(ctx, secretOwner, secretRepo, secretName, value); err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: could not store %s on %s/%s: %v\n", secretName, secretOwner, secretRepo, err)
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "forgejo-sources-secret: the run was interrupted during the store")
		}
		revokeMinted(ctx, admin, name, stderr)
		return forgejoprov.ExitFailure
	}
	fmt.Fprintf(stdout, "RESULT: stored %s on %s/%s\n", secretName, secretOwner, secretRepo)
	return forgejoprov.ExitOK
}

// parseOptions reads the command line. A flag error is printed by the flag set,
// which also prints the usage; flag.ErrHelp is returned for -h.
func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("forgejo-sources-secret", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.APIURL, "api-url", forgejoprov.DefaultAPIURL, "Forgejo API root")
	fs.StringVar(&opts.AdminTokenFile, "admin-token-file", "", "file holding FORGEJO_ADMIN_TOKEN=... (the broker hands it over)")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print the plan; mint and store nothing")
	fs.BoolVar(&opts.Check, "check", false, "read the secret, the collaborators and the tokens; mint, revoke and store nothing")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if opts.AdminTokenFile == "" {
		return options{}, errors.New("--admin-token-file is required")
	}
	if opts.Check && opts.DryRun {
		return options{}, errors.New("--check and --dry-run cannot be combined")
	}
	return opts, nil
}

// noEnv is the environment the admin token is not read from: the command takes the
// token from its --admin-token-file alone.
func noEnv(string) string { return "" }

// readAdminToken reads the site-admin token from path, the way forgejo-provision
// does, with the environment left out.
func readAdminToken(readFile func(string) ([]byte, error), path string) (forgejoprov.Secret, error) {
	secret, err := forgejoprov.ResolveAdminToken(noEnv, readFile, path)
	if errors.Is(err, forgejoprov.ErrNoAdminToken) {
		return forgejoprov.Secret{}, fmt.Errorf("no admin token in %s", path)
	}
	return secret, err
}

// tokenName is the name of the token a run mints: the UTC time it was minted at.
func tokenName(now time.Time) string {
	return tokenNamePrefix + now.UTC().Format(tokenNameLayout)
}

// printPlan states what a real run would do, without any request.
func printPlan(stdout io.Writer, name string) {
	fmt.Fprintln(stdout, "dry run: nothing is minted, read or stored")
	fmt.Fprintf(stdout, "account: %s\n", viewerAccount)
	fmt.Fprintf(stdout, "scope: %s\n", tokenScope)
	fmt.Fprintf(stdout, "token name: %s\n", name)
	for _, repo := range readRepos {
		fmt.Fprintf(stdout, "read check: %s/%s\n", secretOwner, repo)
	}
	fmt.Fprintf(stdout, "target: %s/%s secret %s\n", secretOwner, secretRepo, secretName)
}

// mintedName matches a token name this command mints. Only names of that shape are
// printed: any other name is free text from the server, so it is counted and not shown.
var mintedName = regexp.MustCompile(`^` + regexp.QuoteMeta(tokenNamePrefix) + `[0-9]{8}-[0-9]{6}$`)

// checkState reads the state a real run depends on and prints it: whether SOURCES_TOKEN is
// listed on sloan/gastown, the status of bot-viewer's collaborator check on each read
// repository, and the sources-token- tokens bot-viewer holds. It sends GETs only, so it
// mints, revokes and stores nothing. Every read is made before any is judged, so the
// operator sees each one. A collaborator check that answers 204 is readable; 404 is a
// required repository that is not readable, and the real run refuses on it. Any other
// status, or no reply, is a read that failed. An unlisted secret is printed as a state
// and is not a failure: the check runs before the first real run, when the secret is
// legitimately absent. It returns the exit code the command header names for a check:
// ExitOK when every read answered and every read repository is readable, ExitFailure when
// a read failed or a required repository is unreadable. A failure's detail goes to stderr
// and carries no reply body.
func checkState(admin *forgejoprov.Client, stdout, stderr io.Writer) int {
	code := forgejoprov.ExitOK

	secrets, err := admin.ListRepoSecretNames(secretOwner, secretRepo)
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: could not list the Actions secrets of %s/%s: %v\n", secretOwner, secretRepo, err)
		code = forgejoprov.ExitFailure
	} else {
		state := "not listed"
		if slices.Contains(secrets, secretName) {
			state = "listed"
		}
		fmt.Fprintf(stdout, "%s/%s secret %s: %s\n", secretOwner, secretRepo, secretName, state)
	}

	for _, repo := range readRepos {
		status, err := collaboratorStatus(admin, repo)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "forgejo-sources-secret: the collaborator check on %s/%s got no HTTP reply\n", secretOwner, repo)
			code = forgejoprov.ExitFailure
		case status == http.StatusNoContent:
			fmt.Fprintf(stdout, "%s/%s: readable by %s (HTTP %d)\n", secretOwner, repo, viewerAccount, status)
		case status == http.StatusNotFound:
			fmt.Fprintf(stdout, "%s/%s: not readable by %s (HTTP %d)\n", secretOwner, repo, viewerAccount, status)
			fmt.Fprintf(stderr, "forgejo-sources-secret: %s/%s is not readable by %s; a real run refuses on it, so add %s as a read collaborator on %s/%s first\n",
				secretOwner, repo, viewerAccount, viewerAccount, secretOwner, repo)
			code = forgejoprov.ExitFailure
		default:
			fmt.Fprintf(stderr, "forgejo-sources-secret: the collaborator check on %s/%s answered HTTP %d\n", secretOwner, repo, status)
			code = forgejoprov.ExitFailure
		}
	}

	names, err := admin.ListTokenNames(viewerAccount)
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: could not list the tokens of %s: %v\n", viewerAccount, err)
		code = forgejoprov.ExitFailure
	} else {
		printTokenNames(stdout, names)
	}

	if code == forgejoprov.ExitOK {
		fmt.Fprintln(stdout, "RESULT: checked the secret, the collaborators and the tokens; nothing minted, revoked or stored")
	} else {
		fmt.Fprintln(stderr, "forgejo-sources-secret: the check failed a read or found a required repository unreadable; the lines above say which")
	}
	return code
}

// collaboratorStatus asks whether viewerAccount is a collaborator on the read repository
// repo, with the admin token, and returns the status alone.
func collaboratorStatus(admin *forgejoprov.Client, repo string) (int, error) {
	path := "/repos/" + url.PathEscape(secretOwner) + "/" + url.PathEscape(repo) + "/collaborators/" + url.PathEscape(viewerAccount)
	status, _, err := admin.Do(http.MethodGet, path, nil)
	return status, err
}

// printTokenNames prints each token name of this command's shape, and counts the other
// sources-token- names without printing them.
func printTokenNames(stdout io.Writer, names []string) {
	printed, other := 0, 0
	for _, name := range names {
		if !strings.HasPrefix(name, tokenNamePrefix) {
			continue
		}
		if !mintedName.MatchString(name) {
			other++
			continue
		}
		printed++
		fmt.Fprintf(stdout, "%s token: %s\n", viewerAccount, name)
	}
	if printed+other == 0 {
		fmt.Fprintf(stdout, "%s: no %s tokens listed\n", viewerAccount, tokenNamePrefix)
	}
	if other > 0 {
		fmt.Fprintf(stdout, "%s: %d more %s token(s) do not have a name this command mints; their names are not printed\n", viewerAccount, other, tokenNamePrefix)
	}
}

// readRepoStatus asks for one repository with the minted token and returns the status
// alone: the reply body is never read into the command's output.
func readRepoStatus(reader *forgejoprov.Client, repo string) (int, error) {
	path := "/repos/" + url.PathEscape(secretOwner) + "/" + url.PathEscape(repo)
	status, _, err := reader.Do(http.MethodGet, path, nil)
	return status, err
}

// revokeMinted revokes the token the run minted. The revoke runs on a context detached
// from the run's, with its own timeout, so a signal that canceled the run cannot stop
// it and leave the token live. A revoke that finds no token has nothing live to revoke.
// A revoke that fails leaves the token live, so the last line names it and the operator
// must remove it by hand; that line is the only signal a live read token remains.
func revokeMinted(ctx context.Context, admin *forgejoprov.Client, name string, stderr io.Writer) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	revoker := *admin
	revoker.Ctx = detached
	err := revoker.RevokeToken(viewerAccount, name)
	switch {
	case err == nil:
		fmt.Fprintf(stderr, "forgejo-sources-secret: revoked the minted token %s\n", name)
	case errors.Is(err, forgejoprov.ErrNoToken):
		fmt.Fprintf(stderr, "forgejo-sources-secret: no token %s is on the server, so none is live\n", name)
	default:
		fmt.Fprintf(stderr, stillLiveLine+"\n", name, viewerAccount)
	}
}
