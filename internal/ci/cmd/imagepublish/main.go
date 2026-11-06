// Command imagepublish runs the images workflow's registry steps
// (gt-gg9hv.62): push both images at one green commit and publish each
// registry digest, or check the commit label both images carry. The workflow
// at deploy/ci/images.workflow.yml calls it; the logic and its refusal tests
// live in internal/ci/imagepublish.
//
//	imagepublish push  -registry <host:port> -owner <account> -daemon <name> -polecat <name> -tag <sha12> -sha <full sha>
//	imagepublish check -registry <host:port> -owner <account> -daemon <name> -polecat <name> -tag <sha12> -sha <full sha>
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/ci/imagepublish"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "imagepublish: give a subcommand, push or check")
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("imagepublish "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := imagepublish.Options{Run: execRunner, Out: stdout, Err: stderr}
	fs.StringVar(&o.Registry, "registry", "", "the Forgejo host:port the images are pushed to")
	fs.StringVar(&o.Owner, "owner", "", "the registry account that owns the images")
	fs.StringVar(&o.Daemon, "daemon", "", "the daemon image's name")
	fs.StringVar(&o.Polecat, "polecat", "", "the polecat image's name")
	fs.StringVar(&o.Tag, "tag", "", "the twelve-character tag both images carry")
	fs.StringVar(&o.SHA, "sha", "", "the full green commit both images must carry")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	var err error
	switch sub {
	case "push":
		err = imagepublish.Push(o)
	case "check":
		err = imagepublish.Check(o)
	default:
		fmt.Fprintf(stderr, "imagepublish: unknown subcommand %q, want push or check\n", sub)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// execRunner runs one command through the container engine and captures both
// streams.
func execRunner(c imagepublish.Command) imagepublish.Result {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var res imagepublish.Result
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.Exit = exit.ExitCode()
		} else {
			res.Exit = -1
		}
	}
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	return res
}
