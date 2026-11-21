package townreboot

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// its nameservers and the start fix, and a not-OK row naming a read error,
// which does not show the VM stopped and so carries no start fix (gt-x0ijs7).
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
		},
		{
			name:      "both read errors",
			responses: map[string]dnsResponse{"gt-control": readError, "gt-sbx": readError},
			wantDetail: "read DNS in gt-control: ssh: connect: connection refused; " +
				"read DNS in gt-sbx: ssh: connect: connection refused",
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

// writeVerbs are the verbs that change the VM or its host, so a read-only check
// must run none of them. CheckDNS passes its read to the VM as an sh -c string,
// so a write composed there hides inside one argv element unless the element is
// read as a script (gt-x0ijs7, gt-40i6it).
var writeVerbs = map[string]bool{
	"start": true, "stop": true, "restart": true, "delete": true,
	"cancel": true, "rm": true, "mv": true, "cp": true, "tee": true,
	"kill": true, "systemctl": true, "service": true, "write": true,
}

// shellTokens splits one argv element into the words a shell would run from it,
// so a write verb composed into the sh -c string is seen as a word of its own
// (gt-x0ijs7).
func shellTokens(arg string) []string {
	tokens := strings.FieldsFunc(arg, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ';', '&', '|', '(', ')':
			return true
		}
		return false
	})
	for i, tok := range tokens {
		tokens[i] = strings.Trim(tok, "\"'`")
	}
	return tokens
}

// shellCommandFlag reports whether arg runs the next argument as a shell
// command: -c, or a combined short flag holding c such as bash's -lc (gt-40i6it).
func shellCommandFlag(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	return strings.ContainsRune(arg[1:], 'c')
}

// writesAnywhere reports the first write verb or file redirect in argv and true
// when it found one. Each argument is read as a command word, and the string
// after a -c flag is read as a script, so a repair composed into the sh -c
// string cannot pass the read-only tests unseen (gt-40i6it).
func writesAnywhere(argv []string) (string, bool) {
	for i, arg := range argv {
		if i+1 < len(argv) && shellCommandFlag(arg) {
			if verb, found := writesInScript(argv[i+1]); found {
				return verb, found
			}
			continue
		}
		if verb, found := writeFirstWord(arg); found {
			return verb, found
		}
	}
	return "", false
}

// writesInScript reports the first write in one sh -c string: a command whose
// first word is a write verb, or one that redirects output to a file other than
// /dev/null.
func writesInScript(script string) (string, bool) {
	for _, command := range splitShellCommands(script) {
		if verb, found := writeFirstWord(command); found {
			return verb, found
		}
		if verb, found := fileRedirect(command); found {
			return verb, found
		}
	}
	return "", false
}

// splitShellCommands splits a shell string on ;, &&, ||, |, & and newlines,
// one command per part (gt-40i6it).
func splitShellCommands(script string) []string {
	var commands []string
	start := 0
	for i := 0; i < len(script); i++ {
		if script[i] != ';' && script[i] != '|' && script[i] != '&' && script[i] != '\n' {
			continue
		}
		commands = append(commands, script[start:i])
		start = i + 1
	}
	return append(commands, script[start:])
}

// writeFirstWord reports the first word of one shell command when it is a write
// verb. A leading env, sudo, sudo's own flags and a VAR=value assignment are
// prefixes, not the command (gt-40i6it).
func writeFirstWord(command string) (string, bool) {
	words := strings.Fields(command)
	i := 0
	for i < len(words) {
		w := words[i]
		if w != "env" && w != "sudo" && !strings.HasPrefix(w, "-") && !isAssignment(w) {
			break
		}
		i++
	}
	if i == len(words) {
		return "", false
	}
	if writeVerbs[words[i]] {
		return words[i], true
	}
	if words[i] == "sed" && sedInPlace(words[i:]) {
		return "sed", true
	}
	return "", false
}

