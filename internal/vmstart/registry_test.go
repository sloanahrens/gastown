package vmstart

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// exitStatus stands in for *exec.ExitError: the Runner contract carries only an
// error, so the tests hand one that still reports the exit code.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// registryRun answers RegistryOwner's three reads from a fixed script and
// records every argv, so a test asserts the answer and the calls together.
type registryRun struct {
	lsofOut, psOut, curlOut string
	lsofErr, psErr, curlErr error
	calls                   [][]string
}

func (r *registryRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "lsof":
		return []byte(r.lsofOut), r.lsofErr
	case "ps":
		return []byte(r.psOut), r.psErr
	case "curl":
		return []byte(r.curlOut), r.curlErr
	}
	return nil, fmt.Errorf("unexpected command %q", name)
}

var (
	registryLsofArgv = []string{"lsof", "-nP", "-iTCP:5001", "-sTCP:LISTEN", "-Fp"}
	registryCurlArgv = []string{"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:5001/v2/"}
)

// TestVMStartRegistryOwner: the profile follows the Lima control socket in the
// listener's command, health follows the registry's 200, and nothing listening
// is an empty profile with no ps read.
func TestVMStartRegistryOwner(t *testing.T) {
	t.Parallel()
	sandbox := "ssh -S /Users/me/.colima/_lima/colima-gt-sbx/ssh.sock -L 127.0.0.1:5001:127.0.0.1:5001"
	control := "socat -d TCP-LISTEN:5001,fork,reuseaddr /Users/me/.colima/_lima/colima-gt-control/ssh.sock"
	psArgv := func(pid string) []string { return []string{"ps", "-o", "command=", "-p", pid} }
	cases := map[string]struct {
		lsofOut, psOut, curlOut string
		lsofErr, psErr, curlErr error
		want                    Owner
		wantCalls               [][]string
	}{
		"the sandbox forward": {
			lsofOut: "p4242\n", psOut: sandbox, curlOut: "200",
			want:      Owner{Profile: "gt-sbx", Healthy: true},
			wantCalls: [][]string{registryLsofArgv, psArgv("4242"), registryCurlArgv},
		},
		"the control forwarder": {
			lsofOut: "p5151\n", psOut: control, curlOut: "200",
			want:      Owner{Profile: "gt-control", Healthy: true},
			wantCalls: [][]string{registryLsofArgv, psArgv("5151"), registryCurlArgv},
		},
		"nothing listens": {
			lsofOut: "", lsofErr: exitStatus(1), curlOut: "000", curlErr: exitStatus(7),
			want:      Owner{},
			wantCalls: [][]string{registryLsofArgv, registryCurlArgv},
		},
		"the listener exited before the ps read": {
			lsofOut: "p4242\n", psOut: "", psErr: exitStatus(1), curlOut: "000", curlErr: exitStatus(7),
			want:      Owner{},
			wantCalls: [][]string{registryLsofArgv, psArgv("4242"), registryCurlArgv},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			script := &registryRun{lsofOut: tc.lsofOut, psOut: tc.psOut, curlOut: tc.curlOut,
				lsofErr: tc.lsofErr, psErr: tc.psErr, curlErr: tc.curlErr}
			got, err := RegistryOwner(context.Background(), script.run)
			if err != nil {
				t.Fatalf("RegistryOwner: %v", err)
			}
			if got != tc.want {
				t.Fatalf("owner = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(script.calls, tc.wantCalls) {
				t.Fatalf("calls = %q, want %q", script.calls, tc.wantCalls)
			}
		})
	}
}

