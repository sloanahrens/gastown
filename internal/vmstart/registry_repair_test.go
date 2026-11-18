package vmstart

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// repairHome is the home directory the registry repair tests build the
// gt-control Lima paths from; RepairRegistry receives it and hands it to the
// Runner, so no file on disk is read.
const repairHome = "/Users/me"

const (
	repairLogPath  = repairHome + "/.colima/_lima/colima-gt-control/ha.stderr.log"
	repairSockPath = repairHome + "/.colima/_lima/colima-gt-control/ssh.sock"
)

var (
	repairTailArgv    = []string{"tail", "-n", "2000", repairLogPath}
	repairRestartArgv = []string{"docker", "--context", "colima-gt-sbx", "restart", "gt-registry"}
)

func repairCancelArgv(port string) []string {
	return []string{"ssh", "-S", repairSockPath, "-O", "cancel",
		"-L", "127.0.0.1:5001:127.0.0.1:5001", "-p", port, "127.0.0.1"}
}

func repairPSArgv(pid string) []string { return []string{"ps", "-o", "command=", "-p", pid} }

// The command lines ps prints for the listener each VM's forward leaves behind.
const (
	listenerSandbox = "ssh -S /Users/me/.colima/_lima/colima-gt-sbx/ssh.sock -L 127.0.0.1:5001:127.0.0.1:5001"
	listenerControl = "socat -d TCP-LISTEN:5001,fork,reuseaddr /Users/me/.colima/_lima/colima-gt-control/ssh.sock"
)

// registryOwnerReplies answers RegistryOwner's three reads: the named listener
// owns the port and the registry answers with the named code.
func registryOwnerReplies(listener, health string) map[string]reply {
	return map[string]reply{
		cmdKey(registryLsofArgv):     {out: "p4242\n"},
		cmdKey(repairPSArgv("4242")): {out: listener},
		cmdKey(registryCurlArgv):     {out: health},
	}
}

// repairLog is gt-control's Lima log as it announces its forwards: two lines
// matching the Mac's loopback forward, the later one the live port, then a
// later port for another host, which is not the forward the repair cancels.
const repairLog = `time="2026-11-12T09:58:02Z" level=info msg="forwarding" argv=[ssh -S /Users/me/.colima/_lima/colima-gt-control/ssh.sock -O forward -L 127.0.0.1:5001:127.0.0.1:5001 -p 60021 127.0.0.1]
time="2026-11-12T09:59:41Z" level=info msg="forwarding" argv=[ssh -S /Users/me/.colima/_lima/colima-gt-control/ssh.sock -O forward -L 127.0.0.1:5001:127.0.0.1:5001 -p 60022 127.0.0.1]
time="2026-11-12T10:00:00Z" level=info msg="ssh" argv=[ssh -p 60099 192.168.5.2]
`

// TestVMStartRegistryRepairNoop: a healthy gt-sbx forward already owns the
// port, so the repair runs nothing after the owner read.
func TestVMStartRegistryRepairNoop(t *testing.T) {
	t.Parallel()
	r := &replyRunner{table: registryOwnerReplies(listenerSandbox, "200")}
	steps, err := RepairRegistry(context.Background(), r.run, repairHome)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %v, want none", steps)
	}
	want := [][]string{registryLsofArgv, repairPSArgv("4242"), registryCurlArgv}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want only the owner read %q", r.calls, want)
	}
}

// TestVMStartRegistryRepairOrder: a gt-control owner is canceled on the port
// of the last matching log line — not the earlier port, not the later one for
// another host — and gt-registry restarts after it, in that order.
func TestVMStartRegistryRepairOrder(t *testing.T) {
	t.Parallel()
	cancel := repairCancelArgv("60022")
	table := registryOwnerReplies(listenerControl, "200")
	table[cmdKey(repairTailArgv)] = reply{out: repairLog}
	table[cmdKey(cancel)] = reply{}
	table[cmdKey(repairRestartArgv)] = reply{}
	r := &replyRunner{table: table}
	steps, err := RepairRegistry(context.Background(), r.run, repairHome)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	wantSteps := []Step{{Command: cmdKey(cancel)}, {Command: cmdKey(repairRestartArgv)}}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("steps = %v, want %v", steps, wantSteps)
	}
	want := [][]string{registryLsofArgv, repairPSArgv("4242"), registryCurlArgv,
		repairTailArgv, cancel, repairRestartArgv}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
}