// sedInPlace reports whether sed edits in place, the only sed mode that writes:
// -i, a combined short flag holding i, or --in-place (gt-40i6it).
func sedInPlace(words []string) bool {
	for _, w := range words[1:] {
		if w == "--in-place" {
			return true
		}
		if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.ContainsRune(w, 'i') {
			return true
		}
	}
	return false
}

// isAssignment reports whether w is a VAR=value word the shell sets before it
// runs the command that follows (gt-40i6it).
func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := w[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// fileRedirect reports the output redirect of one shell command when it writes
// a file other than /dev/null; an empty target such as >&2 duplicates a
// descriptor and writes nothing, and a read's 2>/dev/null stays allowed
// (gt-40i6it).
func fileRedirect(command string) (string, bool) {
	for i := 0; i < len(command); i++ {
		if command[i] != '>' {
			continue
		}
		op := ">"
		j := i + 1
		if j < len(command) && command[j] == '>' {
			op = ">>"
			j++
		}
		for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
			j++
		}
		k := j
		for k < len(command) && !strings.ContainsRune(" \t;&|<>", rune(command[k])) {
			k++
		}
		target := strings.Trim(command[j:k], "\"'")
		if target == "" || target == "/dev/null" {
			continue
		}
		return op + " " + target, true
	}
	return "", false
}

// firstWrite returns the first write verb or file redirect writesAnywhere finds
// in the commands the Runner saw, and the command that carried it (gt-40i6it).
func firstWrite(calls [][]string) (verb string, call []string, found bool) {
	for _, call := range calls {
		if verb, found := writesAnywhere(call); found {
			return verb, call, true
		}
	}
	return "", nil, false
}

// repairRunner answers like dnsRunner but chains a repair into the shell string
// of every command it records, as a check that repaired while it read would;
// the read-only test must report it (gt-40i6it).
func repairRunner(calls *[][]string, responses map[string]dnsResponse) Runner {
	read := dnsRunner(calls, responses)
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for i := 0; i+1 < len(args); i++ {
			if shellCommandFlag(args[i]) {
				args[i+1] += "; systemctl restart gt-sbx"
				break
			}
		}
		return read(ctx, name, args...)
	}
}

// TestRebootCheckNetworkDNSReadOnly pins that the check only reads: the Runner
// sees one colima ssh read per profile and no write verb or file redirect in any
// command, including one composed into the sh -c string, whether or not a
// lookup answers (gt-x0ijs7, gt-40i6it).
func TestRebootCheckNetworkDNSReadOnly(t *testing.T) {
	t.Parallel()
	answering := dnsResponse{out: answeringConf + lookupAnswer}
	deadLookup := dnsResponse{out: deadConf, err: errors.New("exit status 1")}
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
			}
			if verb, call, found := firstWrite(calls); found {
				t.Errorf("runner saw the write verb %q in %q", verb, call)
			}
		})
	}
	t.Run("a repair chained into the shell string is caught", func(t *testing.T) {
		t.Parallel()
		var calls [][]string
		healthy := map[string]dnsResponse{"gt-control": answering, "gt-sbx": answering}
		dnsRow(repairRunner(&calls, healthy))
		verb, call, found := firstWrite(calls)
		if !found {
			t.Fatalf("read-only check passed commands carrying a chained write verb: %q", calls)
		}
		if verb != "systemctl" {
			t.Errorf("found verb = %q, want systemctl in %q", verb, call)
		}
	})
}

