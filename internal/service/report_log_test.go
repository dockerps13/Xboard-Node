package service

import "testing"

func TestReportTrafficTotals(t *testing.T) {
	tests := []struct {
		name     string
		traffic  map[int][2]int64
		up, down uint64
	}{
		{"empty", nil, 0, 0},
		{"multiple users", map[int][2]int64{1: {100, 200}, 2: {30, 40}}, 130, 240},
		{"negative counters ignored", map[int][2]int64{1: {-1, 200}, 2: {100, -1}}, 100, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up, down := reportTrafficTotals(tt.traffic)
			if up != tt.up || down != tt.down {
				t.Fatalf("got (%d, %d), want (%d, %d)", up, down, tt.up, tt.down)
			}
		})
	}
}
