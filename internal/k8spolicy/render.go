package k8spolicy

import (
	"fmt"
	"regexp"
	"strings"
)

// The tracker endpoint's values when a render names none: trackerd's in-cluster
// Service and its default port, the same value the Docker policy carries.
const (
	DefaultTrackerHost = "tracker.openshell.svc.cluster.local"
	DefaultTrackerPort = "7433"
)

// staleAlias is the header phrase that names the Docker route, and routeAlias
// what it becomes.
const (
	staleAlias = "Docker Desktop's host alias"
	routeAlias = "in-cluster Services"
)

// Params are the values Render writes into the policy: the in-cluster name of
// each endpoint it rewrites, the tracker's port, and the address pinned on each
// rewritten endpoint (without the /32, which Render adds). Callers validate
// them first (Options.Parse and Validate use ValidHost and ValidIPv4); Render
// checks them again, because they are written into YAML and a value with a
// newline, a colon and a space, or a quote would change what the file means.
type Params struct {
	DoltHost    string
	ForgejoHost string
	TrackerHost string
	TrackerPort string
	AllowedIP   string
}

// validate refuses a value that is not a DNS name, an IPv4 address or a port
// number, so nothing written into the YAML can break out of its line.
func (p Params) validate() error {
	for _, h := range []struct{ field, value string }{
		{"DoltHost", p.DoltHost}, {"ForgejoHost", p.ForgejoHost}, {"TrackerHost", p.TrackerHost},
	} {
		if !ValidHost(h.value) {
			return fmt.Errorf("k8spolicy: %s %q is not a DNS name", h.field, h.value)
		}
	}
	if !ValidIPv4(p.AllowedIP) {
		return fmt.Errorf("k8spolicy: AllowedIP %q is not an IPv4 address", p.AllowedIP)
	}
	if !validPort(p.TrackerPort) {
		return fmt.Errorf("k8spolicy: TrackerPort %q is not a port number", p.TrackerPort)
	}
	return nil
}

// validPort reports whether s is one to five decimal digits. The empty string is
// not a port: an empty TrackerPort would otherwise match a `port:` line with no
// value.
func validPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

var (
	dockerHostLine = regexp.MustCompile(`^ *- host: *host\.docker\.internal[ \t]*$`)
	portLine       = regexp.MustCompile(`^ *port:`)
	listItemLine   = regexp.MustCompile(`^ *- `)
	hostToRewrite  = regexp.MustCompile(`host: *host\.docker\.internal`)
)

// Render returns the policy in base rewritten for the cluster: each
// `- host: host.docker.internal` endpoint whose port is 3000 (Forgejo), 3307
// (Dolt) or p.TrackerPort gets its in-cluster name and an allowed_ips pin, an
// endpoint on any other port is left as it is, and the first mention of
// Docker Desktop's host alias on a line becomes "in-cluster Services". Every
// line of the result ends in a newline, whether or not the input's last line did.
// It returns an error, and no output, for a Params value that is not a DNS name,
// an IPv4 address or a port number (see Params).
//
// It is a line state machine, the port of the awk program of
// deploy/k8s/render-policy.sh. A host line is held, not printed, and the lines
// after it are buffered until one decides the block:
//
//   - a `port:` line settles it. The port is the digits after `port:`. For an
//     endpoint it names, Render prints the host line renamed, the pin, the port
//     line, and then the buffered lines, so a `protocol:` line that sat between
//     host and port now follows the port line. For any other port it prints the
//     held line, the buffer and the port line unchanged.
//   - another host line, or any other list item, ends the block unchanged
//     (the list item is then handled as an ordinary line); a second host line
//     is held in its turn.
//   - any other line, a blank line or an unindented key included, joins the
//     buffer.
//
// Known quirk, kept on purpose (gt-ywwf2): only those three ends a block, so a
// host line with no list item after it captures a `port:` line from much later
// in the file, and the lines between come out after the pin.
func Render(base string, p Params) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	var out strings.Builder
	emit := func(line string) {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	lines := strings.Split(base, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	pending := false
	held := ""
	var buf []string
	flush := func() {
		emit(held)
		for _, b := range buf {
			emit(b)
		}
		pending, held, buf = false, "", nil
	}

	for _, line := range lines {
		if pending {
			if portLine.MatchString(line) {
				name := hostForPort(portOf(line), p)
				if name == "" {
					flush()
					emit(line)
					continue
				}
				emit(renameHost(held, name))
				emit(leadingSpaces(line) + `allowed_ips: ["` + p.AllowedIP + `/32"]`)
				emit(line)
				for _, b := range buf {
					emit(b)
				}
				pending, held, buf = false, "", nil
				continue
			}
			if dockerHostLine.MatchString(line) {
				emit(held)
				for _, b := range buf {
					emit(b)
				}
				held, buf = line, nil
				continue
			}
			if listItemLine.MatchString(line) {
				flush()
				// This line is handled as an ordinary line below.
			} else {
				buf = append(buf, line)
				continue
			}
		}
		if dockerHostLine.MatchString(line) {
			held, buf, pending = line, nil, true
			continue
		}
		if strings.Contains(line, staleAlias) {
			line = strings.Replace(line, staleAlias, routeAlias, 1)
		}
		emit(line)
	}
	if pending {
		flush()
	}
	return out.String(), nil
}

// hostForPort is the in-cluster name for an endpoint port, or "" for a port
// that is not rewritten. 3000 is checked first, then 3307, then the tracker's
// port, as the script did.
func hostForPort(port string, p Params) string {
	switch {
	case port == "3000":
		return p.ForgejoHost
	case port == "3307":
		return p.DoltHost
	case p.TrackerPort != "" && port == p.TrackerPort:
		return p.TrackerHost
	}
	return ""
}

// portOf is the digits after `port:` on a port line, up to the first non-digit
// and after any spaces or tabs; "" when none follow.
func portOf(line string) string {
	rest := strings.TrimLeft(line, " ")
	rest = strings.TrimPrefix(rest, "port:")
	rest = strings.TrimLeft(rest, " \t")
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	return rest[:end]
}

// leadingSpaces is the run of spaces a line starts with.
func leadingSpaces(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " "))]
}

// renameHost replaces the first `host: host.docker.internal` on the held line
// with `host: <name>`, leaving anything after it as it was.
func renameHost(held, name string) string {
	loc := hostToRewrite.FindStringIndex(held)
	if loc == nil {
		return held
	}
	return held[:loc[0]] + "host: " + name + held[loc[1]:]
}
