package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
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

func readManagedSource(src Source) ([]byte, error) {
	switch {
	case len(src.Metainfo) > 0:
		return src.Metainfo, nil
	case src.MetainfoPath != "":
		data, err := os.ReadFile(src.MetainfoPath)
		if err != nil {
			return nil, fmt.Errorf("session: read metainfo: %w", err)
		}
		return data, nil
	default:
		return nil, nil
	}
}

func (s *Session) newRegistryEntry(hash metainfo.Hash, name string, state TorrentState) *registryEntry {
	return &registryEntry{
		ID:        hash.HexString(),
		InfoHash:  hash.HexString(),
		Name:      name,
		State:     state,
		CreatedAt: time.Now().UTC(),
	}
}

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
	if hash == (metainfo.Hash{}) {
		err := fmt.Errorf("%w: missing info hash", ErrInvalidSource)
		logFailure(err)
		return nil, err
	}
	data, err := readManagedSource(src)
	if err != nil {
		logFailure(err)
		return nil, err
	}

	unlock := s.lockHash(hash)
	defer unlock()

	// A canonical final file wins over a duplicate magnet. It is validated
	// before being attached to the existing registry task.
	if src.MagnetURI != "" && data == nil {
		path := s.finalMetainfoPath(hash)
		_, statErr := os.Lstat(path)
		if statErr == nil {
			if _, err := loadMetainfoFile(path, hash); err != nil {
				logFailure(err)
				return nil, err
			}
			data, err = os.ReadFile(path)
			if err != nil {
				logFailure(err)
				return nil, err
			}
			spec, err = specFromSource(Source{Metainfo: data})
			if err != nil || spec.InfoHash != hash {
				err = fmt.Errorf("session: existing metainfo %s is invalid", hash)
				logFailure(err)
				return nil, err
			}
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			err = fmt.Errorf("session: inspect metainfo %s: %w", hash, statErr)
			logFailure(err)
			return nil, err
		}
	}

	return s.addManagedLocked(ctx, hash, spec, data, src.MagnetURI, logFailure)
}

