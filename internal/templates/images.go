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

	// DindImage is the Docker engine the daemon's tier sweep drives, pinned by
	// digest rather than tag so a restart runs the engine the ladder proved.
	// It is the same digest the live Forgejo project's engine runs, so the
	// sweep meets the engine its CI jobs already use.
	DindImage = "data.forgejo.org/oci/docker@sha256:7dcdfc4a20246236f558175182ccace1eb15a41bd3eb119dd2284f393498b7c1"
)