// TestVMStartRegistryRepairUnhealthySandbox: a gt-sbx forward that holds the
// port while the registry does not answer is repaired by the restart alone —
// no gt-control forward is there to cancel and no log port is needed — so the
// restart runs and the log is never read (gt-77zpql).
func TestVMStartRegistryRepairUnhealthySandbox(t *testing.T) {
	t.Parallel()
	table := registryOwnerReplies(listenerSandbox, "503")
	table[cmdKey(repairRestartArgv)] = reply{}
	r := &replyRunner{table: table}
	steps, err := RepairRegistry(context.Background(), r.run, repairHome)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	wantSteps := []Step{{Command: cmdKey(repairRestartArgv)}}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("steps = %v, want %v", steps, wantSteps)
	}
	want := [][]string{registryLsofArgv, repairPSArgv("4242"), registryCurlArgv, repairRestartArgv}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
}

// TestVMStartRegistryRepairPort: the cancel carries the log's port only when
// the run is a TCP port, and the smallest and largest ports are accepted in
// their written form; any other run is refused with the log named and no
// command after the read (gt-77zpql).
func TestVMStartRegistryRepairPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  string
		port string // the port the cancel carries; "" for a refusal
	}{
		{name: "the smallest port", run: "1", port: "1"},
		{name: "the largest port", run: "65535", port: "65535"},
		{name: "zero", run: "0"},
		{name: "one past the largest port", run: "65536"},
		{name: "a digit run too long for an int", run: "999999999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := `time="2026-11-12T09:58:02Z" level=info msg="forwarding" argv=[ssh -S /Users/me/.colima/_lima/colima-gt-control/ssh.sock -O forward -L 127.0.0.1:5001:127.0.0.1:5001 -p ` + tc.run + ` 127.0.0.1]` + "\n"
			table := registryOwnerReplies(listenerControl, "200")
			table[cmdKey(repairTailArgv)] = reply{out: log}
			if tc.port != "" {
				table[cmdKey(repairCancelArgv(tc.port))] = reply{}
				table[cmdKey(repairRestartArgv)] = reply{}
			}
			r := &replyRunner{table: table}
			steps, err := RepairRegistry(context.Background(), r.run, repairHome)
			if tc.port == "" {
				if err == nil {
					t.Fatal("RepairRegistry put a run outside 1..65535 in the cancel's argv")
				}
				if !strings.Contains(err.Error(), "ha.stderr.log") {
					t.Fatalf("error = %q, want it to name ha.stderr.log", err)
				}
				if steps != nil {
					t.Fatalf("steps = %v, want none", steps)
				}
				want := [][]string{registryLsofArgv, repairPSArgv("4242"), registryCurlArgv, repairTailArgv}
				if !reflect.DeepEqual(r.calls, want) {
					t.Fatalf("calls = %q, want %q and no repair", r.calls, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("RepairRegistry: %v", err)
			}
			cancel := repairCancelArgv(tc.port)
			wantSteps := []Step{{Command: cmdKey(cancel)}, {Command: cmdKey(repairRestartArgv)}}
			if !reflect.DeepEqual(steps, wantSteps) {
				t.Fatalf("steps = %v, want %v", steps, wantSteps)
			}
			want := [][]string{registryLsofArgv, repairPSArgv("4242"), registryCurlArgv,
				repairTailArgv, cancel, repairRestartArgv}
			if !reflect.DeepEqual(r.calls, want) {
				t.Fatalf("calls = %q, want %q", r.calls, want)
			}
		})
	}
}

