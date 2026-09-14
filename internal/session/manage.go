package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

var (
	// ErrDeleting reports that a torrent is being deleted and cannot be
	// re-added until the deletion finishes.
	ErrDeleting = errors.New("session: torrent is being deleted")
	// ErrUnknownTorrent reports an id with no registered torrent or deletion.
	ErrUnknownTorrent = errors.New("session: unknown torrent")
	// ErrInvalidSource reports malformed add input.
	ErrInvalidSource = errors.New("session: invalid torrent source")
	// ErrUnsafePurge reports a purge target that fails the data safety
	// boundary. The payload is left untouched when it is returned.
	ErrUnsafePurge = errors.New("session: refusing to purge unmanaged data")
	// ErrExternalReference reports a deletion refused because a .torrent file
	// the user placed in the torrents directory still references the hash. The
	// service never deletes such a torrent or its data behind the user's back.
	ErrExternalReference = errors.New("session: torrent is referenced by a torrents directory file")
)

// lockHash serializes operations on one info hash so an add and a delete can
// never interleave their registry transitions.
func (s *Session) lockHash(hash metainfo.Hash) func() {
	s.opMu.Lock()
	lock := s.opLocks[hash]
	if lock == nil {
		lock = &sync.Mutex{}
		s.opLocks[hash] = lock
	}
	s.opMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// PayloadRoot is the managed root under which every torrent owns an exclusive
// payloadRoot/<info_hash> directory.
func (s *Session) PayloadRoot() string {
	return s.payloadRoot
}

// AddTorrentAndPersist registers a torrent and durably records both its
// metainfo and its management state. A duplicate info hash returns the
// existing task; a torrent that is deleting or failed to delete is rejected
// with ErrDeleting.
func (s *Session) AddTorrentAndPersist(ctx context.Context, src Source) (*TorrentView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	spec, err := specFromSource(src)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	hash := spec.InfoHash
	var zero metainfo.Hash
	if hash == zero {
		return nil, fmt.Errorf("%w: missing info hash", ErrInvalidSource)
	}

	unlock := s.lockHash(hash)
	defer unlock()

	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if entry, ok := s.states[hash]; ok && (entry.State == StateDeleting || entry.State == StateDeleteFailed) {
		s.mu.Unlock()
		return nil, ErrDeleting
	}
	st, _, err := s.addTorrentSpecLocked(ctx, spec)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if s.states[hash] == nil {
		s.states[hash] = &registryEntry{
			ID:        hash.HexString(),
			InfoHash:  hash.HexString(),
			Name:      st.Name(),
			State:     StateAdding,
			CreatedAt: time.Now().UTC(),
		}
	}
	s.mu.Unlock()

	// Persist the metainfo so the torrent survives a restart. Magnet sources
	// persist asynchronously once their info arrives.
	if len(src.Metainfo) > 0 || src.MetainfoPath != "" {
		if err := s.persistMetainfo(ctx, hash, src); err != nil {
			return nil, err
		}
	} else {
		s.startMetadataFetch(hash, st)
	}
	return s.viewFor(hash, st), nil
}

// persistMetainfo publishes the torrent's metainfo into the metadata control
// directory, skipping the write when the hash is already persisted.
func (s *Session) persistMetainfo(ctx context.Context, hash metainfo.Hash, src Source) error {
	var data []byte
	switch {
	case len(src.Metainfo) > 0:
		data = src.Metainfo
	case src.MetainfoPath != "":
		b, err := os.ReadFile(src.MetainfoPath)
		if err != nil {
			return fmt.Errorf("session: read metainfo: %w", err)
		}
		data = b
	default:
		return nil
	}
	return s.writeMetadataBytes(ctx, hash, data)
}

func (s *Session) writeMetadataBytes(ctx context.Context, hash metainfo.Hash, data []byte) error {
	name := hash.HexString() + ".torrent"
	s.mu.RLock()
	_, exists := s.metadata[name]
	pending := s.deletionPendingLocked(hash)
	s.mu.RUnlock()
	if pending {
		// A late metadata write for a torrent that is being (or failed to be)
		// deleted must never recreate its sidecar or re-register it.
		return fmt.Errorf("%w: %s", ErrDeleting, hash)
	}
	if exists {
		return nil
	}
	w, err := s.BeginMetadata(ctx, name, syscall.O_WRONLY)
	if err != nil {
		// A racing writer or an existing file means the hash is already
		// persisted; anything else is a real failure.
		if errors.Is(err, filesystem.ErrExists) {
			return nil
		}
		return fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	}
	if _, err := w.WriteAt(data, 0); err != nil {
		_ = w.Abort()
		return fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	}
	if err := w.Commit(); err != nil {
		return fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	}
	return nil
}

// startMetadataFetch waits for a magnet source's info in the background and
// then persists the metainfo. Each worker is cancellable per info hash so a
// deletion stops exactly its own hash instead of leaving the worker alive
// until the session closes.
func (s *Session) startMetadataFetch(hash metainfo.Hash, st *Torrent) {
	ctx, cancel := context.WithCancel(s.bgCtx)
	fetch := &metadataFetch{cancel: cancel, done: make(chan struct{})}

	s.fetchMu.Lock()
	if _, running := s.metadataFetches[hash]; running {
		// A worker is already tracking this hash; do not stack another.
		s.fetchMu.Unlock()
		cancel()
		return
	}
	s.metadataFetches[hash] = fetch
	s.fetchMu.Unlock()

	s.bgWg.Add(1)
	go func() {
		defer s.bgWg.Done()
		defer cancel()
		defer close(fetch.done)

		select {
		case <-ctx.Done():
			return
		case <-st.GotInfo():
		}
		if metadataFetchHook != nil {
			metadataFetchHook(hash)
		}
		mi := st.tor.Metainfo()
		var buf bytes.Buffer
		if err := mi.Write(&buf); err != nil {
			return
		}
		_ = s.writeMetadataBytes(ctx, hash, buf.Bytes())
	}()
}

// metadataFetchHook is a test-only seam that runs after a metadata fetch
// resolves, before it persists. Production never sets it.
var metadataFetchHook func(metainfo.Hash)

// stopMetadataFetch cancels the metadata worker for hash, if any, and returns
// it so the caller can wait for it to exit.
func (s *Session) stopMetadataFetch(hash metainfo.Hash) *metadataFetch {
	s.fetchMu.Lock()
	fetch := s.metadataFetches[hash]
	delete(s.metadataFetches, hash)
	s.fetchMu.Unlock()
	if fetch != nil {
		fetch.cancel()
	}
	return fetch
}

// deletionPendingLocked reports whether the hash is mid-delete or in a failed
// delete that still owns the task. Callers must hold s.mu.
func (s *Session) deletionPendingLocked(hash metainfo.Hash) bool {
	entry, ok := s.states[hash]
	return ok && (entry.State == StateDeleting || entry.State == StateDeleteFailed)
}

// ListTorrents returns a snapshot of every task, including tasks whose
// deletion is still running and whose final state is delete_failed.
func (s *Session) ListTorrents() []TorrentView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != stateActive {
		return nil
	}
	views := make([]TorrentView, 0, len(s.torrents))
	seen := make(map[metainfo.Hash]struct{}, len(s.torrents))
	for hash, st := range s.torrents {
		views = append(views, buildView(hash, st, s.states[hash]))
		seen[hash] = struct{}{}
	}
	for hash, entry := range s.states {
		if _, ok := seen[hash]; ok {
			continue
		}
		if entry.State == StateDeleting || entry.State == StateDeleteFailed {
			views = append(views, buildView(hash, nil, entry))
		}
	}
	return views
}

