package templates

// The image versions the daemon container and its compose file run at, in one
// table, so a Dockerfile and a template that both name an image read the same
// string instead of each carrying its own copy.
//
// Every reference names a version: a moving tag such as `latest` would make a
// container restart run a different image than the one the deployer built and
// the ladder proved. The daemon's own image is the exception and is not here:
// it is a local tag the deployer builds from this host's binaries
// (`gt-daemon:<rev>`), never something pulled by name.
const (
	// DoltVersion is the Dolt client version the daemon image installs, the
	// same version the town's Dolt server runs.
	DoltVersion = "2.3.2"

	// DoltImageDigest pins the official Dolt image the daemon image copies its
	// client from: the OCI index digest of dolthub/dolt:2.3.2 (linux/amd64 and
	// linux/arm64), read with `crane digest dolthub/dolt:2.3.2` on 2026-10-29. A
	// tag can move; the digest cannot, so the client the image carries is the one
	// that was read. Moving DoltVersion means reading the new tag's digest.
	DoltImageDigest = "sha256:9a077bc9c8cb6604a6d458c734096b482735f640144e5665b36161d560f14861"

	// DaemonBaseImage is the base image the daemon image builds on, the same
	// one deploy/polecat/Dockerfile uses.
	DaemonBaseImage = "ubuntu:24.04"

	// ClaudeCLIVersion is the claude CLI the daemon image carries: the exact
	// version om's reviewer backend runs (claude -p --model haiku). It is the
	// version the operator's host runs, so the image and the host agree.
	ClaudeCLIVersion = "2.1.314"

	// ClaudeCLISHA256 is the sha256 of the linux-arm64 claude binary at
	// ClaudeCLIVersion, taken from Anthropic's release manifest at
	// https://downloads.claude.ai/claude-code-releases/2.1.314/manifest.json
	// (fetched 2026-10-30, platform linux-arm64, the glibc build the ubuntu:24.04
	// base needs). It is 64 lower-case hex digits; a fetch whose bytes differ is
	// refused.
	ClaudeCLISHA256 = "a66c0a8f5d0c87a439953d797985192bbcbaefbce2e0d7b2eaa8df30ef0f3ef2"

	// ClaudeCLIURLFormat is the artifact URL the linux-arm64 claude binary is
	// fetched from, with the version filled in: the manifest's own download path
	// for the platform. Keep it beside ClaudeCLIVersion so a version bump reads
	// the URL it changes.
	ClaudeCLIURLFormat = "https://downloads.claude.ai/claude-code-releases/%s/linux-arm64/claude"

	// DindImage is the Docker engine the daemon's tier sweep drives, pinned by
	// digest rather than tag so a restart runs the engine the ladder proved.
	// It is the same digest the live Forgejo project's engine runs, so the
	// sweep meets the engine its CI jobs already use.
	DindImage = "data.forgejo.org/oci/docker@sha256:7dcdfc4a20246236f558175182ccace1eb15a41bd3eb119dd2284f393498b7c1"

	// DoltServerImage is the Dolt SQL server the scratch project runs, at the
	// version DoltVersion names, so the scratch town's Dolt and the daemon
	// image's client are one version. It is pinned by the digest of the image
	// index of that tag, read from Docker Hub's registry API on 2026-10-30
	// (docker-content-digest of dolthub/dolt-sql-server:2.3.2). DoltImageDigest
	// does not carry over: a digest names one repository, and that one is
	// dolthub/dolt. Moving DoltVersion means reading this tag's digest too. The
	// tag stays in the reference for the reader; the digest is what docker pulls.
	DoltServerImage = "dolthub/dolt-sql-server:" + DoltVersion + "@sha256:38d5e900583267f35e36ad738e13f202e62860b351aa4c088dceaf7dbaed7ab6"

	// ForgejoImage, ForgePostgresImage and ForgejoRunnerImage are the throwaway
	// Forgejo stack the scratch project runs for the L2 window, at the versions
	// the operator's live Forgejo compose project (~/forgejo/compose.yml) runs,
	// so the ladder proves the daemon against the forge it will meet. They are
	// tags, as the live project names them; the scratch project's engine for the
	// runner is DindImage, the digest the live project runs.
	ForgejoImage       = "data.forgejo.org/forgejo/forgejo:16.0.5"
	ForgePostgresImage = "postgres:17.11"
	ForgejoRunnerImage = "data.forgejo.org/forgejo/runner:13.2.0"
)

// ClientToolPin is one pinned client program of the daemon image (see
// DaemonClientTools).
type ClientToolPin struct {
	Name   string
	URL    string
	SHA256 string
	Member string
}

// DaemonClientTools are the client programs the daemon image carries beside gt,
// bd, om and claude, each a pinned linux-arm64 download whose sha256 the
// deployer verifies before it writes the file the Dockerfile COPYs. Name is
// that file's name in the build context and in /usr/local/bin. A Member names
// the file to take out of a .tar.gz download; empty means the download is the
// binary itself. The sha256 is of the download as published.
//
//   - openshell is the NVIDIA/OpenShell release asset openshell-aarch64-unknown-linux-musl.tar.gz
//     at v0.1.5, the version the host runs; its sha256 is the one in that
//     release's openshell-checksums-sha256.txt (read 2026-11-01).
//   - docker is the static client from download.docker.com at 29.8.2, the
//     version inside the sweep engine (DindImage). Docker publishes no checksum
//     file, so this sha256 is our own pin of the download.
//   - kubectl is the v1.37.0 binary from dl.k8s.io, the cluster's node version,
//     with the sha256 published beside it as kubectl.sha256.
var DaemonClientTools = []ClientToolPin{
	{
		Name:   "openshell",
		URL:    "https://github.com/NVIDIA/OpenShell/releases/download/v0.1.5/openshell-aarch64-unknown-linux-musl.tar.gz",
		SHA256: "30c949b4ac07a9c8245c1248eb82e4eb241615134fcabf3c9bf30dec7d00addf",
		Member: "openshell",
	},
	{
		Name:   "docker",
		URL:    "https://download.docker.com/linux/static/stable/aarch64/docker-29.8.2.tgz",
		SHA256: "922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3",
		Member: "docker/docker",
	},
	{
		Name:   "kubectl",
		URL:    "https://dl.k8s.io/release/v1.37.0/bin/linux/arm64/kubectl",
		SHA256: "76a624e4a8e5da654d1150e808175125efb5a6f1b6aa1cbd9caee18f51047a50",
	},
}
