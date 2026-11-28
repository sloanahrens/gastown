package main

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

func TestInImageFor(t *testing.T) {
	t.Parallel()
	checkErr := errors.New("gt is not the wrapper in the image")
	tests := []struct {
		name             string
		kube, unreadable bool
		diag             gtwrap.Diagnosis
		checkErr         error
		wantErr          string
	}{
		{"default target runs the passed check", false, false, gtwrap.Diagnosis{}, nil, ""},
		{"default target returns the check's refusal", false, false, gtwrap.Diagnosis{}, checkErr, checkErr.Error()},
		{"cluster target asks its target", true, false, gtwrap.Diagnosis{Rung: gtwrap.RungExec}, nil, ""},
		{"cluster target returns its refusal", true, false, gtwrap.Diagnosis{Rung: gtwrap.RungDown, Reason: "the daemon pod is not Running"}, nil, "the daemon pod is not Running"},
		{"unreadable config runs neither check", false, true, gtwrap.Diagnosis{}, nil, "read the target config file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tt.kube {
				h.writeConfig(t, kubernetesConfig)
				h.fake.diag = tt.diag
			}
			reads := 0
			if tt.unreadable {
				h.surface.readFile = func(string) ([]byte, error) { reads++; return nil, fs.ErrPermission }
			}
			checked := ""
			err := inImageFor(h.surface, "gt", func(name string) error { checked = name; return tt.checkErr })
			if (tt.wantErr == "" && err != nil) || (tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr))) {
				t.Fatalf("inImageFor = %v, want %q", err, tt.wantErr)
			}
			wantDefault := !tt.kube && !tt.unreadable
			if ran := checked != ""; ran != wantDefault || (ran && checked != "gt") || (tt.unreadable && reads != 1) {
				t.Fatalf("check ran for %q after %d config reads, want gt-or-nothing and one read on error", checked, reads)
			}
			if asked := h.kubeBuilt && h.fake.doctored; asked != tt.kube {
				t.Fatalf("target opened = %v, doctor asked = %v, want both %v", h.kubeBuilt, h.fake.doctored, tt.kube)
			}
		})
	}
}
