package config

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestCloudCheckConfigDecodesAndRefusesMistakes: the block decodes its four
// fields, an interval the patrol cannot schedule is refused rather than read as
// the default, and an unknown key is named.
func TestCloudCheckConfigDecodesAndRefusesMistakes(t *testing.T) {
	t.Parallel()
	var c DaemonPatrolConfig
	body := `{"patrols":{"cloud_check":{"enabled":true,"interval":"10m","command":"/usr/local/bin/cloudcheck","key_file":"/keys/gcp-key"}}}`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("decoding the block: %v", err)
	}
	cc := c.Patrols.CloudCheck
	if cc == nil || !cc.Enabled || cc.Interval != "10m" || cc.Command != "/usr/local/bin/cloudcheck" || cc.KeyFile != "/keys/gcp-key" {
		t.Fatalf("decoded %+v, want the four fields", cc)
	}

	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "an interval the patrol cannot schedule",
			body: `{"patrols":{"cloud_check":{"enabled":true,"interval":"soon"}}}`,
			want: "patrols.cloud_check.interval",
		},
		{
			name: "an unknown key",
			body: `{"patrols":{"cloud_check":{"enabled":true,"keyfile":"/keys/gcp-key"}}}`,
			want: "keyfile",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c DaemonPatrolConfig
			err := json.Unmarshal([]byte(tt.body), &c)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("decode = %v, want a refusal naming %q", err, tt.want)
			}
		})
	}
}

// TestCloudCheckKeyFilePath: the setting wins; empty is the default name in the
// config directory, and a directory that cannot be resolved is an error, never a
// path in no directory.
func TestCloudCheckKeyFilePath(t *testing.T) {
	t.Parallel()
	dir := func() (string, error) { return "/cfg", nil }

	got, err := (&CloudCheckConfig{KeyFile: "/keys/gcp-key"}).keyFilePath(dir)
	if err != nil || got != "/keys/gcp-key" {
		t.Errorf("a set key file = %q, %v; want the setting", got, err)
	}

	got, err = (&CloudCheckConfig{}).keyFilePath(dir)
	if err != nil || got != filepath.Join("/cfg", CloudCheckDefaultKeyFile) {
		t.Errorf("an empty key file = %q, %v; want the default name in the config directory", got, err)
	}

	if _, err := (&CloudCheckConfig{}).keyFilePath(func() (string, error) { return "", errors.New("no home") }); err == nil {
		t.Error("a failure to resolve the config directory returned no error")
	}
}
