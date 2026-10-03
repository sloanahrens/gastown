package beads_test

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// These tests run the channel, group and queue helpers over beadsfake, the
// way any Client runs them now that they are free functions (gt-7iwy0.4.4).
// The helpers are exported from package beads, so the tests live in the
// external test package to import the fake without an import cycle.

func TestChannelHelpersOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	issue, err := beads.CreateChannelBead(c, "alerts", []string{"gastown/witness"}, "mayor")
	if err != nil {
		t.Fatalf("CreateChannelBead: %v", err)
	}
	if issue.ID != "hq-channel-alerts" {
		t.Errorf("ID = %q, want hq-channel-alerts", issue.ID)
	}
	if !beads.HasLabel(issue, "gt:channel") {
		t.Errorf("created channel is missing the gt:channel label: %v", issue.Labels)
	}

	_, fields, err := beads.GetChannelBead(c, "alerts")
	if err != nil {
		t.Fatalf("GetChannelBead: %v", err)
	}
	if fields == nil || fields.Name != "alerts" {
		t.Fatalf("fields = %+v, want the alerts channel", fields)
	}
	if len(fields.Subscribers) != 1 || fields.Subscribers[0] != "gastown/witness" {
		t.Errorf("Subscribers = %v, want [gastown/witness]", fields.Subscribers)
	}
	if fields.Status != beads.ChannelStatusActive {
		t.Errorf("Status = %q, want active", fields.Status)
	}

	// A missing channel is nil, nil, nil, not an error.
	missing, missingFields, err := beads.GetChannelBead(c, "nope")
	if err != nil || missing != nil || missingFields != nil {
		t.Errorf("GetChannelBead(nope) = %v, %v, %v; want nil, nil, nil", missing, missingFields, err)
	}

	if _, byID, err := beads.GetChannelByID(c, "hq-channel-alerts"); err != nil || byID == nil {
		t.Errorf("GetChannelByID = %v, %v; want the channel", byID, err)
	}

	// Subscribe is idempotent, unsubscribe is not.
	if err := beads.SubscribeToChannel(c, "alerts", "gastown/witness"); err != nil {
		t.Fatalf("SubscribeToChannel (dup): %v", err)
	}
	if err := beads.SubscribeToChannel(c, "alerts", "deacon/"); err != nil {
		t.Fatalf("SubscribeToChannel: %v", err)
	}
	_, fields, _ = beads.GetChannelBead(c, "alerts")
	if len(fields.Subscribers) != 2 {
		t.Fatalf("Subscribers = %v, want 2 entries", fields.Subscribers)
	}
	if err := beads.UnsubscribeFromChannel(c, "alerts", "gastown/witness"); err != nil {
		t.Fatalf("UnsubscribeFromChannel: %v", err)
	}
	_, fields, _ = beads.GetChannelBead(c, "alerts")
	if len(fields.Subscribers) != 1 || fields.Subscribers[0] != "deacon/" {
		t.Errorf("Subscribers = %v, want [deacon/]", fields.Subscribers)
	}

	if err := beads.UpdateChannelRetention(c, "alerts", 10, 24); err != nil {
		t.Fatalf("UpdateChannelRetention: %v", err)
	}
	_, fields, _ = beads.GetChannelBead(c, "alerts")
	if fields.RetentionCount != 10 || fields.RetentionHours != 24 {
		t.Errorf("retention = %d/%d, want 10/24", fields.RetentionCount, fields.RetentionHours)
	}

	if err := beads.UpdateChannelStatus(c, "alerts", beads.ChannelStatusClosed); err != nil {
		t.Fatalf("UpdateChannelStatus: %v", err)
	}
	if err := beads.UpdateChannelStatus(c, "alerts", "bogus"); err == nil {
		t.Error("UpdateChannelStatus(bogus) = nil, want an error")
	}

	channels, err := beads.ListChannelBeads(c)
	if err != nil {
		t.Fatalf("ListChannelBeads: %v", err)
	}
	if _, ok := channels["alerts"]; !ok {
		t.Errorf("ListChannelBeads = %v, want the alerts channel", channels)
	}

	lookup, _, err := beads.LookupChannelByName(c, "alerts")
	if err != nil || lookup == nil {
		t.Fatalf("LookupChannelByName = %v, %v; want the channel", lookup, err)
	}
	if lookup, _, err := beads.LookupChannelByName(c, "absent"); err != nil || lookup != nil {
		t.Errorf("LookupChannelByName(absent) = %v, %v; want nil, nil", lookup, err)
	}

	if err := beads.DeleteChannelBead(c, "alerts"); err != nil {
		t.Fatalf("DeleteChannelBead: %v", err)
	}
	if gone, _, err := beads.GetChannelBead(c, "alerts"); err != nil || gone != nil {
		t.Errorf("GetChannelBead after delete = %v, %v; want nil, nil", gone, err)
	}
}