// AddTorrentAndPersist's concrete transaction is kept in this typed helper so
// the source parsing boundary remains separate from the state transitions.
func (s *Session) addManagedLocked(ctx context.Context, hash metainfo.Hash, spec *torrent.TorrentSpec, data []byte, uri string, logFailure func(error)) (*TorrentView, error) {
	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	if s.deletionPendingLocked(hash) {
		s.mu.Unlock()
		logFailure(ErrDeleting)
		return nil, ErrDeleting
	}

	st, clientNew, err := s.prepareTorrentSpecLocked(ctx, spec)
	if err != nil {
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	entry := cloneRegistryEntry(s.states[hash])
	if entry == nil {
		state := StateReady
		if data == nil {
			state = StateAdding
		}
		entry = s.newRegistryEntry(hash, firstNonEmpty(st.Name(), specName(spec)), state)
	}
	if entry.State == StateDeleting || entry.State == StateDeleteFailed {
		if clientNew {
			st.tor.Drop()
		}
		s.mu.Unlock()
		logFailure(ErrDeleting)
		return nil, ErrDeleting
	}

	var publishedFinal, publishedPending bool
	if data != nil {
		publishedFinal, _, err = s.publishMetainfo(ctx, hash, data)
		if err == nil {
			entry.State = StateReady
			entry.Name = firstNonEmpty(st.Name(), specName(spec), entry.Name)
			entry.Error = ""
			entry.OperationID = ""
		}
	} else {
		publishedPending, err = s.publishPendingMagnet(ctx, hash, uri)
		if err == nil {
			entry.State = StateAdding
			entry.Name = firstNonEmpty(entry.Name, st.Name(), specName(spec))
			entry.Error = ""
			entry.OperationID = ""
		}
	}
	if err != nil {
		if clientNew {
			st.tor.Drop()
		}
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	if err := s.writeRegistryEntryLocked(entry); err != nil {
		if clientNew {
			st.tor.Drop()
		}
		if publishedFinal {
			if cleanupErr := s.removeFinalMetainfo(hash); cleanupErr != nil {
				s.logger.Error("managed add cleanup failed", "hash", hash.HexString(), "err", cleanupErr)
			}
		}
		if publishedPending {
			if cleanupErr := s.removePendingMagnet(hash); cleanupErr != nil {
				s.logger.Error("managed add cleanup failed", "hash", hash.HexString(), "err", cleanupErr)
			}
		}
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	s.states[hash] = entry
	if clientNew {
		s.publishTorrentLocked(hash, st)
	}
	s.mu.Unlock()

	if data != nil {
		if err := s.removePendingMagnet(hash); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.logger.Warn("stale pending magnet cleanup failed", "hash", hash.HexString(), "err", err)
			}
		}
	}
	if data == nil {
		s.startMetadataFetch(hash, st)
	}
	s.logger.Info("managed torrent added", "hash", hash.HexString(), "source", sourceKind(Source{Metainfo: data, MagnetURI: uri}))
	view := s.viewFor(hash, st)
	return view, nil
}

func (s *Session) startMetadataFetch(hash metainfo.Hash, st *Torrent) {
	ctx, cancel := context.WithCancel(s.bgCtx)
	fetch := &metadataFetch{cancel: cancel, done: make(chan struct{})}

	s.fetchMu.Lock()
	if running := s.metadataFetches[hash]; running != nil {
		running.retry = true
		s.fetchMu.Unlock()
		cancel()
		return
	}
	s.metadataFetches[hash] = fetch
	s.fetchMu.Unlock()

	s.mu.RLock()
	active := s.state == stateActive
	if active {
		s.bgMu.Lock()
		s.bgWg.Add(1)
		s.bgMu.Unlock()
	}
	s.mu.RUnlock()
	if !active {
		s.fetchMu.Lock()
		delete(s.metadataFetches, hash)
		s.fetchMu.Unlock()
		cancel()
		return
	}
	go func() {
		defer s.bgWg.Done()
		defer cancel()
		defer close(fetch.done)
		defer func() {
			s.fetchMu.Lock()
			retry := fetch.retry
			if s.metadataFetches[hash] == fetch {
				delete(s.metadataFetches, hash)
			}
			s.fetchMu.Unlock()
			if retry {
				s.retryMetadataFetch(hash)
			}
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
		defer unlock()
		s.mu.Lock()
		if err := s.ensureActiveLocked(); err != nil || s.deletionPendingLocked(hash) {
			s.mu.Unlock()
			return
		}
		created, spec, err := s.publishMetainfo(ctx, hash, buf.Bytes())
		if err == nil {
			entry := cloneRegistryEntry(s.states[hash])
			if entry == nil {
				s.mu.Unlock()
				if created {
					_ = s.removeFinalMetainfo(hash)
				}
				s.recordMetadataFetchFailure(hash, errors.New("missing registry entry"))
				return
			}
			entry.State = StateReady
			entry.Name = firstNonEmpty(st.Name(), specName(spec), entry.Name)
			entry.Error = ""
			entry.OperationID = ""
			if err = s.writeRegistryEntryLocked(entry); err == nil {
				s.states[hash] = entry
			}
		}
		s.mu.Unlock()
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrDeleting) {
				s.recordMetadataFetchFailure(hash, err)
			}
			return
		}
		if err := s.removePendingMagnet(hash); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("stale pending magnet cleanup failed", "hash", hash.HexString(), "err", err)
		}
	}()
}

