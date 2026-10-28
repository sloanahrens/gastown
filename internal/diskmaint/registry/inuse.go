package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// InUse is the set of images running pods and sandbox records name. A plan
// keeps every entry, so a prune never deletes an image something is running.
type InUse struct {
	Digests map[string]bool // manifest digests named by pods and records
	Tags    map[string]bool // "repo:tag" named by container image fields and records
	Repos   map[string]bool // every repository named
}

// podList is the part of `kubectl get pods -o json` that names images. Items
// is a pointer so a reply without the key is told apart from an empty list: a
// reply that names no pod list must not pass for "no pods running".
type podList struct {
	Items *[]pod `json:"items"`
}

type pod struct {
	Spec struct {
		Containers     []podContainer `json:"containers"`
		InitContainers []podContainer `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses     []podContainerStatus `json:"containerStatuses"`
		InitContainerStatuses []podContainerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type podContainer struct {
	Image string `json:"image"`
}

type podContainerStatus struct {
	Image   string `json:"image"`
	ImageID string `json:"imageID"`
}

// recordRefPattern finds the tokens of a sandbox record. It takes a whole run
// of reference characters, not a shape with optional parts, so a malformed
// digest reaches validation instead of being cut short at the bad character.
// refCandidate then decides which tokens are image references.
var recordRefPattern = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._:/@-]*`)

// imageSchemes prefix an image id in a pod status or a record; parseRef strips
// them.
var imageSchemes = []string{"docker-pullable://", "docker://"}

// recordRefs returns the image references a sandbox record names. Record files
// are session JSON, so their text also holds timestamps, paths and URLs; a
// token counts only when it is shaped like an image reference, and anything
// else is skipped whole. That keeps a URL or a path from refusing the read,
// which would be a refusal on a valid record. Each token that does count
// reaches parseRef whole, so a reference-shaped token with a bad digest or tag
// still refuses instead of passing as absent.
func recordRefs(text string) []string {
	tokens := recordRefPattern.FindAllString(text, -1)
	refs := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if refCandidate(tok) {
			refs = append(refs, tok)
		}
	}
	return refs
}

// refCandidate reports whether a record token is shaped like an image
// reference: it names a digest, carries a registry host with a port, or reads
// as repo:tag. Anything else (prose, a URL, a path) is not a reference and is
// left to be skipped.
func refCandidate(tok string) bool {
	s := tok
	for _, scheme := range imageSchemes {
		if rest, ok := strings.CutPrefix(s, scheme); ok {
			s = rest
			break
		}
	}
	if strings.Contains(s, "://") {
		return false // a URL names a location, not an image
	}
	if strings.HasPrefix(s, "sha256:") || strings.Contains(s, "@sha256:") {
		return true // image id or manifest digest: validated whole
	}
	if host, _, ok := strings.Cut(s, "/"); ok && registryHostPort(host) {
		return true // host:port/repo[:tag], the shape a record names
	}
	repo, tag := repoTags(s)
	return tag != "" && ValidRepo(repo) && ValidTag(tag)
}

// repoTags splits s at its host:port and its tag, the way parseRef does,
// without validating either part.
func repoTags(s string) (repo, tag string) {
	if at := strings.Index(s, "@"); at >= 0 {
		s = s[:at]
	}
	if host, rest, ok := strings.Cut(s, "/"); ok && registryHostPort(host) {
		s = rest
	}
	if colon := strings.LastIndex(s, ":"); colon >= 0 {
		tag = s[colon+1:]
		s = s[:colon]
	}
	return s, tag
}

