// Command forgejo-sources-secret stores the SOURCES_TOKEN Actions secret on
// sloan/gastown without any person seeing the token. It mints a read:repository
// token for bot-viewer, checks that the token reads four repositories, and stores
// it only when all four answer 200. It exits 0 when the secret is stored or a dry
// run finishes, 1 when a step fails and 2 when the command line is refused.
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
// before the secret is stored.
var readRepos = []string{"beads", "om", "hm", "devops"}

// noReply is the status a read records when the request got no HTTP reply at all. No
// HTTP status is 0, so it marks a transport failure and is never printed as a status.
const noReply = 0

// revokeTimeout bounds the revoke a refusal or an interrupt makes after the mint.
const revokeTimeout = 30 * time.Second

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
	name := tokenName(env.Now())
	if opts.DryRun {
		printPlan(stdout, name)
		return forgejoprov.ExitOK
	}
	admin := &forgejoprov.Client{API: opts.APIURL, HTTP: env.HTTP, Token: secret, Ctx: ctx}

	value, err := admin.MintToken(viewerAccount, name, []string{tokenScope})
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: could not mint %s for %s: %v\n", name, viewerAccount, err)
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
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if opts.AdminTokenFile == "" {
		return options{}, errors.New("--admin-token-file is required")
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

// readRepoStatus asks for one repository with the minted token and returns the status
// alone: the reply body is never read into the command's output.
func readRepoStatus(reader *forgejoprov.Client, repo string) (int, error) {
	path := "/repos/" + url.PathEscape(secretOwner) + "/" + url.PathEscape(repo)
	status, _, err := reader.Do(http.MethodGet, path, nil)
	return status, err
}

// revokeMinted revokes the token the run minted. The revoke runs on a context detached
// from the run's, with its own timeout, so a signal that canceled the run cannot stop
// it and leave the token live. A revoke that fails is reported by name, since that
// token is then still live and the operator must remove it.
func revokeMinted(ctx context.Context, admin *forgejoprov.Client, name string, stderr io.Writer) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	revoker := *admin
	revoker.Ctx = detached
	if err := revoker.RevokeToken(viewerAccount, name); err != nil {
		fmt.Fprintf(stderr, "forgejo-sources-secret: could not revoke the minted token %s, so it is still live: revoke it in the Forgejo UI (%v)\n", name, err)
		return
	}
	fmt.Fprintf(stderr, "forgejo-sources-secret: revoked the minted token %s\n", name)
}
