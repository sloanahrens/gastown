package main

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

// vmRead answers lsof -p pid with paths[pid], failing a pid it has no path for (gt-co0vrw).
func vmRead(paths map[string]string) func(time.Duration, string, ...string) (string, error) {
	return func(_ time.Duration, _ string, args ...string) (string, error) {
		path, ok := paths[args[1]]
		if !ok {
			return "", errors.New("exit status 1")
		}
		return path, nil
	}
}

// checkVM runs one vmSeconds case: its seconds by VM and whether it got one error (gt-co0vrw).
func checkVM(t *testing.T, name string, procs []hostload.Proc, paths map[string]string, want map[string]float64, wantErr bool) {
	got, errs := vmSeconds(procs, vmRead(paths))
	if (len(errs) == 1) != wantErr || !reflect.DeepEqual(got, want) {
		t.Errorf("%s: vmSeconds = %v, %v; want %v and one error %v", name, got, errs, want, wantErr)
	}
}

// TestHostCPUReadVMs totals each VM's seconds: two processes of one VM add, an
// unnamed and a non-VM process are ignored, and a failed lsof loses only its
// process and reports one error (gt-co0vrw).
func TestHostCPUReadVMs(t *testing.T) {
	t.Parallel()
	vm := func(pid int, sec float64) hostload.Proc {
		return hostload.Proc{PID: pid, CPUTime: sec, Command: "/System/Library/Frameworks/Virtualization.framework/Versions/A/Support/com.apple.Virtualization.VirtualMachine"}
	}
	sbx, control, docker := "/Users/me/.colima/_lima/colima-gt-sbx/disk", "/Users/me/.colima/_lima/colima-gt-control/disk", "/Users/me/Library/Containers/com.docker.docker/Data/vms/0/console.sock"
	checkVM(t, "three VMs", []hostload.Proc{vm(1, 10), vm(2, 20), vm(3, 30)}, map[string]string{"1": sbx, "2": control, "3": docker}, map[string]float64{hostload.VMName(sbx): 10, hostload.VMName(control): 20, hostload.VMName(docker): 30}, false)
	checkVM(t, "two processes of one VM add", []hostload.Proc{vm(1, 10), vm(4, 25)}, map[string]string{"1": sbx, "4": sbx}, map[string]float64{hostload.VMName(sbx): 35}, false)
	checkVM(t, "unnamed and non-VM ignored", []hostload.Proc{{PID: 5, CPUTime: 99, Command: "/bin/sh"}, vm(6, 40)}, map[string]string{"6": "/usr/lib/dyld"}, map[string]float64{}, false)
	checkVM(t, "one failed lsof loses only its process", []hostload.Proc{vm(7, 10), vm(8, 20), vm(9, 30)}, map[string]string{"7": sbx, "9": docker}, map[string]float64{hostload.VMName(sbx): 10, hostload.VMName(docker): 30}, true)
}
