//go:build integration

package cmd

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestIntegrationInstallDoltServerReuseRejectsNonMySQLPort probes a real TCP
// listener that is not a MySQL server.
func TestIntegrationInstallDoltServerReuseRejectsNonMySQLPort(t *testing.T) {
	t.Parallel()
	ln := listenAndHoldTCP(t)
	port := ln.Addr().(*net.TCPAddr).Port
	start := time.Now()
	if canReuseInstallDoltServer(t.TempDir(), port) {
		t.Fatal("non-MySQL listener should not be reusable as an existing Dolt server")
	}
	if elapsed := time.Since(start); elapsed > installDoltServerProbeTimeout+time.Second {
		t.Fatalf("non-MySQL listener probe took too long: %s", elapsed)
	}
}

func listenAndHoldTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				<-done
			}(conn)
		}
	}()
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})
	return ln
}

func installTestEnvWithFakeBD(t *testing.T, homeDir string) []string {
	t.Helper()
	return installTestEnv(t, homeDir, false)
}

func installTestEnvWithFakeBDAndDolt(t *testing.T, homeDir string) []string {
	t.Helper()
	return installTestEnv(t, homeDir, true)
}

func installTestEnv(t *testing.T, homeDir string, includeDolt bool) []string {
	t.Helper()

	binDir := t.TempDir()
	bdName := "bd"
	mode := os.FileMode(0755)
	content := "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then\n  echo 'bd version 999.0.0'\n  exit 0\nfi\necho 'fake bd only supports version' >&2\nexit 1\n"
	if runtime.GOOS == "windows" {
		bdName = "bd.bat"
		mode = 0644
		content = "@echo off\r\nif \"%1\"==\"version\" (\r\n  echo bd version 999.0.0\r\n  exit /b 0\r\n)\r\necho fake bd only supports version 1>&2\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(filepath.Join(binDir, bdName), []byte(content), mode); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	if includeDolt {
		doltName := "dolt"
		doltMode := os.FileMode(0755)
		doltContent := "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then\n  echo 'dolt version 999.0.0'\n  exit 0\nfi\nif [ \"$1\" = \"config\" ] && [ \"$2\" = \"--global\" ] && [ \"$3\" = \"--get\" ]; then\n  case \"$4\" in\n    user.name) echo 'Gas Town Test'; exit 0 ;;\n    user.email) echo 'gastown-test@example.com'; exit 0 ;;\n  esac\nfi\necho 'fake dolt only supports version and config --global --get' >&2\nexit 1\n"
		if runtime.GOOS == "windows" {
			doltName = "dolt.bat"
			doltMode = 0644
			doltContent = "@echo off\r\nif \"%1\"==\"version\" (\r\n  echo dolt version 999.0.0\r\n  exit /b 0\r\n)\r\nif \"%1\"==\"config\" if \"%2\"==\"--global\" if \"%3\"==\"--get\" (\r\n  if \"%4\"==\"user.name\" (\r\n    echo Gas Town Test\r\n    exit /b 0\r\n  )\r\n  if \"%4\"==\"user.email\" (\r\n    echo gastown-test@example.com\r\n    exit /b 0\r\n  )\r\n)\r\necho fake dolt only supports version and config --global --get 1>&2\r\nexit /b 1\r\n"
		}
		if err := os.WriteFile(filepath.Join(binDir, doltName), []byte(doltContent), doltMode); err != nil {
			t.Fatalf("write fake dolt: %v", err)
		}
	}

	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HOME", "PATH", "BEADS_DIR", "BEADS_DB", "BEADS_DOLT_SERVER_DATABASE", "GT_DOLT_PORT":
			continue
		default:
			env = append(env, entry)
		}
	}

	return append(env,
		"HOME="+homeDir,
		"PATH="+binDir,
		"BEADS_DIR=",
		"BEADS_DB=",
		"BEADS_DOLT_SERVER_DATABASE=",
		"GT_DOLT_PORT=",
	)
}
