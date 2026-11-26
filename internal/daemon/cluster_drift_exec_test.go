package daemon

import (
	"errors"
	"reflect"
	"strings"
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
			got, err := parseClusterDriftRun(tt.exit, []byte(tt.stdout), nil, tt.runErr)
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

// TestParseClusterDriftRunExitTwo: exit 2 is the cannot-say answer, always an
// error with no result. It names the first non-blank stderr line cut to 200
// characters, never stdout, and wraps errNoRecordedRender when either stream
// says no live render has recorded its flags (gt-b7p6jn).
func TestParseClusterDriftRunExitTwo(t *testing.T) {
	t.Parallel()
	recorded := "no live render has recorded its flags, so there is nothing to compare; run gt town cluster render --record-flags"
	forbidden := "forbidden: the read grant for the control cluster is missing"
	long := "forbidden " + strings.Repeat("a", 490)
	objects := "deployment/gt-daemon: spec.replicas fingerprint a1b2 != c3d4\n"
	tests := []struct {
		name         string
		stdout       string
		stderr       string
		wantSentinel bool
		wantSub      string // the error must carry it; empty is not checked
		notSub       string // the error must not carry it; empty is not checked
	}{
		{name: "recorded words in stderr wrap the sentinel", stderr: recorded + "\n", wantSentinel: true, wantSub: recorded},
		{name: "recorded words in stdout wrap the sentinel", stdout: recorded + "\n", stderr: forbidden + "\n", wantSentinel: true, wantSub: forbidden},
		{name: "forbidden carries its first line, no sentinel", stderr: forbidden + "\n", wantSub: forbidden},
		{name: "only the first stderr line is carried", stderr: forbidden + "\nsecond line\n", wantSub: forbidden, notSub: "second line"},
		{name: "a 500 character line is cut to 200", stderr: long + "\n", wantSub: long[:200], notSub: long[:201]},
		{name: "stdout object values are never echoed", stdout: objects, stderr: forbidden + "\n", wantSub: forbidden, notSub: "a1b2"},
		{name: "empty stderr is still an error", stdout: objects},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseClusterDriftRun(2, []byte(tt.stdout), []byte(tt.stderr), nil)
			if err == nil {
				t.Fatalf("parseClusterDriftRun(2, %q, %q, nil) = %+v, nil; want an error", tt.stdout, tt.stderr, got)
			}
			if len(got.Differing) != 0 {
				t.Errorf("result = %+v, want an empty result with the error", got)
			}
			if errors.Is(err, errNoRecordedRender) != tt.wantSentinel {
				t.Errorf("errors.Is(err, errNoRecordedRender) = %t, want %t: %v", !tt.wantSentinel, tt.wantSentinel, err)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want it to carry %q", err, tt.wantSub)
			}
			if tt.notSub != "" && strings.Contains(err.Error(), tt.notSub) {
				t.Errorf("error = %q, want it to omit %q", err, tt.notSub)
			}
		})
	}
}
