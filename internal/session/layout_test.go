package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestLegacyCanonicalMetainfoMigratesToRootAndRegistry(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	data, hash := buildSingleFileTorrentBytes(t, "legacy.bin", []byte("legacy content"), nil)
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	legacyPath := filepath.Join(metadataDir, hash.HexString()+".torrent")
	if err := os.WriteFile(legacyPath, data, 0o644); err != nil {
		t.Fatalf("write legacy metainfo: %v", err)
	}

	sess, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	finalPath := filepath.Join(torrentsDir, hash.HexString()+".torrent")
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("migrated final missing: %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy final survived migration: %v", err)
	}
	if _, err := os.Stat(registryPath(torrentsDir, hash)); err != nil {
		t.Fatalf("migrated registry missing: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(metadataDir, "layout_version"))
	if err != nil || strings.TrimSpace(string(marker)) != "2" {
		t.Fatalf("layout marker = %q (%v), want 2", marker, err)
	}

	restarted, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() {
		if err := restarted.Close(context.Background()); err != nil {
			t.Errorf("restart Close: %v", err)
		}
	}()
	if _, ok := restarted.Torrent(hash); !ok {
		t.Fatal("migrated task was not restored from registry")
	}
}

func TestLegacyMigrationRejectsDifferentTargetHashWithoutClobber(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	legacy, hash := buildSingleFileTorrentBytes(t, "legacy.bin", []byte("legacy"), nil)
	target, otherHash := buildSingleFileTorrentBytes(t, "other.bin", []byte("other"), nil)
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	legacyPath := filepath.Join(metadataDir, hash.HexString()+".torrent")
	targetPath := filepath.Join(torrentsDir, hash.HexString()+".torrent")
	if err := os.WriteFile(legacyPath, legacy, 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if err := os.WriteFile(targetPath, target, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if _, err := session.New(testConfig(), torrentsDir); err == nil {
		t.Fatal("New accepted a target info-hash mismatch")
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy source was removed after conflict: %v", err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil || string(got) != string(target) {
		t.Fatalf("target was overwritten after conflict: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(metadataDir, "layout_version")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("layout marker written after failed migration: %v", err)
	}
	_ = otherHash
}

func TestRegistryReadyWithoutFinalFailsStartup(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	_, hash := buildSingleFileTorrentBytes(t, "missing.bin", []byte("missing"), nil)
	writeRegistry(t, torrentsDir, hash, string(session.StateReady), "")
	if _, err := session.New(testConfig(), torrentsDir); err == nil || !strings.Contains(err.Error(), "final metainfo") {
		t.Fatalf("New ready registry without final = %v, want explicit final error", err)
	}
}

func TestMalformedRegistryFilenameFailsStartup(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	stateDir := filepath.Join(torrentsDir, ".metadata", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("make state dir: %v", err)
	}
	_, hash := buildSingleFileTorrentBytes(t, "malformed.bin", []byte("malformed"), nil)
	now := time.Now().UTC()
	data, err := json.Marshal(map[string]any{
		"id": hash.HexString(), "info_hash": hash.HexString(), "name": "malformed.bin",
		"state": session.StateReady, "created_at": now, "updated_at": now,
	})
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	path := filepath.Join(stateDir, "wrong-name.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write malformed state: %v", err)
	}
	if _, err := session.New(testConfig(), torrentsDir); err == nil || !strings.Contains(err.Error(), "wrong-name.json") {
		t.Fatalf("New malformed registry = %v, want filename error", err)
	}
}

func TestMarkerStopsReadingNewLegacyFlatFile(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	first, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	data, hash := buildSingleFileTorrentBytes(t, "late-legacy.bin", []byte("late legacy"), nil)
	legacyPath := filepath.Join(torrentsDir, ".metadata", hash.HexString()+".torrent")
	if err := os.WriteFile(legacyPath, data, 0o644); err != nil {
		t.Fatalf("write late legacy file: %v", err)
	}
	second, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}()
	if _, ok := second.Torrent(hash); ok {
		t.Fatal("late legacy file was imported after marker")
	}
}

func TestManagedAddRejectsMismatchedExistingFinal(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := buildSingleFileTorrentBytes(t, "expected.bin", []byte("expected"), nil)
	other, _ := buildSingleFileTorrentBytes(t, "other.bin", []byte("other"), nil)
	path := filepath.Join(torrentsDir, hash.HexString()+".torrent")
	if err := os.WriteFile(path, other, 0o644); err != nil {
		t.Fatalf("write mismatched final: %v", err)
	}
	sess := newManageSession(t, torrentsDir)
	if _, err := sess.AddTorrentAndPersist(context.Background(), session.Source{Metainfo: data}); err == nil {
		t.Fatal("managed add replaced a mismatched final")
	}
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("failed managed add left runtime task")
	}
	if _, err := os.Stat(registryPath(torrentsDir, hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed managed add wrote registry: %v", err)
	}
}
