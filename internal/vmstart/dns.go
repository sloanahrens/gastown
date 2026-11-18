// Package vmstart reads the DNS state of a Colima VM, so the start command and
// the reboot check share one definition (gt-6ggud7).
package vmstart

import (
	"context"
	"fmt"
	"strings"
)

// Runner runs one command and returns its standard output. An error means the
// command exited non-zero; it must not carry the command's output, which can
// hold a secret.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// LimaGateway is the address the Lima gateway answers DNS on from inside a
// Colima VM; it is the only resolver that serves host.lima.internal after a Mac
// reboot (gt-6ggud7).
const LimaGateway = "192.168.5.2"

// Step is one command a repair ran, argv joined by spaces, kept for the reboot
// report.
type Step struct {
	Command string
}

// DNSState is what CheckDNS read from one VM.
type DNSState struct {
	// ResolvConf is the text the read printed: /etc/resolv.conf, followed by
	// the lookup's line when the lookup answered.
	ResolvConf string
	// Nameservers holds the address on each nameserver line, in file order.
	Nameservers []string
	// Answers is true when the lookup of github.com exited 0.
	Answers bool
}

// dnsCommand is the reboot sheet's read: cat the file, then a bounded lookup.
// The shell exits with the lookup's status, so a non-zero exit is a failed
// answer rather than a failed read.
const dnsCommand = "cat /etc/resolv.conf; timeout 5 getent hosts github.com"

// vmProfiles are the two VMs the town runs; any other profile is refused before
// a call is made.
var vmProfiles = map[string]bool{"gt-control": true, "gt-sbx": true}

// CheckDNS reads the resolv.conf and DNS answer of the named VM. Answers is
// false both for a lookup that failed and for a command that failed after
// printing the file; when the command printed nothing the state is unknown and
// CheckDNS returns an error instead. A profile that is not gt-control or gt-sbx
// is refused before any call.
func CheckDNS(ctx context.Context, run Runner, profile string) (DNSState, error) {
	if !vmProfiles[profile] {
		return DNSState{}, fmt.Errorf("unknown Colima profile %q: want gt-control or gt-sbx", profile)
	}
	out, err := run(ctx, "colima", "ssh", "-p", profile, "--", "sh", "-c", dnsCommand)
	if err != nil && len(out) == 0 {
		return DNSState{}, fmt.Errorf("read DNS in %s: %w", profile, err)
	}
	state := DNSState{ResolvConf: string(out), Answers: err == nil}
	state.Nameservers = parseNameservers(state.ResolvConf)
	return state, nil
}

// parseNameservers returns the address on every nameserver line, ignoring
// commented lines.
func parseNameservers(text string) []string {
	var addrs []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			addrs = append(addrs, fields[1])
		}
	}
	return addrs
}
