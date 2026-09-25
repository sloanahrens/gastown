package doltserver

import (
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// gt-p7zy0: a port holder or pid-file PID is only a candidate; nothing is
// signaled until its argv shows a dolt sql-server.

func TestLooksLikeDoltSQLServer(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"/opt/homebrew/bin/dolt", "sql-server", "--config", "/t/.dolt-data/config.yaml"}, true},
		{[]string{"dolt", "--data-dir", "/x", "sql-server"}, true},
		{[]string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}, false},
		{[]string{"dolt", "sql"}, false},
		{[]string{"/bin/sleep", "sql-server"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := looksLikeDoltSQLServer(c.args); got != c.want {
			t.Errorf("looksLikeDoltSQLServer(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestVerifyDoltSQLServerPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no argv check on Windows")
	}
	orig := processArgsForIdentity
	t.Cleanup(func() { processArgsForIdentity = orig })

	processArgsForIdentity = func(int) []string {
		return []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}
	}
	if err := VerifyDoltSQLServerPID(4242); err == nil || !strings.Contains(err.Error(), "not a dolt sql-server") {
		t.Errorf("Docker's port forwarder accepted as dolt: %v", err)
	}
	processArgsForIdentity = func(int) []string { return nil }
	if err := VerifyDoltSQLServerPID(4242); err == nil {
		t.Error("unreadable argv accepted as dolt")
	}
	processArgsForIdentity = func(int) []string { return []string{"dolt", "sql-server"} }
	if err := VerifyDoltSQLServerPID(4242); err != nil {
		t.Errorf("dolt sql-server rejected: %v", err)
	}
	if err := VerifyDoltSQLServerPID(os.Getpid()); err == nil {
		t.Error("this process accepted as dolt")
	}
	if err := VerifyDoltSQLServerPID(0); err == nil {
		t.Error("PID 0 accepted")
	}
}

// KillImposters on a port held by a non-dolt process — here this test
// process, standing in for com.docker.backend holding a container port —
// must refuse instead of signaling it. Before gt-p7zy0 this SIGTERMed the
// holder (the test binary itself here; Docker Desktop in the gate).
func TestKillImpostersDoesNotSignalNonDoltPortHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no argv check on Windows")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	if findDoltServerOnPort(port) == 0 {
		t.Skip("neither lsof nor ss can name the port holder here")
	}
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))

	err = KillImposters(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a verified dolt sql-server") {
		t.Fatalf("KillImposters = %v, want a refusal naming the unverified holder", err)
	}
}
