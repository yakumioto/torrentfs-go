package session_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// The window is a configuration value now: a session built from a config must
// hand its window to every coordinator it creates, and keep the compile-time
// default when the key is absent.
func TestPrefetchWindowComesFromConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name  string
		bytes int64
		want  int64
	}{
		{name: "configured", bytes: 8 << 20, want: 8 << 20},
		{name: "zero falls back", bytes: 0, want: 32 << 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Filesystem.Prefetch.WindowBytes = config.ByteQuantity(tt.bytes)
			sess, err := session.New(cfg, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				if err := sess.Close(context.Background()); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			if got := session.PrefetchWindowForTest(sess); got != tt.want {
				t.Fatalf("prefetch window = %d, want %d", got, tt.want)
			}
		})
	}
}
