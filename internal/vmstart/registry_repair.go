package vmstart

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// repairLogTail bounds the log read to the lines the newest forward wrote; the
// whole log can run to megabytes and only the last forward line matters
// (gt-6imyon).
const repairLogTail = "2000"

// controlLimaDir is where the colima-gt-control profile keeps the Lima state
// the repair reads and drives: the log that announces its ssh forwards and the
// control socket those forwards were opened on. It is relative to the home
// directory RepairRegistry takes rather than reads, so a test needs no file.
var controlLimaDir = filepath.Join(".colima", "_lima", "colima-"+profileControl)

// RepairRegistry cancels the gt-control VM's ssh forward of the Mac's registry
// port and restarts gt-registry, returning the commands that ran; a healthy
// gt-sbx forward already owns the port, so nothing runs (gt-6imyon).
func RepairRegistry(ctx context.Context, run Runner, home string) ([]Step, error) {
	if run == nil {
		return nil, errors.New("RepairRegistry: nil runner")
	}
	if home == "" {
		return nil, errors.New("RepairRegistry: empty home directory")
	}
	owner, err := RegistryOwner(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("repair the registry forward: %w", err)
	}
	if owner.Profile == profileSandbox && owner.Healthy {
		return nil, nil
	}
	logPath := filepath.Join(home, controlLimaDir, "ha.stderr.log")
	out, err := run(ctx, "tail", "-n", repairLogTail, logPath)
	if err != nil {
		return nil, fmt.Errorf("read the ssh port from %s: %w", logPath, err)
	}
	port, ok := forwardPort(string(out))
	if !ok {
		return nil, fmt.Errorf("find the %s ssh port in %s: no \"-p <port> 127\" argument", profileControl, logPath)
	}
	repair := [][]string{
		// -O cancel drops the forward the gt-control ssh master opened on the
		// Mac's registry port; the gt-sbx forward cannot bind it while held
		// (gt-6imyon).
		{"ssh", "-S", filepath.Join(home, controlLimaDir, "ssh.sock"), "-O", "cancel",
			"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", registryPort, registryPort),
			"-p", port, "127.0.0.1"},
		{"docker", "--context", "colima-" + profileSandbox, "restart", "gt-registry"},
	}
	var steps []Step
	for _, argv := range repair {
		if _, err := run(ctx, argv[0], argv[1:]...); err != nil {
			return steps, fmt.Errorf("repair the registry forward: %s failed: %w", strings.Join(argv, " "), err)
		}
		steps = append(steps, Step{Command: strings.Join(argv, " ")})
	}
	return steps, nil
}

// forwardPort returns the port of the last "-p <port> 127" argument pair the
// log printed, the shape Lima's ssh forward carries. Only a digit run is
// accepted, so nothing else reaches the repair's argv; the 127 host is part of
// the match because a line forwarding to another host is not the Mac's loopback
// forward.
func forwardPort(log string) (string, bool) {
	const flag = "-p "
	port := ""
	for rest := log; ; {
		i := strings.Index(rest, flag)
		if i < 0 {
			break
		}
		rest = rest[i+len(flag):]
		n := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
		if n <= 0 || !strings.HasPrefix(rest[n:], " 127") {
			continue
		}
		port = rest[:n]
	}
	return port, port != ""
}
