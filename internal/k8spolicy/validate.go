package k8spolicy

import (
	"fmt"
	"strings"
)

// ValidHost reports whether name is a DNS name the render may write as a host:
// labels of letters, digits and hyphens, none empty, none over 63 bytes, none
// starting or ending with a hyphen, and 253 bytes at most in all. One trailing
// dot is tolerated because the script split the name on dots and a trailing
// separator leaves no field: a.example. is a name and a.. is not.
func ValidHost(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isHostByte(name[i]) {
			return false
		}
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

// isHostByte reports whether c may appear in a host name: the letters, digits,
// dots and hyphens the script's case pattern allowed.
func isHostByte(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '.' || c == '-'
}

// ValidIPv4 reports whether addr is four decimal octets, each at most 255, with
// no other character. Leading zeros are decimal, as the script's arithmetic
// read them (010 is 10). A trailing dot is refused: the split here keeps the
// empty field the script's word splitting dropped, and the script would have
// rendered 1.2.3.4./32 for it, which is this port's first approved departure
// (gt-tpg0p.81).
func ValidIPv4(addr string) bool {
	if addr == "" {
		return false
	}
	for i := 0; i < len(addr); i++ {
		if c := addr[i]; c != '.' && (c < '0' || c > '9') {
			return false
		}
	}
	octets := strings.Split(addr, ".")
	if len(octets) != 4 {
		return false
	}
	for _, octet := range octets {
		if !isOctet(octet) {
			return false
		}
	}
	return true
}

// isOctet reports whether digits is a non-empty decimal number of 255 or less.
// The value is judged digit by digit and refused as soon as it passes 255, so a
// digit string too long for an integer cannot wrap into a small number: the
// script's [ ... -le 255 ] failed on one, and this refuses it the same way.
func isOctet(digits string) bool {
	if digits == "" {
		return false
	}
	value := 0
	for i := 0; i < len(digits); i++ {
		value = value*10 + int(digits[i]-'0')
		if value > 255 {
			return false
		}
	}
	return true
}

// Validate checks the options in the order the script checked them: the pinned
// address, then the dolt name, then the forgejo name, then the tracker name.
// The first refusal is returned, naming the flag and the value it will not
// render.
func Validate(opts Options) error {
	if !ValidIPv4(opts.AllowedIP) {
		return &UsageError{Msg: fmt.Sprintf("%s: --allowed-ip must be an IPv4 address, got '%s'", Name, opts.AllowedIP)}
	}
	if !ValidHost(opts.DoltHost) {
		return &UsageError{Msg: fmt.Sprintf("%s: --dolt-host must be a DNS name, got '%s'", Name, opts.DoltHost)}
	}
	if !ValidHost(opts.ForgejoHost) {
		return &UsageError{Msg: fmt.Sprintf("%s: --forgejo-host must be a DNS name, got '%s'", Name, opts.ForgejoHost)}
	}
	if !ValidHost(opts.TrackerHost) {
		return &UsageError{Msg: fmt.Sprintf("%s: --tracker-host must be a DNS name, got '%s'", Name, opts.TrackerHost)}
	}
	return nil
}
