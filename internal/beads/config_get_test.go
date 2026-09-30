package beads

import "testing"

func TestParseConfigGetJSON(t *testing.T) {
	cases := map[string]string{
		// legacy bd: the bare object
		`{"key":"events-journal","location":"config.yaml","schema_version":1,"value":"false"}`: "false",
		// machine mode: the object under data
		`{"schema_version":1,"contract_version":1,"data":{"key":"events-journal","value":" true "}}`: "true",
	}
	for in, want := range cases {
		got, err := ParseConfigGetJSON([]byte(in))
		if err != nil || got != want {
			t.Errorf("ParseConfigGetJSON(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "false", `{"key":"x"}`, `{"data":null}`, `{"data":{"key":"x"}}`} {
		if got, err := ParseConfigGetJSON([]byte(bad)); err == nil {
			t.Errorf("ParseConfigGetJSON(%q) accepted as %q", bad, got)
		}
	}
}
