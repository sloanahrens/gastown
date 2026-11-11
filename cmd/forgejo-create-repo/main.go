// Command forgejo-create-repo creates the private, empty repository OWNER/NAME on the
// broker's Forgejo, with default branch main and no initialisation, and seeds it with
// every file under a seed directory as one commit through the contents API. A dry run
// prints the plan and sends nothing. The exit codes are 0 when the repository is created
// and seeded or a dry run finishes; 1 when a step fails, or a seed or repository refusal
// applies; and 2 when the command line is refused. The admin token is read from
// GT_AUTH_FORGEJO_TOKEN, then FORGEJO_ADMIN_TOKEN, then the admin token file; it is never
// printed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/forgejocreate"
	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// usage is the --help text. It names every flag and where the token comes from.
const usage = `usage: forgejo-create-repo --repo OWNER/NAME --seed-dir DIR --author NAME [flags]

Creates the private, empty repository OWNER/NAME (default branch main, no
initialisation) and commits every file under DIR as one commit through the
contents API. A dry run prints the files and the commit message and sends nothing.

  --repo OWNER/NAME     the repository to create
  --seed-dir DIR        absolute path of the directory to seed it with; every file
                        under DIR is committed, and a file over 1 MiB is refused by name
  --author NAME         the name the seed commit's author and committer carry
  --dry-run             print the plan (files and commit message); send no write
  --api-url URL         Forgejo API root (default $FORGEJO_API_URL, else the built-in root)
  --admin-token-file F  file holding FORGEJO_ADMIN_TOKEN=... (default $FORGEJO_ADMIN_ENV);
                        optional, read only when no token is in the environment

The admin token comes from the environment first: GT_AUTH_FORGEJO_TOKEN (the auth
broker's), then FORGEJO_ADMIN_TOKEN. The token file is the fallback. No token is printed.

Exit codes: 0 when the repository is created and seeded or a dry run finishes; 1 when a
step fails or a refusal applies; 2 when the command line is refused.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], env{Getenv: os.Getenv, ReadFile: os.ReadFile}, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// env is what the command reads and writes through: the environment the token and the
// defaults come from, the file the token file is read from, and the HTTP client Forgejo is
// reached through (nil means the client's default, with a timeout).
type env struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	HTTP     *http.Client
}

// options is the parsed command line, with OWNER/NAME split out.
type options struct {
	Owner          string
	Name           string
	SeedDir        string
	Author         string
	DryRun         bool
	APIURL         string
	AdminTokenFile string
}

// run is the process minus os.Exit: a test hands it a command line, the env seam and the
// streams, and reads the code the process would have exited with.
func run(ctx context.Context, args []string, env env, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, env.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(stdout, "%s", usage)
		return forgejoprov.ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-create-repo: %v\n", err)
		fmt.Fprintln(stderr, "usage: forgejo-create-repo --help")
		return forgejoprov.ExitUsage
	}

	// The seed is read and checked before anything else, so a refused seed sends nothing.
	files, err := forgejocreate.LoadSeed(opts.SeedDir)
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-create-repo: seed refused, nothing is written: %v\n", err)
		return forgejoprov.ExitFailure
	}
	if opts.DryRun {
		printPlan(stdout, opts, files)
		return forgejoprov.ExitOK
	}

	secret, err := forgejoprov.ResolveAdminToken(env.Getenv, env.ReadFile, opts.AdminTokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-create-repo: %v\n", err)
		return forgejoprov.ExitFailure
	}
	client := &forgejoprov.Client{API: opts.APIURL, HTTP: env.HTTP, Token: secret, Ctx: ctx}

	if err := forgejocreate.CreateRepo(client, opts.Owner, opts.Name); err != nil {
		fmt.Fprintf(stderr, "forgejo-create-repo: could not create %s/%s: %v\n", opts.Owner, opts.Name, err)
		return forgejoprov.ExitFailure
	}
	fmt.Fprintf(stdout, "created private repository %s/%s (default branch %s, no initialisation)\n", opts.Owner, opts.Name, forgejocreate.Branch)

	sha, err := forgejocreate.Seed(client, opts.Owner, opts.Name, opts.Author, files)
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-create-repo: could not seed %s/%s: %v\n", opts.Owner, opts.Name, err)
		fmt.Fprintf(stderr, "forgejo-create-repo: %s/%s was created and is still empty; seed it by hand or delete it before a rerun, since a rerun refuses an existing repository\n", opts.Owner, opts.Name)
		return forgejoprov.ExitFailure
	}
	fmt.Fprintf(stdout, "RESULT: seeded %s/%s with %d files in commit %s\n", opts.Owner, opts.Name, len(files), sha)
	return forgejoprov.ExitOK
}

// parseOptions reads the command line and validates it. Every refusal is returned as an
// error for run to print with the usage hint; flag.ErrHelp is returned for -h or --help.
// The flag set prints nothing itself, so each message appears once.
func parseOptions(args []string, getenv func(string) string) (options, error) {
	var opts options
	var repo string
	fs := flag.NewFlagSet("forgejo-create-repo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&repo, "repo", "", "OWNER/NAME of the repository to create")
	fs.StringVar(&opts.SeedDir, "seed-dir", "", "absolute path of the directory to seed the repository with")
	fs.StringVar(&opts.Author, "author", "", "name the seed commit's author and committer carry")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print the plan; send no write")
	fs.StringVar(&opts.APIURL, "api-url", firstNonEmpty(getenv("FORGEJO_API_URL"), forgejoprov.DefaultAPIURL), "Forgejo API root")
	fs.StringVar(&opts.AdminTokenFile, "admin-token-file", getenv("FORGEJO_ADMIN_ENV"), "file holding FORGEJO_ADMIN_TOKEN=... (optional)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if repo == "" {
		return options{}, errors.New("--repo is required")
	}
	owner, name, err := forgejocreate.ValidateRepo(repo)
	if err != nil {
		return options{}, err
	}
	opts.Owner, opts.Name = owner, name
	if opts.SeedDir == "" {
		return options{}, errors.New("--seed-dir is required")
	}
	if !filepath.IsAbs(opts.SeedDir) {
		return options{}, fmt.Errorf("--seed-dir %q is relative; give an absolute path", opts.SeedDir)
	}
	if strings.TrimSpace(opts.Author) == "" || strings.ContainsAny(opts.Author, "\r\n") {
		return options{}, errors.New("--author is required and must be one line")
	}
	return opts, nil
}

// printPlan states what a real run would do: the repository, the author, the commit message
// and every file with its size. It prints no file contents and no token.
func printPlan(stdout io.Writer, opts options, files []forgejocreate.File) {
	fmt.Fprintln(stdout, "dry run: nothing is created, sent or written")
	fmt.Fprintf(stdout, "repository: %s/%s (private, default branch %s, no initialisation)\n", opts.Owner, opts.Name, forgejocreate.Branch)
	fmt.Fprintf(stdout, "author: %s\n", opts.Author)
	fmt.Fprintf(stdout, "message: %s\n", forgejocreate.CommitMessage(opts.Owner, opts.Name))
	fmt.Fprintf(stdout, "files (%d):\n", len(files))
	for _, f := range files {
		fmt.Fprintf(stdout, "  %s (%d bytes)\n", f.Path, len(f.Data))
	}
}

// firstNonEmpty returns the first of values that is not empty, or "" when all are.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
