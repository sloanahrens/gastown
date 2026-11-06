// Command imagepreflight runs the images workflow's preflight
// (gt-gg9hv.62): the ci-base runner carries jq, go, docker and git, jq is
// 1.7.1, the mango pin names a source on the Forgejo host, and GITHUB_SHA is
// the full green commit. On success it appends SHA= and TAG= to $GITHUB_ENV
// for the later steps. The workflow at deploy/ci/images.workflow.yml calls it;
// the checks and their refusal tests live in internal/ci/preflight.
//
//	imagepreflight -sha <full sha> -mango-url <url> -mango-commit <full sha> -host <host:port> -env-out <$GITHUB_ENV>
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/ci/preflight"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, exec.LookPath, func() (string, error) {
		return jqVersion("jq")
	}))
}

// run is the command, with the tool lookup and the version read injected so a
// test drives every branch of it without starting a tool.
func run(args []string, stdout, stderr io.Writer, lookPath func(string) (string, error), version func() (string, error)) int {
	fs := flag.NewFlagSet("imagepreflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sha := fs.String("sha", "", "GITHUB_SHA, the green commit the job builds both images at")
	mangoURL := fs.String("mango-url", "", "the MANGO_URL repository variable: the mango source the daemon image carries")
	mangoCommit := fs.String("mango-commit", "", "the MANGO_COMMIT repository variable: the mango commit the daemon image carries")
	host := fs.String("host", "", "the Forgejo host:port the pins, the images and the mango source name")
	envOut := fs.String("env-out", "", "the file SHA= and TAG= are appended to, as $GITHUB_ENV")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *envOut == "" {
		fmt.Fprintln(stderr, "imagepreflight: -env-out is required, the path in $GITHUB_ENV")
		return 2
	}
	// Appending to the running file rather than truncating it keeps the lines
	// a step before this one wrote; the mode is the runner's own.
	f, err := os.OpenFile(*envOut, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "imagepreflight: opening %s: %v\n", *envOut, err)
		return 1
	}
	green, err := preflight.Check(preflight.Options{
		LookPath:    lookPath,
		Version:     version,
		SHA:         *sha,
		MangoURL:    *mangoURL,
		MangoCommit: *mangoCommit,
		Host:        *host,
		EnvOut:      f,
	})
	closeErr := f.Close()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if closeErr != nil {
		// A line the runner could not flush is a commit the later steps do not
		// get, so the step fails rather than building at an unset SHA.
		fmt.Fprintf(stderr, "imagepreflight: closing %s: %v\n", *envOut, closeErr)
		return 1
	}
	fmt.Fprintf(stdout, "preflight: the green commit %s, tag %s\n", green.SHA, green.Tag)
	return 0
}

// jqVersion runs the program at path as "jq --version" and answers what it
// printed. The output is the version alone, never a credential, and it does
// not reach an error: the caller names the command.
func jqVersion(path string) (string, error) {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
