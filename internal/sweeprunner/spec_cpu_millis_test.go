package sweeprunner

import (
	"errors"
	"testing"
)

// TestSpecCPUMillis holds the millicore CPU requests to the run description:
// ConfigSpec carries both from the runner block, and Validate refuses a value a
// backend could not render, naming the field (gt-9ji7v9).
func TestSpecCPUMillis(t *testing.T) {
	t.Parallel()

	t.Run("ConfigSpec copies both millicore requests", func(t *testing.T) {
		t.Parallel()
		cfg := capacityConfig()
		cfg.Dind = true
		cfg.RunnerCPURequestMillis = 3500
		cfg.DindCPURequestMillis = 1500
		spec := ConfigSpec(cfg)
		if spec.RunnerCPURequestMillis != 3500 {
			t.Errorf("ConfigSpec runnerCPURequestMillis = %d, want 3500", spec.RunnerCPURequestMillis)
		}
		if spec.DindCPURequestMillis != 1500 {
			t.Errorf("ConfigSpec dindCPURequestMillis = %d, want 1500", spec.DindCPURequestMillis)
		}
	})

	t.Run("Validate accepts and refuses the millicore requests", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name      string
			set       func(*Spec)
			wantField string // empty: the spec is accepted
		}{
			{name: "zero asks for the whole share", set: func(s *Spec) { s.RunnerCPURequestMillis, s.DindCPURequestMillis = 0, 0 }},
			{name: "dind at its share", set: func(s *Spec) { s.DindCPURequestMillis = 2000 }},     // the default 2-CPU sidecar
			{name: "runner at its share", set: func(s *Spec) { s.RunnerCPURequestMillis = 6000 }}, // 8 CPUs less the 2-CPU sidecar
			{name: "both within their shares", set: func(s *Spec) { s.RunnerCPURequestMillis, s.DindCPURequestMillis = 3500, 1500 }},
			{name: "negative runner", set: func(s *Spec) { s.RunnerCPURequestMillis = -1 }, wantField: "runnerCPURequestMillis"},
			{name: "negative dind", set: func(s *Spec) { s.DindCPURequestMillis = -1 }, wantField: "dindCPURequestMillis"},
			{name: "runner set with the whole-core request", set: func(s *Spec) { s.RunnerCPURequest, s.RunnerCPURequestMillis = 3, 3000 }, wantField: "runnerCPURequestMillis"},
			{name: "dind with the sidecar off", set: func(s *Spec) { s.Dind, s.DindCPURequestMillis = false, 500 }, wantField: "dindCPURequestMillis"},
			{name: "dind above its share", set: func(s *Spec) { s.DindCPURequestMillis = 2001 }, wantField: "dindCPURequestMillis"},
			{name: "runner above its share", set: func(s *Spec) { s.RunnerCPURequestMillis = 6001 }, wantField: "runnerCPURequestMillis"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				spec := goldenSpec()
				tc.set(&spec)
				err := spec.Validate()
				if tc.wantField == "" {
					if err != nil {
						t.Fatalf("Validate = %v, want the spec accepted", err)
					}
					return
				}
				var fieldErr *FieldError
				if !errors.As(err, &fieldErr) || fieldErr.Field != tc.wantField {
					t.Fatalf("Validate = %v, want a *FieldError naming %s", err, tc.wantField)
				}
			})
		}
	})
}