// TestVMStartRegistryOwnerHealth: a registry answer other than 200 is
// unhealthy, and a command naming neither VM has no profile.
func TestVMStartRegistryOwnerHealth(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		psOut   string
		curlOut string
		curlErr error
		want    Owner
	}{
		"a 404 is unhealthy": {
			psOut:   "ssh -S /Users/me/.colima/_lima/colima-gt-sbx/ssh.sock",
			curlOut: "404",
			want:    Owner{Profile: "gt-sbx", Healthy: false},
		},
		"a refused connection prints 000": {
			psOut:   "ssh -S /Users/me/.colima/_lima/colima-gt-sbx/ssh.sock",
			curlOut: "000", curlErr: exitStatus(7),
			want: Owner{Profile: "gt-sbx", Healthy: false},
		},
		"a command naming neither VM": {
			psOut:   "dockerd --host unix:///var/run/docker.sock",
			curlOut: "200",
			want:    Owner{Healthy: true},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			script := &registryRun{lsofOut: "p4242\n", psOut: tc.psOut, curlOut: tc.curlOut, curlErr: tc.curlErr}
			got, err := RegistryOwner(context.Background(), script.run)
			if err != nil {
				t.Fatalf("RegistryOwner: %v", err)
			}
			if got != tc.want {
				t.Fatalf("owner = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestVMStartRegistryOwnerReadError: a read that failed for a reason other than
// no match is an error, so a broken lsof or curl is never read as "nothing
// listens" or "unhealthy". Each case also checks the reads after it did not run.
func TestVMStartRegistryOwnerReadError(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		lsofErr, psErr, curlErr error
		curlOut                 string
		wantCalls               int
	}{
		"lsof is not installed": {
			lsofErr:   errors.New("exec: \"lsof\": executable file not found in $PATH"),
			wantCalls: 1,
		},
		"lsof failed for another reason": {
			lsofErr:   exitStatus(2),
			wantCalls: 1,
		},
		"the ps read failed": {
			psErr:     errors.New("ps: operation not permitted"),
			wantCalls: 2,
		},
		"curl printed nothing": {
			curlErr:   errors.New("exec: \"curl\": executable file not found in $PATH"),
			wantCalls: 3,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			script := &registryRun{lsofOut: "p4242\n", psOut: "ssh -S /Users/me/.colima/_lima/colima-gt-sbx/ssh.sock",
				lsofErr: tc.lsofErr, psErr: tc.psErr, curlErr: tc.curlErr, curlOut: tc.curlOut}
			if _, err := RegistryOwner(context.Background(), script.run); err == nil {
				t.Fatal("RegistryOwner read a failed read as an answer")
			}
			if len(script.calls) != tc.wantCalls {
				t.Fatalf("calls = %q, want %d", script.calls, tc.wantCalls)
			}
		})
	}
}

// TestVMStartRegistryOwnerWrappedExit: the exit status reaches the read through
// whatever wraps the error, as a real Runner's error can be; a wrapped exit 1
// is still the no-match answer and a wrapped other code or a plain error is
// still a failed read.
func TestVMStartRegistryOwnerWrappedExit(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		lsofOut, psOut string
		lsofErr, psErr error
		wantErr        bool
		wantCalls      int
		want           Owner
	}{
		"a wrapped lsof exit 1 is nothing listening": {
			lsofErr:   fmt.Errorf("run lsof: %w", exitStatus(1)),
			wantCalls: 2,
			want:      Owner{Healthy: true},
		},
		"a wrapped ps exit 1 is a listener that has gone": {
			lsofOut:   "p4242\n",
			psErr:     fmt.Errorf("run ps: %w", exitStatus(1)),
			wantCalls: 3,
			want:      Owner{Healthy: true},
		},
		"a wrapped lsof exit 2 is a failed read": {
			lsofErr:   fmt.Errorf("run lsof: %w", exitStatus(2)),
			wantErr:   true,
			wantCalls: 1,
		},
		"a wrapped error with no exit status is a failed read": {
			lsofErr:   fmt.Errorf("run lsof: %w", errors.New("permission denied")),
			wantErr:   true,
			wantCalls: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			script := &registryRun{lsofOut: tc.lsofOut, psOut: tc.psOut, curlOut: "200",
				lsofErr: tc.lsofErr, psErr: tc.psErr}
			got, err := RegistryOwner(context.Background(), script.run)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("RegistryOwner read a wrapped failure as %+v", got)
				}
			} else {
				if err != nil {
					t.Fatalf("RegistryOwner: %v", err)
				}
				if got != tc.want {
					t.Fatalf("owner = %+v, want %+v", got, tc.want)
				}
			}
			if len(script.calls) != tc.wantCalls {
				t.Fatalf("calls = %q, want %d", script.calls, tc.wantCalls)
			}
		})
	}
}

// TestVMStartRegistryOwnerPID: a pid lsof did not print as a decimal number is
// refused before it reaches ps's argv.
func TestVMStartRegistryOwnerPID(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"p", "p-12", "p12x", "p 12"} {
		t.Run(out, func(t *testing.T) {
			script := &registryRun{lsofOut: out + "\n"}
			if _, err := RegistryOwner(context.Background(), script.run); err == nil {
				t.Fatalf("RegistryOwner accepted lsof output %q", out)
			}
			if len(script.calls) != 1 {
				t.Fatalf("calls = %q, want the lsof read only", script.calls)
			}
		})
	}
}
