package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/gtwrap"
	"github.com/steveyegge/gastown/internal/metrics"
)

// hostBdGateway is the gateway check the metrics extract runs over the host's bd:
// the forwarder's own decision on the bd on PATH (gt-g8oimf).
func hostBdGateway() metrics.BdGatewayCheck {
	return metricsBdGateway(exec.LookPath, os.Stat, os.ReadFile)
}

// metricsBdGateway refuses the extract when the bd on PATH is the gt-wrap
// forwarder and its gateway is not configured. The forwarder refuses every bd call
// in that state, so the extract would list no beads and write empty estimates. The
// decision is gtwrap.DecideForwarder's; this only asks it. A host with no bd on
// PATH has no forwarder to refuse. The lookup and the filesystem are injected so
// the decision is tested without the host.
func metricsBdGateway(lookPath func(string) (string, error), stat func(string) (fs.FileInfo, error), readFile func(string) ([]byte, error)) metrics.BdGatewayCheck {
	return func(getenv func(string) string) error {
		bdPath, err := lookPath("bd")
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find bd on PATH: %w", err)
		}
		// Any refusal here is the forwarder refusing every bd call the extract makes,
		// so each one stops the extract, not only a missing BD_GATEWAY_URL.
		_, err = gtwrap.DecideForwarder(gtwrap.ForwarderInputs{
			Tool:       "bd",
			Executable: func() (string, error) { return bdPath, nil },
			Stat:       stat,
			ReadFile:   readFile,
			GatewayURL: getenv("BD_GATEWAY_URL"),
			TokenFile:  getenv("BD_GATEWAY_TOKEN_FILE"),
		})
		return err
	}
}