// TorrentViewFor returns one task's snapshot.
func (s *Session) TorrentViewFor(id string) (TorrentView, error) {
	hash, err := parseInfoHash(id)
	if err != nil {
		return TorrentView{}, ErrUnknownTorrent
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.torrents[hash]
	entry := s.states[hash]
	if !ok && entry == nil {
		return TorrentView{}, ErrUnknownTorrent
	}
	return buildView(hash, st, entry), nil
}

func (s *Session) viewFor(hash metainfo.Hash, st *Torrent) *TorrentView {
	s.mu.RLock()
	entry := s.states[hash]
	s.mu.RUnlock()
	view := buildView(hash, st, entry)
	return &view
}

func buildView(hash metainfo.Hash, st *Torrent, entry *registryEntry) TorrentView {
	view := TorrentView{ID: hash.HexString(), InfoHash: hash.HexString(), State: StateAdding}
	if entry != nil {
		view.Name = entry.Name
		view.CreatedAt = entry.CreatedAt
		view.Error = entry.Error
		view.State = entry.State
	}
	if st == nil {
		return view
	}
	view.Name = st.Name()
	view.TotalBytes = st.Length()
	view.CompletedBytes = st.BytesCompleted()
	if view.TotalBytes > 0 {
		view.Progress = float64(view.CompletedBytes) / float64(view.TotalBytes)
	}
	if view.State == StateDeleting || view.State == StateDeleteFailed {
		return view
	}
	switch {
	case st.Info() == nil:
		view.State = StateAdding
	case view.TotalBytes > 0 && view.CompletedBytes >= view.TotalBytes:
		view.State = StateSeeding
	default:
		view.State = StateDownloading
	}
	return view
}

// DeleteTorrent starts the durable deletion of one torrent. The heavy cleanup
// runs in the background; the returned operation observes it through
// Operation. Repeated calls for the same hash return the same operation while
// it runs.
func (s *Session) DeleteTorrent(ctx context.Context, id string, purgeData bool) (*Operation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hash, err := parseInfoHash(id)
	if err != nil {
		return nil, ErrUnknownTorrent
	}

	unlock := s.lockHash(hash)
	defer unlock()

	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if opID, ok := s.activeOps[hash]; ok {
		if op, ok := s.operations[opID]; ok {
			out := op.clone()
			s.mu.Unlock()
			return &out, nil
		}
	}
	if opID, ok := s.lastOps[hash]; ok {
		if op, ok := s.operations[opID]; ok && op.State == StateDeleted {
			// A repeated DELETE returns the same finished operation.
			out := op.clone()
			s.mu.Unlock()
			return &out, nil
		}
	}
	st, ok := s.torrents[hash]
	entry, hasEntry := s.states[hash]
	if !ok && !hasEntry {
		s.mu.Unlock()
		return nil, ErrUnknownTorrent
	}
	if s.directoryRefs[hash] > 0 {
		// The user's own .torrent file still references this hash. Removing
		// the task would hide data they expect to keep, and purging would
		// delete data still in use, so the deletion is refused outright.
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrExternalReference, hash)
	}
	opID := ""
	if last, ok := s.lastOps[hash]; ok {
		if op, ok := s.operations[last]; ok && op.State == StateDeleteFailed {
			// Retrying a failed deletion reuses its operation id.
			opID = last
		}
	}
	if opID == "" {
		id, err := newOperationID()
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		opID = id
	}
	now := time.Now().UTC()
	if entry == nil {
		entry = &registryEntry{
			ID:        hash.HexString(),
			InfoHash:  hash.HexString(),
			Name:      st.Name(),
			CreatedAt: now,
		}
	}
	entry.State = StateDeleting
	entry.Error = ""
	entry.PurgeRequested = purgeData
	entry.OperationID = opID
	if err := s.writeRegistryEntryLocked(entry); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.states[hash] = entry
	op := &Operation{
		ID:        opID,
		TorrentID: hash.HexString(),
		State:     StateDeleting,
		PurgeData: purgeData,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.operations[opID] = op
	s.activeOps[hash] = opID
	s.lastOps[hash] = opID
	// Drop the references that keep the torrent alive, but keep the
	// s.torrents entry so the task stays visible while deleting.
	delete(s.manualRefs, hash)
	s.releaseMetadataIndexLocked(hash)
	out := op.clone()
	s.mu.Unlock()

	s.bgWg.Add(1)
	go func() {
		defer s.bgWg.Done()
		s.runDelete(hash, st, purgeData, opID)
	}()
	return &out, nil
}

// Operation returns the current state of a deletion operation.
func (s *Session) Operation(id string) (Operation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, false
	}
	return op.clone(), true
}

