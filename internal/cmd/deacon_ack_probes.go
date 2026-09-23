package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var deaconAckProbesJSON bool

var deaconAckProbesCmd = &cobra.Command{
	Use:   "ack-probes",
	Short: "Enumerate and acknowledge pending doctor-dog self-probes",
	Long: `Enumerate and acknowledge pending DEACON_SELF_PROBE messages.

DEACON_SELF_PROBE messages (see daemon.SendDeaconSelfProbe, gt-jmy3) are a
mechanical supervision signal, not mail for a human or agent to triage, so
'gt mail inbox' strips them from its default view — which also means the
deacon can never discover their message IDs through the normal inbox to run
'gt mail read <id>' on them. This command reads the deacon's mailbox
directly (bypassing that filter), finds pending probes, and acknowledges
each one exactly as 'gt mail read' would: marks it read and records delivery
receipt. That ack is what the deacon-self-probe doctor check reads back on
the next doctor-dog cycle.

Probes are left in place after acking, not archived: the normal wisp GC
reaps them once the doctor-dog cycle that reads the ack back has run.

Examples:
  gt deacon ack-probes          # Ack all pending probes
  gt deacon ack-probes --json   # Machine-readable output`,
	RunE: runDeaconAckProbes,
}

func init() {
	deaconAckProbesCmd.Flags().BoolVar(&deaconAckProbesJSON, "json", false, "Output results as JSON")
	deaconCmd.AddCommand(deaconAckProbesCmd)
}

// DeaconAckProbesResult is the JSON-serializable outcome of 'gt deacon ack-probes'.
type DeaconAckProbesResult struct {
	Found int      `json:"found"`
	Acked int      `json:"acked"`
	IDs   []string `json:"ids,omitempty"`
}

// ackableMailbox is the narrow mailbox surface ackDeaconSelfProbes depends
// on, satisfied by *mail.Mailbox. A dedicated interface lets tests inject a
// fake instead of shelling out to bd.
type ackableMailbox interface {
	List() ([]*mail.Message, error)
	MarkReadOnly(id string) error
	AcknowledgeDeliveries(recipientAddress string, messages []*mail.Message) error
}

func runDeaconAckProbes(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	mailbox := mail.NewMailboxFromAddress(constants.RoleDeacon, townRoot)
	result, err := ackDeaconSelfProbes(mailbox, constants.RoleDeacon)
	if err != nil {
		return fmt.Errorf("acking probes: %w", err)
	}

	if deaconAckProbesJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if result.Found == 0 {
		fmt.Printf("%s No pending probes\n", style.Dim.Render("○"))
		return nil
	}
	fmt.Printf("%s Acked %d/%d pending probe(s)\n", style.Bold.Render("✓"), result.Acked, result.Found)
	return nil
}

// ackDeaconSelfProbes finds DEACON_SELF_PROBE messages in mailbox that
// haven't been acked yet and acknowledges them (mark read + delivery ack),
// exactly as 'gt mail read <id>' would per-message — but discoverable
// without going through 'gt mail inbox', which filters probes out by design
// (see daemon.DeaconSelfProbeSubjectPrefix).
func ackDeaconSelfProbes(mailbox ackableMailbox, address string) (DeaconAckProbesResult, error) {
	messages, err := mailbox.List()
	if err != nil {
		return DeaconAckProbesResult{}, err
	}

	var pending []*mail.Message
	for _, msg := range messages {
		if msg == nil || !strings.HasPrefix(msg.Subject, daemon.DeaconSelfProbeSubjectPrefix) {
			continue
		}
		if msg.DeliveryState == mail.DeliveryStateAcked {
			continue
		}
		pending = append(pending, msg)
	}

	result := DeaconAckProbesResult{Found: len(pending)}
	for _, msg := range pending {
		if err := mailbox.MarkReadOnly(msg.ID); err != nil {
			return result, fmt.Errorf("marking %s read: %w", msg.ID, err)
		}
		if err := mailbox.AcknowledgeDeliveries(address, []*mail.Message{msg}); err != nil {
			return result, fmt.Errorf("acking %s: %w", msg.ID, err)
		}
		result.Acked++
		result.IDs = append(result.IDs, msg.ID)
	}
	return result, nil
}
