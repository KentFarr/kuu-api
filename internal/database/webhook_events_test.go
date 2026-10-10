package database

import (
	"sync"
	"testing"
)

// Stripe can deliver the same event to two server instances at once. The
// claim must let exactly one of them through.
func TestClaimWebhookEventConcurrentClaimsOneWinner(t *testing.T) {
	db := newTestDB(t)

	const workers = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := db.ClaimWebhookEvent("evt_race")
			if err != nil {
				t.Errorf("ClaimWebhookEvent: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d of %d concurrent claims succeeded, want exactly 1", wins, workers)
	}

	// After a release, Stripe's retry must be able to claim it again.
	if err := db.ReleaseWebhookEvent("evt_race"); err != nil {
		t.Fatalf("ReleaseWebhookEvent: %v", err)
	}
	ok, err := db.ClaimWebhookEvent("evt_race")
	if err != nil || !ok {
		t.Fatalf("claim after release = %v, %v; want true, nil", ok, err)
	}
}
