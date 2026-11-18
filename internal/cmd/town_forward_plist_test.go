package cmd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const forwardPlistGoldenPath = "town_forward_plist.golden"

// forwardPlistHome is the home directory the test passes in, so the golden holds
// no host path.
const forwardPlistHome = "/Users/operator"

// forwardPlistKubectl is the client path the default fake lookup returns, so the
// tests need no kubectl on the host.
const forwardPlistKubectl = "/opt/homebrew/bin/kubectl"

// forwardPlistArgs are the flags of a valid `plist` run.
func forwardPlistArgs() []string {
	return []string{
		"--service", "dashboard",
		"--context", "colima-gt-control",
		"--namespace", "gt",
		"--port", "8787",
	}
}

// forwardPlistArgsWith is the valid run with one flag's value replaced, or added
// when the valid run does not pass it. An empty value drops the flag.
func forwardPlistArgsWith(name, value string) []string {
	var out []string
	replaced := false
	args := forwardPlistArgs()
	for i := 0; i < len(args); i += 2 {
		if args[i] != name {
			out = append(out, args[i], args[i+1])
			continue
		}
		replaced = true
		if value != "" {
			out = append(out, name, value)
		}
	}
	if !replaced && value != "" {
		out = append(out, name, value)
	}
	return out
}

// runForwardPlist runs the production command with args, writing stdout to out,
// and returns what it wrote to stderr and its error.
func runForwardPlist(home func() (string, error), out io.Writer, args []string) (string, error) {
	return runForwardPlistWithLookup(home, fixedForwardPlistLookup, out, args)
}

// runForwardPlistWithLookup is runForwardPlist with the client lookup supplied.
func runForwardPlistWithLookup(home func() (string, error), lookPath func(string) (string, error), out io.Writer, args []string) (string, error) {
	var errBuf bytes.Buffer
	err := execForwardPlistWithLookup(home, lookPath, out, &errBuf, args)
	return errBuf.String(), err
}

// execForwardPlist runs the production command with args, writing stdout to out
// and stderr to errOut.
func execForwardPlist(home func() (string, error), out, errOut io.Writer, args []string) error {
	return execForwardPlistWithLookup(home, fixedForwardPlistLookup, out, errOut, args)
}

// execForwardPlistWithLookup is execForwardPlist with the client lookup supplied.
func execForwardPlistWithLookup(home func() (string, error), lookPath func(string) (string, error), out, errOut io.Writer, args []string) error {
	c := newForwardPlistCmd(home, lookPath)
	c.SetOut(out)
	c.SetErr(errOut)
	c.SetArgs(args)
	c.SilenceUsage = true
	return c.Execute()
}

// fixedForwardPlistLookup stands in for exec.LookPath, which would make the test
// depend on the host's PATH.
func fixedForwardPlistLookup(string) (string, error) { return forwardPlistKubectl, nil }

func fixedForwardPlistHome() (string, error) { return forwardPlistHome, nil }

// forwardPlistFailingWriter is a stdout that refuses every write.
type forwardPlistFailingWriter struct{}

func (forwardPlistFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk full")
}

// plistProgramArguments returns the strings of the ProgramArguments array in the
// property list doc.
func plistProgramArguments(t *testing.T, doc string) []string {
	t.Helper()
	type stringArray struct {
		Items []string `xml:"string"`
	}
	dec := xml.NewDecoder(strings.NewReader(doc))
	var key string
	var args []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the plist is not well-formed XML: %v", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			if err := dec.DecodeElement(&key, &start); err != nil {
				t.Fatalf("reading a key: %v", err)
			}
		case "array":
			if key == "ProgramArguments" {
				var a stringArray
				if err := dec.DecodeElement(&a, &start); err != nil {
					t.Fatalf("reading ProgramArguments: %v", err)
				}
				args = a.Items
			}
		}
	}
	return args
}

// TestTownForwardPlistGolden pins the printed job to the golden and checks that
// its arguments and the stderr URL carry the flags.
func TestTownForwardPlistGolden(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	stderr, err := runForwardPlist(fixedForwardPlistHome, &out, forwardPlistArgs())
	if err != nil {
		t.Fatalf("plist with valid flags: %v", err)
	}

	goldenFile := filepath.Join("testdata", forwardPlistGoldenPath)
	want, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	if out.String() != string(want) {
		t.Errorf("plist differs from %s:\n%s", goldenFile, out.String())
	}

	wantArgs := []string{
		forwardPlistKubectl,
		"--context=colima-gt-control",
		"--namespace=gt",
		"port-forward",
		"--address=127.0.0.1",
		"service/dashboard",
		"8787:8787",
	}
	if got := plistProgramArguments(t, out.String()); !slices.Equal(got, wantArgs) {
		t.Errorf("program arguments = %q, want %q", got, wantArgs)
	}
	const wantStderr = "dashboard URL: http://127.0.0.1:8787/\n"
	if stderr != wantStderr {
		t.Errorf("stderr = %q, want %q", stderr, wantStderr)
	}
}

