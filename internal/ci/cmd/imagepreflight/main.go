// Command imagepreflight runs the images workflow's preflight
// (gt-gg9hv.62): the ci-base runner carries jq, go, docker and git, jq is
// 1.7.1, every digest-pinned image still has every layer its registry serves,
// the mango pin names a source on the Forgejo host, and GITHUB_SHA is the
// full green commit. On success it appends SHA= and TAG= to $GITHUB_ENV for
// the later steps. The workflow at deploy/ci/images.workflow.yml calls it;
// the checks and their refusal tests live in internal/ci/preflight.
//
//	imagepreflight -sha <full sha> -mango-url <url> -mango-commit <full sha> -host <host:port> -env-out <$GITHUB_ENV>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/ci/preflight"
	"github.com/steveyegge/gastown/internal/pincheck"
	"github.com/steveyegge/gastown/internal/templates"
)

const (
	// pinnedPlatform is the platform the images workflow builds for, so the
	// pins are checked at the one platform their build pulls.
	pinnedPlatform = "linux/arm64"
	// pinCheckTimeout bounds the whole pin check, so a registry that never
	// answers fails the preflight rather than holding the job past the build
	// it guards. The HTTP registry bounds each request; this bounds them all.
	pinCheckTimeout = 2 * time.Minute
	// hubHost is the registry a reference without a registry host names:
	// docker resolves such a reference at Docker Hub, whose API host is
	// registry-1.docker.io. docker.io itself redirects there, and the
	// registry's redirect rule refuses a change of host.
	hubHost = "registry-1.docker.io"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, exec.LookPath, func() (string, error) {
		return jqVersion("jq")
	}, pincheck.HTTP{}))
}

// run is the command, with the tool lookup, the version read and the registry
// injected so a test drives every branch of it without starting a tool or
// opening a socket.
func run(args []string, stdout, stderr io.Writer, lookPath func(string) (string, error), version func() (string, error), registry pincheck.Registry) int {
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
		PinCheck:    pinCheck(registry),
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

// pinCheck is the preflight's pin check over reg, bounded by pinCheckTimeout.
// It asks about every image templates pins by digest, at pinnedPlatform, so a
// pin added to templates.DigestPinnedImages is checked without editing here.
func pinCheck(registry pincheck.Registry) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), pinCheckTimeout)
		defer cancel()
		return checkPinnedImages(ctx, registry, templates.DigestPinnedImages())
	}
}

// checkPinnedImages refuses the build at the first pinned image in refs whose
// layer the registry no longer serves, naming the image as templates pins it
// and each dead digest with its position. A pin the registry cannot be asked
// about and an unreachable registry are refusals too: a pin nobody could check
// is not a pin that still holds, and a check given no pin at all verifies
// nothing.
func checkPinnedImages(ctx context.Context, registry pincheck.Registry, refs []string) error {
	if len(refs) == 0 {
		return errors.New("no digest-pinned image was named to check")
	}
	for _, pinned := range refs {
		ref, err := registryRef(pinned)
		if err != nil {
			return err
		}
		res, err := pincheck.Check(ctx, registry, ref, []string{pinnedPlatform})
		if err != nil {
			return fmt.Errorf("the pinned image %s cannot be checked: %w", pinned, err)
		}
		if len(res.Dead) > 0 {
			return fmt.Errorf("the pinned image %s has layers the registry no longer serves: %s", pinned, strings.Join(res.Dead, ", "))
		}
	}
	return nil
}

// registryRef is the reference pincheck.Check reads for a pinned image: the
// tag a pin keeps for its readers is dropped, because the registry addresses a
// manifest by its digest and a tag would otherwise travel inside the
// repository path, and a reference that names no registry host is qualified
// with Docker Hub's, the registry docker resolves it at.
func registryRef(pinned string) (string, error) {
	repo, digest, found := strings.Cut(pinned, "@")
	if !found || repo == "" || digest == "" {
		return "", fmt.Errorf("the pinned image %q is not repo@digest", pinned)
	}
	repo = strings.TrimSuffix(repo, tagOf(repo))
	host, name, hasRepo := strings.Cut(repo, "/")
	if hasRepo && name == "" {
		return "", fmt.Errorf("the pinned image %q names no repository", pinned)
	}
	if !hasRepo {
		return hubHost + "/library/" + repo + "@" + digest, nil
	}
	if host != "localhost" && !strings.ContainsAny(host, ".:") {
		return hubHost + "/" + repo + "@" + digest, nil
	}
	return repo + "@" + digest, nil
}

// tagOf answers the tag a repository part ends in, colon included, or "" when
// it ends in the repository name. A colon before the last slash belongs to the
// registry's port, not to a tag.
func tagOf(repo string) string {
	colon := strings.LastIndexByte(repo, ':')
	if colon < 0 || colon < strings.LastIndexByte(repo, '/') {
		return ""
	}
	return repo[colon:]
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
