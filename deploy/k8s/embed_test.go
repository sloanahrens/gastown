package k8s

import (
	"bytes"
	"os"
	"testing"
)

// TestEmbeddedManifestsMatchTheFiles holds each exported byte copy to the file it
// embeds, so a drift compare reads the text the reviewed file holds at the
// commit the binary was built from (gt-aar839).
func TestEmbeddedManifestsMatchTheFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		got  []byte
	}{
		{file: "update-admission.yaml", got: UpdateAdmissionYAML},
		{file: "bd-gateway-service-admission.yaml", got: BdGatewayServiceAdmissionYAML},
		{file: "update-rbac.yaml", got: UpdateRBACYAML},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			if len(tc.got) == 0 {
				t.Fatalf("the embedded copy of %s is empty", tc.file)
			}
			want, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("reading %s: %v", tc.file, err)
			}
			if !bytes.Equal(tc.got, want) {
				t.Errorf("the embedded copy of %s differs from the file on disk", tc.file)
			}
		})
	}
}
