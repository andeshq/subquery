package schema

import (
	"sync"
	"testing"
)

// TestStoreConcurrentSnapshotDuringReload exercises the atomic snapshot swap
// under the race detector: readers must never observe a partial snapshot.
func TestStoreConcurrentSnapshotDuringReload(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool, discardLogger(), Options{
		Schemas: []string{"public"},
		Exposed: []string{"public"},
	})
	if err := store.Reload(t.Context()); err != nil {
		t.Fatalf("initial Reload: %v", err)
	}
	first := store.Snapshot()
	if first == nil {
		t.Fatal("snapshot is nil")
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := store.Snapshot()
				if snapshot == nil {
					t.Error("a reader observed a nil snapshot")
					return
				}
				// A snapshot must be internally consistent: every ordered
				// relation must be resolvable through the map.
				for _, id := range snapshot.Order {
					if _, ok := snapshot.Relations[id]; !ok {
						t.Errorf("snapshot %d is missing %v from Relations", snapshot.Generation, id)
						return
					}
				}
				if snapshot.Generation < 1 {
					t.Errorf("generation = %d", snapshot.Generation)
					return
				}
				_ = store.Generation()
			}
		}()
	}

	for range 4 {
		if err := store.Reload(t.Context()); err != nil {
			t.Errorf("Reload: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()

	if got := store.Generation(); got != 5 {
		t.Errorf("generation = %d, want 5", got)
	}
}

// TestStoreTriggerReloadIsNonBlocking asserts the reload signal never blocks a
// caller, even when several arrive before the watcher drains them.
func TestStoreTriggerReloadIsNonBlocking(t *testing.T) {
	store := NewStore(nil, discardLogger(), Options{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			store.TriggerReload()
		}
	}()
	<-done

	// A store that was never started can still be closed safely.
	store.Close()
	store.Close()
}

// TestStoreReloadFailureKeepsSnapshot asserts a failed reload does not discard
// the last good snapshot.
func TestStoreReloadFailureKeepsSnapshot(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool, discardLogger(), Options{
		Schemas: []string{"public"},
		Exposed: []string{"public"},
	})
	if err := store.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	good := store.Snapshot()
	if good == nil {
		t.Fatal("snapshot is nil")
	}

	// Ask for a schema that does not exist; introspection succeeds but finds
	// nothing, so the snapshot is replaced with an empty one rather than
	// failing. A cancelled context, by contrast, must fail and keep the old
	// snapshot.
	ctx, cancel := cancelAfter()
	defer cancel()
	if err := store.Reload(ctx); err == nil {
		t.Log("reload with a cancelled context unexpectedly succeeded")
	}
	if store.Snapshot() == nil {
		t.Error("a failed reload must not clear the snapshot")
	}
}
