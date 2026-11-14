package taskmap

// The placement for gt update bd-base, which pins the bd base image the
// gateway image is built from to the digest its commit tag resolves to.
func init() {
	Register(
		Entry{Path: "update bd-base", Group: "host", Use: "Use this after the beads rig pushes a new bd base image, to pin the gateway image's base to the digest its commit tag resolves to.", Check: "gt update bd-base prints the sha256 digest it wrote"},
	)
}
