package cmd

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// TestHostAgentFillsDockerDisk pins what the agent publishes for the Docker
// Desktop disk: the percentage in the field when df answers, and an error with
// no value when the read fails or its output names no capacity. A zero value
// with no error would read as a disk at 0% used.
func TestHostAgentFillsDockerDisk(t *testing.T) {
	t.Parallel()
	const dfOut = "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		"/dev/vdb 1000000 724000 276000 72% /\n"
	cases := []struct {
		name    string
		out     string
		err     error
		want    hostprobe.Disk
		wantErr bool
	}{
		{name: "df answers", out: dfOut, want: hostprobe.Disk{UsedPct: 72}},
		// A failed command can still print a df: the error, not the output,
		// decides whether the reading happened.
		{name: "the runner fails after printing a df", out: dfOut, err: errors.New("exit status 1"), wantErr: true},
		{name: "the output names no capacity", out: "not df output\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran [][]string
			run := func(_ context.Context, argv []string) ([]byte, error) {
				ran = append(ran, argv)
				return []byte(tc.out), tc.err
			}

			got := hostAgentWithDockerDisk(context.Background(), hostprobe.Probes{V: hostprobe.SchemaVersion}, run)

			wantArgv := [][]string{{"docker", "exec", "gt-dolt", "df", "-P", "/"}}
			if !slices.EqualFunc(ran, wantArgv, func(a, b []string) bool { return slices.Equal(a, b) }) {
				t.Errorf("runner ran %q, want %q", ran, wantArgv)
			}
			if tc.wantErr {
				if got.DockerDisk.Err == "" {
					t.Fatalf("a failed read published %+v, want an error in the field", got.DockerDisk)
				}
				if got.DockerDisk.Value != (hostprobe.Disk{}) {
					t.Errorf("a failed read left Value = %+v, want it zero", got.DockerDisk.Value)
				}
				return
			}
			if got.DockerDisk.Err != "" || got.DockerDisk.Value != tc.want {
				t.Errorf("published %+v, want %+v with no error", got.DockerDisk, tc.want)
			}
		})
	}
}
