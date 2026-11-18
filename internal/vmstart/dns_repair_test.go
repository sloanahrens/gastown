package vmstart

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// repairStamp is the backup suffix repairClock names in UTC: 2026-11-18
// 16:27:26 UTC, read through a zone eight hours west, so a stamp formatted
// without the UTC conversion names a different file.
const repairStamp = "20261118-162726"

func repairClock() time.Time {
	return time.Date(2026, 11, 18, 8, 27, 26, 0, time.FixedZone("PST", -8*60*60))
}

// replyRunner answers the commands in table and records every call; a command
// outside the table is an error, so a repair that runs more than the test
// answered fails instead of passing quietly.
type replyRunner struct {
	table map[string]reply
	calls [][]string
}

type reply struct {
	out string
	err error
}

func (r *replyRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	got, ok := r.table[cmdKey(argv)]
	if !ok {
		return nil, fmt.Errorf("unexpected command %q", cmdKey(argv))
	}
	return []byte(got.out), got.err
}

func cmdKey(argv []string) string { return strings.Join(argv, " ") }

func readArgv(profile string) []string {
	return []string{"colima", "ssh", "-p", profile, "--", "sh", "-c",
		"cat /etc/resolv.conf; timeout 5 getent hosts github.com"}
}

func backupArgv(profile string) []string {
	return []string{"colima", "ssh", "-p", profile, "--",
		"sudo -n cp -p /etc/resolv.conf /etc/resolv.conf.bak-" + repairStamp}
}

func writeArgv(profile string) []string {
	return []string{"colima", "ssh", "-p", profile, "--",
		`sudo -n sh -c "echo nameserver 192.168.5.2 > /etc/resolv.conf"`}
}

var (
	corednsRestartArgv = []string{"kubectl", "--context", "colima-gt-control",
		"-n", "kube-system", "rollout", "restart", "deploy/coredns"}
	kindRestartArgv = []string{"docker", "--context", "colima-gt-sbx", "restart",
		"gt-sbx-control-plane", "gt-sbx-worker", "gt-sbx-worker2"}
)

// TestVMStartDNSRepairNoop: a VM whose lookup answers already works, so the
// repair writes nothing and reports no steps.
func TestVMStartDNSRepairNoop(t *testing.T) {
	t.Parallel()
	read := readArgv("gt-control")
	r := &replyRunner{table: map[string]reply{
		cmdKey(read): {out: testResolvConf + "140.82.113.4  github.com\n"},
	}}
	steps, err := RepairDNS(context.Background(), r.run, "gt-control", repairClock)
	if err != nil {
		t.Fatalf("RepairDNS: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %v, want none", steps)
	}
	if !reflect.DeepEqual(r.calls, [][]string{read}) {
		t.Fatalf("calls = %q, want only the read %q", r.calls, read)
	}
}

// TestVMStartDNSRepairOrder: a failed lookup backs up the file under the clock's
// UTC name, writes the gateway, then restarts coredns on gt-control and the
// three kind nodes on gt-sbx, in that order.
func TestVMStartDNSRepairOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		profile string
		restart []string
	}{
		{"gt-control", corednsRestartArgv},
		{"gt-sbx", kindRestartArgv},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			t.Parallel()
			read := readArgv(tc.profile)
			want := [][]string{read, backupArgv(tc.profile), writeArgv(tc.profile), tc.restart}
			table := map[string]reply{
				cmdKey(read): {out: testResolvConf, err: errors.New("exit status 1")},
			}
			for _, argv := range want[1:] {
				table[cmdKey(argv)] = reply{}
			}
			r := &replyRunner{table: table}
			steps, err := RepairDNS(context.Background(), r.run, tc.profile, repairClock)
			if err != nil {
				t.Fatalf("RepairDNS: %v", err)
			}
			var wantSteps []Step
			for _, argv := range want[1:] {
				wantSteps = append(wantSteps, Step{Command: cmdKey(argv)})
			}
			if !reflect.DeepEqual(steps, wantSteps) {
				t.Fatalf("steps = %v, want %v", steps, wantSteps)
			}
			if !reflect.DeepEqual(r.calls, want) {
				t.Fatalf("calls = %q, want %q", r.calls, want)
			}
		})
	}
}

// TestVMStartDNSRepairStops: a failed command ends the repair, so the steps
// report only what ran and the restart never happens.
func TestVMStartDNSRepairStops(t *testing.T) {
	t.Parallel()
	read := readArgv("gt-control")
	backup := backupArgv("gt-control")
	write := writeArgv("gt-control")
	cases := []struct {
		name      string
		fail      []string
		wantCalls [][]string
		wantSteps []Step
	}{
		{
			name:      "the backup fails",
			fail:      backup,
			wantCalls: [][]string{read, backup},
		},
		{
			name:      "the write fails",
			fail:      write,
			wantCalls: [][]string{read, backup, write},
			wantSteps: []Step{{Command: cmdKey(backup)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			table := map[string]reply{
				cmdKey(read): {out: testResolvConf, err: errors.New("exit status 1")},
			}
			for _, argv := range tc.wantCalls[1:] {
				table[cmdKey(argv)] = reply{}
			}
			table[cmdKey(tc.fail)] = reply{err: errors.New("exit status 1")}
			r := &replyRunner{table: table}
			steps, err := RepairDNS(context.Background(), r.run, "gt-control", repairClock)
			if err == nil {
				t.Fatal("RepairDNS reported success after a failed command")
			}
			if !reflect.DeepEqual(steps, tc.wantSteps) {
				t.Fatalf("steps = %v, want %v", steps, tc.wantSteps)
			}
			if !reflect.DeepEqual(r.calls, tc.wantCalls) {
				t.Fatalf("calls = %q, want %q and no restart", r.calls, tc.wantCalls)
			}
		})
	}
}

// TestVMStartDNSRepairProfiles: every profile the read accepts has a restart
// command, so a repair cannot rewrite the file and leave the process that
// cached the old resolver running.
func TestVMStartDNSRepairProfiles(t *testing.T) {
	t.Parallel()
	for profile := range vmProfiles {
		if len(restartCommands[profile]) == 0 {
			t.Errorf("profile %q has no restart command", profile)
		}
	}
}

// TestVMStartDNSRepairRefuses: an unknown profile, and a missing runner or
// clock, are refused before any VM call.
func TestVMStartDNSRepairRefuses(t *testing.T) {
	t.Parallel()
	answering := func(context.Context, string, ...string) ([]byte, error) {
		return []byte(testResolvConf + "140.82.113.4  github.com\n"), nil
	}
	cases := []struct {
		name    string
		run     Runner
		profile string
		now     func() time.Time
	}{
		{name: "an unknown profile", run: answering, profile: "gt-control-2", now: repairClock},
		{name: "a nil runner", profile: "gt-control", now: repairClock},
		{name: "a nil clock", run: answering, profile: "gt-control"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			steps, err := RepairDNS(context.Background(), tc.run, tc.profile, tc.now)
			if err == nil {
				t.Fatalf("RepairDNS accepted %s", tc.name)
			}
			if steps != nil {
				t.Fatalf("steps = %v, want none", steps)
			}
		})
	}
}
