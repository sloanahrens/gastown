package gtwrap

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestDoctorReportsExecForARunningContainer(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{psOut: "5c1e0a7f\n"}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	diag, err := d.Doctor()
	if err != nil {
		t.Fatalf("Doctor returned an error: %v", err)
	}
	want := Diagnosis{
		Rung:      RungExec,
		Container: "gt-daemon",
		Image:     testImage,
		Reason:    "the daemon container gt-daemon is running",
	}
	if diag != want {
		t.Fatalf("Doctor = %+v, want %+v", diag, want)
	}
	if len(shell.attached) != 0 {
		t.Fatalf("Doctor ran a call: %q", shell.attached)
	}
}

func TestDoctorReportsRunWhenTheDaemonIsStopped(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{psOut: ""}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	diag, err := d.Doctor()
	if err != nil {
		t.Fatalf("Doctor returned an error: %v", err)
	}
	if diag.Rung != RungRun || diag.Container != "gt-daemon" || diag.Image != testImage {
		t.Fatalf("Doctor = %+v, want run against gt-daemon and %s", diag, testImage)
	}
	if !strings.Contains(diag.Reason, "is not running") {
		t.Fatalf("reason %q does not say the daemon is not running", diag.Reason)
	}
	if len(shell.attached) != 0 {
		t.Fatalf("Doctor ran a call: %q", shell.attached)
	}
}

func TestDoctorReportsDownWhenDockerIsDown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		infoExit int
		infoErr  error
	}{
		{"info exits non-zero", 1, nil},
		{"info cannot run", 0, errors.New("docker: executable not found")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{infoExit: tt.infoExit, infoErr: tt.infoErr}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			diag, err := d.Doctor()
			if err != nil {
				t.Fatalf("Doctor returned an error: %v", err)
			}
			if diag.Rung != RungDown || diag.Container != "gt-daemon" || diag.Image != testImage {
				t.Fatalf("Doctor = %+v, want down against gt-daemon and %s", diag, testImage)
			}
			for _, want := range []string{"Docker Desktop", "open -a Docker", "compose -p gt-live -f " + testComposePath + " up -d daemon"} {
				if !strings.Contains(diag.Reason, want) {
					t.Fatalf("reason %q does not name %q", diag.Reason, want)
				}
			}
			if len(shell.attached) != 0 {
				t.Fatalf("Doctor ran a call: %q", shell.attached)
			}
		})
	}
}

func TestDoctorReportsDownWhenTheContainerStateIsUnknown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		shell fakeShell
	}{
		{"ps exits non-zero", fakeShell{psExit: 1}},
		{"ps cannot run", fakeShell{psErr: errors.New("docker: executable not found")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := tt.shell
			d := Docker{Town: testTown, Shell: &shell, ReadFile: readCompose(testCompose)}
			diag, err := d.Doctor()
			if err != nil {
				t.Fatalf("Doctor returned an error: %v", err)
			}
			if diag.Rung != RungDown {
				t.Fatalf("Doctor rung = %s, want down", diag.Rung)
			}
			if !strings.Contains(diag.Reason, "could not list the containers") {
				t.Fatalf("reason %q does not say the state is unknown", diag.Reason)
			}
			if len(shell.attached) != 0 {
				t.Fatalf("Doctor ran a call: %q", shell.attached)
			}
		})
	}
}

func TestDoctorRefusesAnUnusableComposeFile(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{}
	d := Docker{Town: testTown, Shell: shell, ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist }}
	diag, err := d.Doctor()
	if err == nil {
		t.Fatalf("Doctor returned %+v with no error, want a refusal", diag)
	}
	if diag != (Diagnosis{}) {
		t.Fatalf("Doctor returned %+v with its error, want the zero diagnosis", diag)
	}
	if len(shell.captured) != 0 || len(shell.attached) != 0 {
		t.Fatalf("compose file is unusable but docker ran: probes %q, calls %q", shell.captured, shell.attached)
	}
}

func TestRungString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rung Rung
		want string
	}{
		{RungExec, "exec"},
		{RungRun, "run"},
		{RungDown, "down"},
		{Rung(99), "Rung(99)"},
	}
	for _, tt := range tests {
		if got := tt.rung.String(); got != tt.want {
			t.Errorf("Rung(%d) = %q, want %q", int(tt.rung), got, tt.want)
		}
	}
}
