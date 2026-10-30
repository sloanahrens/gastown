package k8spolicy

import (
	"os"
	"strings"
	"testing"
)

// defaultParams are the values a render with no options carries. DefaultDoltHost,
// DefaultForgejoHost and DefaultAllowedIP are defined in internal/k8spolicy/
// options.go (lines 27-29, gt-tpg0p.81), which is not part of this change.
func defaultParams() Params {
	return Params{
		DoltHost:    DefaultDoltHost,
		ForgejoHost: DefaultForgejoHost,
		TrackerHost: DefaultTrackerHost,
		TrackerPort: DefaultTrackerPort,
		AllowedIP:   DefaultAllowedIP,
	}
}

// mustRender renders base and fails the test on an error.
func mustRender(t *testing.T, base string, p Params) string {
	t.Helper()
	out, err := Render(base, p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

// readRepoFile reads a file of the repository by a path relative to this
// package; it writes nothing.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// lineMultiset counts how often each line occurs.
func lineMultiset(text string) map[string]int {
	m := map[string]int{}
	for _, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		m[l]++
	}
	return m
}

// changedLines counts the lines a diff would show as removed or added between a
// and b, the way `diff a b | grep -c '^[<>]'` does for a change that only
// rewrites and inserts lines.
func changedLines(a, b string) int {
	ma, mb := lineMultiset(a), lineMultiset(b)
	n := 0
	for l, c := range ma {
		if d := c - mb[l]; d > 0 {
			n += d
		}
	}
	for l, c := range mb {
		if d := c - ma[l]; d > 0 {
			n += d
		}
	}
	return n
}

// endpointOf returns the three lines before the line `port: <port>` and the
// line itself: the host, the pin and the port of the endpoint that listens there.
func endpointOf(t *testing.T, out, port string) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "port: "+port && i >= 2 {
			return lines[i-2 : i+1]
		}
	}
	t.Fatalf("no endpoint on port %s in:\n%s", port, out)
	return nil
}

