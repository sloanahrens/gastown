package k8spolicy

import (
	"strings"
	"testing"
)

// TestValidHostTable: a host is a dotted label sequence, so the only things
// refused are the shapes that would put something other than a name into the
// rendered YAML. One trailing dot is the script's own tolerance; two are not.
func TestValidHostTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		host string
		ok   bool
	}{
		{name: "the default name", host: DefaultDoltHost, ok: true},
		{name: "a two label name", host: "dolt.example", ok: true},
		{name: "one trailing dot", host: "a.example.", ok: true},
		{name: "a 253 byte name", host: longName(63, 63, 63, 61), ok: true},
		{name: "empty", host: ""},
		{name: "a space", host: "dolt example"},
		{name: "a label starting with a hyphen", host: "-bad.example"},
		{name: "a label ending with a hyphen", host: "bad-.example"},
		{name: "an empty label", host: "bad..example"},
		{name: "two dots at the end", host: "a.."},
		{name: "a leading dot", host: ".a"},
		{name: "a lone dot", host: "."},
		{name: "an underscore", host: "bad_host.example"},
		{name: "a 64 byte label", host: strings.Repeat("a", 64)},
		{name: "a 254 byte name", host: longName(63, 63, 63, 62)},
		{name: "the injection string", host: `1.2.3.4/32"]; evil`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidHost(tc.host); got != tc.ok {
				t.Errorf("ValidHost(%q) = %t, want %t", tc.host, got, tc.ok)
			}
		})
	}
}

// longName builds a dotted name of the given label lengths, for the byte
// boundary the whole-name rule checks.
func longName(labels ...int) string {
	parts := make([]string, len(labels))
	for i, n := range labels {
		parts[i] = strings.Repeat("a", n)
	}
	return strings.Join(parts, ".")
}

// TestValidIPv4Table: four decimal octets of 255 or less. Leading zeros are
// decimal (010 is 10), an octet too large for an integer is refused rather than
// wrapped, and a trailing dot is refused where the script accepted it and
// rendered 1.2.3.4./32 (departure 1, gt-tpg0p.81).
func TestValidIPv4Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		addr string
		ok   bool
	}{
		{name: "the default address", addr: DefaultAllowedIP, ok: true},
		{name: "the zero address", addr: "0.0.0.0", ok: true},
		{name: "the largest address", addr: "255.255.255.255", ok: true},
		{name: "leading zeros are decimal", addr: "010.0.0.1", ok: true},
		{name: "three octets", addr: "1.2.3"},
		{name: "five octets", addr: "1.2.3.4.5"},
		{name: "an octet over 255", addr: "256.1.1.1"},
		{name: "a hex-looking octet", addr: "1.2.3.4x"},
		{name: "an empty address", addr: ""},
		{name: "a trailing dot", addr: "1.2.3.4."},
		{name: "an empty octet", addr: "1..2.3"},
		{name: "a space after the address", addr: "1.2.3.4 "},
		{name: "an octet too long for an integer", addr: "1234567890123456789012345.1.2.3"},
		{name: "the injection string", addr: `1.2.3.4/32"]; evil`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidIPv4(tc.addr); got != tc.ok {
				t.Errorf("ValidIPv4(%q) = %t, want %t", tc.addr, got, tc.ok)
			}
		})
	}
}
