package singbox

import (
	"context"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

func trackerForAccountingTest(uid int, up, down int64) *ConnTracker {
	ct := NewConnTracker(0)
	ct.users[uid] = &userStats{ips: make(map[string]int)}
	ct.users[uid].upload.Store(up)
	ct.users[uid].download.Store(down)
	return ct
}

func TestTrafficAccountingSurvivesRetiredTrackerArchive(t *testing.T) {
	s := New(config.KernelConfig{})
	active := trackerForAccountingTest(7, 10, 20)
	retired := trackerForAccountingTest(7, 100, 200)
	s.connTracker = active
	s.retiredTrackers = []*ConnTracker{retired}

	before, _, _, err := s.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before[7] != ([2]int64{110, 220}) {
		t.Fatalf("pre-archive totals: %v", before)
	}

	s.archiveRetiredTracker(retired)
	after, _, _, err := s.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after[7] != before[7] {
		t.Fatalf("archiving changed cumulative traffic: before %v after %v", before, after)
	}
	if len(s.retiredTrackers) != 0 {
		t.Fatalf("retired tracker should be released after archive")
	}

	// The new kernel continues accounting without resetting the panel's
	// cumulative baseline.
	active.users[7].upload.Add(17)
	active.users[7].download.Add(23)
	later, _, _, err := s.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if later[7] != ([2]int64{127, 243}) {
		t.Fatalf("traffic after archive should remain monotonic: %v", later)
	}
}