func (s *Session) retryMetadataFetch(hash metainfo.Hash) {
	unlock := s.lockHash(hash)
	s.mu.Lock()
	entry := cloneRegistryEntry(s.states[hash])
	st := s.torrents[hash]
	if s.state != stateActive || entry == nil || st == nil ||
		(entry.State != StateAdding && entry.State != StateError) {
		s.mu.Unlock()
		unlock()
		return
	}
	entry.State = StateAdding
	entry.Error = ""
	entry.OperationID = ""
	if err := s.writeRegistryEntryLocked(entry); err != nil {
		s.mu.Unlock()
		unlock()
		s.logger.Error("persist metadata retry state", "hash", hash.HexString(), "err", err)
		return
	}
	s.states[hash] = entry
	s.mu.Unlock()
	unlock()

	s.fetchMu.Lock()
	running := s.metadataFetches[hash] != nil
	s.fetchMu.Unlock()
	if !running {
		s.startMetadataFetch(hash, st)
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

// ListTorrents returns a snapshot of every registry task, including tasks whose
// deletion is still running and whose final state is delete_failed.
func (s *Session) ListTorrents() []TorrentView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != stateActive {
		return nil
	}
	hashes := make([]metainfo.Hash, 0, len(s.states))
	for hash := range s.states {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].HexString() < hashes[j].HexString() })
	views := make([]TorrentView, 0, len(hashes))
	for _, hash := range hashes {
		views = append(views, s.buildView(hash, s.torrents[hash], s.states[hash]))
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
	entry, ok := s.states[hash]
	if !ok {
		return TorrentView{}, ErrUnknownTorrent
	}
	return s.buildView(hash, s.torrents[hash], entry), nil
}

func (s *Session) viewFor(hash metainfo.Hash, st *Torrent) *TorrentView {
	s.mu.RLock()
	entry := cloneRegistryEntry(s.states[hash])
	s.mu.RUnlock()
	view := s.buildView(hash, st, entry)
	return &view
}

// buildView snapshots one torrent for the management API. Its state is a
// lifecycle stage, never a completion percentage.
func (s *Session) buildView(hash metainfo.Hash, st *Torrent, entry *registryEntry) TorrentView {
	return s.buildViewWithCached(hash, st, entry, s.pieceCache.SizeOf(hash.HexString()))
}

// buildViewWithCached builds a torrent view from a cache occupancy the caller
// already measured.
func (s *Session) buildViewWithCached(hash metainfo.Hash, st *Torrent, entry *registryEntry, cachedBytes int64) TorrentView {
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
	view.Name = firstNonEmpty(view.Name, st.Name())
	view.TotalBytes = st.Length()
	view.CachedBytes = cachedBytes
	if entry == nil {
		if st.Info() != nil {
			view.State = StateReady
		}
	}
	return view
}

