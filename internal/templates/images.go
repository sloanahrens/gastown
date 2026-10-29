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

	// DaemonBaseImage is the base image the daemon image builds on, the same
	// one deploy/polecat/Dockerfile uses.
	DaemonBaseImage = "ubuntu:24.04"
)
