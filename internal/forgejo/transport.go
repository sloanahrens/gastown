package forgejo

import "strings"

// PlaintextHost is the town's own Forgejo service name and port. Inside the
// town network that instance has no https endpoint, so the viewer credential
// may cross to it over http. It is the one non-loopback host that may: a host
// name this town controls, and no other.
const PlaintextHost = "forgejo:3000"

// PlaintextHostOf is the non-loopback host the viewer credential may reach over
// http for a registry served at scheme://host: PlaintextHost when that registry
// is http on it, and "" otherwise. The host matches without regard to case, as a
// host name does, but the port must be 3000: any other host, a look-alike such
// as forgejo.example.com, or forgejo on another port is read over https only.
func PlaintextHostOf(scheme, host string) string {
	if scheme == "http" && strings.EqualFold(host, PlaintextHost) {
		return PlaintextHost
	}
	return ""
}