// DeleteTorrent starts the durable deletion of one torrent. The heavy cleanup
// runs in the background; the returned operation observes it through Operation.
func (s *Session) DeleteTorrent(ctx context.Context, id string) (*Operation, error) {
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
			out := op.clone()
			s.mu.Unlock()
			return &out, nil
		}
	}
	entry, ok := s.states[hash]
	if !ok {
		s.mu.Unlock()
		logFailure(ErrUnknownTorrent)
		return nil, ErrUnknownTorrent
	}
	opID := entry.OperationID
	if opID == "" {
		if last, ok := s.lastOps[hash]; ok {
			if op, ok := s.operations[last]; ok && op.State == StateDeleteFailed {
				opID = last
			}
		}
	}
	if opID == "" {
		opID, err = newOperationID()
		if err != nil {
			s.mu.Unlock()
			logFailure(err)
			return nil, err
		}
	}
	now := time.Now().UTC()
	entry = cloneRegistryEntry(entry)
	entry.State = StateDeleting
	entry.Error = ""
	entry.OperationID = opID
	if err := s.writeRegistryEntryLocked(entry); err != nil {
		s.mu.Unlock()
		logFailure(err)
		return nil, err
	}
	s.states[hash] = entry
	st := s.torrents[hash]
	delete(s.torrents, hash)
	op := &Operation{ID: opID, TorrentID: hash.HexString(), State: StateDeleting, CreatedAt: now, UpdatedAt: now}
	s.operations[opID] = op
	s.activeOps[hash] = opID
	s.lastOps[hash] = opID

	s.bgMu.Lock()
	s.bgWg.Add(1)
	s.bgMu.Unlock()
	out := op.clone()
	s.mu.Unlock()
	s.logger.Info("torrent deletion started", "hash", hash.HexString(), "operation_id", opID)
	go func() {
		defer s.bgWg.Done()
		s.runDelete(hash, st, opID)
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

func (s *Session) setDeleteFailedLocked(hash metainfo.Hash, err error) {
	entry := cloneRegistryEntry(s.states[hash])
	if entry != nil {
		entry.State = StateDeleteFailed
		entry.Error = err.Error()
		if writeErr := s.writeRegistryEntryLocked(entry); writeErr == nil {
			s.states[hash] = entry
		} else {
			s.logger.Error("persist delete failure", "hash", hash.HexString(), "err", writeErr)
		}
	}
}

// runDelete performs cleanup outside s.mu and records the outcome.
func (s *Session) runDelete(hash metainfo.Hash, st *Torrent, opID string) {
	if fetch := s.stopMetadataFetch(hash); fetch != nil {
		<-fetch.done
	}
	err := s.performDelete(hash, st)
	if err != nil {
		s.logger.Error("torrent deletion failed", "hash", hash.HexString(), "operation_id", opID, "err", err)
	} else {
		s.logger.Info("torrent deletion completed", "hash", hash.HexString(), "operation_id", opID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[opID]
	if err != nil {
		s.setDeleteFailedLocked(hash, err)
		if op != nil {
			op.State = StateDeleteFailed
			op.Error = err.Error()
			op.UpdatedAt = time.Now().UTC()
		}
		delete(s.activeOps, hash)
		return
	}
	if err := s.removeRegistryEntryLocked(hash); err != nil {
		s.setDeleteFailedLocked(hash, err)
		if op != nil {
			op.State = StateDeleteFailed
			op.Error = err.Error()
			op.UpdatedAt = time.Now().UTC()
		}
		delete(s.activeOps, hash)
		return
	}
	if op != nil {
		op.State = StateDeleted
		op.Error = ""
		op.UpdatedAt = time.Now().UTC()
	}
	delete(s.activeOps, hash)
}

// performDelete stops the task, releases its resources, and removes only files
// owned by the registry hash.
func (s *Session) performDelete(hash metainfo.Hash, st *Torrent) error {
	var errs []error
	if st != nil {
		if err := st.close(); err != nil {
			errs = append(errs, err)
		}
		st.tor.Drop()
	}
	if err := s.removeFinalMetainfo(hash); err != nil {
		errs = append(errs, err)
	}
	if err := s.removePendingMagnet(hash); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// resumeDeletions completes deletions interrupted by a crash before active
// registry entries are restored.
func (s *Session) resumeDeletions() error {
	s.mu.Lock()
	hashes := make([]metainfo.Hash, 0)
	for hash, entry := range s.states {
		if entry.State == StateDeleting {
			hashes = append(hashes, hash)
		}
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].HexString() < hashes[j].HexString() })
	items := make([]struct {
		hash metainfo.Hash
		st   *Torrent
		opID string
	}, 0, len(hashes))
	for _, hash := range hashes {
		entry := cloneRegistryEntry(s.states[hash])
		if entry.OperationID == "" {
			opID, err := newOperationID()
			if err != nil {
				s.mu.Unlock()
				return err
			}
			entry.OperationID = opID
			if err := s.writeRegistryEntryLocked(entry); err != nil {
				s.mu.Unlock()
				return err
			}
			s.states[hash] = entry
		}
		now := time.Now().UTC()
		opID := entry.OperationID
		s.operations[opID] = &Operation{ID: opID, TorrentID: hash.HexString(), State: StateDeleting, CreatedAt: now, UpdatedAt: now}
		s.activeOps[hash] = opID
		s.lastOps[hash] = opID
		items = append(items, struct {
			hash metainfo.Hash
			st   *Torrent
			opID string
		}{hash: hash, st: s.torrents[hash], opID: opID})
		delete(s.torrents, hash)
	}
	s.mu.Unlock()

	for _, item := range items {
		s.runDelete(item.hash, item.st, item.opID)
	}
	return nil
}