// TestTownForwardPlistPrintsTheAbsoluteKubectl checks that the job's program is
// the path the lookup returned, since launchd does not search PATH for it.
func TestTownForwardPlistPrintsTheAbsoluteKubectl(t *testing.T) {
	t.Parallel()
	const kubectl = "/opt/homebrew/opt/kubernetes-cli/bin/kubectl"
	lookup := func(name string) (string, error) {
		if name != "kubectl" {
			return "", errors.New("unexpected lookup of " + name)
		}
		return kubectl, nil
	}
	var out bytes.Buffer
	if _, err := runForwardPlistWithLookup(fixedForwardPlistHome, lookup, &out, forwardPlistArgs()); err != nil {
		t.Fatalf("plist with a resolved kubectl: %v", err)
	}
	args := plistProgramArguments(t, out.String())
	if len(args) == 0 {
		t.Fatalf("the plist has no ProgramArguments: %s", out.String())
	}
	if args[0] != kubectl {
		t.Errorf("first program argument = %q, want the lookup's path %q", args[0], kubectl)
	}
}

// TestTownForwardPlistAcceptsThePortBounds checks that the first and last ports
// are accepted, so the range check cannot be one off at either end.
func TestTownForwardPlistAcceptsThePortBounds(t *testing.T) {
	t.Parallel()
	for _, port := range []string{"1", "65535"} {
		var out bytes.Buffer
		args := forwardPlistArgsWith("--port", port)
		if _, err := runForwardPlist(fixedForwardPlistHome, &out, args); err != nil {
			t.Errorf("--port %s refused: %v", port, err)
		}
	}
}

// TestTownForwardPlistRefusals checks that each bad flag exits non-zero naming
// the flag and prints nothing, and that a failed write exits non-zero.
func TestTownForwardPlistRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		flag string
	}{
		{"missing context", forwardPlistArgsWith("--context", ""), "--context"},
		{"blank context", forwardPlistArgsWith("--context", "  "), "--context"},
		{"context that reads as a flag", forwardPlistArgsWith("--context", "--namespace=gt"), "--context"},
		{"bind on every interface", forwardPlistArgsWith("--bind", "0.0.0.0"), "--bind"},
		{"bind on another loopback address", forwardPlistArgsWith("--bind", "127.0.0.2"), "--bind"},
		{"port missing", forwardPlistArgsWith("--port", ""), "--port"},
		{"port zero", forwardPlistArgsWith("--port", "0"), "--port"},
		{"port above the range", forwardPlistArgsWith("--port", "65536"), "--port"},
		{"port negative", forwardPlistArgsWith("--port", "-1"), "--port"},
		{"service with uppercase", forwardPlistArgsWith("--service", "Dashboard"), "--service"},
		{"service that injects an argument", forwardPlistArgsWith("--service", "dash --kubeconfig"), "--service"},
		{"service longer than a label", forwardPlistArgsWith("--service", strings.Repeat("a", 64)), "--service"},
		{"namespace that is not a label", forwardPlistArgsWith("--namespace", "gt/x"), "--namespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			_, err := runForwardPlist(fixedForwardPlistHome, &out, tc.args)
			if err == nil {
				t.Fatalf("run with %s succeeded", tc.name)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("error %q does not name %s", err, tc.flag)
			}
			if out.Len() != 0 {
				t.Errorf("a refused run wrote %d bytes of plist", out.Len())
			}
		})
	}

	t.Run("failed write of the output", func(t *testing.T) {
		t.Parallel()
		_, err := runForwardPlist(fixedForwardPlistHome, forwardPlistFailingWriter{}, forwardPlistArgs())
		if err == nil {
			t.Fatal("a failed write of the plist exited zero")
		}
	})

	t.Run("failed write of the URL", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		err := execForwardPlist(fixedForwardPlistHome, &out, forwardPlistFailingWriter{}, forwardPlistArgs())
		if err == nil {
			t.Fatal("a failed write of the URL exited zero")
		}
	})

	t.Run("kubectl cannot be found", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		noKubectl := func(string) (string, error) { return "", errors.New("executable file not found in $PATH") }
		_, err := runForwardPlistWithLookup(fixedForwardPlistHome, noKubectl, &out, forwardPlistArgs())
		if err == nil {
			t.Fatal("a run without kubectl exited zero")
		}
		if !strings.Contains(err.Error(), "kubectl") {
			t.Errorf("error %q does not name kubectl", err)
		}
		if !strings.Contains(err.Error(), "executable file not found") {
			t.Errorf("error %q loses the lookup's cause", err)
		}
		if out.Len() != 0 {
			t.Errorf("a run without kubectl wrote %d bytes of plist", out.Len())
		}
	})

	t.Run("kubectl resolved to a relative path", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		relative := func(string) (string, error) { return "bin/kubectl", nil }
		_, err := runForwardPlistWithLookup(fixedForwardPlistHome, relative, &out, forwardPlistArgs())
		if err == nil || !strings.Contains(err.Error(), "kubectl") {
			t.Fatalf("run with a relative kubectl: err = %v, want one naming kubectl", err)
		}
		if out.Len() != 0 {
			t.Errorf("a run with a relative kubectl wrote %d bytes of plist", out.Len())
		}
	})

	t.Run("home cannot be found", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		noHome := func() (string, error) { return "", errors.New("no HOME") }
		_, err := runForwardPlist(noHome, &out, forwardPlistArgs())
		if err == nil || !strings.Contains(err.Error(), "HOME") {
			t.Fatalf("run without a home directory: err = %v, want one naming HOME", err)
		}
		if out.Len() != 0 {
			t.Errorf("a run without a home directory wrote %d bytes", out.Len())
		}
	})
}

