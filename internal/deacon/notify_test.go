package deacon

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

func TestMayorNotifierKeepsAnInjectedOne(t *testing.T) {
	t.Parallel()
	rec := notifyfake.New()
	if got := mayorNotifier(rec, "/town"); got != rec {
		t.Fatalf("mayorNotifier = %T, want the injected recorder", got)
	}
}

// TestMayorNotifierDefaultsToGtWithRoutingEnv pins the invocation the
// redispatch escalation mails always used: gt from the town root, with the
// deacon's mutation-routing environment.
func TestMayorNotifierDefaultsToGtWithRoutingEnv(t *testing.T) {
	t.Parallel()
	cli, ok := mayorNotifier(nil, "/town").(*notify.CLI)
	if !ok {
		t.Fatalf("mayorNotifier(nil) = %T, want *notify.CLI", mayorNotifier(nil, "/town"))
	}
	if cli.Dir != "/town" || cli.Bin != "" || cli.Env == nil {
		t.Fatalf("CLI = %+v, want gt from /town with an explicit env", cli)
	}
	if got, want := cli.Env(), deaconMutationRoutingEnv("/town"); !reflect.DeepEqual(got, want) {
		t.Errorf("env = %q\nwant %q", got, want)
	}
}
