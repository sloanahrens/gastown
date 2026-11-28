package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

func TestDoctorAnswers(t *testing.T) {
	t.Parallel()
	doctorErr := errors.New("the compose file cannot be read")
	tests := []struct {
		name    string
		diag    gtwrap.Diagnosis
		doctor  error
		wantErr string
	}{
		{"exec rung holds", gtwrap.Diagnosis{Rung: gtwrap.RungExec}, nil, ""},
		{"run rung holds", gtwrap.Diagnosis{Rung: gtwrap.RungRun}, nil, ""},
		{"down rung refuses", gtwrap.Diagnosis{Rung: gtwrap.RungDown, Reason: "Docker is down"}, nil, "Docker is down"},
		{"doctor error refuses", gtwrap.Diagnosis{}, doctorErr, doctorErr.Error()},
	}
	for _, tt := range tests {
		err := doctorAnswers(&fakeTarget{diag: tt.diag, doctorErr: tt.doctor})
		if tt.wantErr == "" && err != nil {
			t.Errorf("%s: doctorAnswers = %v, want nil", tt.name, err)
		}
		if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%s: doctorAnswers = %v, want an error containing %q", tt.name, err, tt.wantErr)
		}
	}
}
