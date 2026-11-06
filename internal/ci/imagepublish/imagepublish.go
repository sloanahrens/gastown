// Package imagepublish is the images workflow's two registry steps: push both
// images at one green commit and publish each registry digest, then read the
// commit label back from both images (gt-gg9hv.62). The workflow text in
// deploy/ci/images.workflow.yml only calls it, so every refusal here is driven
// by a unit test through the injected runner.
package imagepublish

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// digestPattern is the registry digest's shape: the deploy pins this form, and
// a local image id never matches it.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// digestFormat lists every repository digest docker holds for the image, one
// per line.
const digestFormat = `{{range .RepoDigests}}{{println .}}{{end}}`

// labelFormat prints the one label the deploy reads.
const labelFormat = `{{index .Config.Labels "org.gastown.gt-commit"}}`

// Command is one program invocation.
type Command struct {
	Name string
	Args []string
}

// Result is one program's exit: what it wrote to each stream and its status.
type Result struct {
	Stdout string
	Stderr string
	Exit   int
}

// Runner runs one command and answers with its output and status.
type Runner func(Command) Result

// Options is the job's registry values and the seams the steps run through.
type Options struct {
	// Docker is the container engine's program; empty means "docker".
	Docker string
	// Registry is the Forgejo host:port, Owner the account that owns the
	// images, and Daemon and Polecat the two image names.
	Registry, Owner, Daemon, Polecat string
	// Tag is the twelve-character commit tag both images carry; SHA is the
	// full green commit both commit labels must hold.
	Tag, SHA string
	// Run runs docker; a nil runner is refused rather than treated as success.
	Run Runner
	// Out takes the IMAGE lines the deploy reads; a nil writer is refused, so a
	// digest cannot be read and then dropped.
	Out io.Writer
	// Err takes a failed command's stderr for the operator; nil drops it.
	Err io.Writer
}

// validate refuses an option that would make a step report a success it did
// not establish.
func (o Options) validate() error {
	for _, field := range []struct{ name, value string }{
		{"registry", o.Registry},
		{"owner", o.Owner},
		{"daemon", o.Daemon},
		{"polecat", o.Polecat},
		{"tag", o.Tag},
		{"sha", o.SHA},
	} {
		if field.value == "" {
			return fmt.Errorf("imagepublish: %s is required", field.name)
		}
	}
	if o.Run == nil {
		return errors.New("imagepublish: no runner is wired")
	}
	if o.Out == nil {
		return errors.New("imagepublish: no output is wired")
	}
	return nil
}

// program is the container engine's program.
func (o Options) program() string {
	if o.Docker == "" {
		return "docker"
	}
	return o.Docker
}

// base is the image's repository name without a tag.
func (o Options) base(name string) string {
	return o.Registry + "/" + o.Owner + "/" + name
}

// images is the two images, in the order the job builds them.
func (o Options) images() []struct{ kind, name string } {
	return []struct{ kind, name string }{
		{"daemon", o.Daemon},
		{"polecat", o.Polecat},
	}
}

// docker runs one docker command. A failed command's stderr goes to the
// operator; the error names the command and its status, never the output.
func (o Options) docker(args ...string) (Result, error) {
	res := o.Run(Command{Name: o.program(), Args: args})
	if res.Exit != 0 {
		if o.Err != nil && res.Stderr != "" {
			fmt.Fprintf(o.Err, "%s", res.Stderr)
		}
		return res, fmt.Errorf("imagepublish: docker %s failed with status %d", args[0], res.Exit)
	}
	return res, nil
}

// Push pushes both images, reads each registry digest back, and prints the
// IMAGE line the deploy reads, only for an image whose digest the registry
// answered.
func Push(o Options) error {
	if err := o.validate(); err != nil {
		return err
	}
	for _, img := range o.images() {
		ref := o.base(img.name) + ":" + o.Tag
		if _, err := o.docker("push", ref); err != nil {
			return err
		}
		digest, err := o.registryDigest(o.base(img.name), ref)
		if err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "IMAGE %s %s %s\n", img.kind, o.Tag, digest)
	}
	return nil
}

// registryDigest pulls the pushed tag back and reads the digest the registry
// serves, never the local image id.
func (o Options) registryDigest(base, ref string) (string, error) {
	if _, err := o.docker("pull", "--quiet", ref); err != nil {
		return "", err
	}
	res, err := o.docker("image", "inspect", "--format", digestFormat, ref)
	if err != nil {
		return "", err
	}
	digest, err := digestFrom(base, res.Stdout)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// digestFrom is the one digest the image's RepoDigests lists for base.
func digestFrom(base, output string) (string, error) {
	var found []string
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), base+"@"); ok {
			found = append(found, rest)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("imagepublish: the registry listed %d digests for %s, want exactly one", len(found), base)
	}
	if !digestPattern.MatchString(found[0]) {
		return "", fmt.Errorf("imagepublish: the registry digest %q for %s is not sha256:<64 lowercase hex>", found[0], base)
	}
	return found[0], nil
}

// Check reads the commit label back from both built images and fails when
// either is not the green commit. It reads both first, so one job reports
// every image that is off the commit.
func Check(o Options) error {
	if err := o.validate(); err != nil {
		return err
	}
	var problems []string
	for _, img := range o.images() {
		ref := o.base(img.name) + ":" + o.Tag
		res, err := o.docker("image", "inspect", "--format", labelFormat, ref)
		if err != nil {
			return err
		}
		if got := strings.TrimSpace(res.Stdout); got != o.SHA {
			problems = append(problems, fmt.Sprintf("%s carries org.gastown.gt-commit=%q", ref, got))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("imagepublish: not both images are at the green commit %s: %s", o.SHA, strings.Join(problems, "; "))
	}
	return nil
}
