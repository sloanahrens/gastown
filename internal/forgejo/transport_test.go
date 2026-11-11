package forgejo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPlaintextHostOf: the viewer credential may go over http to the town's own
// Forgejo at its one port, and to nothing else. A look-alike host, another port
// or an https registry gets no plaintext host.
func TestPlaintextHostOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, scheme, host, want string
	}{
		{name: "http to the town's Forgejo", scheme: "http", host: "forgejo:3000", want: PlaintextHost},
		{name: "host matches without case", scheme: "http", host: "Forgejo:3000", want: PlaintextHost},
		{name: "https to the town's Forgejo", scheme: "https", host: "forgejo:3000", want: ""},
		{name: "forgejo on another port", scheme: "http", host: "forgejo:3001", want: ""},
		{name: "a look-alike host", scheme: "http", host: "forgejo.example.com:3000", want: ""},
		{name: "another dotless service name", scheme: "http", host: "registry:3000", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, PlaintextHostOf(tc.scheme, tc.host))
		})
	}
}
