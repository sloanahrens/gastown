package secretpreflight

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// kubectlExit is a failed child's exit status, matched through ExitCode() as
// *exec.ExitError is, so a unit test drives the branch without a process.
type kubectlExit struct {
	code int
	text string
}

func (e kubectlExit) Error() string { return e.text }
func (e kubectlExit) ExitCode() int { return e.code }

// answer is what one Secret's get returns.
type answer struct {
	out []byte
	err error
}

// call is one recorded invocation of the runner.
type call struct {
	name string
	args []string
}

// fakeRunner answers a get by the Secret name anywhere in its argv, and records
// every call so a test reads the argv the function built. Looking the name up
// rather than reading a fixed position leaves a changed argv to the assertion
// that judges it, not to a failed lookup here.
type fakeRunner struct {
	answers map[string]answer
	calls   []call
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	for _, arg := range args {
		if a, ok := f.answers[arg]; ok {
			return a.out, a.err
		}
	}
	return nil, fmt.Errorf("fake: no answer for argv %q", args)
}

// secretTable is the default `kubectl get secret` table: the name column, then
// the type, data count and age, with no data.
func secretTable(name string) []byte {
	return []byte("NAME      TYPE     DATA   AGE\n" + name + "   Opaque   4      12d\n")
}

// TestSecretPreflightMissing checks the one answer read as missing: exit status
// 1 with NotFound, in the output or in the error text. A table that names the
// Secret is present and is not listed, and the result is sorted.
func TestSecretPreflightMissing(t *testing.T) {
	t.Parallel()
	run := &fakeRunner{answers: map[string]answer{
		"alpha": {out: secretTable("alpha")},
		"bravo": {
			out: []byte(`Error from server (NotFound): secrets "bravo" not found` + "\n"),
			err: kubectlExit{code: 1, text: "exit status 1"},
		},
		"charlie": {
			err: kubectlExit{code: 1, text: `exit status 1: Error from server (NotFound): secrets "charlie" not found`},
		},
	}}

	got, err := Missing(context.Background(), run.run, "gt-sbx", "gt", []string{"charlie", "alpha", "bravo"})
	if err != nil {
		t.Fatalf("Missing = %v, want no error", err)
	}
	want := []string{"bravo", "charlie"}
	if !slices.Equal(got, want) {
		t.Errorf("Missing = %q, want %q: bravo's NotFound is in the output, charlie's in the error text, and alpha is present", got, want)
	}
}

// TestSecretPreflightArgv holds every call to the read: the context and
// namespace named, `get secret <name>`, and no output flag anywhere, so the
// cluster's answer carries the name column and no data.
func TestSecretPreflightArgv(t *testing.T) {
	t.Parallel()
	run := &fakeRunner{answers: map[string]answer{
		"alpha": {out: secretTable("alpha")},
		"bravo": {out: secretTable("bravo")},
	}}

	if _, err := Missing(context.Background(), run.run, "gt-sbx", "gt", []string{"alpha", "bravo"}); err != nil {
		t.Fatalf("Missing = %v, want no error", err)
	}

	want := [][]string{
		{"--context", "gt-sbx", "-n", "gt", "get", "secret", "alpha"},
		{"--context", "gt-sbx", "-n", "gt", "get", "secret", "bravo"},
	}
	if len(run.calls) != len(want) {
		t.Fatalf("Missing made %d calls, want %d", len(run.calls), len(want))
	}
	for i, c := range run.calls {
		if c.name != "kubectl" {
			t.Errorf("call %d runs %q, want kubectl", i, c.name)
		}
		if !slices.Equal(c.args, want[i]) {
			t.Errorf("call %d argv = %q, want %q", i, c.args, want[i])
		}
		for _, arg := range c.args {
			if arg == "-o" || arg == "--output" || arg == "--raw" ||
				(strings.HasPrefix(arg, "-o") && len(arg) > 2) ||
				strings.HasPrefix(arg, "--output=") || strings.HasPrefix(arg, "--raw=") {
				t.Errorf("call %d argv = %q carries output flag %q", i, c.args, arg)
			}
		}
	}
}

// TestSecretPreflightErrors holds the other failures apart from missing: a
// Forbidden answer, another exit status carrying NotFound, NotFound with no
// exit status to read, and a connection failure are each an error naming the
// Secret, never a Secret listed as missing.
func TestSecretPreflightErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		out       []byte
		err       error
		wantInErr string
	}{
		{
			name:      "forbidden",
			out:       []byte(`Error from server (Forbidden): secrets "alpha" is forbidden: User "gt" cannot get resource "secrets"` + "\n"),
			err:       kubectlExit{code: 1, text: "exit status 1"},
			wantInErr: "alpha",
		},
		{
			name:      "another exit status with NotFound",
			out:       []byte(`Error from server (NotFound): secrets "alpha" not found` + "\n"),
			err:       kubectlExit{code: 2, text: "exit status 2"},
			wantInErr: "alpha",
		},
		{
			name:      "NotFound with no exit status",
			err:       errors.New(`secrets "alpha" not found`),
			wantInErr: "alpha",
		},
		{
			name:      "the cluster did not answer",
			err:       errors.New("The connection to the server refused"),
			wantInErr: "alpha",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{answers: map[string]answer{"alpha": {out: tc.out, err: tc.err}}}
			got, err := Missing(context.Background(), run.run, "gt-sbx", "gt", []string{"alpha"})
			if err == nil {
				t.Fatalf("Missing = %q, nil; want an error for a failure that is not NotFound", got)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("Missing error = %q, want it to name %q", err, tc.wantInErr)
			}
			if slices.Contains(got, "alpha") {
				t.Errorf("Missing = %q lists alpha, but a failure the check could not read is not a missing Secret", got)
			}
		})
	}
}

// TestSecretPreflightRefuses holds the answers and inputs the check refuses
// before or after the read: nothing is passed to kubectl for an unnamed
// cluster, namespace or Secret, and a successful get that names another Secret
// is not read as present.
func TestSecretPreflightRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		kube      string
		namespace string
		names     []string
		answers   map[string]answer
		wantInErr string
		wantCalls int
	}{
		{
			name:      "no context",
			namespace: "gt",
			names:     []string{"alpha"},
			wantInErr: "context",
		},
		{
			name:      "no namespace",
			kube:      "gt-sbx",
			names:     []string{"alpha"},
			wantInErr: "namespace",
		},
		{
			name:      "no name",
			kube:      "gt-sbx",
			namespace: "gt",
			names:     []string{""},
			wantInErr: "Secret name",
		},
		{
			name:      "a name kubectl reads as a flag",
			kube:      "gt-sbx",
			namespace: "gt",
			names:     []string{"--raw"},
			wantInErr: "Secret name",
		},
		{
			name:      "an answer that names another Secret",
			kube:      "gt-sbx",
			namespace: "gt",
			names:     []string{"alpha"},
			answers:   map[string]answer{"alpha": {out: secretTable("bravo")}},
			wantInErr: "alpha",
			wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{answers: tc.answers}
			got, err := Missing(context.Background(), run.run, tc.kube, tc.namespace, tc.names)
			if err == nil {
				t.Fatalf("Missing = %q, nil; want a refusal", got)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("Missing error = %q, want it to name %q", err, tc.wantInErr)
			}
			if len(run.calls) != tc.wantCalls {
				t.Errorf("Missing made %d kubectl calls, want %d", len(run.calls), tc.wantCalls)
			}
		})
	}
}
