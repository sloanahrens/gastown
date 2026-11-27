package hostprobe

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// decodeCause is the prefix Err carries when a value does not decode.
const decodeCause = "value does not decode: "

// TestProbeField: one field that does not decode is that field's error, not a
// refusal of the file, and a value that only partly decodes stays at the zero
// value. A field that is not an object, or whose err is not a string, refuses
// the file.
func TestProbeField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fields string
		refuse       bool
		want         Disk
		wantErr      string
	}{
		{"a bad value keeps its neighbours", `"disk":{"value":{"used_pct":61.5}},"go_cache_trim":{"value":{"last_good":""}}`, false, Disk{UsedPct: 61.5}, ""},
		{"a partly decoded value stays at the zero value", `"disk":{"value":{"used_pct":61.5,"free_gb":"lots"}}`, false, Disk{}, decodeCause},
		{"the publisher's err comes ahead of the decode cause", `"disk":{"value":{"used_pct":61.5,"free_gb":"lots"},"err":"df /: exit status 1"}`, false, Disk{}, "df /: exit status 1; " + decodeCause},
		{"a field that is not an object refuses the file", `"disk":"oops"`, true, Disk{}, ""},
		{"an err that is not a string refuses the file", `"mem_free_pct":{"value":94,"err":7}`, true, Disk{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeProbes(t, fmt.Sprintf(`{"v":%d,"at":%q,%s}`, SchemaVersion, testAt().Format(time.RFC3339), tc.fields))
			got, err := Read(path, testAt(), 3*time.Minute)
			if tc.refuse {
				if err == nil || !strings.Contains(err.Error(), "decode probe file") {
					t.Fatalf("Read(%s) = %+v, %v, want the file refused with %q", tc.fields, got, err, "decode probe file")
				}
				return
			}
			if err != nil {
				t.Fatalf("Read refused the file over one field's value: %v", err)
			}
			if got.Disk.Value != tc.want || (tc.wantErr == "" && got.Disk.Err != "") ||
				(tc.wantErr != "" && !strings.HasPrefix(got.Disk.Err, tc.wantErr)) {
				t.Errorf("Disk = %+v, want value %+v with Err beginning %q", got.Disk, tc.want, tc.wantErr)
			}
			if tc.wantErr == "" && (got.GoCacheTrim == nil || !strings.HasPrefix(got.GoCacheTrim.Err, decodeCause) ||
				!got.GoCacheTrim.Value.LastGood.IsZero()) {
				t.Errorf("GoCacheTrim = %+v, want the zero time with Err stating %q", got.GoCacheTrim, decodeCause)
			}
		})
	}
}
