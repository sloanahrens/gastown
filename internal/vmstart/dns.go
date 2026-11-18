// Package vmstart reads and repairs the DNS state of a Colima VM and finds and
// repairs the owner of the Mac's registry port, so the start command and the
// reboot check share one definition (gt-6ggud7, gt-x5a7bz, gt-8e43fp,
// gt-6imyon).
package vmstart

import (
	"context"
	"fmt"
	"strings"
)

// Runner runs one command and returns its standard output. An error means the
// command exited non-zero, and it carries the exit status as an ExitCode() int
// method, as *exec.ExitError does, wrapped or not; it must not carry the
// command's output, which can hold a secret. RegistryOwner reads that status to
// tell a tool's exit 1, which can be an answer, from a failed read.
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

// unreadableMarker is what the shell prints when cat could not read
// /etc/resolv.conf; the lookup still runs and its status is the command's, so
// without the marker a failed read on a VM whose lookup passes would pass as a
// clean state (gt-8bs4dv).
const unreadableMarker = "gt:resolv.conf:unreadable"

// dnsCommand is the reboot sheet's read: cat the file, then a bounded lookup.
// The shell exits with the lookup's status, so a non-zero exit is a failed
// answer rather than a failed read.
const dnsCommand = "cat /etc/resolv.conf || echo " + unreadableMarker +
	"; timeout 5 getent hosts github.com"

// vmProfiles are the two VMs the town runs; any other profile is refused before
// a call is made.
var vmProfiles = map[string]bool{"gt-control": true, "gt-sbx": true}

// CheckDNS reads the resolv.conf and DNS answer of the named VM. Answers is
// false for a lookup that failed, whether or not the command also failed after
// printing the file. The state is unknown — an error, never a state — when the
// command printed nothing, and when the read of /etc/resolv.conf failed, which
// the shell marks even if the lookup answered (gt-8bs4dv). A profile that is not
// gt-control or gt-sbx is refused before any call.
func CheckDNS(ctx context.Context, run Runner, profile string) (DNSState, error) {
	if !vmProfiles[profile] {
		return DNSState{}, fmt.Errorf("unknown Colima profile %q: want gt-control or gt-sbx", profile)
	}
	out, err := run(ctx, "colima", "ssh", "-p", profile, "--", "sh", "-c", dnsCommand)
	if err != nil && len(out) == 0 {
		return DNSState{}, fmt.Errorf("read DNS in %s: %w", profile, err)
	}
	if resolvConfUnreadable(string(out)) {
		return DNSState{}, fmt.Errorf("read /etc/resolv.conf in %s: file is unreadable", profile)
	}
	state := DNSState{ResolvConf: string(out), Answers: err == nil}
	state.Nameservers = parseNameservers(state.ResolvConf)
	return state, nil
}

// resolvConfUnreadable reports whether the read printed unreadableMarker, the
// only sign the shell's cat failed: an unread file is an unknown state, never a
// clean one. The check is per line because the lookup's answer follows the
// marker, and a resolv.conf that itself holds that line is refused with it.
func resolvConfUnreadable(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == unreadableMarker {
			return true
		}
	}
	return false
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
