package townreboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// vmProfiles are the two Colima VMs a reboot leaves stopped: the control
// cluster's and the sandbox's, in the order the check reads them (gt-qrc7hp).
var vmProfiles = []string{"gt-control", "gt-sbx"}

// vmRunning is the status `colima list --json` reports for a started VM.
const vmRunning = "Running"

// vmFix is the repair when a VM is not running: the same portable start the DNS
// check names. A host-only command such as launchctl may not appear in a gt
// string literal, because the host-tools ratchet refuses one (gt-qrc7hp).
const vmFix = "gt town vms start"

// vmInstance is the part of one `colima list --json` record the check reads.
type vmInstance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// VMCheck reads the Colima VMs over run and passes when gt-control and gt-sbx
// are Running (gt-qrc7hp).
func VMCheck(run Runner) Check {
	return Check{
		Name: "vms",
		Run: func(ctx context.Context) Result {
			if run == nil {
				// A check built without a runner is a wiring bug; a panic here
				// would hide every check after it (gt-qrc7hp).
				return Result{Detail: "no runner configured"}
			}
			out, err := run(ctx, "colima", "list", "--json")
			if err != nil {
				// The VM list is unknown, so the row says what failed rather
				// than reading an empty list as both VMs running.
				return Result{Detail: fmt.Sprintf("list VMs: %v", err)}
			}
			instances, err := parseVMList(out)
			if err != nil {
				// Output the check cannot read is a finding about the host, not
				// a pass: a person must see the VM state is unknown.
				return Result{Detail: fmt.Sprintf("read colima list output: %v", err)}
			}
			status := make(map[string]string, len(instances))
			for _, inst := range instances {
				status[inst.Name] = inst.Status
			}
			var notRunning []string
			for _, name := range vmProfiles {
				switch s, listed := status[name]; {
				case !listed:
					notRunning = append(notRunning, name+" (not listed)")
				case s != vmRunning:
					notRunning = append(notRunning, name+" ("+s+")")
				}
			}
			if len(notRunning) > 0 {
				return Result{Detail: "not running: " + strings.Join(notRunning, ", "), Fix: vmFix}
			}
			return Result{OK: true, Detail: strings.Join(vmProfiles, ", ") + " running"}
		},
	}
}

// parseVMList reads `colima list --json` as one JSON array of instances or as
// one instance per line, so a Colima release that changes which shape it prints
// does not turn every run of the check into an unknown. Empty output is an
// error, never an empty list, because no listed VMs is a different finding from
// a list the check could not read. A record with no name is an error too: a
// bare null decodes to one, and a nameless record is a list the check cannot
// read rather than a VM it can report on (gt-qrc7hp).
func parseVMList(out []byte) ([]vmInstance, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("no output")
	}
	var list []vmInstance
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		for {
			var inst vmInstance
			if err := dec.Decode(&inst); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, err
			}
			list = append(list, inst)
		}
	}
	for i, inst := range list {
		if inst.Name == "" {
			return nil, fmt.Errorf("record %d has no name", i+1)
		}
	}
	return list, nil
}
