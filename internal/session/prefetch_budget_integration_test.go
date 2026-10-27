package session_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestPrefetchAdmittedPrefixSchedulesAndKeepsPinsAfterCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := testConfig()
	cfg.Cache.CapacityBytes = 8 << 20
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
	hash, content := seedPrefetchTorrent(t, sess, "prefix.bin", 64, 0)
	opened, st := openPrefetchFile(t, sess, hash, "prefix.bin")
	defer opened.close()
	opened.read(t, 0, 64)
	initial := waitPrefetchState(t, ctx, st, "admitted prefix to claim background slots", func(s session.PrefetchSnapshot) bool {
		return s.ActivePieces == defaultTestPrefetchPieces() && s.EffectiveHigh == 2<<20-64
	})
	if initial.PinnedPieces != 8 || initial.EffectiveHigh != 2<<20-64 || initial.EffectiveLow != (2<<20-64)/2 {
		t.Fatalf("admitted prefix: %+v", initial)
	}
	for _, index := range initial.ActivePieceIndexes {
		if index < 1 || index >= initial.PinnedPieces {
			t.Fatalf("unreserved tail scheduled: %+v", initial)
		}
		if err := sess.SeedPieceForTest(hash, index, content); err != nil {
			t.Fatal(err)
		}
	}
	refill := waitPrefetchState(t, ctx, st, "completed slots to refill the rest of the admitted prefix", func(s session.PrefetchSnapshot) bool {
		return s.PriorityCancels >= uint64(len(initial.ActivePieceIndexes)) && s.ActivePieces == 3
	})
	for _, index := range refill.ActivePieceIndexes {
		if index < 5 || index >= 8 {
			t.Fatalf("refill escaped admitted prefix: %+v", refill)
		}
		if err := sess.SeedPieceForTest(hash, index, content); err != nil {
			t.Fatal(err)
		}
	}
	complete := waitPrefetchState(t, ctx, st, "completed prefix to return slots but retain its window pins", func(s session.PrefetchSnapshot) bool {
		return s.State == "paused" && s.ActivePieces == 0
	})
	if complete.PinnedPieces != 8 || complete.MaxActivePieces > 4 {
		t.Fatalf("completion accounting: %+v", complete)
	}
	if limit, used := session.PrefetchBudgetUsageForTest(sess); limit != 4 || used != 0 {
		t.Fatalf("completed slots: %d/%d", used, limit)
	}
	opened.close()
	waitPrefetchState(t, ctx, st, "close to release admitted pins", func(s session.PrefetchSnapshot) bool {
		return s.PinnedPieces == 0 && s.ActivePieces == 0
	})
}

func TestPrefetchZeroAdmissionDoesNotScheduleUnreservedTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := testConfig()
	cfg.Cache.CapacityBytes = 8 << 20
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
	firstHash, _ := seedPrefetchTorrent(t, sess, "first-prefix.bin", 64, 0)
	secondHash, _ := seedPrefetchTorrent(t, sess, "second-prefix.bin", 64, 0)
	first, firstTorrent := openPrefetchFile(t, sess, firstHash, "first-prefix.bin")
	defer first.close()
	second, secondTorrent := openPrefetchFile(t, sess, secondHash, "second-prefix.bin")
	defer second.close()
	first.read(t, 0, 64)
	waitPrefetchState(t, ctx, firstTorrent, "first prefix to fill the pin budget", func(s session.PrefetchSnapshot) bool {
		return s.PinnedPieces == 8
	})
	second.read(t, 0, 64)
	blocked := waitPrefetchState(t, ctx, secondTorrent, "empty admitted prefix to block scheduling", func(s session.PrefetchSnapshot) bool {
		return s.BudgetBlocks > 0 && s.EffectiveHigh == 0
	})
	if blocked.ActivePieces != 0 || blocked.PinnedPieces != 0 {
		t.Fatalf("zero admission acquired work: %+v", blocked)
	}
	first.close()
	second.read(t, 0, 64)
	waitPrefetchState(t, ctx, secondTorrent, "released budget to admit the next torrent", func(s session.PrefetchSnapshot) bool {
		return s.ActivePieces > 0 && s.PinnedPieces == 8
	})
}
