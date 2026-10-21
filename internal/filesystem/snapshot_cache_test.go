package filesystem

import "testing"

func TestSnapshotCacheListingFencesRecreatedPlaceholderPublication(t *testing.T) {
	var cache snapshotCache[int]
	hash := hashN(1)
	first := cache.observe(hash)
	firstValue := 1
	cache.mu.Lock()
	prefilled := cache.publishLocked(hash, first, &firstValue)
	cache.mu.Unlock()
	if !prefilled {
		t.Fatal("initial cache value was not published")
	}
	listing := cache.observeListing()
	cache.forget(hash, cache.observe(hash))
	recreated := cache.observe(hash)
	if recreated.slot == first.slot || recreated.value != nil {
		t.Fatal("miss and re-add did not create a new empty slot")
	}

	cache.finishListing(listing, nil)
	lateValue := 2
	cache.mu.Lock()
	retained := len(cache.slots)
	publishedLate := cache.publishLocked(hash, recreated, &lateValue)
	afterPublish, pending := len(cache.slots), len(cache.listings)
	cache.mu.Unlock()
	if retained != 0 || publishedLate || afterPublish != 0 || pending != 0 {
		t.Fatalf("empty listing did not fence the recreated placeholder: retained=%d, publishedLate=%v, after-publish=%d, pending=%d", retained, publishedLate, afterPublish, pending)
	}

	readded := cache.observe(hash)
	readdedValue := 3
	cache.mu.Lock()
	published := cache.publishLocked(hash, readded, &readdedValue)
	publishedLate = cache.publishLocked(hash, recreated, &lateValue)
	current, retained := cache.currentLocked(hash), len(cache.slots)
	cache.mu.Unlock()
	if !published || publishedLate || current != &readdedValue || retained != 1 {
		t.Fatalf("legitimate re-add was fenced or overwritten: published=%v, publishedLate=%v, current=%v, slots=%d", published, publishedLate, current, retained)
	}
}