// execForwardBdGateway runs the production bd-gateway command with args, writing
// stdout to out and stderr to errOut.
func execForwardBdGateway(home func() (string, error), lookPath func(string) (string, error), out, errOut io.Writer, args []string) error {
	c := newForwardBdGatewayCmd(home, lookPath)
	c.SetOut(out)
	c.SetErr(errOut)
	c.SetArgs(args)
	c.SilenceUsage = true
	return c.Execute()
}

// runForwardBdGateway runs bd-gateway with the fixed home and client lookup and
// returns what it wrote to stderr and its error.
func runForwardBdGateway(out io.Writer, args []string) (string, error) {
	var errBuf bytes.Buffer
	err := execForwardBdGateway(fixedForwardPlistHome, fixedForwardPlistLookup, out, &errBuf, args)
	return errBuf.String(), err
}

// TestTownForwardBdGatewayIsRegisteredUnderForward checks that `gt town forward
// bd-gateway` resolves to a command, so the subcommand is wired into the tree.
func TestTownForwardBdGatewayIsRegisteredUnderForward(t *testing.T) {
	t.Parallel()
	c, _, err := townForwardCmd.Find([]string{"bd-gateway"})
	if err != nil {
		t.Fatalf("finding bd-gateway under forward: %v", err)
	}
	if c.Name() != "bd-gateway" {
		t.Errorf("forward resolves bd-gateway to %q, so the subcommand is not registered", c.Name())
	}
}

// TestTownForwardBdGatewayPlistNamesTheService checks that the job forwards
// svc/bd-gateway on 7440, that its program is the absolute kubectl path, and that
// its stderr names the bd gateway URL. The namespace is not passed, so the default
// is checked too.
func TestTownForwardBdGatewayPlistNamesTheService(t *testing.T) {
	t.Parallel()
	const kubectl = "/opt/homebrew/opt/kubernetes-cli/bin/kubectl"
	lookup := func(string) (string, error) { return kubectl, nil }
	var out, errOut bytes.Buffer
	if err := execForwardBdGateway(fixedForwardPlistHome, lookup, &out, &errOut, []string{"--context", "colima-gt-control"}); err != nil {
		t.Fatalf("bd-gateway with a context: %v", err)
	}
	const wantStderr = "bd gateway URL: http://127.0.0.1:7440/\n"
	if errOut.String() != wantStderr {
		t.Errorf("stderr = %q, want %q", errOut.String(), wantStderr)
	}
	wantArgs := []string{
		kubectl,
		"--context=colima-gt-control",
		"--namespace=gt",
		"port-forward",
		"--address=127.0.0.1",
		"service/bd-gateway",
		"7440:7440",
	}
	if got := plistProgramArguments(t, out.String()); !slices.Equal(got, wantArgs) {
		t.Errorf("program arguments = %q, want %q", got, wantArgs)
	}
}

