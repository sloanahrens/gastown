package cmd

import (
	"encoding/json"
	"testing"
)

// unwrapFreshSetupJSON returns the payload of a bd --json reply. bd wraps
// every --json reply as {"schema_version": N, "data": ...}; older bd printed
// the payload bare. A reply that is not that envelope is returned unchanged,
// so the caller's decode reports it rather than reading an empty value.
func unwrapFreshSetupJSON(out []byte) []byte {
	var probe map[string]json.RawMessage
	if json.Unmarshal(out, &probe) != nil {
		return out
	}
	_, hasVersion := probe["schema_version"]
	data, hasData := probe["data"]
	if !hasVersion || !hasData {
		return out
	}
	return data
}

func TestUnwrapFreshSetupJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare create payload passes through",
			in:   `{"id":"fs1-cja","title":"t","status":"open"}`,
			want: `{"id":"fs1-cja","title":"t","status":"open"}`,
		},
		{
			name: "bare show array passes through",
			in:   `[{"id":"fs1-cja","status":"open"}]`,
			want: `[{"id":"fs1-cja","status":"open"}]`,
		},
		{
			name: "enveloped create payload is unwrapped",
			in:   `{"schema_version":1,"data":{"id":"fs1-cja","title":"t","status":"open"}}`,
			want: `{"id":"fs1-cja","title":"t","status":"open"}`,
		},
		{
			name: "enveloped show array is unwrapped",
			in:   `{"schema_version":1,"data":[{"id":"fs1-cja","status":"open"}]}`,
			want: `[{"id":"fs1-cja","status":"open"}]`,
		},
		{
			name: "data without schema_version is not an envelope",
			in:   `{"data":{"id":"fs1-cja"}}`,
			want: `{"data":{"id":"fs1-cja"}}`,
		},
		{
			name: "non-JSON passes through",
			in:   `not json`,
			want: `not json`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := string(unwrapFreshSetupJSON([]byte(tt.in)))
			if got != tt.want {
				t.Errorf("unwrapFreshSetupJSON(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
