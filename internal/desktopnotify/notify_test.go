package desktopnotify

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
)

func TestSkipsHeadlessAndStaleAlertsAndContinuesAfterFailedDelivery(t *testing.T) {
	var uid atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var delivered []string
	n := newNotifier(func() int { return int(uid.Load()) }, func(_ context.Context, _ int, body string) {
		if body == "failed" {
			close(started)
			<-release
			// Simulate a delivery attempt with no resulting alert.
			return
		}
		delivered = append(delivered, body)
	})
	n.Notify("headless")
	uid.Store(501)
	n.Notify("failed")
	<-started
	n.Notify("stale")
	uid.Store(502)
	n.Notify("current")
	close(release)
	n.Close()
	if want := []string{"current"}; !slices.Equal(delivered, want) {
		t.Fatalf("delivered %q, want %q", delivered, want)
	}
}
