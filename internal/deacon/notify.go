package deacon

import "github.com/steveyegge/gastown/internal/notify"

// mayorNotifier returns n, or, when n is nil, gt run from the town root with
// the deacon's mutation-routing environment: the invocation the redispatch
// escalation mails have always used.
func mayorNotifier(n notify.Notifier, townRoot string) notify.Notifier {
	if n != nil {
		return n
	}
	return &notify.CLI{
		Dir: townRoot,
		Env: func() []string { return deaconMutationRoutingEnv(townRoot) },
	}
}
