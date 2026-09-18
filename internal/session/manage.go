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
	"time"

	"github.com/anacrolix/torrent/metainfo"
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
	logFailure := func(err error) {
		attrs := []any{"source", sourceKind(src), "err", err}
		if src.MetainfoPath != "" {
			attrs = append(attrs, "path", src.MetainfoPath)
		}
		s.logger.Error("managed torrent add failed", attrs...)
	}
	spec, err := specFromSource(src)
	if err != nil {
		err = fmt.Errorf("%w: %v", ErrInvalidSource, err)
		logFailure(err)
		return nil, err
	}
	hash := spec.InfoHash
	var zero metainfo.Hash
	if hash == zero {
		err := fmt.Errorf("%w: missing info hash", ErrInvalidSource)
		logFailure(err)
		return nil, err
	}

	unlock := s.lockHash(hash)
	defer unlock()

	if src.MagnetURI != "" {
		if err := s.persistMagnetIntent(ctx, hash, src.MagnetURI); err != nil {
			logFailure(err)
			return nil, err
		}
	}

	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	if s.deletionPendingLocked(hash) {
		err := ErrDeleting
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	st, added, err := s.addTorrentSpecLocked(ctx, spec)
	if err != nil {
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	stateCreated := false
	if s.states[hash] == nil {
		s.states[hash] = &registryEntry{
			ID:        hash.HexString(),
			InfoHash:  hash.HexString(),
			Name:      st.Name(),
			State:     StateAdding,
			CreatedAt: time.Now().UTC(),
		}
		stateCreated = true
	}
	s.mu.Unlock()

	if len(src.Metainfo) > 0 || src.MetainfoPath != "" {
		if err := s.persistMetainfo(ctx, hash, src); err != nil {
			if added {
				s.mu.Lock()
				s.rollbackAddedTorrentLocked(hash, st, stateCreated)
				s.mu.Unlock()
			}
			logFailure(err)
			return nil, err
		}
	} else {
		s.startMetadataFetch(hash, st)
	}
	s.logger.Info("managed torrent added", "hash", hash.HexString(), "source", sourceKind(src))
	return s.viewFor(hash, st), nil
}

// persistMetainfo publishes the torrent's metainfo into the internal metadata
// directory.
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

func (s *Session) rollbackAddedTorrentLocked(hash metainfo.Hash, st *Torrent, stateCreated bool) {
	if current, ok := s.torrents[hash]; !ok || current != st {
		return
	}
	if s.metadataRefs[hash] > 0 || s.directoryRefs[hash] > 0 {
		return
	}
	if _, ok := s.manualRefs[hash]; ok {
		return
	}
	_ = s.removeTorrentLocked(hash, st)
	if stateCreated {
		delete(s.states, hash)
	}
}

func (s *Session) removeTorrentLocked(hash metainfo.Hash, st *Torrent) error {
	if current, ok := s.torrents[hash]; ok && current == st {
		delete(s.torrents, hash)
	}
	stErr := st.close()
	st.tor.Drop()
	return stErr
}

// startMetadataFetch waits for a magnet source's info in the background and
// then promotes its durable intent to metainfo. Each worker is cancellable per
// info hash so a deletion stops exactly its own hash.
func (s *Session) startMetadataFetch(hash metainfo.Hash, st *Torrent) {
	ctx, cancel := context.WithCancel(s.bgCtx)
	fetch := &metadataFetch{cancel: cancel, done: make(chan struct{})}

	s.fetchMu.Lock()
	if _, running := s.metadataFetches[hash]; running {
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
		defer func() {
			s.fetchMu.Lock()
			if s.metadataFetches[hash] == fetch {
				delete(s.metadataFetches, hash)
			}
			s.fetchMu.Unlock()
		}()

		select {
		case <-ctx.Done():
			return
		case <-st.GotInfo():
		}
		if metadataFetchHook != nil {
			metadataFetchHook(hash)
		}
		if err := ctx.Err(); err != nil {
			return
		}
		mi := st.tor.Metainfo()
		var buf bytes.Buffer
		if err := mi.Write(&buf); err != nil {
			s.recordMetadataFetchFailure(hash, err)
			return
		}

		unlock := s.lockHash(hash)
		err := s.writeMetadataBytes(ctx, hash, buf.Bytes())
		unlock()
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrDeleting) {
			s.recordMetadataFetchFailure(hash, err)
		}
	}()
}

