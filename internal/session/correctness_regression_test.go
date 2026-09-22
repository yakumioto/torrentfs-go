package session

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func newRegressionSession(t *testing.T) *Session {
	t.Helper()
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess, err := New(config.Default(), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sess
}

func regressionTorrent(t *testing.T) ([]byte, metainfo.Hash, *torrent.TorrentSpec) {
	t.Helper()
	content := []byte("metadata retry regression")
	sum := sha1.Sum(content)
	info := metainfo.Info{
		Name:        "payload.bin",
		Length:      int64(len(content)),
		PieceLength: 256 << 10,
		Pieces:      sum[:],
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("encode info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes)}
	data, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode metainfo: %v", err)
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(&mi)
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	return data, mi.HashInfoBytes(), spec
}

func waitRegression(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func TestMetadataFetchAndDeleteUseOneBackgroundLockOrder(t *testing.T) {
	sess := newRegressionSession(t)
	_, hash, spec := regressionTorrent(t)
	sess.mu.Lock()
	st, _, err := sess.addTorrentSpecLocked(context.Background(), spec)
	if err != nil {
		sess.mu.Unlock()
		t.Fatalf("add spec: %v", err)
	}
	started := make(chan struct{})
	go func() {
		sess.startMetadataFetch(hash, st)
		close(started)
	}()
	waitRegression(t, func() bool {
		sess.fetchMu.Lock()
		defer sess.fetchMu.Unlock()
		return sess.metadataFetches[hash] != nil
	})
	for range 100 {
		runtime.Gosched()
	}
	acquired := make(chan struct{})
	go func() {
		sess.bgMu.Lock()
		close(acquired)
		sess.bgMu.Unlock()
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		sess.mu.Unlock()
		t.Fatal("background lock was blocked behind a metadata worker waiting for session lock")
	}
	sess.mu.Unlock()
	fetch := sess.stopMetadataFetch(hash)
	if fetch != nil {
		<-fetch.done
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("metadata worker did not exit after cancellation")
	}
}

func TestMetadataFetchFailureRetryHandsOffToNextWorker(t *testing.T) {
	sess := newRegressionSession(t)
	data, hash, spec := regressionTorrent(t)
	magnet := "magnet:?xt=urn:btih:" + hash.HexString()
	if _, err := sess.publishPendingMagnet(context.Background(), hash, magnet); err != nil {
		t.Fatalf("publish pending magnet: %v", err)
	}
	sess.mu.Lock()
	st, _, err := sess.addTorrentSpecLocked(context.Background(), spec)
	if err != nil {
		sess.mu.Unlock()
		t.Fatalf("add spec: %v", err)
	}
	sess.states[hash] = &registryEntry{
		ID:        hash.HexString(),
		InfoHash:  hash.HexString(),
		Name:      st.Name(),
		State:     StateAdding,
		CreatedAt: time.Now().UTC(),
	}
	sess.mu.Unlock()
	if err := os.Mkdir(sess.finalMetainfoPath(hash), 0o755); err != nil {
		t.Fatalf("block final path: %v", err)
	}

	firstReached := make(chan struct{})
	firstRelease := make(chan struct{})
	secondReached := make(chan struct{})
	secondRelease := make(chan struct{})
	var hookMu sync.Mutex
	calls := 0
	previousHook := metadataFetchHook
	metadataFetchHook = func(got metainfo.Hash) {
		if got != hash {
			return
		}
		hookMu.Lock()
		calls++
		call := calls
		hookMu.Unlock()
		switch call {
		case 1:
			close(firstReached)
			<-firstRelease
		case 2:
			close(secondReached)
			<-secondRelease
		}
	}
	defer func() { metadataFetchHook = previousHook }()

	sess.startMetadataFetch(hash, st)
	select {
	case <-firstReached:
	case <-time.After(5 * time.Second):
		t.Fatal("first metadata worker did not reach promotion")
	}
	// This duplicate request arrives while the first worker is still running.
	// It must hand off a retry instead of leaving the task in adding forever.
	sess.startMetadataFetch(hash, st)
	close(firstRelease)
	select {
	case <-secondReached:
	case <-time.After(5 * time.Second):
		t.Fatal("retry metadata worker was not started")
	}
	if err := os.Remove(sess.finalMetainfoPath(hash)); err != nil {
		t.Fatalf("remove blocking final path: %v", err)
	}
	close(secondRelease)
	waitRegression(t, func() bool {
		view, err := sess.TorrentViewFor(hash.HexString())
		return err == nil && view.State == StateReady && sess.PendingMetadataFetches() == 0
	})
	if _, err := os.Stat(sess.finalMetainfoPath(hash)); err != nil {
		t.Fatalf("retry did not publish final metainfo: %v", err)
	}
	if _, err := os.Stat(sess.pendingMagnetPath(hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending magnet survived successful retry: %v", err)
	}
	_ = data
}

func TestValidStalePendingCleanupFailureKeepsReadyTask(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	data, hash, _ := regressionTorrent(t)
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	stateDir := filepath.Join(metadataDir, "state")
	pendingDir := filepath.Join(metadataDir, "pending")
	for _, dir := range []string{metadataDir, stateDir, pendingDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "layout_version"), []byte("2\n"), 0o644); err != nil {
		t.Fatalf("write layout marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(torrentsDir, hash.HexString()+".torrent"), data, 0o644); err != nil {
		t.Fatalf("write final metainfo: %v", err)
	}
	magnet := "magnet:?xt=urn:btih:" + hash.HexString()
	if err := os.WriteFile(filepath.Join(pendingDir, hash.HexString()+".magnet"), []byte(magnet+"\n"), 0o644); err != nil {
		t.Fatalf("write pending magnet: %v", err)
	}
	now := time.Now().UTC()
	entry := &registryEntry{
		ID: hash.HexString(), InfoHash: hash.HexString(), Name: "payload.bin",
		State: StateReady, CreatedAt: now, UpdatedAt: now,
	}
	stateData, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, hash.HexString()+".json"), stateData, 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	previousHook := pendingMagnetCleanupHook
	pendingMagnetCleanupHook = func(metainfo.Hash) error {
		return errors.New("injected pending cleanup failure")
	}
	defer func() { pendingMagnetCleanupHook = previousHook }()
	sess, err := New(config.Default(), torrentsDir)
	if err != nil {
		t.Fatalf("New with valid stale pending cleanup failure: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	view, err := sess.TorrentViewFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentViewFor: %v", err)
	}
	if view.State != StateReady {
		t.Fatalf("state = %s, want ready", view.State)
	}
	if _, err := os.Stat(filepath.Join(pendingDir, hash.HexString()+".magnet")); err != nil {
		t.Fatalf("valid stale pending should remain for retry: %v", err)
	}
}

func TestCanonicalMetainfoDocumentationNamesRootPath(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(readme)
	if !strings.Contains(text, "metainfo 持久化在 `torrents-dir/<infohash>.torrent`") {
		t.Fatal("README does not state the canonical metainfo root path")
	}
	if strings.Contains(text, "由 API 管理的 metainfo、未完成的磁力链接意图和 peer identity 持久化在 `torrents-dir/.metadata`") {
		t.Fatal("README still describes metainfo as living in .metadata")
	}
}