// TestChannelRetentionOverClient pins the count-based pruning: the oldest
// messages go, the newest RetentionCount stay.
func TestChannelRetentionOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	if _, err := beads.CreateChannelBead(c, "alerts", nil, "mayor"); err != nil {
		t.Fatalf("CreateChannelBead: %v", err)
	}
	if err := beads.UpdateChannelRetention(c, "alerts", 1, 0); err != nil {
		t.Fatalf("UpdateChannelRetention: %v", err)
	}

	var ids []string
	for i := 0; i < 3; i++ {
		msg, err := c.Create(beads.CreateOptions{
			Title:  "message",
			Labels: []string{"gt:message", "channel:alerts"},
		})
		if err != nil {
			t.Fatalf("creating message %d: %v", i, err)
		}
		ids = append(ids, msg.ID)
	}

	if err := beads.EnforceChannelRetention(c, "alerts"); err != nil {
		t.Fatalf("EnforceChannelRetention: %v", err)
	}
	for i, id := range ids {
		issue, err := c.Show(id)
		if err != nil {
			t.Fatalf("Show(%s): %v", id, err)
		}
		closed := issue.Status == "closed"
		if want := i < 2; closed != want {
			t.Errorf("message %d (%s) closed = %v, want %v", i, id, closed, want)
		}
	}

	if _, err := beads.PruneAllChannels(c); err != nil {
		t.Fatalf("PruneAllChannels: %v", err)
	}
}

func TestGroupHelpersOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	issue, err := beads.CreateGroupBead(c, "ops-team", &beads.GroupFields{
		Members:   []string{"gastown/witness"},
		CreatedBy: "mayor",
	})
	if err != nil {
		t.Fatalf("CreateGroupBead: %v", err)
	}
	if issue.ID != "hq-group-ops-team" {
		t.Errorf("ID = %q, want hq-group-ops-team", issue.ID)
	}
	if !beads.HasLabel(issue, "gt:group") {
		t.Errorf("created group is missing the gt:group label: %v", issue.Labels)
	}

	if _, err := beads.CreateGroupBead(c, "Bad Name", nil); err == nil {
		t.Error("CreateGroupBead(Bad Name) = nil, want a validation error")
	}

	_, fields, err := beads.GetGroupByName(c, "ops-team")
	if err != nil {
		t.Fatalf("GetGroupByName: %v", err)
	}
	if fields == nil || len(fields.Members) != 1 {
		t.Fatalf("fields = %+v, want one member", fields)
	}
	if _, _, err := beads.GetGroupByName(c, "absent"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("GetGroupByName(absent) err = %v, want ErrNotFound", err)
	}
	if _, byID, err := beads.GetGroupByID(c, "hq-group-ops-team"); err != nil || byID == nil {
		t.Errorf("GetGroupByID = %v, %v; want the group", byID, err)
	}

	if _, err := beads.AddGroupMember(c, "ops-team", "gastown/witness"); err != nil {
		t.Fatalf("AddGroupMember (dup): %v", err)
	}
	updated, err := beads.AddGroupMember(c, "ops-team", "deacon/")
	if err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
	if len(updated.Description) == 0 {
		t.Fatal("AddGroupMember returned an issue with no description")
	}
	_, fields, _ = beads.GetGroupByName(c, "ops-team")
	if len(fields.Members) != 2 {
		t.Fatalf("Members = %v, want 2 entries", fields.Members)
	}

	updated, err = beads.RemoveGroupMember(c, "ops-team", "gastown/witness")
	if err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}
	if updated.ID != "hq-group-ops-team" {
		t.Errorf("RemoveGroupMember ID = %q, want hq-group-ops-team", updated.ID)
	}
	_, fields, _ = beads.GetGroupByName(c, "ops-team")
	if len(fields.Members) != 1 || fields.Members[0] != "deacon/" {
		t.Errorf("Members = %v, want [deacon/]", fields.Members)
	}

	if _, err := beads.UpdateGroupMembers(c, "ops-team", []string{"mayor/"}); err != nil {
		t.Fatalf("UpdateGroupMembers: %v", err)
	}
	if _, err := beads.AddGroupMember(c, "absent", "x"); err == nil {
		t.Error("AddGroupMember(absent) = nil, want an error")
	}

	groups, err := beads.ListGroupBeads(c)
	if err != nil {
		t.Fatalf("ListGroupBeads: %v", err)
	}
	if _, ok := groups["ops-team"]; !ok {
		t.Errorf("ListGroupBeads = %v, want ops-team", groups)
	}

	lookup, _, err := beads.LookupGroupByName(c, "ops-team")
	if err != nil || lookup == nil {
		t.Fatalf("LookupGroupByName = %v, %v; want the group", lookup, err)
	}
	if _, _, err := beads.LookupGroupByName(c, "absent"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("LookupGroupByName(absent) err = %v, want ErrNotFound", err)
	}

	if err := beads.DeleteGroupBead(c, "ops-team"); err != nil {
		t.Fatalf("DeleteGroupBead: %v", err)
	}
	if _, _, err := beads.GetGroupByName(c, "ops-team"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("GetGroupByName after delete err = %v, want ErrNotFound", err)
	}
}

