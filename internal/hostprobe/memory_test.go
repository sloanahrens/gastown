package hostprobe

import "testing"

func TestParseMemoryPressure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		want    float64
		wantErr bool
	}{
		{"real output", "The system has 137438953472 (8388608 pages with a page size of 16384).\n\nStats: \nPages free: 34532 \n\nSystem-wide memory free percentage: 94%\n", 94, false},
		{"low", "System-wide memory free percentage: 9%", 9, false},
		{"no line is an error, not zero", "The system has 1 pages\n", 0, true},
		{"empty", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseMemoryPressure(tt.out)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ParseMemoryPressure = %v, %v; want %v, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
