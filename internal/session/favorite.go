package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// errFavoriteSkip reports that an age-based prune left a favorite torrent in
// place. It never escapes the package.
var errFavoriteSkip = errors.New("session: favorite torrent is excluded from pruning")

// SetFavorite sets or clears the favorite flag on one torrent and persists it.
// Favorites exist to exempt a torrent from age-based pruning; they do not
// protect it from a direct delete.
func (s *Session) SetFavorite(ctx context.Context, id string, favorite bool) (TorrentView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hash, err := parseInfoHash(id)
	if err != nil {
		return TorrentView{}, ErrUnknownTorrent
	}
	unlock := s.lockHash(hash)
	defer unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return TorrentView{}, err
	}
	entry, ok := s.states[hash]
	if !ok {
		return TorrentView{}, ErrUnknownTorrent
	}
	switch entry.State {
	case StateDeleting, StateDeleteFailed:
		return TorrentView{}, ErrDeleting
	}
	updated := cloneRegistryEntry(entry)
	updated.Favorite = favorite
	if err := s.writeRegistryEntryLocked(updated); err != nil {
		s.logger.Error("torrent favorite update failed", "hash", hash.HexString(), "err", err)
		return TorrentView{}, err
	}
	s.states[hash] = updated
	s.logger.Info("torrent favorite updated", "hash", hash.HexString(), "favorite", favorite)
	return s.buildView(hash, s.torrents[hash], updated), nil
}

// PruneFailure records one candidate the batch could not turn into a deletion.
type PruneFailure struct {
	TorrentID string
	Error     string
}

// PruneResult reports one age-based prune pass.
type PruneResult struct {
	// Operations holds one entry per deletion that was actually started.
	Operations []Operation
	// ExcludedFavorites counts torrents older than the cutoff that were kept
	// because they are favorites.
	ExcludedFavorites int
	// Failures holds the candidates that matched but whose deletion could not
	// be started. One failure never stops the batch, so this can be non-empty
	// alongside a populated Operations.
	Failures []PruneFailure
}

// DeleteUnfavoritedOlderThan deletes every non-favorite torrent created more
// than olderThan ago. Favorites are never deleted, however old they are.
func (s *Session) DeleteUnfavoritedOlderThan(ctx context.Context, olderThan time.Duration) (PruneResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if olderThan < 0 {
		return PruneResult{}, fmt.Errorf("%w: older_than must not be negative", ErrInvalidSource)
	}
	cutoff := time.Now().UTC().Add(-olderThan)

	// Collect under the read lock, then release it before deleting: the delete
	// path takes the write lock and would self-deadlock if called from here.
	s.mu.RLock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.RUnlock()
		return PruneResult{}, err
	}
	candidates := make([]metainfo.Hash, 0, len(s.states))
	for hash, entry := range s.states {
		switch entry.State {
		case StateDeleting, StateDeleteFailed:
			continue
		}
		if !entry.CreatedAt.Before(cutoff) {
			continue
		}
		// Favorite is deliberately not checked here: the flag can change
		// between collection and deletion, so the real check happens inside
		// deleteByHash's critical section.
		candidates = append(candidates, hash)
	}
	s.mu.RUnlock()

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].HexString() < candidates[j].HexString()
	})

	var result PruneResult
	for _, hash := range candidates {
		op, err := s.deleteByHash(ctx, hash, true)
		switch {
		case errors.Is(err, errFavoriteSkip):
			result.ExcludedFavorites++
		case errors.Is(err, ErrUnknownTorrent), errors.Is(err, ErrDeleting):
			// Raced with a direct delete or the torrent left the registry;
			// skipping it keeps the rest of the batch going.
		case err != nil:
			s.logger.Error("prune delete failed", "hash", hash.HexString(), "err", err)
			result.Failures = append(result.Failures, PruneFailure{TorrentID: hash.HexString(), Error: err.Error()})
		default:
			result.Operations = append(result.Operations, op.clone())
		}
	}
	return result, nil
}
