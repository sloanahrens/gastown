package rehearsal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRehearsalTownCreate(t *testing.T) {
	t.Parallel()
	bdFailed := errors.New("exit status 1")
	const leaked = "bd: secret output"
	cases := []struct {
		name, rig, title, body, answer string
		runErr                         error
		wantArgv                       []string
		wantID                         string
		wantErr                        bool
	}{
		{name: "a rig's bead runs bd -C the rig", rig: "rehearsal", title: "probe one", body: "body one\n", answer: "  rh-01\n",
			wantArgv: []string{"bd", "-C", "rehearsal", "create", "--silent", "--title=probe one", "--description=body one\n"}, wantID: "rh-01"},
		{name: "no rig leaves -C out", title: "probe two", body: "body two\n", answer: "rh-02\n",
			wantArgv: []string{"bd", "create", "--silent", "--title=probe two", "--description=body two\n"}, wantID: "rh-02"},
		{name: "a failed command is wrapped and carries no output", rig: "rehearsal", title: "probe three", body: "body three\n", answer: leaked, runErr: bdFailed,
			wantArgv: []string{"bd", "-C", "rehearsal", "create", "--silent", "--title=probe three", "--description=body three\n"}, wantErr: true},
		{name: "an answer empty after trimming is an error", rig: "rehearsal", title: "probe four", body: "body four\n", answer: " \n",
			wantArgv: []string{"bd", "-C", "rehearsal", "create", "--silent", "--title=probe four", "--description=body four\n"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotArgv []string
			town := Town{Rig: tc.rig, Run: func(_ context.Context, argv []string) ([]byte, error) {
				gotArgv = argv
				return []byte(tc.answer), tc.runErr
			}}

			id, err := town.Create(context.Background(), Task{Title: tc.title, Body: tc.body})
			if !slices.Equal(gotArgv, tc.wantArgv) {
				t.Errorf("Create ran argv %q, want %q", gotArgv, tc.wantArgv)
			}
			if !tc.wantErr {
				if err != nil || id != tc.wantID {
					t.Fatalf("Create = %q, %v; want %q, nil", id, err, tc.wantID)
				}
				return
			}
			if err == nil || id != "" {
				t.Fatalf("Create = %q, %v; want no id and an error", id, err)
			}
			if tc.runErr != nil && !errors.Is(err, tc.runErr) {
				t.Errorf("Create error = %v, want it to wrap %v", err, tc.runErr)
			}
			if !strings.Contains(err.Error(), "bd create") || strings.Contains(err.Error(), leaked) {
				t.Errorf("Create error %q must name bd create and carry no output", err)
			}
		})
	}
}
