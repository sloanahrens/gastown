package cmd

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge/deliver"
)

// nudgeSenderCases pin who gt nudge says a nudge is from, by the caller's
// working directory (relative to the town root) and identity environment.
var nudgeSenderCases = []struct {
	name string
	cwd  string
	env  map[string]string
	want string
}{
	{"town root, no identity (the daemon)", ".", nil, "unknown"},
	{"mayor dir", "mayor", nil, "mayor"},
	{"crew dir", "gastown/crew/max", nil, "gastown/crew/max"},
	{"polecat dir", "gastown/polecats/toast", nil, "gastown/toast"},
	{"rig root", "gastown", nil, "unknown"},
	{"retired deacon dir", "deacon", nil, "unknown"},
	{"GT_ROLE mayor anywhere", ".", map[string]string{"GT_ROLE": "mayor"}, "mayor"},
	{"GT_ROLE crew", ".", map[string]string{"GT_ROLE": "gastown/crew/max"}, "gastown/crew/max"},
	{"GT_ROLE crew with GT_RIG and GT_CREW", ".", map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "max"}, "gastown/crew/max"},
	{"GT_ROLE polecat filled from cwd", "gastown/polecats/toast", map[string]string{"GT_ROLE": "polecat"}, "gastown/toast"},
	{"GT_ROLE beats cwd", "gastown/crew/max", map[string]string{"GT_ROLE": "mayor"}, "mayor"},
	{"GT_ROLE unknown simple role", ".", map[string]string{"GT_ROLE": "overseer"}, "overseer"},
	{"GT_ROLE retired witness", ".", map[string]string{"GT_ROLE": "gastown/witness"}, "gastown/witness"},
}

func TestNudgeSenderFromRole(t *testing.T) {
	t.Parallel()
	town := "/town"
	for _, tc := range nudgeSenderCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info, err := getRoleWithContextEnv(filepath.Join(town, tc.cwd), town, func(k string) string { return tc.env[k] })
			if got := nudgeSender(info, err); got != tc.want {
				t.Errorf("sender = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNudgeSenderUnreadableRoleIsUnknown(t *testing.T) {
	t.Parallel()
	if got := nudgeSender(RoleInfo{Role: RoleMayor}, errors.New("not in a Gas Town workspace")); got != "unknown" {
		t.Errorf("sender = %q, want unknown", got)
	}
}

// TestNudgeSenderMatchesDeliverSender holds deliver.Sender, which the daemon's
// in-process Notifier attributes its nudges with, to what gt nudge derives
// from the caller's role.
func TestNudgeSenderMatchesDeliverSender(t *testing.T) {
	t.Parallel()
	town := "/town"
	for _, tc := range nudgeSenderCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cwd, getenv := filepath.Join(town, tc.cwd), func(k string) string { return tc.env[k] }
			want := nudgeSender(getRoleWithContextEnv(cwd, town, getenv))
			if got := deliver.Sender(cwd, town, getenv); got != want {
				t.Errorf("deliver.Sender = %q, gt nudge's sender = %q", got, want)
			}
		})
	}
	if got := deliver.Sender("/elsewhere", "", func(string) string { return "mayor" }); got != "unknown" {
		t.Errorf("deliver.Sender outside a town = %q, want unknown (gt nudge cannot read a role there)", got)
	}
}
