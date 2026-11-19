package townreboot

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/vmstart"
)

// registryProfile is the VM whose Lima forward must own the Mac's registry
// port; the control VM's node-local forwarder binds the same port, so only one
// of the two can own it (gt-8e43fp).
const registryProfile = "gt-sbx"

// registryFix is the repair when the registry forward is wrong: the same
// portable start the VM and DNS checks name.
const registryFix = "gt town vms start"

// RegistryCheck reads who owns the Mac's registry port and passes when the
// gt-sbx forward owns it and the registry answers there (gt-yrcdtw).
func RegistryCheck(run Runner) Check {
	return Check{
		Name: "registry",
		Run: func(ctx context.Context) Result {
			if run == nil {
				// A check built without a runner is a wiring bug; a panic here
				// would hide every check after it (gt-yrcdtw).
				return Result{Detail: "no runner configured"}
			}
			owner, err := vmstart.RegistryOwner(ctx, vmstart.Runner(run))
			if err != nil {
				// The owner is unknown, so the row says what failed rather than
				// reading it as nothing listening.
				return Result{Detail: fmt.Sprintf("read the registry forward: %v", err)}
			}
			if owner.Profile == registryProfile && owner.Healthy {
				return Result{OK: true, Detail: registryProfile + " owns port 5001 and answers"}
			}
			return Result{Detail: registryDetail(owner), Fix: registryFix}
		},
	}
}

// registryDetail names the owner the read found. An empty profile is a
// listener for neither VM, or none at all; the registry's answer tells the two
// apart, so a port that answers is never reported as nothing listening.
func registryDetail(owner vmstart.Owner) string {
	switch {
	case owner.Profile == registryProfile:
		return registryProfile + " owns port 5001 but the registry does not answer"
	case owner.Profile != "":
		return owner.Profile + " owns port 5001"
	case owner.Healthy:
		return "no gt-sbx or gt-control forward owns port 5001"
	}
	return "nothing listens on port 5001"
}

// NetworkChecks returns the checks that read the network path a reboot breaks,
// in the order the report runs them: the DNS resolver in both VMs, then the
// registry forward on the Mac (gt-yrcdtw).
func NetworkChecks(run Runner) []Check {
	return []Check{DNSCheck(run), RegistryCheck(run)}
}
