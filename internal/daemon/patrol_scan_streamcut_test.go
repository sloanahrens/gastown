package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// streamCutFixture is a patrol host over a rig whose registry lists its prefix,
// so the seat's sandbox session is named from the prefix. record is what the
// sandbox record read answers (nil: no record, a host seat); readErr, when set,
// is the error that read answers. cutAnswer and cutErr are what the sandbox
// backend's StreamCut answers, and asked records every session it is asked
// about.
type streamCutFixture struct {
	h     *patrolScanHost
	asked *[]string
}

func newStreamCutFixture(t *testing.T, record *session.SandboxRecord, readErr error, cut bool, cutErr error) streamCutFixture {
	t.Helper()
	d, _ := newOverrunDaemon(t, false)
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	d.readSandboxRecord = func(townRoot, name string) (session.SandboxRecord, bool, error) {
		if readErr != nil {
			return session.SandboxRecord{}, false, readErr
		}
		if record == nil {
			return session.SandboxRecord{}, false, nil
		}
		return *record, true, nil
	}
	var asked []string
	d.sandboxStreamCutFn = func(townRoot string, rec session.SandboxRecord) (bool, error) {
		asked = append(asked, rec.Session)
		return cut, cutErr
	}
	return streamCutFixture{h: &patrolScanHost{d: d}, asked: &asked}
}

func streamCutRecord() *session.SandboxRecord {
	return &session.SandboxRecord{
		Session: overrunStopSessionName,
		Rig:     stopSeat.Rig,
		Polecat: stopSeat.Polecat,
		WorkDir: "/work/gastown/capable",
	}
}

// TestPatrolScanStreamCutAnswers pins what the patrol scan's revive reads: a
// sandbox seat answers its backend's answer for its record's session, a host
// seat and a seat whose backend says intact answer false, and a failed read is
// an error naming the seat and its cause.
func TestPatrolScanStreamCutAnswers(t *testing.T) {
	t.Parallel()
	seat := stopSeat.Rig + "/" + stopSeat.Polecat
	cases := []struct {
		name      string
		record    *session.SandboxRecord
		readErr   error
		cut       bool
		cutErr    error
		want      bool
		wantErr   string
		wantAsked []string
	}{
		{
			name:      "sandbox seat whose stream was cut answers true",
			record:    streamCutRecord(),
			cut:       true,
			want:      true,
			wantAsked: []string{overrunStopSessionName},
		},
		{
			name:      "sandbox seat whose stream is intact answers false",
			record:    streamCutRecord(),
			cut:       false,
			want:      false,
			wantAsked: []string{overrunStopSessionName},
		},
		{
			name:      "host seat with no sandbox record answers false without asking the backend",
			record:    nil,
			want:      false,
			wantAsked: nil,
		},
		{
			name:      "failing backend is an error naming the seat and its cause",
			record:    streamCutRecord(),
			cutErr:    errors.New("tail read timed out"),
			wantErr:   "tail read timed out",
			wantAsked: []string{overrunStopSessionName},
		},
		{
			name:    "unreadable sandbox record is an error naming the seat and its cause",
			readErr: errors.New("record file is corrupt"),
			wantErr: "record file is corrupt",
		},
		{
			name: "record that names no session is an error naming the seat",
			record: &session.SandboxRecord{
				Rig:     stopSeat.Rig,
				Polecat: stopSeat.Polecat,
			},
			wantErr: "names no session",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newStreamCutFixture(t, tc.record, tc.readErr, tc.cut, tc.cutErr)
			got, err := f.h.StreamCut(stopSeat.Rig, stopSeat.Polecat)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), seat) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("StreamCut error = %v, want an error naming %s and containing %q", err, seat, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("StreamCut error = %v, want none", err)
			}
			if got != tc.want {
				t.Errorf("StreamCut = %v, want %v", got, tc.want)
			}
			if strings.Join(*f.asked, ",") != strings.Join(tc.wantAsked, ",") {
				t.Errorf("backend asked about %v, want %v", *f.asked, tc.wantAsked)
			}
		})
	}
}
