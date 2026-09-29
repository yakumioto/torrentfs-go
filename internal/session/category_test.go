package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestCategoryCreateAssignAndRestart(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	bytes, hash := buildSingleFileTorrentBytes(t, "Movie1.mp4", []byte("movie"), nil)

	sess, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	view, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: bytes})
	if err != nil {
		t.Fatalf("add torrent: %v", err)
	}
	if view.Category != "" {
		t.Fatalf("new torrent category = %q, want empty", view.Category)
	}
	created, err := sess.CreateCategory(ctx, "movies")
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	if created.Name != "movies" || created.CreatedAt.IsZero() {
		t.Fatalf("created category = %+v", created)
	}
	assigned, err := sess.SetTorrentCategory(ctx, hash.HexString(), "movies")
	if err != nil {
		t.Fatalf("assign category: %v", err)
	}
	if assigned.Category != "movies" {
		t.Fatalf("assigned category = %q, want movies", assigned.Category)
	}
	if got := sess.ListCategories(); len(got) != 1 || got[0].Name != "movies" {
		t.Fatalf("categories = %+v, want movies", got)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("close first session: %v", err)
	}

	restarted, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("restart session: %v", err)
	}
	defer func() {
		if err := restarted.Close(ctx); err != nil {
			t.Errorf("close restarted session: %v", err)
		}
	}()
	if got := restarted.ListCategories(); len(got) != 1 || got[0].Name != "movies" {
		t.Fatalf("restarted categories = %+v, want movies", got)
	}
	got, err := restarted.TorrentViewFor(hash.HexString())
	if err != nil {
		t.Fatalf("restarted torrent view: %v", err)
	}
	if got.Category != "movies" {
		t.Fatalf("restarted category = %q, want movies", got.Category)
	}
	state, err := os.ReadFile(filepath.Join(torrentsDir, ".metadata", "state", hash.HexString()+".json"))
	if err != nil {
		t.Fatalf("read registry sidecar: %v", err)
	}
	if string(state) == "" {
		t.Fatal("registry sidecar is empty")
	}
}

func TestCategoryValidationAndAssignmentErrors(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	bytes, hash := buildSingleFileTorrentBytes(t, "Movie1.mp4", []byte("movie"), nil)
	sess := newManageSession(t, torrentsDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: bytes}); err != nil {
		t.Fatalf("add torrent: %v", err)
	}
	for _, name := range []string{"", " ", " movies", "movies ", ".", "..", "a/b", "a\\b", "bad\x00name"} {
		if _, err := sess.CreateCategory(ctx, name); !errors.Is(err, session.ErrInvalidCategory) {
			t.Errorf("CreateCategory(%q) = %v, want ErrInvalidCategory", name, err)
		}
	}
	if _, err := sess.CreateCategory(ctx, "movies"); err != nil {
		t.Fatalf("create valid category: %v", err)
	}
	if _, err := sess.CreateCategory(ctx, "movies"); !errors.Is(err, session.ErrCategoryExists) {
		t.Fatalf("duplicate category = %v, want ErrCategoryExists", err)
	}
	if _, err := sess.SetTorrentCategory(ctx, hash.HexString(), "missing"); !errors.Is(err, session.ErrUnknownCategory) {
		t.Fatalf("unknown category = %v, want ErrUnknownCategory", err)
	}
	if _, err := sess.SetTorrentCategory(ctx, "not-a-hash", "movies"); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("unknown torrent = %v, want ErrUnknownTorrent", err)
	}
}

func TestCategoryNameCannotShadowRootTorrent(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	bytes, _ := buildSingleFileTorrentBytes(t, "movies", []byte("movie"), nil)
	sess := newManageSession(t, torrentsDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: bytes}); err != nil {
		t.Fatalf("add torrent: %v", err)
	}
	if _, err := sess.CreateCategory(ctx, "movies"); !errors.Is(err, session.ErrCategoryNamespaceConflict) {
		t.Fatalf("shadowing category = %v, want ErrCategoryNamespaceConflict", err)
	}
}