// registryHostPort reports whether host is a name followed by a numeric port.
func registryHostPort(host string) bool {
	name, port, ok := strings.Cut(host, ":")
	if !ok || name == "" || port == "" {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ReadInUse collects the images running pods and sandbox records use. An
// unreadable input or a name failing validation returns an error, never an
// empty set: a plan that reads "nothing is running" deletes what is
// (docs/plans/2026-10-28-disk-maintenance-design.md, rule 4).
func ReadInUse(ctx context.Context, run diskmaint.Runner, kubeconfig, recordsDir string) (InUse, error) {
	inUse := InUse{Digests: map[string]bool{}, Tags: map[string]bool{}, Repos: map[string]bool{}}
	out, err := run(ctx, nil, "kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return InUse{}, fmt.Errorf("in use: kubectl get pods: %w", err)
	}
	// A null reply and a reply without an items key both decode without error,
	// so they are refused separately: an answer that names no pod list must
	// not pass for "no pods running".
	var pods *podList
	if err := json.Unmarshal(out, &pods); err != nil {
		return InUse{}, fmt.Errorf("in use: decode kubectl output: %w", err)
	}
	if pods == nil || pods.Items == nil {
		return InUse{}, fmt.Errorf("in use: decode kubectl output: no pod list in the reply")
	}
	for _, pod := range *pods.Items {
		var refs []string
		for _, c := range pod.Spec.Containers {
			refs = append(refs, c.Image)
		}
		for _, c := range pod.Spec.InitContainers {
			refs = append(refs, c.Image)
		}
		for _, s := range pod.Status.ContainerStatuses {
			refs = append(refs, s.Image, s.ImageID)
		}
		for _, s := range pod.Status.InitContainerStatuses {
			refs = append(refs, s.Image, s.ImageID)
		}
		if err := inUse.addRefs(refs...); err != nil {
			return InUse{}, err
		}
	}
	if err := inUse.addRecords(recordsDir); err != nil {
		return InUse{}, err
	}
	return inUse, nil
}

// addRecords adds every registry reference the sandbox records name. A missing
// directory is an error: a town that runs sandboxes always has it, so its
// absence means something is wrong.
func (u InUse) addRecords(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("in use: sandbox records: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("in use: sandbox record %s: %w", e.Name(), err)
		}
		if err := u.addRefs(recordRefs(string(data))...); err != nil {
			return err
		}
	}
	return nil
}

func (u InUse) addRefs(refs ...string) error {
	for _, raw := range refs {
		if err := u.addRef(raw); err != nil {
			return err
		}
	}
	return nil
}

func (u InUse) addRef(raw string) error {
	r, err := parseRef(raw)
	if err != nil {
		return err
	}
	if r.digest != "" {
		u.Digests[r.digest] = true
	}
	if r.tag != "" {
		u.Tags[r.repo+":"+r.tag] = true
	}
	if r.repo != "" {
		u.Repos[r.repo] = true
	}
	return nil
}

// imageRef is a container reference with the registry host dropped: the target
// knows its own host.
type imageRef struct{ repo, tag, digest string }

// parseRef splits a container reference into its parts and validates each. A
// bare digest returns the zero ref: containerd reports an image ID as a config
// digest or a bare image id, and neither names a manifest.
func parseRef(raw string) (imageRef, error) {
	s := strings.TrimSpace(raw)
	for _, scheme := range imageSchemes {
		s = strings.TrimPrefix(s, scheme)
	}
	if s == "" {
		return imageRef{}, nil
	}
	if strings.HasPrefix(s, "sha256:") {
		if !ValidDigest(s) {
			return imageRef{}, fmt.Errorf("image id %q: %w", raw, ErrBadName)
		}
		return imageRef{}, nil
	}
	var r imageRef
	if at := strings.Index(s, "@"); at >= 0 {
		r.digest = s[at+1:]
		s = s[:at]
	}
	if host, rest, ok := strings.Cut(s, "/"); ok && strings.ContainsAny(host, ".:") {
		s = rest
	}
	if colon := strings.LastIndex(s, ":"); colon >= 0 {
		r.tag = s[colon+1:]
		s = s[:colon]
	}
	r.repo = s
	if !ValidRepo(r.repo) {
		return imageRef{}, fmt.Errorf("image %q: repo %q: %w", raw, r.repo, ErrBadName)
	}
	if r.tag != "" && !ValidTag(r.tag) {
		return imageRef{}, fmt.Errorf("image %q: tag %q: %w", raw, r.tag, ErrBadName)
	}
	if r.digest != "" && !ValidDigest(r.digest) {
		return imageRef{}, fmt.Errorf("image %q: digest %q: %w", raw, r.digest, ErrBadName)
	}
	return r, nil
}
