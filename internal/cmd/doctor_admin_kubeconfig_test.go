package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doctor"
)

// TestDoctorAdminKubeconfigOptIn: a run that names admin-kubeconfig registers
// the check over the host kubeconfig, and fails on an embedded credential or a
// kubeconfig it cannot read; a run naming another check never reads one; the
// check refuses inside a pod and without a resolvable path.
//
//testpolicy:allow parallel — runDoctor reads the flag variables, the process environment and the working directory, all process-wide
func TestDoctorAdminKubeconfigOptIn(t *testing.T) {
	if !strings.Contains(doctorCmd.Long, doctor.AdminKubeconfigCheckName) {
		t.Errorf("doctor help does not name %s", doctor.AdminKubeconfigCheckName)
	}

	adminPath := writeKubeconfig(t, adminKubeconfigText)
	cleanPath := writeKubeconfig(t, cleanKubeconfigText)
	sep := string(filepath.ListSeparator)

	cases := []struct {
		name       string
		checks     []string
		kubeconfig string
		configEnv  string
		pod        bool
		noHome     bool
		wantErr    string
	}{
		{
			name:   "a run naming another check never reads a kubeconfig",
			checks: []string{"disk-space"},
			pod:    true,
			noHome: true,
		},
		{
			name:       "--check admin-kubeconfig fails the run on an embedded admin credential",
			checks:     []string{doctor.AdminKubeconfigCheckName},
			kubeconfig: adminPath,
			wantErr:    "doctor found 1 error(s)",
		},
		{
			name:       "--check admin-kubeconfig passes a clean kubeconfig",
			checks:     []string{doctor.AdminKubeconfigCheckName},
			kubeconfig: cleanPath,
		},
		{
			name:      "--check admin-kubeconfig reads $KUBECONFIG",
			checks:    []string{doctor.AdminKubeconfigCheckName},
			configEnv: adminPath,
			wantErr:   "doctor found 1 error(s)",
		},
		{
			name:      "--check admin-kubeconfig skips a leading empty $KUBECONFIG entry and reads the file after it",
			checks:    []string{doctor.AdminKubeconfigCheckName},
			configEnv: sep + adminPath,
			wantErr:   "doctor found 1 error(s)",
		},
		{
			name:      "--check admin-kubeconfig skips a trailing empty $KUBECONFIG entry and reads the file before it",
			checks:    []string{doctor.AdminKubeconfigCheckName},
			configEnv: cleanPath + sep,
		},
		{
			name:       "--check admin-kubeconfig fails the run on a kubeconfig it cannot read",
			checks:     []string{doctor.AdminKubeconfigCheckName},
			kubeconfig: filepath.Join(t.TempDir(), "missing"),
			wantErr:    "doctor found 1 error(s)",
		},
		{
			name:    "--check admin-kubeconfig refuses inside a pod",
			checks:  []string{doctor.AdminKubeconfigCheckName},
			pod:     true,
			wantErr: "refuses inside a pod",
		},
		{
			name:    "--check admin-kubeconfig refuses a path it cannot resolve",
			checks:  []string{doctor.AdminKubeconfigCheckName},
			noHome:  true,
			wantErr: "HOME is not set",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
				t.Fatal(err)
			}
			//testpolicy:allow no-chdir — runDoctor finds the town from the working directory
			t.Chdir(townRoot)
			home := t.TempDir()
			if tc.noHome {
				home = ""
			}
			pod := ""
			if tc.pod {
				pod = "10.20.30.40"
			}
			//testpolicy:allow no-env — runDoctor reads the process environment for the kubeconfig path and the pod marker
			t.Setenv("HOME", home)
			//testpolicy:allow no-env — runDoctor reads the process environment for the kubeconfig path and the pod marker
			t.Setenv("KUBECONFIG", tc.configEnv)
			//testpolicy:allow no-env — runDoctor reads the process environment for the kubeconfig path and the pod marker
			t.Setenv(podEnv, pod)

			savedCheck, savedPodTown, savedKubeconfig := doctorCheck, doctorPodTown, doctorKubeconfig
			t.Cleanup(func() {
				//testpolicy:allow no-global-swap — restores the flag variables swapped below
				doctorCheck, doctorPodTown, doctorKubeconfig = savedCheck, savedPodTown, savedKubeconfig
			})
			//testpolicy:allow no-global-swap — runDoctor reads the --check, --pod-town and --kubeconfig flag variables; they are not arguments
			doctorCheck, doctorPodTown, doctorKubeconfig = tc.checks, "", tc.kubeconfig

			err := runDoctor(doctorCmd, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("runDoctor = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runDoctor = %v, want an error holding %q", err, tc.wantErr)
			}
		})
	}
}

// TestDoctorAdminKubeconfigRegistration: the check is opt-in by name. A default
// run, which names no check, and a run that names other checks register nothing
// and read no kubeconfig, even inside a pod or with no HOME; a run that names
// admin-kubeconfig, by --check or by position, registers it exactly once and, in
// a pod, is refused. Mutation: drop the name guard from
// registerAdminKubeconfigIfNamed and the default-run rows go red.
func TestDoctorAdminKubeconfigRegistration(t *testing.T) {
	t.Parallel()
	registered := func(d *doctor.Doctor) int {
		n := 0
		for _, c := range d.Checks() {
			if c.Name() == doctor.AdminKubeconfigCheckName {
				n++
			}
		}
		return n
	}
	cases := []struct {
		name    string
		flags   []string
		args    []string
		env     map[string]string
		want    int
		wantErr string
	}{
		{name: "a default run registers nothing", want: 0},
		{name: "a default run in a pod registers nothing and is not refused", env: map[string]string{podEnv: "10.20.30.40"}, want: 0},
		{name: "a run naming another check registers nothing", flags: []string{"disk-space"}, env: map[string]string{podEnv: "10.20.30.40"}, want: 0},
		{name: "--check admin-kubeconfig registers it once", flags: []string{doctor.AdminKubeconfigCheckName}, env: map[string]string{"HOME": "/home/x"}, want: 1},
		{name: "naming it by position registers it once", args: []string{doctor.AdminKubeconfigCheckName}, env: map[string]string{"HOME": "/home/x"}, want: 1},
		{name: "naming it in a pod is refused and registers nothing", flags: []string{doctor.AdminKubeconfigCheckName}, env: map[string]string{podEnv: "10.20.30.40", "HOME": "/home/x"}, want: 0, wantErr: "refuses inside a pod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDoctorForCommand("")
			before := registered(d)
			if before != 0 {
				t.Fatalf("the default doctor already registers %s %d time(s)", doctor.AdminKubeconfigCheckName, before)
			}
			err := registerAdminKubeconfigIfNamed(d, doctorCheckNames(tc.flags, tc.args), "", adminCheckGetenv(tc.env))
			if tc.wantErr == "" && err != nil {
				t.Fatalf("registerAdminKubeconfigIfNamed = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("registerAdminKubeconfigIfNamed = %v, want an error holding %q", err, tc.wantErr)
			}
			if got := registered(d); got != tc.want {
				t.Errorf("%s registered %d time(s), want %d", doctor.AdminKubeconfigCheckName, got, tc.want)
			}
		})
	}
}
