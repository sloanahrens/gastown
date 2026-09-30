package mail

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestParseDeliveryLabels_CrashAndRetryStates(t *testing.T) {
	t.Parallel()
	t.Run("pending only", func(t *testing.T) {
		state, by, at := ParseDeliveryLabels([]string{
			DeliveryLabelPending,
		})
		if state != DeliveryStatePending {
			t.Fatalf("state = %q, want %q", state, DeliveryStatePending)
		}
		if by != "" || at != nil {
			t.Fatalf("pending state should not include ack metadata, got by=%q at=%v", by, at)
		}
	})

	t.Run("partial ack write keeps pending", func(t *testing.T) {
		state, by, at := ParseDeliveryLabels([]string{
			DeliveryLabelPending,
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		})
		if state != DeliveryStatePending {
			t.Fatalf("state = %q, want %q", state, DeliveryStatePending)
		}
		if by != "" || at != nil {
			t.Fatalf("partial ack should not flip state, got by=%q at=%v", by, at)
		}
	})

	t.Run("acked label flips state", func(t *testing.T) {
		state, by, at := ParseDeliveryLabels([]string{
			DeliveryLabelPending,
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			DeliveryLabelAcked,
		})
		if state != DeliveryStateAcked {
			t.Fatalf("state = %q, want %q", state, DeliveryStateAcked)
		}
		if by != "gastown/worker" {
			t.Fatalf("ackedBy = %q, want %q", by, "gastown/worker")
		}
		if at == nil {
			t.Fatal("ackedAt should be populated for acked state")
		}
	})

	t.Run("lexicographic label order still parses correctly", func(t *testing.T) {
		// bd show --json returns labels in lexicographic order.
		state, by, at := ParseDeliveryLabels([]string{
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery-acked-by:gastown/worker",
			"delivery:acked",
			"delivery:pending",
		})
		if state != DeliveryStateAcked {
			t.Fatalf("state = %q, want %q", state, DeliveryStateAcked)
		}
		if by != "gastown/worker" {
			t.Fatalf("ackedBy = %q, want %q", by, "gastown/worker")
		}
		if at == nil {
			t.Fatal("ackedAt should be populated for acked state with lex-ordered labels")
		}
	})
}

func TestDeliveryAckLabelSequence(t *testing.T) {
	t.Parallel()
	t.Run("no existing labels uses new timestamp", func(t *testing.T) {
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/worker", at, nil)
		want := []string{
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T14:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("existing timestamp is reused on retry", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		}
		// Use a different time — should be ignored in favor of existing.
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/worker", at, existing)
		want := []string{
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("lexicographic label order still reuses timestamp", func(t *testing.T) {
		// bd show --json returns labels in lexicographic order, so acked-at
		// appears before acked-by. The function must be order-independent.
		existing := []string{
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery-acked-by:gastown/worker",
			"delivery:acked",
			"delivery:pending",
		}
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/worker", at, existing)
		want := []string{
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("different recipient gets fresh timestamp", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/workerA",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		}
		// Different recipient — should NOT reuse workerA's timestamp.
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/workerB", at, existing)
		want := []string{
			"delivery-acked-by:gastown/workerB",
			"delivery-acked-at:2026-02-17T14:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("mixed labels after crash: B must not reuse A's timestamp", func(t *testing.T) {
		// Scenario: A acked fully, then B started acking but crashed after
		// writing acked-by:B (before acked-at). Labels accumulated:
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/workerA",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
			"delivery-acked-by:gastown/workerB",
		}
		// B retries — must generate a fresh timestamp, not reuse A's t1.
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/workerB", at, existing)
		want := []string{
			"delivery-acked-by:gastown/workerB",
			"delivery-acked-at:2026-02-17T14:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestDeliveryAckLabelsToWriteSkipsExistingLabels(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)

	t.Run("partial retry only writes missing ack label", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		}
		got := deliveryAckLabelsToWrite("gastown/worker", at, existing)
		want := []string{"delivery:acked"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("complete retry writes nothing", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
		}
		got := deliveryAckLabelsToWrite("gastown/worker", at, existing)
		if len(got) != 0 {
			t.Fatalf("got %v, want no labels", got)
		}
	})
}

func TestDeliveryPendingRemovalNeeded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"no delivery labels", []string{"gt:message"}, false},
		{"pending only", []string{DeliveryLabelPending}, false},
		{"partial ack keeps pending", []string{DeliveryLabelPending, "delivery-acked-by:gastown/worker", "delivery-acked-at:2026-02-17T12:00:00Z"}, false},
		{"pending and acked converges", []string{DeliveryLabelPending, DeliveryLabelAcked}, true},
		{"acked only", []string{DeliveryLabelAcked}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deliveryPendingRemovalNeeded(tt.labels); got != tt.want {
				t.Fatalf("deliveryPendingRemovalNeeded(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestAcknowledgeDeliveryBeadConvergesPendingLabel(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	labelsPath := filepath.Join(tmp, "labels.txt")
	initialLabels := strings.Join([]string{
		DeliveryLabelPending,
		"delivery-acked-by:gastown/worker",
		"delivery-acked-at:2026-02-17T12:00:00Z",
		DeliveryLabelAcked,
	}, "\n") + "\n"
	if err := os.WriteFile(labelsPath, []byte(initialLabels), 0644); err != nil {
		t.Fatalf("write labels: %v", err)
	}

	// bd keeps the bead's labels in labelsPath: show lists them, label add
	// appends one it lacks, label remove drops one or fails when it is not
	// there.
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		args := c.Args
		data, _ := os.ReadFile(labelsPath)
		var labels []string
		for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if l != "" {
				labels = append(labels, l)
			}
		}
		write := func(ls []string) {
			_ = os.WriteFile(labelsPath, []byte(strings.Join(ls, "\n")+"\n"), 0o644)
		}
		switch {
		case args[0] == "show":
			quoted := make([]string, len(labels))
			for i, l := range labels {
				quoted[i] = `"` + l + `"`
			}
			return `[{"id":"` + args[1] + `","labels":[` + strings.Join(quoted, ",") + `]}]`, "", 0
		case len(args) >= 4 && args[0] == "label" && args[1] == "add":
			if !containsDeliveryTestLabel(labels, args[3]) {
				write(append(labels, args[3]))
			}
			return "", "", 0
		case len(args) >= 4 && args[0] == "label" && args[1] == "remove":
			var kept []string
			for _, l := range labels {
				if l != args[3] {
					kept = append(kept, l)
				}
			}
			if len(kept) == len(labels) {
				return "", "does not have label", 1
			}
			write(kept)
			return "", "", 0
		}
		return "", "unsupported bd args: " + strings.Join(args, " "), 1
	}}

	if err := acknowledgeDeliveryBead(bd.run, tmp, "", "msg-1", "gastown/worker"); err != nil {
		t.Fatalf("AcknowledgeDeliveryBead: %v", err)
	}

	data, err := os.ReadFile(labelsPath)
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	labels := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, label := range labels {
		if label == DeliveryLabelPending {
			t.Fatalf("%s should have been removed after ack convergence; labels=%v", DeliveryLabelPending, labels)
		}
	}
	if !containsDeliveryTestLabel(labels, DeliveryLabelAcked) {
		t.Fatalf("%s should remain; labels=%v", DeliveryLabelAcked, labels)
	}

	if log := strings.Join(bd.argvs(), "\n"); !strings.Contains(log, "label remove msg-1 "+DeliveryLabelPending) {
		t.Fatalf("expected pending label removal command, calls:\n%s", log)
	}
}

func containsDeliveryTestLabel(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
