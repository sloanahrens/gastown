package vmstart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// backupStamp formats the suffix of the backup file in UTC.
const backupStamp = "20060102-150405"

// restartCommands names what reads the resolver once at startup on each VM, so
// it has to restart after the file is rewritten (gt-x5a7bz). A profile CheckDNS
// accepts with no entry here would write the file and then index a nil command,
// so TestVMStartDNSRepairProfiles keeps the two profile sets equal.
var restartCommands = map[string][]string{
	"gt-control": {"kubectl", "--context", "colima-gt-control", "-n", "kube-system", "rollout", "restart", "deploy/coredns"},
	"gt-sbx":     {"docker", "--context", "colima-gt-sbx", "restart", "gt-sbx-control-plane", "gt-sbx-worker", "gt-sbx-worker2"},
}

// vmArgv wraps one command to run inside the named VM. The command is a single
// argv element because ssh hands its arguments to the VM's shell as one line,
// so the root shell that must perform the redirect has to be quoted here, not
// split into words that the outer shell would re-parse.
func vmArgv(profile, command string) []string {
	return []string{"colima", "ssh", "-p", profile, "--", command}
}

// repairCommands is the repair's commands in order: keep the old resolver,
// point it at the gateway, then restart what cached the old one.
func repairCommands(profile, stamp string) [][]string {
	return [][]string{
		vmArgv(profile, "sudo -n cp -p /etc/resolv.conf /etc/resolv.conf.bak-"+stamp),
		vmArgv(profile, `sudo -n sh -c "echo nameserver `+LimaGateway+` > /etc/resolv.conf"`),
		restartCommands[profile],
	}
}

// RepairDNS rewrites the named VM's resolver to LimaGateway and restarts the
// service that read the resolver at startup, returning the commands that ran;
// a VM whose lookup already answers runs nothing (gt-x5a7bz).
func RepairDNS(ctx context.Context, run Runner, profile string, now func() time.Time) ([]Step, error) {
	if run == nil {
		return nil, errors.New("RepairDNS: nil runner")
	}
	if now == nil {
		return nil, errors.New("RepairDNS: nil clock")
	}
	state, err := CheckDNS(ctx, run, profile)
	if err != nil {
		return nil, err
	}
	if state.Answers {
		return nil, nil
	}
	var steps []Step
	for _, argv := range repairCommands(profile, now().UTC().Format(backupStamp)) {
		if _, err := run(ctx, argv[0], argv[1:]...); err != nil {
			return steps, fmt.Errorf("repair DNS in %s: %s failed: %w", profile, strings.Join(argv, " "), err)
		}
		steps = append(steps, Step{Command: strings.Join(argv, " ")})
	}
	return steps, nil
}