// TestRenderDefaultsOnTheDockerPolicy renders the checked-in Docker policy with
// the defaults. It is the ONE test of this package that reads the live
// deploy/polecat/policy.yaml (the work item allows one); every other test uses a
// stored fixture under testdata: no Docker host is left, the header names the route, and exactly
// 11 lines differ (the header line removed and added, and for each of the three
// endpoints the host line removed and added and the pin added). Each endpoint
// holds the name and the pin of its own port.
func TestRenderDefaultsOnTheDockerPolicy(t *testing.T) {
	t.Parallel()
	base := readRepoFile(t, "../../deploy/polecat/policy.yaml")
	out := mustRender(t, base, defaultParams())
	if strings.Contains(out, "host.docker.internal") {
		t.Errorf("host.docker.internal is left in the render")
	}
	if strings.Contains(out, "Docker Desktop") {
		t.Errorf("the header still claims a Docker host alias")
	}
	if !strings.Contains(out, "Forgejo on :3000 are reached through in-cluster Services.") {
		t.Errorf("the header does not name the in-cluster route")
	}
	if got := changedLines(base, out); got != 11 {
		t.Errorf("the render differs from the Docker policy in %d lines, want 11", got)
	}
	pin := `        allowed_ips: ["192.168.5.2/32"]`
	for _, tc := range []struct{ port, host string }{
		{"3307", DefaultDoltHost},
		{"3000", DefaultForgejoHost},
		{"7433", DefaultTrackerHost},
	} {
		ep := endpointOf(t, out, tc.port)
		if ep[0] != "      - host: "+tc.host || ep[1] != pin {
			t.Errorf("endpoint on %s = %q, want host %s and the pin", tc.port, ep, tc.host)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("the render does not end in a newline")
	}
}

// TestRenderSeparateNamesAndPin gives each endpoint its own name and a distinct
// address: every name lands only on its own port, and the pin appears once per
// rewritten endpoint.
func TestRenderSeparateNamesAndPin(t *testing.T) {
	t.Parallel()
	out := mustRender(t, readRepoFile(t, "testdata/three.yaml"), Params{
		DoltHost: "dolt.example", ForgejoHost: "forgejo.example", TrackerHost: "tracker.example",
		TrackerPort: "7433", AllowedIP: "10.0.0.9",
	})
	for _, tc := range []struct{ port, want string }{{"3307", "dolt.example"}, {"3000", "forgejo.example"}, {"7433", "tracker.example"}} {
		ep := strings.Join(endpointOf(t, out, tc.port), "\n")
		if !strings.Contains(ep, "host: "+tc.want+"\n") {
			t.Errorf("port %s endpoint lacks %s:\n%s", tc.port, tc.want, ep)
		}
		for _, other := range []string{"dolt.example", "forgejo.example", "tracker.example"} {
			if other != tc.want && strings.Contains(ep, other) {
				t.Errorf("port %s endpoint carries %s:\n%s", tc.port, other, ep)
			}
		}
	}
	if got := strings.Count(out, "10.0.0.9/32"); got != 3 {
		t.Errorf("the pin appears %d times, want 3", got)
	}
}

// TestRenderLeavesOtherEndpointsAlone checks an endpoint on a port that is not
// rewritten stays unchanged and unpinned, an unrelated host is untouched, and a
// host line that ends the file is kept.
func TestRenderLeavesOtherEndpointsAlone(t *testing.T) {
	t.Parallel()
	got := mustRender(t, readRepoFile(t, "testdata/other.yaml"), defaultParams())
	want := `# Gas Town sandbox policy fixture: Dolt and Forgejo are reached through in-cluster Services.
network_policies:
  golang:
    endpoints:
      - host: proxy.golang.org
        port: 443
        protocol: rest
  unrelated:
    endpoints:
      - host: host.docker.internal
        port: 9999
        protocol: tcp
        tls: skip
  last:
    endpoints:
      - host: host.docker.internal
`
	if got != want {
		t.Errorf("render of other.yaml =\n%s\nwant\n%s", got, want)
	}
}

// TestRenderDoesNotDependOnLineOrder checks the port need not follow its host
// line: of two adjacent host lines the first is kept, an endpoint with a protocol
// key between its host and its port is pinned and the protocol line follows the
// port line, and an endpoint with no port stays intact.
func TestRenderDoesNotDependOnLineOrder(t *testing.T) {
	t.Parallel()
	got := mustRender(t, readRepoFile(t, "testdata/order.yaml"), defaultParams())
	want := `network_policies:
  adjacent:
    endpoints:
      - host: host.docker.internal
      - host: control-plane.openshell.svc.cluster.local
        allowed_ips: ["192.168.5.2/32"]
        port: 3307
        protocol: tcp
        tls: skip
  between:
    endpoints:
      - host: control-plane.openshell.svc.cluster.local
        allowed_ips: ["192.168.5.2/32"]
        port: 3000
        protocol: tcp
        tls: skip
  noport:
    endpoints:
      - host: host.docker.internal
        tls: skip
      - path: /usr/bin/git
`
	if got != want {
		t.Errorf("render of order.yaml =\n%s\nwant\n%s", got, want)
	}
}

// TestRenderQuirkPendingBlockSwallowsLaterPort pins the quirk kept on purpose
// (gt-ywwf2): a host line that no list item follows stays pending, so a `port:`
// line from much later in the file settles it, and the lines between come out
// after the pin. Fixing it is out of scope.
func TestRenderQuirkPendingBlockSwallowsLaterPort(t *testing.T) {
	t.Parallel()
	in := "x:\n  - host: host.docker.internal\n\nk: v\nport: 3000\nz: 1\n"
	want := "x:\n  - host: control-plane.openshell.svc.cluster.local\nallowed_ips: [\"192.168.5.2/32\"]\nport: 3000\n\nk: v\nz: 1\n"
	if got := mustRender(t, in, defaultParams()); got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}

// TestRenderEndOfInputFlushesPending checks a host line still pending when the
// input ends is printed, with the lines buffered after it, and that every line of
// the result ends in a newline whether or not the input's last line did.
func TestRenderEndOfInputFlushesPending(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, want string }{
		{"a final host line", "a:\n      - host: host.docker.internal\n", "a:\n      - host: host.docker.internal\n"},
		{"a final host line and its buffer", "      - host: host.docker.internal\n        tls: skip\n\n", "      - host: host.docker.internal\n        tls: skip\n\n"},
		{"no final newline", "a\n      - host: host.docker.internal\n        port: 3307", "a\n      - host: control-plane.openshell.svc.cluster.local\n        allowed_ips: [\"192.168.5.2/32\"]\n        port: 3307\n"},
		{"empty input", "", ""},
		{"a line without a newline", "just text", "just text\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mustRender(t, tc.in, defaultParams()); got != tc.want {
				t.Errorf("Render(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderPortParsing checks the port is the digits after `port:` and any
// spaces or tabs, up to the first non-digit, and that only 3000, 3307 and the
// tracker's port select a name.
func TestRenderPortParsing(t *testing.T) {
	t.Parallel()
	host := "  - host: host.docker.internal\n"
	renamed := func(name string, portLine string) string {
		return "  - host: " + name + "\n" + `  allowed_ips: ["192.168.5.2/32"]` + "\n" + portLine + "\n"
	}
	cases := []struct{ name, portLine, want string }{
		{"spaces after the colon", "  port: 3307", renamed(DefaultDoltHost, "  port: 3307")},
		{"no space", "  port:3307", renamed(DefaultDoltHost, "  port:3307")},
		{"a tab", "  port:\t3000", renamed(DefaultForgejoHost, "  port:\t3000")},
		{"a trailing comment", "  port: 7433 # trackerd", renamed(DefaultTrackerHost, "  port: 7433 # trackerd")},
		{"more digits is another port", "  port: 33070", host + "  port: 33070\n"},
		{"not a number", "  port: abc", host + "  port: abc\n"},
		{"empty", "  port:", host + "  port:\n"},
		{"another port", "  port: 443", host + "  port: 443\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mustRender(t, host+tc.portLine+"\n", defaultParams()); got != tc.want {
				t.Errorf("render = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("the tracker's port is configurable", func(t *testing.T) {
		t.Parallel()
		p := defaultParams()
		p.TrackerPort = "9999"
		got := mustRender(t, host+"  port: 9999\n", p)
		if want := renamed(DefaultTrackerHost, "  port: 9999"); got != want {
			t.Errorf("port 9999 as the tracker's port: %q, want %q", got, want)
		}
		if got := mustRender(t, host+"  port: 7433\n", p); got != host+"  port: 7433\n" {
			t.Errorf("7433 renamed though it is not the tracker's port now: %q", got)
		}
	})
}

// TestRenderTextRules covers the line rules around the endpoints: the first
// mention of the header phrase on a line is replaced and a second is not, a host
// line keeps any trailing whitespace, and other lines pass through unchanged.
func TestRenderTextRules(t *testing.T) {
	t.Parallel()
	stale := "Docker Desktop's host alias"
	cases := []struct{ name, in, want string }{
		{"first mention only", "# " + stale + " and " + stale + "\n", "# in-cluster Services and " + stale + "\n"},
		{"each line on its own", "# " + stale + "\n# " + stale + "\n", "# in-cluster Services\n# in-cluster Services\n"},
		{"trailing whitespace stays", "  - host: host.docker.internal \t\n  port: 3000\n", "  - host: control-plane.openshell.svc.cluster.local \t\n  allowed_ips: [\"192.168.5.2/32\"]\n  port: 3000\n"},
		{"other lines unchanged", "key: value\n\n  - item\n", "key: value\n\n  - item\n"},
		{"a list item ends the block and is handled as a line", "  - host: host.docker.internal\n  - note: " + stale + "\n", "  - host: host.docker.internal\n  - note: in-cluster Services\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mustRender(t, tc.in, defaultParams()); got != tc.want {
				t.Errorf("Render(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderRefusesParamsThatBreakTheYAML checks a value that is not a DNS name,
// an IPv4 address or a port number is refused with an error and no output: a
// newline, a colon and a space, or a quote would change what the rendered YAML
// means. An empty TrackerPort is refused too, since it would match a `port:`
// line with no value.
func TestRenderRefusesParamsThatBreakTheYAML(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Params)
		want   string
	}{
		{"newline in the Dolt host", func(p *Params) { p.DoltHost = "dolt.example\nport: 1" }, "DoltHost"},
		{"colon and space in the Forgejo host", func(p *Params) { p.ForgejoHost = "forgejo: evil" }, "ForgejoHost"},
		{"quote in the tracker host", func(p *Params) { p.TrackerHost = `tracker"x` }, "TrackerHost"},
		{"empty host", func(p *Params) { p.DoltHost = "" }, "DoltHost"},
		{"quote in the address", func(p *Params) { p.AllowedIP = `10.0.0.9"]` }, "AllowedIP"},
		{"newline in the address", func(p *Params) { p.AllowedIP = "10.0.0.9\n" }, "AllowedIP"},
		{"trailing dot address", func(p *Params) { p.AllowedIP = "1.2.3.4." }, "AllowedIP"},
		{"empty tracker port", func(p *Params) { p.TrackerPort = "" }, "TrackerPort"},
		{"tracker port with a letter", func(p *Params) { p.TrackerPort = "74x3" }, "TrackerPort"},
		{"tracker port too long", func(p *Params) { p.TrackerPort = "123456" }, "TrackerPort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := defaultParams()
			tc.mutate(&p)
			out, err := Render("  - host: host.docker.internal\n  port: 3307\n", p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Render error = %v, want one naming %s", err, tc.want)
			}
			if out != "" {
				t.Errorf("Render returned output with an error: %q", out)
			}
		})
	}
	// The defaults pass.
	if _, err := Render("", defaultParams()); err != nil {
		t.Errorf("Render with the defaults: %v", err)
	}
}

// TestRenderEmptyPortValueIsNotTheTracker checks a `port:` line with no value
// leaves the block unchanged: it must not match a tracker port.
func TestRenderEmptyPortValueIsNotTheTracker(t *testing.T) {
	t.Parallel()
	in := "  - host: host.docker.internal\n  port:\n"
	if got := mustRender(t, in, defaultParams()); got != in {
		t.Errorf("render = %q, want the block unchanged: %q", got, in)
	}
	if got := hostForPort("", Params{TrackerHost: "tracker.example"}); got != "" {
		t.Errorf("hostForPort(\"\") with an empty TrackerPort = %q, want no host", got)
	}
}
