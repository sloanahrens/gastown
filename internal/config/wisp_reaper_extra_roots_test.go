package config

import (
	"slices"
	"strings"
	"testing"
)

// TestWispReaperConfigExtraBeadsRoots: extra_beads_roots survives strict
// decoding into the field the reaper reads, parses to the roots searched after
// the town, and is empty when unset. A relative root is refused rather than
// resolved against the daemon's working directory, which is not the
// operator's (gt-33etw).
func TestWispReaperConfigExtraBeadsRoots(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"wisp_reaper":{"enabled":true,"extra_beads_roots":["/home/op/.claude"]}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.WispReaper == nil {
		t.Fatal("wisp_reaper decoded as absent")
	}
	got, err := cfg.Patrols.WispReaper.ParseExtraBeadsRoots()
	if err != nil {
		t.Fatalf("ParseExtraBeadsRoots: %v", err)
	}
	if want := []string{"/home/op/.claude"}; !slices.Equal(got, want) {
		t.Errorf("extra_beads_roots = %v, want %v", got, want)
	}

	// Unset means the town root is the only place searched, and an absent
	// wisp_reaper section reads the same way.
	if got, err := (&WispReaperConfig{}).ParseExtraBeadsRoots(); len(got) != 0 || err != nil {
		t.Errorf("unset extra_beads_roots = %v, %v; want empty, nil", got, err)
	}
	var absent *WispReaperConfig
	if got, err := absent.ParseExtraBeadsRoots(); len(got) != 0 || err != nil {
		t.Errorf("absent wisp_reaper section = %v, %v; want empty, nil", got, err)
	}

	tests := []struct {
		name    string
		roots   []string
		want    []string
		wantErr bool
	}{
		{name: "two roots", roots: []string{"/a/.claude", "/b"}, want: []string{"/a/.claude", "/b"}},
		{name: "cleaned", roots: []string{"/home/op/../op/.claude"}, want: []string{"/home/op/.claude"}},
		{name: "relative root", roots: []string{"relative/root"}, wantErr: true},
		{name: "tilde is not absolute", roots: []string{"~/.claude"}, wantErr: true},
		{name: "empty entry", roots: []string{""}, wantErr: true},
		{name: "blank entry", roots: []string{"  "}, wantErr: true},
		{name: "one bad root refuses the list", roots: []string{"/ok", "bad"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &WispReaperConfig{ExtraBeadsRoots: tt.roots}
			got, err := c.ParseExtraBeadsRoots()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseExtraBeadsRoots(%v) = %v, want an error", tt.roots, got)
				}
				if !strings.Contains(err.Error(), "patrols.wisp_reaper.extra_beads_roots") {
					t.Errorf("error %q does not name the field", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseExtraBeadsRoots(%v): %v", tt.roots, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParseExtraBeadsRoots(%v) = %v, want %v", tt.roots, got, tt.want)
			}
		})
	}
}
