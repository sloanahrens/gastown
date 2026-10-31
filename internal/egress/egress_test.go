package egress

import "testing"

// mustLoad reads a policy the test expects to be valid.
func mustLoad(t *testing.T, data string) Policy {
	t.Helper()
	p, err := Load([]byte(data))
	if err != nil {
		t.Fatalf("Load(%s): %v", data, err)
	}
	return p
}

// The default is deny: an empty policy, an empty list, and a host the list does
// not name are all refused.
func TestUnlistedHostRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy Policy
		host   string
	}{
		{"an empty policy", Policy{}, "example.com"},
		{"a policy that lists nothing", mustLoad(t, `{"workspace":"w","hosts":[]}`), "example.com"},
		{"a policy with no hosts field", mustLoad(t, `{"workspace":"w"}`), "example.com"},
		{"a host not in the list", mustLoad(t, `{"hosts":["a.com"]}`), "b.com"},
		{"a subdomain of a listed exact host", mustLoad(t, `{"hosts":["a.com"]}`), "x.a.com"},
		{"a host that only ends with a listed name", mustLoad(t, `{"hosts":["a.com"]}`), "xa.com"},
		{"a host that only starts with a listed name", mustLoad(t, `{"hosts":["a.com"]}`), "a.com.evil.net"},
		{"an empty host", mustLoad(t, `{"hosts":["a.com"]}`), ""},
		{"a host given as a URL", mustLoad(t, `{"hosts":["a.com"]}`), "https://a.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.policy.Allowed(tc.host) {
				t.Errorf("Allowed(%q) = true, want false", tc.host)
			}
		})
	}
}

// An exact entry matches only itself, a "*." entry matches one or more labels
// below its domain and never the domain, and case and :port are ignored on both
// the entry and the host.
func TestExactAndWildcardMatching(t *testing.T) {
	t.Parallel()
	p := mustLoad(t, `{"workspace":"w","hosts":["api.example.com","*.a.com"]}`)
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"api.example.com", true},
		{"API.Example.COM", true},
		{"api.example.com:443", true},
		{"x.a.com", true},
		{"x.y.a.com", true},
		{"X.Y.A.COM:8443", true},
		{"api.example.com:1", true},
		{"api.example.com:65535", true},
		{"a.com", false},
		{"A.COM:443", false},
		{"xa.com", false},
		{"other.example.com", false},
		{"example.com", false},
	} {
		if got := p.Allowed(tc.host); got != tc.want {
			t.Errorf("Allowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// A host argument that is not a host name is refused even when its suffix would
// match a wildcard entry, so a wildcard cannot name a string no one can dial.
func TestNonHostArgumentRefused(t *testing.T) {
	t.Parallel()
	p := mustLoad(t, `{"workspace":"w","hosts":["api.example.com","*.a.com"]}`)
	for _, host := range []string{
		"x/a.com",
		"x y.a.com",
		"x?a.com",
		"x#a.com",
		"*.a.com",
		"*.example.com",
		"https://x.a.com",
		"x.a.com:99999",
		"x.a.com:0",
		"x.a.com:",
		"x.a.com:https",
		".a.com",
		"x..a.com",
	} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			if p.Allowed(host) {
				t.Errorf("Allowed(%q) = true, want false", host)
			}
		})
	}
}

// Load reads the workspace and its list, and a port on an entry is ignored the
// same way it is on the host being checked.
func TestLoadReadsPolicy(t *testing.T) {
	t.Parallel()
	p := mustLoad(t, `{"workspace":"shop","hosts":["API.example.com:443","*.cdn.example.com:8443"]}`)
	if p.Workspace != "shop" {
		t.Errorf("Workspace = %q, want %q", p.Workspace, "shop")
	}
	if len(p.Hosts) != 2 {
		t.Fatalf("Hosts = %q, want 2 entries", p.Hosts)
	}
	if !p.Allowed("api.example.com") {
		t.Errorf("Allowed(%q) = false with matching entry %q, want true", "api.example.com", p.Hosts[0])
	}
	if !p.Allowed("x.cdn.example.com") {
		t.Errorf("Allowed(%q) = false with matching entry %q, want true", "x.cdn.example.com", p.Hosts[1])
	}
}

// A malformed file, an empty entry, or an entry with a scheme or a path is an
// error, never a policy that quietly allows or denies something else.
func TestLoadRejectsMalformedEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data string }{
		{"invalid JSON", `{"hosts":[`},
		{"not an object", `["a.com"]`},
		{"JSON null", `null`},
		{"empty data", ``},
		{"trailing data", `{"hosts":["a.com"]}{"hosts":["b.com"]}`},
		{"an unknown field", `{"host":["a.com"]}`},
		{"an empty entry", `{"hosts":[""]}`},
		{"an entry with a scheme", `{"hosts":["https://a.com"]}`},
		{"an entry with a scheme and no slashes", `{"hosts":["https:a.com"]}`},
		{"an entry with a path", `{"hosts":["a.com/path"]}`},
		{"an entry with a query", `{"hosts":["a.com?x=1"]}`},
		{"a wildcard that is not the leading label", `{"hosts":["a*.com"]}`},
		{"a bare wildcard", `{"hosts":["*"]}`},
		{"an empty wildcard", `{"hosts":["*."]}`},
		{"an empty label", `{"hosts":["a..com"]}`},
		{"a non-numeric port", `{"hosts":["a.com:https"]}`},
		{"an entry that is only a port", `{"hosts":[":443"]}`},
		{"an empty port", `{"hosts":["a.com:"]}`},
		{"a port of zero", `{"hosts":["a.com:0"]}`},
		{"a port above the range", `{"hosts":["a.com:65536"]}`},
		{"a wildcard with a port out of range", `{"hosts":["*.a.com:99999"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if p, err := Load([]byte(tc.data)); err == nil {
				t.Errorf("Load(%s) = %+v, want an error", tc.data, p)
			}
		})
	}
}

// A Policy built by hand can hold an entry Load would refuse. It is skipped
// rather than matched loosely, so it can only deny.
func TestHandBuiltInvalidEntryDenies(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"", "https://a.com", "a*.com", "*.", "a.com/x"} {
		p := Policy{Hosts: []string{entry}}
		if p.Allowed("a.com") {
			t.Errorf("Allowed(%q) = true with entry %q, want false", "a.com", entry)
		}
	}
}
