//go:build unix

package bird

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestProbeAnswerCountsOnlyAGoneProcessAsDeath(t *testing.T) {
	t.Parallel()
	unclear := errors.New("the kernel would not answer")
	cases := []struct {
		name    string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "the probe succeeded", err: nil, want: true},
		{name: "no such process", err: syscall.ESRCH},
		{name: "no such process, wrapped", err: fmt.Errorf("kill: %w", syscall.ESRCH)},
		{name: "exists under another uid", err: syscall.EPERM, want: true},
		{name: "an answer that is not a death", err: unclear, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			alive, err := probeAnswer(tc.err)
			if (err != nil) != tc.wantErr {
				t.Fatalf("probeAnswer(%v) = %v, %v, want error %v", tc.err, alive, err, tc.wantErr)
			}
			if alive != tc.want {
				t.Errorf("probeAnswer(%v) = %v, want %v", tc.err, alive, tc.want)
			}
		})
	}
}
