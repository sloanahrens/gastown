package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/attention"
)

// tipsFileName is the direct-push collector's own state, written under the
// attention directory beside state.json (gt-vsct7.3). It is the daemon's, not
// the queue's: gt attention does not read it.
const tipsFileName = "tips.json"

// tipsPath is <town>/.runtime/attention/tips.json.
func tipsPath(townRoot string) string {
	return filepath.Join(attention.Dir(townRoot), tipsFileName)
}

// directPushTip is one landing rig's origin tip as the direct-push collector
// has observed it. Tip and Pending are what make the check "one tick later":
// a tip that moved is judged on the next tick, when the landing worker's
// record has had time to appear.
type directPushTip struct {
	Rig string `json:"rig"`
	// Tip is the last tip this collector read. It is empty before the first
	// read, which is a baseline: the tip that is already on origin when the
	// daemon starts is not a move.
	Tip string `json:"tip,omitempty"`
	// Pending is a tip that moved on an earlier tick and has not had its
	// grace tick yet.
	Pending string `json:"pending,omitempty"`
	// PendingSince is when Pending was first read. It becomes the push's
	// first_seen, so the hold is measured from the tip appearing rather than
	// from the tick that judged it.
	PendingSince time.Time `json:"pending_since,omitempty"`
	// Pushes are the tips judged direct pushes and still inside the hold.
	Pushes []directPush `json:"pushes,omitempty"`
}

// directPush is one raised direct-push item's persisted evidence, so a daemon
// restart does not drop an unacked push.
type directPush struct {
	SHA string `json:"sha"`
	// FirstSeen is when the tip was first observed on origin.
	FirstSeen time.Time `json:"first_seen"`
	// Author and Subject describe the commit, when the rig repo can resolve
	// it. Empty for a tip the local repo has never seen.
	Author  string `json:"author,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// directPushTips is tips.json: one entry per landing rig.
type directPushTips struct {
	Updated time.Time       `json:"updated"`
	Rigs    []directPushTip `json:"rigs"`
}

// rig returns the entry for rig, creating it in place when the rig is new.
func (t *directPushTips) rig(rig string) *directPushTip {
	for i := range t.Rigs {
		if t.Rigs[i].Rig == rig {
			return &t.Rigs[i]
		}
	}
	t.Rigs = append(t.Rigs, directPushTip{Rig: rig})
	return &t.Rigs[len(t.Rigs)-1]
}

// addPush records a judged direct push. A sha already recorded keeps its
// original first_seen: the hold runs from the first sighting, and a tip that
// moved away and came back must not restart it.
func (t *directPushTip) addPush(p directPush, now time.Time) {
	if p.FirstSeen.IsZero() {
		p.FirstSeen = now
	}
	for i := range t.Pushes {
		if t.Pushes[i].SHA == p.SHA {
			return
		}
	}
	t.Pushes = append(t.Pushes, p)
}

// expire drops the pushes older than attentionDirectPushHold.
func (t *directPushTip) expire(now time.Time) {
	kept := make([]directPush, 0, len(t.Pushes))
	for _, p := range t.Pushes {
		if now.Sub(p.FirstSeen) < attentionDirectPushHold {
			kept = append(kept, p)
		}
	}
	t.Pushes = kept
}

// summary is the item's one line: the tip and, when the repo can resolve it,
// who authored the commit that reached main outside the landing worker.
func (p directPush) summary() string {
	line := "main -> " + sha12(p.SHA) + " with no landing record"
	if p.Author != "" || p.Subject != "" {
		line += ": " + p.Author + ": " + p.Subject
	}
	return line
}

// sha12 is a commit's twelve-character short form, the width the direct-push
// item's key uses. The daemon has an eight-character shortSHA for logs; the key
// keeps more so two landings a second apart cannot collide.
func sha12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// readDirectPushTips loads tips.json. A missing file reads as no tips with no
// error, so a town whose daemon has never ticked is not a failure.
func readDirectPushTips(townRoot string) (directPushTips, error) {
	var t directPushTips
	data, err := os.ReadFile(tipsPath(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return directPushTips{}, fmt.Errorf("parse %s: %w", tipsFileName, err)
	}
	return t, nil
}

// writeDirectPushTips replaces tips.json, stamped with the tick that wrote it.
func writeDirectPushTips(townRoot string, t directPushTips, now time.Time) error {
	t.Updated = now
	if t.Rigs == nil {
		t.Rigs = []directPushTip{}
	}
	return atomicfile.EnsureDirAndWriteJSON(tipsPath(townRoot), t)
}

// collectDirectPush raises one item per landing rig whose landing target tip
// reached the branch with no landing record naming it (gt-vsct7.3): work that
// got to main outside the landing worker. It is the daemon's copy of the
// queue-watch DIRECT-PUSH check, and it is the only collector that writes the
// state it reads: the tip it sees this tick is judged on the next one, so the
// observation has to survive the tick (and a daemon restart) to be judged at
// all.
//
// The read is one git ls-remote per landing rig. It is never a fetch: fetching
// the rig repo behind the landing worker would race its own fetch, and
// <town>/gastown/mayor/rig is what install-gt builds from post-merge, so
// fetching there races the build.
func (s *attentionSources) collectDirectPush(ctx context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for _, rig := range s.landingRigs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tip, err := s.remoteTip(rig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rig, err)
		}
		t := s.tips.rig(rig)

		// The tip seen before this one is judged now: a landing that wrote its
		// record in between is the landing worker's, not a direct push.
		if t.Pending != "" {
			landed, err := s.landedCommit(rig, t.Pending)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rig, err)
			}
			if !landed {
				author, subject := s.commitInfo(rig, t.Pending)
				t.addPush(directPush{SHA: t.Pending, FirstSeen: t.PendingSince, Author: author, Subject: subject}, s.now)
			}
			t.Pending, t.PendingSince = "", time.Time{}
		}

		switch {
		case tip == "":
			// The remote answered without a tip: nothing to compare against.
		case t.Tip == "":
			t.Tip = tip
		case tip != t.Tip:
			t.Tip, t.Pending, t.PendingSince = tip, tip, s.now
		}

		t.expire(s.now)
		for _, p := range t.Pushes {
			out = append(out, attention.Item{
				Key:      "direct-push:" + rig + ":" + sha12(p.SHA),
				Kind:     attention.KindDirectPush,
				Severity: attention.SeverityHigh,
				Rig:      rig,
				SHA:      p.SHA,
				Summary:  p.summary(),
			})
		}
	}
	return out, nil
}
