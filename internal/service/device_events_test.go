package service

import "testing"

func TestDeviceChangeSignalCoalescesWithoutBlocking(t *testing.T) {
	s := &Service{deviceChanges: make(chan struct{}, 1)}
	for i := 0; i < 10000; i++ {
		s.signalDeviceChange()
	}
	if got := len(s.deviceChanges); got != 1 {
		t.Fatalf("device change notifications queued = %d, want 1", got)
	}
	<-s.deviceChanges
	s.signalDeviceChange()
	if got := len(s.deviceChanges); got != 1 {
		t.Fatalf("device change notifications after drain = %d, want 1", got)
	}
}