// TestRebootCheckNetworkDNSWritesAnywhere pins writesAnywhere, the scan the
// read-only test runs: it finds a write verb at an argv head, after sh -c, after
// each shell separator, in bash -lc and behind an env, sudo or VAR=value
// prefix; a file redirect and sed -i are writes, while a /dev/null redirect, a
// descriptor duplication, a plain sed read and the argv CheckDNS runs are not
// (gt-40i6it).
func TestRebootCheckNetworkDNSWritesAnywhere(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		argv      []string
		wantVerb  string
		wantFound bool
	}{
		{name: "at the argv head", argv: []string{"systemctl", "restart", "gt-control"}, wantVerb: "systemctl", wantFound: true},
		{name: "after sh -c", argv: []string{"colima", "ssh", "-p", "gt-control", "--", "sh", "-c", "cat /etc/resolv.conf; rm /etc/resolv.conf"}, wantVerb: "rm", wantFound: true},
		{name: "after ;", argv: []string{"sh", "-c", "cat /etc/resolv.conf; systemctl restart gt-control"}, wantVerb: "systemctl", wantFound: true},
		{name: "after &&", argv: []string{"sh", "-c", "cat /etc/resolv.conf && mv a b"}, wantVerb: "mv", wantFound: true},
		{name: "after ||", argv: []string{"sh", "-c", "cat /etc/resolv.conf || sudo rm /etc/resolv.conf"}, wantVerb: "rm", wantFound: true},
		{name: "after |", argv: []string{"sh", "-c", "cat /etc/resolv.conf | tee /etc/resolv.conf"}, wantVerb: "tee", wantFound: true},
		{name: "after a bare &", argv: []string{"sh", "-c", "cat /etc/resolv.conf & cp a b"}, wantVerb: "cp", wantFound: true},
		{name: "after a newline", argv: []string{"sh", "-c", "cat /etc/resolv.conf\nservice dns restart"}, wantVerb: "service", wantFound: true},
		{name: "in bash -lc", argv: []string{"bash", "-lc", "cat /etc/resolv.conf; cp a b"}, wantVerb: "cp", wantFound: true},
		{name: "behind env and a variable assignment", argv: []string{"sh", "-c", "FOO=1 env BAR=2 kill 1"}, wantVerb: "kill", wantFound: true},
		{name: "behind sudo's own flag", argv: []string{"sh", "-c", "sudo -n rm /etc/resolv.conf"}, wantVerb: "rm", wantFound: true},
		{name: "in sed -i", argv: []string{"sh", "-c", "sed -i s/a/b/ /etc/resolv.conf"}, wantVerb: "sed", wantFound: true},
		{name: "in sed --in-place", argv: []string{"sh", "-c", "sed --in-place s/a/b/ /etc/resolv.conf"}, wantVerb: "sed", wantFound: true},
		{name: "in a redirect to a file", argv: []string{"sh", "-c", "echo nameserver 192.168.5.2 > /etc/resolv.conf"}, wantVerb: "> /etc/resolv.conf", wantFound: true},
		{name: "in an append redirect to a file", argv: []string{"sh", "-c", "echo nameserver 192.168.5.2 >> /etc/resolv.conf"}, wantVerb: ">> /etc/resolv.conf", wantFound: true},
		{name: "in a redirect to /dev/null", argv: []string{"sh", "-c", "cat /etc/resolv.conf 2>/dev/null"}},
		{name: "in a descriptor duplication", argv: []string{"sh", "-c", "cat /etc/resolv.conf 2>&1"}},
		{name: "in sed without -i", argv: []string{"sh", "-c", "sed -n 1p /etc/resolv.conf"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			verb, found := writesAnywhere(tt.argv)
			if found != tt.wantFound {
				t.Fatalf("writesAnywhere(%q) found = %v (%q), want %v", tt.argv, found, verb, tt.wantFound)
			}
			if verb != tt.wantVerb {
				t.Errorf("writesAnywhere(%q) verb = %q, want %q", tt.argv, verb, tt.wantVerb)
			}
		})
	}
	// The read CheckDNS composes must pass the scan, captured from a healthy run
	// so the test reads the production command rather than a copy of it.
	healthy := map[string]dnsResponse{"gt-control": {out: answeringConf + lookupAnswer}, "gt-sbx": {out: answeringConf + lookupAnswer}}
	var calls [][]string
	dnsRow(dnsRunner(&calls, healthy))
	if len(calls) != len(dnsProfiles) {
		t.Fatalf("runner calls = %q, want one read per profile %q", calls, dnsProfiles)
	}
	for _, call := range calls {
		if verb, found := writesAnywhere(call); found {
			t.Errorf("writesAnywhere found %q in the read CheckDNS runs: %q", verb, call)
		}
	}
}
