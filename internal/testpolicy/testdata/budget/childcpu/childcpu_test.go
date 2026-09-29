// Package childcpu is a budget-runner fixture: its one test spends its CPU in
// a child process it waits for, so only a runner that counts the test
// binary's waited-for descendants sees it.
package childcpu

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

const childEnv = "BUDGET_FIXTURE_CHILD"

// childUser is the user CPU the child burns before it exits.
const childUser = 1500 * time.Millisecond

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		burn(childUser)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func burn(d time.Duration) {
	var ru syscall.Rusage
	x := 0
	for {
		for i := 0; i < 1_000_000; i++ {
			x += i
		}
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
			panic(err)
		}
		if time.Duration(ru.Utime.Nano()) >= d {
			_ = x
			return
		}
	}
}

func TestChildBurnsCPU(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}
