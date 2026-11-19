package townreboot

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/vmstart"
)

// dnsProfiles are the two VMs a Mac reboot can leave with a resolver that no
// longer answers, in the order the check reads them.
var dnsProfiles = []string{"gt-control", "gt-sbx"}

// dnsFix starts both VMs, the repair for a resolver that does not answer
// (gt-x0ijs7).
const dnsFix = "gt town vms start"

// DNSCheck reads each VM's resolv.conf and a bounded lookup over run and passes
// when both lookups answer.
func DNSCheck(run Runner) Check {
	return Check{
		Name: "dns",
		Run: func(ctx context.Context) Result {
			if run == nil {
				// A check built without a runner is a wiring bug; a panic here
				// would hide every check after it (gt-x0ijs7).
				return Result{Detail: "no runner configured"}
			}
			vrun := vmstart.Runner(run)
			var problems []string
			for _, profile := range dnsProfiles {
				state, err := vmstart.CheckDNS(ctx, vrun, profile)
				if err != nil {
					// The resolver state is unknown, so the row says what failed
					// rather than reading it as no nameservers; the other VM's
					// read still runs because a reboot breaks both at once.
					problems = append(problems, err.Error())
					continue
				}
				if !state.Answers {
					problems = append(problems, fmt.Sprintf("%s does not answer (nameservers: %s)",
						profile, nameserverList(state.Nameservers)))
				}
			}
			if len(problems) > 0 {
				return Result{Detail: strings.Join(problems, "; "), Fix: dnsFix}
			}
			return Result{OK: true, Detail: strings.Join(dnsProfiles, ", ") + " answer"}
		},
	}
}

// nameserverList is the nameserver addresses CheckDNS read, or "none" when the
// file held no nameserver line.
func nameserverList(addrs []string) string {
	if len(addrs) == 0 {
		return "none"
	}
	return strings.Join(addrs, ", ")
}