func (s *Session) recordMetadataFetchFailure(hash metainfo.Hash, err error) {
	s.logger.Warn("metadata fetch failed", "hash", hash.HexString(), "err", err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deletionPendingLocked(hash) {
		return
	}
	if entry := s.states[hash]; entry != nil {
		entry.State = StateError
		entry.Error = err.Error()
	}
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
	if view.State == StateDeleting || view.State == StateDeleteFailed || view.State == StateError {
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
		s.logger.Error("torrent delete failed", "op", "parse-id", "err", ErrUnknownTorrent)
		return nil, ErrUnknownTorrent
	}
	logFailure := func(err error) {
		s.logger.Error("torrent delete failed", "hash", hash.HexString(), "op", "delete", "err", err)
	}

	unlock := s.lockHash(hash)
	defer unlock()

	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		logFailure(err)
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
		err := ErrUnknownTorrent
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	if s.directoryRefs[hash] > 0 {
		// The user's own .torrent file still references this hash. Removing
		// the task would hide data they expect to keep, and purging would
		// delete data still in use, so the deletion is refused outright.
		err := fmt.Errorf("%w: %s", ErrExternalReference, hash)
		s.mu.Unlock()
		logFailure(err)
		return nil, err
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
			logFailure(err)
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
		logFailure(err)
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
	sources := s.managedSourcesForHashLocked(hash)
	// Drop the references that keep the torrent alive, but keep the
	// s.torrents entry so the task stays visible while deleting.
	delete(s.manualRefs, hash)
	s.releaseManagedSourcesLocked(hash)
	out := op.clone()
	s.mu.Unlock()
	s.logger.Info("torrent deletion started", "hash", hash.HexString(), "purge_data", purgeData, "operation_id", opID)

	s.bgWg.Add(1)
	go func() {
		defer s.bgWg.Done()
		s.runDelete(hash, st, purgeData, opID, sources)
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
func (s *Session) runDelete(hash metainfo.Hash, st *Torrent, purge bool, opID string, sources managedSources) {
	// Stop the hash's metadata worker and wait for it to exit, so no late
	// write can recreate a managed source after this deletion finalizes.
	if fetch := s.stopMetadataFetch(hash); fetch != nil {
		<-fetch.done
	}
	err := s.performDelete(hash, st, purge, sources)
	if err != nil {
		s.logger.Error("torrent deletion failed", "hash", hash.HexString(), "operation_id", opID, "err", err)
	} else {
		s.logger.Info("torrent deletion completed", "hash", hash.HexString(), "operation_id", opID, "purge_data", purge)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[opID]
	if err != nil {
		s.restoreManagedSourcesLocked(hash, sources)
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
		if cur, ok := s.torrents[hash]; ok && cur == st {
			delete(s.torrents, hash)
		}
		s.releaseManagedSourcesLocked(hash)
		_ = s.removeRegistryEntryLocked(hash)
		if op != nil {
			op.State = StateDeleted
			op.Error = ""
			op.UpdatedAt = time.Now().UTC()
		}
	}
	delete(s.activeOps, hash)
}

// performDelete stops the task, releases its resources, removes every internal
// source for the hash, and applies the payload policy. It runs without s.mu
// held.
func (s *Session) performDelete(hash metainfo.Hash, st *Torrent, purge bool, sources managedSources) error {
	var errs []error
	if st != nil {
		if err := st.close(); err != nil {
			errs = append(errs, err)
		}
		st.tor.Drop()
	}
	for _, name := range sources.metadata {
		if err := os.Remove(s.metadataPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("session: remove metadata %s: %w", name, err))
		}
	}
	for _, source := range sources.magnets {
		if err := os.Remove(s.metadataPath(source.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("session: remove pending magnet %s: %w", source.name, err))
		}
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
		hash    metainfo.Hash
		opID    string
		purge   bool
		sources managedSources
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
		sources := s.managedSourcesForHashLocked(hash)
		delete(s.manualRefs, hash)
		s.releaseManagedSourcesLocked(hash)
		pending = append(pending, pendingDelete{hash: hash, opID: opID, purge: entry.PurgeRequested, sources: sources})
	}
	for _, hash := range stale {
		_ = s.removeRegistryEntryLocked(hash)
	}
	s.mu.Unlock()

	for _, item := range pending {
		s.mu.RLock()
		st := s.torrents[item.hash]
		s.mu.RUnlock()
		s.runDelete(item.hash, st, item.purge, item.opID, item.sources)
	}
	return nil
}
