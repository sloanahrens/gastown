package vmstart

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// registryPort is the Mac loopback port the gt-sbx VM's Lima forward publishes
// the sandbox registry on; the gt-control node-local forwarder binds the same
// port, so only one of the two can own it (gt-8e43fp).
const registryPort = 5001

// Profile names as they appear in the path of a Lima ssh control socket.
const (
	profileSandbox = "gt-sbx"
	profileControl = "gt-control"
)

// Owner is who holds the Mac's registry port and whether the registry answers
// there.
type Owner struct {
	// Profile is gt-sbx when the listener is the sandbox VM's Lima forward,
	// gt-control when it is the control VM's, and "" when nothing listens or
	// the command names neither.
	Profile string
	// Healthy is true when the registry answered 200 on /v2/.
	Healthy bool
}

// RegistryOwner reads who owns the Mac's registry port and whether the
// registry answers there. Nothing listening is the answer given when lsof
// exits without a match; any other failure of a read is an error, because a
// failure read as no listener would start a repair over a live forward.
func RegistryOwner(ctx context.Context, run Runner) (Owner, error) {
	listen, err := run(ctx, "lsof", "-nP", fmt.Sprintf("-iTCP:%d", registryPort), "-sTCP:LISTEN", "-Fp")
	if err != nil && !exitsWithoutMatch(err) {
		return Owner{}, fmt.Errorf("read the listener on port %d: %w", registryPort, err)
	}
	pid, err := listenerPID(listen)
	if err != nil {
		return Owner{}, err
	}
	var owner Owner
	if pid != "" {
		command, err := run(ctx, "ps", "-o", "command=", "-p", pid)
		if err != nil && !exitsWithoutMatch(err) {
			return Owner{}, fmt.Errorf("read the command of listener pid %s: %w", pid, err)
		}
		owner.Profile = profileOf(string(command))
	}
	code, err := run(ctx, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", fmt.Sprintf("http://127.0.0.1:%d/v2/", registryPort))
	if err != nil && len(code) == 0 {
		return Owner{}, fmt.Errorf("read the registry's answer on port %d: %w", registryPort, err)
	}
	owner.Healthy = strings.TrimSpace(string(code)) == "200"
	return owner, nil
}

// listenerPID returns the pid on the first p line of lsof -Fp output, which
// prints no such line when nothing listens. A line whose pid is not a decimal
// number is refused, so only digits reach ps's argv.
func listenerPID(out []byte) (string, error) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "p") {
			continue
		}
		pid := strings.TrimPrefix(line, "p")
		if pid == "" || strings.Trim(pid, "0123456789") != "" {
			return "", fmt.Errorf("lsof printed %q for the listener on port %d, which is not a pid", line, registryPort)
		}
		return pid, nil
	}
	return "", nil
}

// profileOf returns the VM whose Lima forward the command belongs to. The
// forward's ssh control socket carries the profile in its path, e.g.
// ~/.colima/_lima/colima-gt-sbx/ssh.sock.
func profileOf(command string) string {
	switch {
	case strings.Contains(command, "colima-"+profileSandbox):
		return profileSandbox
	case strings.Contains(command, "colima-"+profileControl):
		return profileControl
	}
	return ""
}

// exitsWithoutMatch reports whether err is a tool's exit 1: lsof exits 1 when
// nothing holds the port and ps exits 1 when the pid is gone, which are
// answers. An error with no exit code to read is a failure, not an answer.
func exitsWithoutMatch(err error) bool {
	var coded interface{ ExitCode() int }
	return errors.As(err, &coded) && coded.ExitCode() == 1
}
