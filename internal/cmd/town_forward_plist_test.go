package cmd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var updateForwardPlistGolden = flag.Bool("update-forward-plist-golden", false, "rewrite testdata/town_forward_plist.golden")

const forwardPlistGoldenPath = "town_forward_plist.golden"

// forwardPlistHome is the home directory the test passes in, so the golden holds
// no host path.
const forwardPlistHome = "/Users/operator"

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
	var errBuf bytes.Buffer
	err := execForwardPlist(home, out, &errBuf, args)
	return errBuf.String(), err
}

// execForwardPlist runs the production command with args, writing stdout to out
// and stderr to errOut.
func execForwardPlist(home func() (string, error), out, errOut io.Writer, args []string) error {
	c := newForwardPlistCmd(home)
	c.SetOut(out)
	c.SetErr(errOut)
	c.SetArgs(args)
	c.SilenceUsage = true
	return c.Execute()
}

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
	if *updateForwardPlistGolden {
		if err := os.WriteFile(goldenFile, out.Bytes(), 0o644); err != nil { //nolint:gosec // a test fixture
			t.Fatalf("rewriting the golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	if out.String() != string(want) {
		t.Errorf("plist differs from %s:\n%s", goldenFile, out.String())
	}

	wantArgs := []string{
		"kubectl",
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
	const wantStderr = "cmux tab URL: http://127.0.0.1:8787/\n"
	if stderr != wantStderr {
		t.Errorf("stderr = %q, want %q", stderr, wantStderr)
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
