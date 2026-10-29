//go:build integration

package rig

import (
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestIntegrationPersistedDoltEndpointIgnoresTheProcessMode sets the real
// container-mode variable, which the unit tier cannot (docs/testing.md,
// `no-env`): the live resolver then names dolt.container_host, and the endpoint
// a rig's config.yaml records still names dolt.host (gt-qpeei.17).
func TestIntegrationPersistedDoltEndpointIgnoresTheProcessMode(t *testing.T) {
	root := writeContainerHostTown(t)
	t.Setenv(config.ContainerModeEnv, config.SupervisorContainer)

	live, ok := config.ResolveDoltEndpoint(root)
	if !ok || live.Host != "host.docker.internal" {
		t.Fatalf("the live resolver in container mode = %+v (ok %v), want the container host: the control for this test", live, ok)
	}
	got, ok := persistedDoltEndpoint(root)
	if !ok || got.Host != "127.0.0.2" || got.Port != 5507 {
		t.Errorf("persistedDoltEndpoint = %+v (ok %v), want dolt.host 127.0.0.2 and port 5507 whatever the process mode", got, ok)
	}
}
