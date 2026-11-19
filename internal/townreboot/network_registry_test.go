package townreboot

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// exitStatus stands in for *exec.ExitError: the Runner contract carries only an
// error, so the tests hand one that still reports the exit code.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// registryCommand is the listener command whose Lima control socket names the
// gt-sbx profile, as RegistryOwner's ps read returns it.
const registryCommand = "ssh -S /Users/me/.colima/_lima/colima-gt-sbx/ssh.sock -L 127.0.0.1:5001:127.0.0.1:5001"

// registryResponse is what the fake Runner answers RegistryOwner's three reads
// with.
type registryResponse struct {
	lsofOut, psOut, curlOut string
	lsofErr, psErr, curlErr error
}

// registryFake is a Runner that records every call and answers lsof, ps and
// curl from resp.
func registryFake(calls *[][]string, resp registryResponse) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		switch name {
		case "lsof":
			return []byte(resp.lsofOut), resp.lsofErr
		case "ps":
			return []byte(resp.psOut), resp.psErr
		case "curl":
			return []byte(resp.curlOut), resp.curlErr
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}
}

// TestRebootCheckNetworkRegistryRows pins the registry check's findings: OK
// only when the gt-sbx forward owns port 5001 and the registry answers; every
// other owner, including nothing listening and a read error, is a not-OK row
// that names what was found and the start fix (gt-yrcdtw).
func TestRebootCheckNetworkRegistryRows(t *testing.T) {
	t.Parallel()
	noListener := exitStatus(1)
	curlDead := exitStatus(7)
	tests := []struct {
		name       string
		resp       *registryResponse
		wantOK     bool
		wantDetail string
		wantFix    string
	}{
		{
			name:       "the sandbox forward answers",
			resp:       &registryResponse{lsofOut: "p4242\n", psOut: registryCommand, curlOut: "200"},
			wantOK:     true,
			wantDetail: "gt-sbx owns port 5001 and answers",
		},
		{
			name:       "the sandbox forward does not answer",
			resp:       &registryResponse{lsofOut: "p4242\n", psOut: registryCommand, curlOut: "000", curlErr: curlDead},
			wantDetail: "gt-sbx owns port 5001 but the registry does not answer",
			wantFix:    "gt town vms start",
		},
		{
			name:       "the control forwarder owns the port",
			resp:       &registryResponse{lsofOut: "p5151\n", psOut: "socat -d TCP-LISTEN:5001,fork,reuseaddr /Users/me/.colima/_lima/colima-gt-control/ssh.sock", curlOut: "200"},
			wantDetail: "gt-control owns port 5001",
			wantFix:    "gt town vms start",
		},
		{
			name:       "nothing listens",
			resp:       &registryResponse{lsofErr: noListener, curlOut: "000", curlErr: curlDead},
			wantDetail: "nothing listens on port 5001",
			wantFix:    "gt town vms start",
		},
		{
			name:       "an unknown listener answers",
			resp:       &registryResponse{lsofOut: "p6000\n", psOut: "/usr/local/bin/registry-proxy", curlOut: "200"},
			wantDetail: "no gt-sbx or gt-control forward owns port 5001",
			wantFix:    "gt town vms start",
		},
		{
			name:       "the listener read fails",
			resp:       &registryResponse{lsofErr: errors.New("lsof: operation not permitted")},
			wantDetail: "read the registry forward: read the listener on port 5001: lsof: operation not permitted",
		},
		{
			name:       "the command read fails",
			resp:       &registryResponse{lsofOut: "p4242\n", psErr: errors.New("ps: permission denied")},
			wantDetail: "read the registry forward: read the command of listener pid 4242: ps: permission denied",
		},
		{
			name:       "no runner",
			wantDetail: "no runner configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			var run Runner
			if tt.resp != nil {
				run = registryFake(&calls, *tt.resp)
			}
			row := RunAll(context.Background(), []Check{RegistryCheck(run)})[0]
			if row.Name != "registry" {
				t.Errorf("row name = %q, want registry", row.Name)
			}
			if row.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (detail %q)", row.OK, tt.wantOK, row.Detail)
			}
			if row.Detail != tt.wantDetail {
				t.Errorf("Detail = %q, want %q", row.Detail, tt.wantDetail)
			}
			if row.Fix != tt.wantFix {
				t.Errorf("Fix = %q, want %q", row.Fix, tt.wantFix)
			}
		})
	}
}

// readCommands are the commands the network checks may run: colima ssh reads
// the VM's resolver, lsof and ps read the port's owner, and curl reads the
// registry's answer.
var readCommands = map[string]bool{"colima": true, "lsof": true, "ps": true, "curl": true}

// networkFake is a Runner that answers every read in NetworkChecks with a
// healthy host and records every argv.
func networkFake(calls *[][]string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		switch name {
		case "colima":
			return []byte(answeringConf + lookupAnswer), nil
		case "lsof":
			return []byte("p4242\n"), nil
		case "ps":
			return []byte(registryCommand), nil
		case "curl":
			return []byte("200"), nil
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}
}

// TestRebootCheckNetworkRegistryOrder pins that NetworkChecks returns the DNS
// check then the registry check, and that on a healthy host the Runner ran only
// reads with no write verb in any argv word (gt-yrcdtw).
func TestRebootCheckNetworkRegistryOrder(t *testing.T) {
	t.Parallel()
	var calls [][]string
	checks := NetworkChecks(networkFake(&calls))
	if len(checks) != 2 {
		t.Fatalf("NetworkChecks = %d checks, want 2", len(checks))
	}
	rows := RunAll(context.Background(), checks)
	if rows[0].Name != "dns" {
		t.Errorf("checks[0] = %q, want dns", rows[0].Name)
	}
	if rows[1].Name != "registry" {
		t.Errorf("checks[1] = %q, want registry", rows[1].Name)
	}
	for _, row := range rows {
		if !row.OK {
			t.Errorf("row %q not OK on a healthy host: %q", row.Name, row.Detail)
		}
	}
	if len(calls) == 0 {
		t.Fatal("runner saw no commands")
	}
	for _, call := range calls {
		if !readCommands[call[0]] {
			t.Errorf("runner ran %q, which is not one of the read commands", call)
		}
		for _, arg := range call {
			for _, tok := range shellTokens(arg) {
				if writeVerbs[tok] {
					t.Errorf("runner saw the write verb %q in %q", tok, call)
				}
			}
		}
	}
}