// runDelete performs the cleanup outside s.mu and records the outcome.
func (s *Session) runDelete(hash metainfo.Hash, st *Torrent, purge bool, opID string) {
	// Stop the hash's metadata worker and wait for it to exit, so no late
	// write can recreate the sidecar or re-register the torrent after this
	// deletion finalizes.
	if fetch := s.stopMetadataFetch(hash); fetch != nil {
		<-fetch.done
	}
	err := s.performDelete(hash, st, purge)

	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[opID]
	if err != nil {
		if entry, ok := s.states[hash]; ok {
			entry.State = StateDeleteFailed
			entry.Error = err.Error()
			_ = s.writeRegistryEntryLocked(entry)
		}
		if op != nil {
			op.State = StateDeleteFailed
			op.Error = err.Error()
			op.UpdatedAt = time.Now().UTC()
		}
	} else {
		// Only remove the handle this deletion started with; a torrent
		// registered while the deletion ran keeps its own entry.
		if cur, ok := s.torrents[hash]; ok && cur == st {
			delete(s.torrents, hash)
		}
		s.releaseMetadataIndexLocked(hash)
		_ = s.removeRegistryEntryLocked(hash)
		if op != nil {
			op.State = StateDeleted
			op.Error = ""
			op.UpdatedAt = time.Now().UTC()
		}
	}
	delete(s.activeOps, hash)
}

