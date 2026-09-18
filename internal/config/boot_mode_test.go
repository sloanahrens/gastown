package config

import "testing"

func TestBootModeValue(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", BootModeMechanical},
		{"mechanical", BootModeMechanical},
		{"agent", BootModeAgent},
		{" Agent ", BootModeAgent},
		{"bogus", BootModeMechanical},
	}
	for _, c := range cases {
		d := &DaemonThresholds{BootMode: c.in}
		if got := d.BootModeValue(); got != c.want {
			t.Errorf("BootModeValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	var nilD *DaemonThresholds
	if got := nilD.BootModeValue(); got != BootModeMechanical {
		t.Errorf("nil thresholds -> %q, want mechanical", got)
	}
}
