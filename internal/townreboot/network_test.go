package townreboot

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Resolver fixtures: answeringConf points at the Lima gateway the VM should
// use, deadConf at a nameserver that does not answer, and lookupAnswer is the
// line a passing lookup prints after the file.
const (
	answeringConf = "# managed by lima\nnameserver 192.168.5.2\nsearch lima.local\n"
	lookupAnswer  = "140.82.113.4  github.com\n"
	deadConf      = "# managed by lima\nnameserver 192.168.5.3\nsearch lima.local\n"
)

// dnsResponse is what the fake Runner returns for one VM's read.
type dnsResponse struct {
	out string
	err error
}

// dnsRunner is a Runner that records every call and answers the read for each
// profile from responses, keyed by the profile CheckDNS passes.
func dnsRunner(calls *[][]string, responses map[string]dnsResponse) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		profile := profileArg(args)
		resp, ok := responses[profile]
		if !ok {
			return nil, fmt.Errorf("no fake response for profile %q", profile)
		}
		return []byte(resp.out), resp.err
	}
}

// profileArg is the value after -p, the profile CheckDNS names in its ssh argv.
func profileArg(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-p" {
			return args[i+1]
		}
	}
	return ""
}

// dnsRow runs the DNS check over run and returns its row, the form the report
// shows.
func dnsRow(run Runner) Row {
	return RunAll(context.Background(), []Check{DNSCheck(run)})[0]
}

// TestRebootCheckNetworkDNSRows pins the DNS check's findings: OK when both
// lookups answer, a not-OK row naming each profile that does not answer with
// its nameservers and the start fix, and a not-OK row naming a read error.
func TestRebootCheckNetworkDNSRows(t *testing.T) {
	t.Parallel()
	answering := dnsResponse{out: answeringConf + lookupAnswer}
	deadLookup := dnsResponse{out: deadConf, err: errors.New("exit status 1")}
	readError := dnsResponse{err: errors.New("ssh: connect: connection refused")}
	tests := []struct {
		name       string
		responses  map[string]dnsResponse
		wantOK     bool
		wantDetail string
		wantFix    string
	}{
		{
			name:       "both answer",
			responses:  map[string]dnsResponse{"gt-control": answering, "gt-sbx": answering},
			wantOK:     true,
			wantDetail: "gt-control, gt-sbx answer",
		},
		{
			name:       "one does not answer",
			responses:  map[string]dnsResponse{"gt-control": answering, "gt-sbx": deadLookup},
			wantDetail: "gt-sbx does not answer (nameservers: 192.168.5.3)",
			wantFix:    "gt town vms start",
		},
		{
			name:       "a read error",
			responses:  map[string]dnsResponse{"gt-control": readError, "gt-sbx": answering},
			wantDetail: "read DNS in gt-control: ssh: connect: connection refused",
			wantFix:    "gt town vms start",
		},
		{
			name:      "both broken",
			responses: map[string]dnsResponse{"gt-control": deadLookup, "gt-sbx": readError},
			wantDetail: "gt-control does not answer (nameservers: 192.168.5.3); " +
				"read DNS in gt-sbx: ssh: connect: connection refused",
			wantFix: "gt town vms start",
		},
		{
			name: "a file with no nameserver line",
			responses: map[string]dnsResponse{
				"gt-control": {out: "search lima.local\n", err: errors.New("exit status 1")},
				"gt-sbx":     answering,
			},
			wantDetail: "gt-control does not answer (nameservers: none)",
			wantFix:    "gt town vms start",
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
			if tt.responses != nil {
				run = dnsRunner(&calls, tt.responses)
			}
			row := dnsRow(run)
			if row.Name != "dns" {
				t.Errorf("row name = %q, want dns", row.Name)
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

// TestRebootCheckNetworkDNSReadOnly pins that the check only reads: the Runner
// sees one colima ssh read per profile and no start, restart, cancel or write
// verb, whether or not a lookup answers.
func TestRebootCheckNetworkDNSReadOnly(t *testing.T) {
	t.Parallel()
	answering := dnsResponse{out: answeringConf + lookupAnswer}
	deadLookup := dnsResponse{out: deadConf, err: errors.New("exit status 1")}
	writeVerbs := map[string]bool{
		"start": true, "restart": true, "cancel": true, "stop": true,
		"rm": true, "delete": true, "kill": true, "write": true,
	}
	tests := []struct {
		name      string
		responses map[string]dnsResponse
	}{
		{
			name:      "both answer",
			responses: map[string]dnsResponse{"gt-control": answering, "gt-sbx": answering},
		},
		{
			name:      "one does not answer",
			responses: map[string]dnsResponse{"gt-control": answering, "gt-sbx": deadLookup},
		},
		{
			name:      "a read error",
			responses: map[string]dnsResponse{"gt-control": answering, "gt-sbx": {err: errors.New("unreachable")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			dnsRow(dnsRunner(&calls, tt.responses))
			if len(calls) != len(dnsProfiles) {
				t.Fatalf("runner calls = %q, want one read per profile %q", calls, dnsProfiles)
			}
			for i, call := range calls {
				if len(call) < 4 || call[0] != "colima" || call[1] != "ssh" || call[2] != "-p" {
					t.Errorf("call %d = %q, want colima ssh -p <profile>", i, call)
					continue
				}
				if call[3] != dnsProfiles[i] {
					t.Errorf("call %d profile = %q, want %q", i, call[3], dnsProfiles[i])
				}
				for _, arg := range call {
					if writeVerbs[arg] {
						t.Errorf("runner saw the write verb %q in %q", arg, call)
					}
				}
			}
		})
	}
}