// performDelete stops the task, releases its resources, removes the managed
// metainfo, and applies the payload policy. It runs without s.mu held.
func (s *Session) performDelete(hash metainfo.Hash, st *Torrent, purge bool) error {
	var errs []error
	if st != nil {
		if err := st.close(); err != nil {
			errs = append(errs, err)
		}
		st.tor.Drop()
	}
	name := hash.HexString() + ".torrent"
	if err := os.Remove(s.metadataPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("session: remove metadata %s: %w", name, err))
	}
	if purge {
		if err := s.purgePayload(hash); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// purgePayload removes payloadRoot/<hash> after checking that the target is
// the torrent's own directory and that no component is a symlink. A missing
// target is success: removal is idempotent.
func (s *Session) purgePayload(hash metainfo.Hash) error {
	root := s.payloadRoot
	target := filepath.Join(root, hash.HexString())
	rel, err := filepath.Rel(root, target)
	if err != nil || rel != hash.HexString() || strings.ContainsRune(rel, os.PathSeparator) {
		return fmt.Errorf("%w: %q is not a direct child of %q", ErrUnsafePurge, target, root)
	}
	for _, path := range []string{root, target} {
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%w: %q: %v", ErrUnsafePurge, path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q is a symlink", ErrUnsafePurge, path)
		}
	}
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", ErrUnsafePurge, target)
	}
	// Quarantine the directory first so removal is a single rename from the
	// perspective of concurrent filesystem readers.
	trash, err := newOperationID()
	if err != nil {
		return err
	}
	quarantine := filepath.Join(root, ".trash-"+hash.HexString()+"-"+trash)
	if err := os.Rename(target, quarantine); err != nil {
		return fmt.Errorf("session: quarantine payload %s: %w", hash, err)
	}
	if err := os.RemoveAll(quarantine); err != nil {
		return fmt.Errorf("session: remove payload %s: %w", hash, err)
	}
	return nil
}

// resumeDeletions completes deletions interrupted by a crash. It runs before
// the session is returned so a restart never reports a deleting task as live.
func (s *Session) resumeDeletions() error {
	type pendingDelete struct {
		hash  metainfo.Hash
		opID  string
		purge bool
	}
	s.mu.Lock()
	pending := make([]pendingDelete, 0)
	stale := make([]metainfo.Hash, 0)
	for hash, entry := range s.states {
		if entry.State != StateDeleting {
			continue
		}
		if s.directoryRefs[hash] > 0 {
			// A user .torrent file reclaimed the hash after the deletion
			// marker was written; keep the torrent and drop the marker.
			stale = append(stale, hash)
			continue
		}
		opID := entry.OperationID
		if opID == "" {
			id, err := newOperationID()
			if err != nil {
				s.mu.Unlock()
				return err
			}
			opID = id
			entry.OperationID = id
		}
		now := time.Now().UTC()
		s.operations[opID] = &Operation{
			ID:        opID,
			TorrentID: hash.HexString(),
			State:     StateDeleting,
			PurgeData: entry.PurgeRequested,
			CreatedAt: now,
			UpdatedAt: now,
		}
		s.activeOps[hash] = opID
		s.lastOps[hash] = opID
		delete(s.manualRefs, hash)
		pending = append(pending, pendingDelete{hash: hash, opID: opID, purge: entry.PurgeRequested})
	}
	for _, hash := range stale {
		_ = s.removeRegistryEntryLocked(hash)
	}
	s.mu.Unlock()

	for _, item := range pending {
		s.mu.RLock()
		st := s.torrents[item.hash]
		s.mu.RUnlock()
		s.runDelete(item.hash, st, item.purge, item.opID)
	}
	return nil
}

// releaseMetadataIndexLocked drops the in-memory metadata mapping and one
// reference for the hash so no ghost sidecar/index survives a deletion.
// Callers must hold s.mu.
func (s *Session) releaseMetadataIndexLocked(hash metainfo.Hash) {
	name, ok := metadataNameForHash(s.metadata, hash)
	if !ok {
		return
	}
	delete(s.metadata, name)
	if refs := s.metadataRefs[hash]; refs > 1 {
		s.metadataRefs[hash] = refs - 1
	} else {
		delete(s.metadataRefs, hash)
	}
}

func metadataNameForHash(metadata map[string]metainfo.Hash, hash metainfo.Hash) (string, bool) {
	for name, h := range metadata {
		if h == hash {
			return name, true
		}
	}
	return "", false
}
