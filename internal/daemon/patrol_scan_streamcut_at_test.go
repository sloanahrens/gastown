package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// streamCutAtWhen is when a fixture's cut record was written.
func streamCutAtWhen() time.Time {
	return time.Date(2026, 11, 24, 12, 0, 28, 0, time.UTC)
}

// newStreamCutAtFixture is the patrol host of newStreamCutFixture, with the record read and
// the prefix registry unchanged, but with sandboxStreamCutAtFn answering cut, cutAt and cutErr.
// asked records every session the backend is asked about.
func newStreamCutAtFixture(t *testing.T, record *session.SandboxRecord, readErr error, cut bool, cutAt time.Time, cutErr error) streamCutFixture {
	t.Helper()
	f := newStreamCutFixture(t, record, readErr, false, nil)
	var asked []string
	f.h.d.sandboxStreamCutAtFn = func(townRoot string, rec session.SandboxRecord) (bool, time.Time, error) {
		asked = append(asked, rec.Session)
		return cut, cutAt, cutErr
	}
	f.asked = &asked
	return f
}

// TestPatrolScanStreamCutAtAnswers pins what the patrol scan's revive reads from StreamCutAt: a
// sandbox seat's cut and the time the cut was written, a host seat and an intact seat answer
// false, and a failed read is an error naming the seat and its cause, with no time.
func TestPatrolScanStreamCutAtAnswers(t *testing.T) {
	t.Parallel()
	seat := stopSeat.Rig + "/" + stopSeat.Polecat
	cases := []struct {
		name      string
		record    *session.SandboxRecord
		readErr   error
		cut       bool
		cutAt     time.Time
		cutErr    error
		want      bool
		wantAt    time.Time
		wantErr   string
		wantAsked []string
	}{
		{
			name:      "sandbox seat whose stream was cut answers true and when the cut was written",
			record:    streamCutRecord(),
			cut:       true,
			cutAt:     streamCutAtWhen(),
			want:      true,
			wantAt:    streamCutAtWhen(),
			wantAsked: []string{overrunStopSessionName},
		},
		{
			name:      "sandbox seat whose stream is intact answers false with no time",
			record:    streamCutRecord(),
			cut:       false,
			want:      false,
			wantAsked: []string{overrunStopSessionName},
		},
		{
			name:      "host seat with no sandbox record answers false with no time, without asking the backend",
			record:    nil,
			want:      false,
			wantAsked: nil,
		},
		{
			name:      "failing backend is an error naming the seat and its cause, with no time",
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
			f := newStreamCutAtFixture(t, tc.record, tc.readErr, tc.cut, tc.cutAt, tc.cutErr)
			got, at, err := f.h.StreamCutAt(stopSeat.Rig, stopSeat.Polecat)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), seat) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("StreamCutAt error = %v, want an error naming %s and containing %q", err, seat, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("StreamCutAt error = %v, want none", err)
			}
			if got != tc.want || !at.Equal(tc.wantAt) {
				t.Errorf("StreamCutAt = %v, %v, want %v, %v", got, at, tc.want, tc.wantAt)
			}
			if strings.Join(*f.asked, ",") != strings.Join(tc.wantAsked, ",") {
				t.Errorf("backend asked about %v, want %v", *f.asked, tc.wantAsked)
			}
		})
	}
}

// TestPatrolScanStreamCutAtWrapsTheBackendError: a failing backend's error is wrapped, so a caller
// can still ask errors.Is of its cause.
func TestPatrolScanStreamCutAtWrapsTheBackendError(t *testing.T) {
	t.Parallel()
	cause := errors.New("tail read timed out")
	f := newStreamCutAtFixture(t, streamCutRecord(), nil, false, time.Time{}, cause)
	if _, _, err := f.h.StreamCutAt(stopSeat.Rig, stopSeat.Polecat); !errors.Is(err, cause) {
		t.Errorf("StreamCutAt error = %v, want it to wrap %v", err, cause)
	}
}

// streamCutAtAnswerer is a backend that answers StreamCutAt, as the openshell one does.
type streamCutAtAnswerer struct {
	session.Backend
	cut bool
	at  time.Time
}

func (s streamCutAtAnswerer) StreamCutAt(string) (bool, time.Time, error) { return s.cut, s.at, nil }

// TestPatrolScanStreamCutAtOverTheBackend: the real read builds the backend from the record's
// profile, returns its construction error, refuses a backend that cannot answer StreamCutAt as an
// error and never as an intact stream (including one that answers only StreamCut), and otherwise
// passes the backend's answer and its time through.
func TestPatrolScanStreamCutAtOverTheBackend(t *testing.T) {
	t.Parallel()
	rec := session.SandboxRecord{Session: "gt-x", Profile: "p", Gateway: "g", Image: "i"}
	build := errors.New("no gateway profile")
	if _, _, err := sandboxStreamCutAtOver(func(string, session.Profile) (session.Backend, error) { return nil, build }, "town", rec); !errors.Is(err, build) {
		t.Errorf("a backend that cannot be built: err = %v, want %v", err, build)
	}
	var gotProfile session.Profile
	_, _, err := sandboxStreamCutAtOver(func(_ string, p session.Profile) (session.Backend, error) {
		gotProfile = p
		return nil, nil
	}, "town", rec)
	if err == nil || !strings.Contains(err.Error(), "cannot read the time of a stream cut") {
		t.Errorf("a backend without StreamCutAt: err = %v, want the refusal, never false with no error", err)
	}
	_, _, err = sandboxStreamCutAtOver(func(string, session.Profile) (session.Backend, error) { return streamCutAnswerer{cut: true}, nil }, "town", rec)
	if err == nil || !strings.Contains(err.Error(), "cannot read the time of a stream cut") {
		t.Errorf("a backend that answers only StreamCut: err = %v, want the refusal, never an intact stream", err)
	}
	if gotProfile.Name != "p" || gotProfile.Gateway != "g" || gotProfile.Image != "i" {
		t.Errorf("backend built from %+v, want the record's profile", gotProfile)
	}
	got, gotAt, err := sandboxStreamCutAtOver(func(string, session.Profile) (session.Backend, error) {
		return streamCutAtAnswerer{cut: true, at: streamCutAtWhen()}, nil
	}, "town", rec)
	if err != nil || !got || !gotAt.Equal(streamCutAtWhen()) {
		t.Errorf("an answering backend = %v, %v, %v; want its true and its time passed through", got, gotAt, err)
	}
}
