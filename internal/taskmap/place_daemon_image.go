package taskmap

// The placement for gt daemon image pins, which moves the image's source pins
// to their tips before the pin-drift guard refuses a build.
func init() {
	Register(
		Entry{Path: "daemon image pins", Group: "host", Use: "Use this when the daemon image build is refused because a source pin has fallen far behind its source's origin/main, to move those pins to their tips in both pins files.", Check: "gt daemon image pins lists no pin to move"},
	)
}