// TestVMStartRegistryRepairNoPort: a log with no matching line is an error
// naming the file, and the repair runs no cancel.
func TestVMStartRegistryRepairNoPort(t *testing.T) {
	t.Parallel()
	table := registryOwnerReplies(listenerControl, "200")
	table[cmdKey(repairTailArgv)] = reply{out: `time="2026-11-12T09:58:02Z" level=info msg="starting"` + "\n"}
	r := &replyRunner{table: table}
	steps, err := RepairRegistry(context.Background(), r.run, repairHome)
	if err == nil {
		t.Fatal("RepairRegistry canceled a forward with no port in the log")
	}
	if !strings.Contains(err.Error(), "ha.stderr.log") {
		t.Fatalf("error = %q, want it to name ha.stderr.log", err)
	}
	if steps != nil {
		t.Fatalf("steps = %v, want none", steps)
	}
	want := [][]string{registryLsofArgv, repairPSArgv("4242"), registryCurlArgv, repairTailArgv}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q and no repair", r.calls, want)
	}
}

// TestVMStartRegistryRepairStops: a failed read or repair command ends the
// repair, so the steps report only what ran and the commands after the failure
// never run.
func TestVMStartRegistryRepairStops(t *testing.T) {
	t.Parallel()
	cancel := repairCancelArgv("60022")
	owner := [][]string{registryLsofArgv, repairPSArgv("4242"), registryCurlArgv}
	before := append(append([][]string{}, owner...), repairTailArgv)
	cases := []struct {
		name      string
		fail      []string
		failOut   string
		wantCalls [][]string
		wantSteps []Step
	}{
		{name: "the owner read fails", fail: registryLsofArgv, wantCalls: owner[:1]},
		// The failed read still prints a usable log, so a repair that ignores
		// the error would cancel a port and pass a check that only wanted none.
		{name: "the log read fails", fail: repairTailArgv, failOut: repairLog, wantCalls: before},
		{name: "the cancel fails", fail: cancel,
			wantCalls: append(append([][]string{}, before...), cancel)},
		{name: "the restart fails", fail: repairRestartArgv,
			wantCalls: append(append([][]string{}, before...), cancel, repairRestartArgv),
			wantSteps: []Step{{Command: cmdKey(cancel)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			table := registryOwnerReplies(listenerControl, "200")
			table[cmdKey(repairTailArgv)] = reply{out: repairLog}
			table[cmdKey(cancel)] = reply{}
			table[cmdKey(repairRestartArgv)] = reply{}
			table[cmdKey(tc.fail)] = reply{out: tc.failOut, err: errors.New("the command failed")}
			r := &replyRunner{table: table}
			steps, err := RepairRegistry(context.Background(), r.run, repairHome)
			if err == nil {
				t.Fatal("RepairRegistry reported success after a failed command")
			}
			if !reflect.DeepEqual(steps, tc.wantSteps) {
				t.Fatalf("steps = %v, want %v", steps, tc.wantSteps)
			}
			if !reflect.DeepEqual(r.calls, tc.wantCalls) {
				t.Fatalf("calls = %q, want %q", r.calls, tc.wantCalls)
			}
		})
	}
}

// TestVMStartRegistryRepairRefuses: a nil runner and an empty home are refused
// before any command runs.
func TestVMStartRegistryRepairRefuses(t *testing.T) {
	t.Parallel()
	t.Run("a nil runner", func(t *testing.T) {
		t.Parallel()
		steps, err := RepairRegistry(context.Background(), nil, repairHome)
		if err == nil {
			t.Fatal("RepairRegistry accepted a nil runner")
		}
		if steps != nil {
			t.Fatalf("steps = %v, want none", steps)
		}
	})
	t.Run("an empty home", func(t *testing.T) {
		t.Parallel()
		var calls [][]string
		record := func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			return nil, nil
		}
		steps, err := RepairRegistry(context.Background(), record, "")
		if err == nil {
			t.Fatal("RepairRegistry accepted an empty home")
		}
		if steps != nil {
			t.Fatalf("steps = %v, want none", steps)
		}
		if len(calls) != 0 {
			t.Fatalf("calls = %q, want none", calls)
		}
	})
}
