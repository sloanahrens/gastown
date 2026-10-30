// Package scratchtown is the documented L1/L2 scratch project: the compose
// project the daemon-container runbook (docs/daemon-container.md) brings up to
// exercise a container town, the containers its compose file renders, and the
// directory its compose file lives under.
//
// A running scratch project is a known holder of the host, not an unwrapped
// container suite: it competes with nothing the town runs, so the container
// gate neither waits behind it nor reaps it, and the install's quiet check
// proceeds past it (gt-4gixm).
//
// That exemption is granted on what docker reports about a container, never on
// what the container says about itself: a name, a label or an image alone is
// something any container can set, so Identity.Matches holds only when every
// one of them agrees at once.
package scratchtown

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// Project is the compose project the runbook starts (`compose -p
	// gt-scratch`), and the token docker puts in the name of anything it builds
	// for it.
	Project = "gt-scratch"

	// ParentDir is the directory, inside the operator's home, the project's
	// compose file lives under (the runbook's <scratch-parent>).
	ParentDir = "gt-scratch.noindex"

	// ProjectLabel and ConfigFilesLabel are the labels compose stamps on every
	// container it starts: the project the container belongs to, and the compose
	// file it was created from. The second is what separates the documented
	// project's containers from a container that only sets the first.
	ProjectLabel     = "com.docker.compose.project"
	ConfigFilesLabel = "com.docker.compose.project.config_files"

	// DaemonContainer, DoltContainer and DindContainer are the containers the
	// project's compose file renders, by the names compose gives them (the
	// project name is a prefix of the service name).
	DaemonContainer = "gt-scratch-daemon-1"
	DoltContainer   = "gt-scratch-scratch-dolt-1"
	DindContainer   = "gt-scratch-scratch-dind-1"

	// buildkitNamePrefix and buildkitImage are docker's build machinery: a
	// buildx builder that runs in a container is named buildx_buildkit_<builder>0
	// and runs the moby/buildkit image.
	buildkitNamePrefix = "buildx_buildkit"
	buildkitImage      = "moby/buildkit"
)

// containerNames are the only containers the project renders.
var containerNames = map[string]bool{
	DaemonContainer: true,
	DoltContainer:   true,
	DindContainer:   true,
}

// imageRepositories are the image repositories the project's compose renders:
// the daemon image gt-daemon:<rev>, the Dolt server image, and the docker:dind
// image (pinned through the Forgejo OCI mirror, or named plainly).
var imageRepositories = map[string]bool{
	"gt-daemon":                   true,
	"dolthub/dolt-sql-server":     true,
	"data.forgejo.org/oci/docker": true,
	"docker.io/library/docker":    true,
	"docker":                      true,
}

// Container is the inspect data a container is judged from: the name, image,
// labels and state docker reports for it, never what its author claims. An
// empty State is a listing that did not say (an older docker), which reads as
// the steady run docker would report for a container it is simply running.
type Container struct {
	Name   string
	Image  string
	Labels map[string]string
	State  string
}

// Class is how the gate reads a container against the documented project.
type Class int

const (
	// Unrelated is every container that is not the project's: the gate treats it
	// exactly as it does today, so an unknown container still blocks and a
	// spoofed gt-scratch-* name exempts nothing.
	Unrelated Class = iota

	// Holder is one of the project's containers and docker is steadily running
	// it: a known holder of the host, which never blocks, is never waited behind
	// and is never reaped.
	Holder

	// Building is the project mid-work: one of its containers is not steadily
	// running (created by the compose up that follows an image build, or
	// restarting), or a build docker is running for the project is still in
	// flight. An image build and a Docker restart are what the install's quiet
	// check exists to keep an install out of, so the exemption is withheld and
	// the install refuses while it is named.
	Building
)

// Identity is the documented project's identity: the directory its compose file
// must live under. The project name, the container names and the image
// repositories are package constants, because they are what the compose file
// renders.
type Identity struct {
	// Parent is the absolute directory the compose file must be under, which on
	// the operator's host is <home>/gt-scratch.noindex. Empty matches nothing:
	// an identity that could not be read must not exempt a container.
	Parent string
}

