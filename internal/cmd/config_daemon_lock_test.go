//go:build unix

package cmd

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// A lock another writer holds is refused without waiting: nothing is written,
// no backup is left, and the refusal says so.
func TestConfigDaemonRespectsTheLock(t *testing.T) {
	t.Parallel()

	townRoot, path := daemonConfigTown(t, daemonConfigFixture)
	env, out, _ := daemonConfigTestEnv(townRoot, time.Now())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}()

	if err := configDaemonSet(env, "cli_theme", `"light"`, "theme"); !errors.Is(err, config.ErrConfigLocked) {
		t.Fatalf("set behind a held lock = %v, want ErrConfigLocked", err)
	}
	if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, before) {
		t.Errorf("file behind the held lock = %q, %v; want it unchanged", after, readErr)
	}
	if backups := daemonConfigBackups(t, path); len(backups) != 0 {
		t.Errorf("backups behind the held lock = %v, want none", backups)
	}
	if out.Len() != 0 {
		t.Errorf("output behind the held lock = %q, want nothing", out.String())
	}
}
