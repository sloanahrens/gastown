package vmstart

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
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

// registryRestartArgv restarts the sandbox VM's registry container, which reads
// the port at startup. It is a package-level list, as the DNS repair's restarts
// are, because the caller's Runner runs it: this package starts no process
// itself (gt-6imyon).
var registryRestartArgv = []string{"docker", "--context", "colima-" + profileSandbox, "restart", "gt-registry"}

// RepairRegistry cancels the gt-control VM's ssh forward of the Mac's registry
// port, when one holds it, and restarts gt-registry, returning the commands
// that ran, running nothing when the gt-sbx forward owns the port and the
// registry answers there (gt-77zpql).
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
	var repair [][]string
	// An unhealthy gt-sbx forward holds the port itself, so no gt-control
	// forward is there to cancel and the log need not name a port: the restart
	// alone is the repair, and a log without a port line must not stop it
	// (gt-77zpql).
	if owner.Profile != profileSandbox {
		logPath := filepath.Join(home, controlLimaDir, "ha.stderr.log")
		out, err := run(ctx, "tail", "-n", repairLogTail, logPath)
		if err != nil {
			return nil, fmt.Errorf("read the ssh port from %s: %w", logPath, err)
		}
		port, ok := forwardPort(string(out))
		if !ok {
			return nil, fmt.Errorf("find the %s ssh port in %s: no \"-p <port> 127\" argument", profileControl, logPath)
		}
		// -O cancel drops the forward the gt-control ssh master opened on the
		// Mac's registry port; the gt-sbx forward cannot bind it while held
		// (gt-6imyon).
		repair = append(repair, []string{"ssh", "-S", filepath.Join(home, controlLimaDir, "ssh.sock"), "-O", "cancel",
			"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", registryPort, registryPort),
			"-p", port, "127.0.0.1"})
	}
	repair = append(repair, registryRestartArgv)
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
// log printed, the shape Lima's ssh forward carries. A digit run outside
// 1..65535 is not a port, so it is skipped and only a TCP port reaches the
// repair's argv; the 127 host is part of the match because a line forwarding to
// another host is not the Mac's loopback forward.
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
		p, err := strconv.Atoi(rest[:n])
		if err != nil || p < 1 || p > 65535 {
			continue
		}
		port = strconv.Itoa(p)
	}
	return port, port != ""
}
