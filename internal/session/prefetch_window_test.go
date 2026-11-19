package session_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// Both read-ahead settings are configuration values now: a session built from a
// config must hand them to the budget and to every coordinator it creates. The
// interface checks would otherwise stay green with the two values swapped.
func TestPrefetchSettingsComeFromConfiguration(t *testing.T) {
	cfg := testConfig()
	cfg.Filesystem.Prefetch.WindowBytes = config.ByteQuantity(8 << 20)
	sess, err := session.New(cfg, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if got, want := session.PrefetchWindowForTest(sess), int64(8<<20); got != want {
		t.Fatalf("prefetch window = %d, want %d", got, want)
	}
	if got, want := session.PrefetchBudgetLimitForTest(sess), int64(cfg.Filesystem.Prefetch.BudgetBytes); got != want {
		t.Fatalf("prefetch budget limit = %d, want %d", got, want)
	}
}