// HostIdentity is the identity the operator's host documents. A home directory
// that cannot be resolved leaves Parent empty, which exempts nothing.
func HostIdentity() Identity {
	home, err := os.UserHomeDir()
	if err != nil {
		return Identity{}
	}
	return Identity{Parent: filepath.Join(home, ParentDir)}
}

// Classify reads a container against the project: Holder when it is the
// project's and docker is steadily running it, Building when it is the
// project's and something is under way, and Unrelated otherwise.
func (id Identity) Classify(c Container) Class {
	if id.Matches(c) {
		if steady(c) {
			return Holder
		}
		return Building
	}
	// Not the full identity, so no exemption either way — but a container that
	// names itself the project's while docker is creating, restarting or
	// removing it is the project mid-work, and the install stays out of it.
	if belongs(c) && !steady(c) {
		return Building
	}
	if ProjectBuild(c) {
		return Building
	}
	return Unrelated
}

// Matches reports whether c is one of the documented project's containers, with
// every one of these true:
//
//   - its name is exactly one of the three the compose file renders;
//   - its com.docker.compose.project label is gt-scratch;
//   - its com.docker.compose.project.config_files label names a file under the
//     scratch parent;
//   - its image repository is one the compose file renders.
//
// Anything less — including a container named gt-scratch-* or carrying the
// compose project label without the rest — is an ordinary container.
func (id Identity) Matches(c Container) bool {
	if !containerNames[soleName(c.Name)] {
		return false
	}
	if c.Labels[ProjectLabel] != Project {
		return false
	}
	if !id.underParent(c.Labels[ConfigFilesLabel]) {
		return false
	}
	return imageRepositories[ImageRepository(c.Image)]
}

// ProjectBuild reports whether c is docker's build machinery running for the
// project: a buildx builder container whose name or labels carry the project
// name. Whether the build is in flight cannot be read from the container — a
// container-driver builder stays up between builds — so its presence is the
// evidence docker reports, and the quiet check reads it as a build in progress.
func ProjectBuild(c Container) bool {
	name := strings.ToLower(soleName(c.Name))
	if !strings.HasPrefix(name, buildkitNamePrefix) && ImageRepository(c.Image) != buildkitImage {
		return false
	}
	if strings.Contains(name, Project) {
		return true
	}
	for k, v := range c.Labels {
		if strings.Contains(strings.ToLower(k+"="+v), Project) {
			return true
		}
	}
	return false
}

// belongs reports whether c names itself the project's without proving it: one
// of the rendered container names, or the compose project label. Enough to
// withhold the exemption while docker is mid-transition, never enough to grant
// it.
func belongs(c Container) bool {
	return containerNames[soleName(c.Name)] || c.Labels[ProjectLabel] == Project
}

// steady reports whether docker is simply running the container, rather than
// creating, restarting, pausing, removing or having failed it.
func steady(c Container) bool {
	return c.State == "" || c.State == "running"
}

// underParent reports whether every file the config_files label names is inside
// the scratch parent. Compose records the path it was given, so a relative one,
// one outside the parent, or none at all answers false.
func (id Identity) underParent(files string) bool {
	if id.Parent == "" || files == "" {
		return false
	}
	seen := false
	for _, file := range strings.Split(files, ",") {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		seen = true
		rel, err := filepath.Rel(filepath.Clean(id.Parent), filepath.Clean(file))
		if err != nil || !filepath.IsAbs(file) || rel == "." || strings.HasPrefix(rel, "..") {
			return false
		}
	}
	return seen
}

// soleName is the container's name when docker reported exactly one, and ""
// otherwise: a comma-joined Names field names several containers, none of which
// is exactly the project's.
func soleName(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" || strings.Contains(name, ",") {
		return ""
	}
	return name
}

// ImageRepository is the repository part of an image reference: the name
// without its tag or digest, so gt-daemon:4f2a1c9 and gt-daemon@sha256:...
// both answer "gt-daemon". A colon before the last slash is a registry port,
// not a tag.
func ImageRepository(image string) string {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}