// TestTownForwardBdGatewayLabelAndLogs checks that the job's label and log paths
// are the bd-gateway ones and carry none of the dashboard's.
func TestTownForwardBdGatewayLabelAndLogs(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if _, err := runForwardBdGateway(&out, []string{"--context", "colima-gt-control"}); err != nil {
		t.Fatalf("bd-gateway with a context: %v", err)
	}
	doc := out.String()
	for _, want := range []string{
		"<string>com.gastown.forward-bd-gateway</string>",
		"<string>/Users/operator/gt/daemon/forward-bd-gateway.log</string>",
		"<string>/Users/operator/gt/daemon/forward-bd-gateway.err.log</string>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("plist lacks %s:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "dashboard") {
		t.Errorf("bd-gateway plist names the dashboard:\n%s", doc)
	}
}

// TestTownForwardBdGatewayLeavesDashboardPlistUnchanged checks that the dashboard
// plist still prints byte-for-byte the golden after bd-gateway is registered.
func TestTownForwardBdGatewayLeavesDashboardPlistUnchanged(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if _, err := runForwardPlist(fixedForwardPlistHome, &out, forwardPlistArgs()); err != nil {
		t.Fatalf("dashboard plist with valid flags: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", forwardPlistGoldenPath))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	if out.String() != string(want) {
		t.Errorf("dashboard plist differs from %s:\n%s", forwardPlistGoldenPath, out.String())
	}
}

// TestTownForwardBdGatewayRefusals checks that each refused run exits non-zero
// naming the flag and prints no plist, and that a failed write exits non-zero.
func TestTownForwardBdGatewayRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		flag string
	}{
		{"missing context", []string{"--namespace", "gt"}, "--context"},
		{"blank context", []string{"--context", "  "}, "--context"},
		{"context that reads as a flag", []string{"--context", "--namespace=gt"}, "--context"},
		{"namespace that is not a label", []string{"--context", "colima-gt-control", "--namespace", "gt/x"}, "--namespace"},
		{"port flag, which the command does not take", []string{"--context", "colima-gt-control", "--port", "9999"}, "--port"},
		{"bind flag, which the command does not take", []string{"--context", "colima-gt-control", "--bind", "0.0.0.0"}, "--bind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			_, err := runForwardBdGateway(&out, tc.args)
			if err == nil {
				t.Fatalf("run with %s succeeded", tc.name)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("error %q does not name %s", err, tc.flag)
			}
			if out.Len() != 0 {
				t.Errorf("a refused run wrote %d bytes of plist", out.Len())
			}
		})
	}

	t.Run("failed write of the output", func(t *testing.T) {
		t.Parallel()
		err := execForwardBdGateway(fixedForwardPlistHome, fixedForwardPlistLookup, forwardPlistFailingWriter{}, io.Discard, []string{"--context", "colima-gt-control"})
		if err == nil {
			t.Fatal("a failed write of the bd-gateway plist exited zero")
		}
	})

	t.Run("kubectl cannot be found", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		noKubectl := func(string) (string, error) { return "", errors.New("executable file not found in $PATH") }
		err := execForwardBdGateway(fixedForwardPlistHome, noKubectl, &out, io.Discard, []string{"--context", "colima-gt-control"})
		if err == nil {
			t.Fatal("a bd-gateway run without kubectl exited zero")
		}
		if !strings.Contains(err.Error(), "kubectl") || !strings.Contains(err.Error(), "executable file not found") {
			t.Errorf("error %q does not name kubectl and keep the lookup's cause", err)
		}
		if out.Len() != 0 {
			t.Errorf("a run without kubectl wrote %d bytes of plist", out.Len())
		}
	})

	t.Run("kubectl resolved to a relative path", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		relative := func(string) (string, error) { return "bin/kubectl", nil }
		err := execForwardBdGateway(fixedForwardPlistHome, relative, &out, io.Discard, []string{"--context", "colima-gt-control"})
		if err == nil || !strings.Contains(err.Error(), "kubectl") {
			t.Fatalf("bd-gateway with a relative kubectl: err = %v, want one naming kubectl", err)
		}
		if out.Len() != 0 {
			t.Errorf("a run with a relative kubectl wrote %d bytes of plist", out.Len())
		}
	})

	t.Run("home cannot be found", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		noHome := func() (string, error) { return "", errors.New("no HOME") }
		err := execForwardBdGateway(noHome, fixedForwardPlistLookup, &out, io.Discard, []string{"--context", "colima-gt-control"})
		if err == nil || !strings.Contains(err.Error(), "HOME") {
			t.Fatalf("bd-gateway without a home directory: err = %v, want one naming HOME", err)
		}
		if out.Len() != 0 {
			t.Errorf("a run without a home directory wrote %d bytes", out.Len())
		}
	})
}