func TestQueueHelpersOverClient(t *testing.T) {
	t.Parallel()
	c := beadsfake.New()

	_, err := beads.CreateQueueBead(c, "hq-q-work", "Queue: work", &beads.QueueFields{
		Name:         "work",
		ClaimPattern: "gastown/polecats/*",
		Status:       beads.QueueStatusActive,
		CreatedBy:    "mayor",
	})
	if err != nil {
		t.Fatalf("CreateQueueBead: %v", err)
	}
	if _, err := beads.CreateQueueBead(c, "hq-q-flag", "--help", nil); !errors.Is(err, beads.ErrFlagTitle) {
		t.Errorf("CreateQueueBead(--help) err = %v, want ErrFlagTitle", err)
	}

	issue, fields, err := beads.GetQueueBead(c, "hq-q-work")
	if err != nil {
		t.Fatalf("GetQueueBead: %v", err)
	}
	if issue == nil || fields == nil || fields.Name != "work" {
		t.Fatalf("GetQueueBead = %v, %+v; want the work queue", issue, fields)
	}
	if missing, _, err := beads.GetQueueBead(c, "hq-q-absent"); err != nil || missing != nil {
		t.Errorf("GetQueueBead(absent) = %v, %v; want nil, nil", missing, err)
	}

	if err := beads.UpdateQueueCounts(c, "hq-q-work", 3, 1, 5, 0); err != nil {
		t.Fatalf("UpdateQueueCounts: %v", err)
	}
	_, fields, _ = beads.GetQueueBead(c, "hq-q-work")
	if fields.AvailableCount != 3 || fields.ProcessingCount != 1 || fields.CompletedCount != 5 {
		t.Errorf("counts = %d/%d/%d, want 3/1/5",
			fields.AvailableCount, fields.ProcessingCount, fields.CompletedCount)
	}

	if err := beads.UpdateQueueStatus(c, "hq-q-work", beads.QueueStatusPaused); err != nil {
		t.Fatalf("UpdateQueueStatus: %v", err)
	}
	if err := beads.UpdateQueueStatus(c, "hq-q-work", "bogus"); err == nil {
		t.Error("UpdateQueueStatus(bogus) = nil, want an error")
	}
	if err := beads.UpdateQueueStatus(c, "hq-q-absent", beads.QueueStatusActive); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("UpdateQueueStatus(absent) err = %v, want ErrNotFound", err)
	}

	queues, err := beads.ListQueueBeads(c)
	if err != nil {
		t.Fatalf("ListQueueBeads: %v", err)
	}
	if _, ok := queues["hq-q-work"]; !ok {
		t.Errorf("ListQueueBeads = %v, want hq-q-work", queues)
	}

	lookup, _, err := beads.LookupQueueByName(c, "work")
	if err != nil || lookup == nil {
		t.Fatalf("LookupQueueByName = %v, %v; want the queue", lookup, err)
	}
	if lookup, _, err := beads.LookupQueueByName(c, "absent"); err != nil || lookup != nil {
		t.Errorf("LookupQueueByName(absent) = %v, %v; want nil, nil", lookup, err)
	}

	// The paused queue is not eligible; reactivating it makes it so.
	if issues, _, err := beads.FindEligibleQueues(c, "gastown/polecats/lapis"); err != nil || len(issues) != 0 {
		t.Fatalf("FindEligibleQueues (paused) = %v, %v; want none", issues, err)
	}
	if err := beads.UpdateQueueStatus(c, "hq-q-work", beads.QueueStatusActive); err != nil {
		t.Fatalf("UpdateQueueStatus: %v", err)
	}
	issues, eligibleFields, err := beads.FindEligibleQueues(c, "gastown/polecats/lapis")
	if err != nil {
		t.Fatalf("FindEligibleQueues: %v", err)
	}
	if len(issues) != 1 || len(eligibleFields) != 1 || eligibleFields[0].Name != "work" {
		t.Errorf("FindEligibleQueues = %v / %v, want the work queue", issues, eligibleFields)
	}
	if issues, _, err := beads.FindEligibleQueues(c, "gastown/crew/max"); err != nil || len(issues) != 0 {
		t.Errorf("FindEligibleQueues(crew) = %v, %v; want none", issues, err)
	}

	if err := beads.DeleteQueueBead(c, "hq-q-work"); err != nil {
		t.Fatalf("DeleteQueueBead: %v", err)
	}
	if missing, _, err := beads.GetQueueBead(c, "hq-q-work"); err != nil || missing != nil {
		t.Errorf("GetQueueBead after delete = %v, %v; want nil, nil", missing, err)
	}
}
