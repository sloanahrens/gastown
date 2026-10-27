package installer

import (
	"fmt"
	"strconv"
	"strings"
)

// slotAcquired is the line `gt slot run` prints the moment it holds the gate
// slot (internal/slot's acquiredFormat, gt-kox0). Without it the build never
// started, whatever the command's status says, so the slot step reads the line
// rather than the status alone.
const slotAcquired = "Container-gate slot acquired"

// Build is the script's "Build and install" section up to the swap: `make
// build` in the checkout, through the container-gate slot when the caller named
// a role (rebuild-gt builds past its starvation threshold that way, gt-kox0).
// It ends the run when the build failed (failed/build-failed) or when the slot
// was not free in time (refused/slot-busy).
func (r *Run) Build() (Exit, bool) {
	expected := r.C.Expected
	r.C.Log("Building " + expected + " in " + r.Paths.RigDir + " (source: " + r.C.Source + ")")
	var stdout, stderr []byte
	var status int
	if r.Cfg.SlotRole == "" {
		stdout, stderr, status = r.Cmd.run(r.Paths.RigDir, "make", "SKIP_UPDATE_CHECK=1", "build")
		r.echo(stdout, stderr)
	} else {
		var ended bool
		var code Exit
		if stdout, stderr, status, ended, code = r.buildInSlot(); ended {
			return code, true
		}
	}
	if status != 0 {
		return r.FailInstall("build-failed", "make build failed for "+expected,
			"medium", "install-gt:build-failed", "failed"), true
	}
	return ExitOK, false
}

// buildInSlot is the build under `gt slot run`. ended is true when the slot was
// never taken: a gt that could not run at all is a failed install, since a
// retry would fail the same way with the same broken gt (gt-bqzoq), while a
// slot that stayed busy is a refusal the caller retries (exit 3).
func (r *Run) buildInSlot() (stdout, stderr []byte, status int, ended bool, code Exit) {
	gt := r.Paths.GT()
	if !r.Files.isExecutable(gt) {
		return nil, nil, 0, true, r.FailInstall("slot-unavailable",
			gt+" is missing or not executable; cannot take the container-gate slot",
			"medium", "install-gt:slot-unavailable", "failed")
	}
	stdout, stderr, status = r.Cmd.run(r.Paths.RigDir, gt, "slot", "run",
		"--role", r.Cfg.SlotRole, "--timeout", r.Cfg.SlotTimeout+"s",
		"--", "make", "SKIP_UPDATE_CHECK=1", "build")
	r.echo(stdout, stderr)
	if strings.Contains(string(stdout)+string(stderr), slotAcquired) {
		return stdout, stderr, status, false, ExitOK
	}
	// Exit 126/127 is a gt that could not run at all, which never asked for the
	// slot: reporting that as busy would tell the caller to retry with the gt
	// the retry has to replace.
	if status == 126 || status == 127 {
		return nil, nil, 0, true, r.FailInstall("slot-unavailable",
			"cannot run "+gt+" slot run (exit "+strconv.Itoa(status)+"); the container-gate slot was never requested",
			"medium", "install-gt:slot-unavailable", "failed")
	}
	r.C.Log("Did not get the container-gate slot within " + r.Cfg.SlotTimeout + "s.")
	r.C.Receipt("refused", r.C.Expected, r.C.Prev, "slot-busy")
	return nil, nil, 0, true, r.C.Finish(Result{Event: "refused", Commit: r.C.Expected, Prev: r.C.Prev, Reason: "slot-busy"}, ExitBusy)
}

// Install is the swap: `make install-local` puts the build in place. A run of
// install-local that exits 1 because the binary is already at HEAD is a no-op,
// not a failure — reachable only when the installed commit could not be read
// before the build. Anything else ends the run failed/build-failed.
func (r *Run) Install() (Exit, bool) {
	stdout, stderr, status := r.Cmd.run(r.Paths.RigDir, "make", "SKIP_UPDATE_CHECK=1",
		"INSTALL_DIR="+r.Paths.BinDir, "install-local")
	combined := string(stdout) + string(stderr)
	// The script echoes the captured output: the command substitution drops the
	// trailing newlines and echo puts one back.
	fmt.Fprintln(r.C.stdout(), strings.TrimRight(combined, "\n"))
	if status == 0 {
		return ExitOK, false
	}
	if strings.Contains(combined, "already at HEAD") {
		r.C.Receipt("noop", r.C.Expected, r.C.Prev, "already-installed")
		return r.C.Finish(Result{Event: "noop", Commit: r.C.Expected, Prev: r.C.Prev, Reason: "already-installed"}, ExitOK), true
	}
	return r.FailInstall("build-failed", "make install-local failed for "+r.C.Expected,
		"medium", "install-gt:build-failed", "failed"), true
}

// echo writes a command's two streams to stdout, where the script's `2>&1`
// leaves them. Each stream is written whole: a reader gets the text, not the
// interleaving.
func (r *Run) echo(stdout, stderr []byte) {
	w := r.C.stdout()
	_, _ = w.Write(stdout)
	_, _ = w.Write(stderr)
}
