package daemon

import (
	"errors"
	"reflect"
	"testing"
)

// TestParseClusterDriftRun: one run of gt town cluster drift answers with its
// exit status, its stdout and whether the child ran at all. Exit 0 is an empty
// result with no error; exit 1 is the non-empty stdout lines, kept as printed;
// every other answer, runErr or no line is an error with no result (gt-hhsi66).
func TestParseClusterDriftRun(t *testing.T) {
	t.Parallel()
	three := []string{
		"deployment/gt-daemon: spec.replicas fingerprint a1b2 != c3d4",
		"deployment/gt-daemon: metadata.labels fingerprint a1b2 != e5f6",
		"cronjob/tracker-backup: spec.schedule fingerprint a1b2 != g7h8",
	}
	runErr := errors.New("fork/exec /usr/local/bin/gt: no such file or directory")
	tests := []struct {
		name    string
		exit    int
		stdout  string
		runErr  error
		want    []string
		wantErr bool
	}{
		{name: "exit 0 is in step", exit: 0},
		{
			name:   "exit 1 names each difference line",
			exit:   1,
			stdout: three[0] + "\n" + three[1] + "\n" + three[2] + "\n",
			want:   three,
		},
		{name: "exit 1 with no stdout names no drift", exit: 1, wantErr: true},
		{name: "exit 1 with only blank lines names no drift", exit: 1, stdout: "\n \t\n", wantErr: true},
		{name: "exit 2 is not the parse's answer", exit: 2, stdout: three[0] + "\n", wantErr: true},
		{name: "exit 3 is not an answer", exit: 3, stdout: three[0] + "\n", wantErr: true},
		{name: "child killed under exit 1", exit: -1, runErr: runErr, wantErr: true},
		{name: "exit 1 did not run", exit: 1, stdout: three[0] + "\n", runErr: runErr, wantErr: true},
		{name: "exit 0 did not run", exit: 0, runErr: runErr, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseClusterDriftRun(tt.exit, []byte(tt.stdout), tt.runErr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseClusterDriftRun(%d, %q, %v) = %+v, nil; want an error and no result", tt.exit, tt.stdout, tt.runErr, got)
				}
				if len(got.Differing) != 0 {
					t.Errorf("result = %+v, want an empty result with the error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClusterDriftRun(%d, %q, %v) error = %v, want nil", tt.exit, tt.stdout, tt.runErr, err)
			}
			if !reflect.DeepEqual(got.Differing, tt.want) {
				t.Errorf("Differing = %q, want %q", got.Differing, tt.want)
			}
		})
	}
}
