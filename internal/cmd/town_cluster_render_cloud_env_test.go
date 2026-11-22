package cmd

import (
	"bytes"
	"testing"
)

// TestTownClusterRenderCloudInputEnv holds the daemon env names to the cloud
// flags: each is present at its flag's path when the flag is set, and absent,
// empty value included, when it is not (gt-kvkia2).
func TestTownClusterRenderCloudInputEnv(t *testing.T) {
	t.Parallel()
	const reports, releases = "/Users/Shared/gt-cloud/reports", "/Users/Shared/gt-cloud/releases"
	for _, tc := range []struct{ name, reports, releases string }{{"both flags", reports, releases}, {"neither flag", "", ""}} {
		f := clusterRenderFixture(t)
		f.cloudReportsHostPath, f.cloudReleasesHostPath = tc.reports, tc.releases
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("%s: reports %q releases %q: render: %v", tc.name, tc.reports, tc.releases, err)
		}
		env := renderedDeployment(t, f.out, "-deployment-daemon.yaml").Spec.Template.Spec.Containers[0].Env
		var found []string
		for _, e := range env {
			found = append(found, e.Name)
		}
		for name, want := range map[string]string{"GT_CLOUD_REPORTS_DIR": tc.reports, "GT_CLOUD_RELEASES_DIR": tc.releases} {
			got, ok := "", false
			for _, e := range env {
				if e.Name == name {
					got, ok = e.Value, true
				}
			}
			if ok != (want != "") || got != want {
				t.Errorf("%s: reports %q releases %q: daemon env %s = %q (present %t), want %q (present %t); the variables found are %q",
					tc.name, tc.reports, tc.releases, name, got, ok, want, want != "", found)
			}
		}
	}
}
