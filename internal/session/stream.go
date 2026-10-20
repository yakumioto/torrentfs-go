package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/stream"
)

const streamKeyFileName = "stream_key"

// resolveStreamSigner runs under the instance lock, before any client resources
// are opened. A damaged key is never replaced automatically: doing so would
// silently revoke every previously generated STRM link.
func resolveStreamSigner(metadataDir string) (*stream.Signer, error) {
	name := filepath.Join(metadataDir, streamKeyFileName)
	info, err := requireRegularFile(name)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, stream.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("session: generate stream key: %w", err)
		}
		if err := writeFileAtomic(name, key); err != nil {
			return nil, fmt.Errorf("session: persist stream key: %w", err)
		}
		return stream.NewSigner(key)
	}
	if err != nil {
		return nil, fmt.Errorf("session: read stream key: %w", err)
	}
	// O_NOFOLLOW and the descriptor check keep a replaced path from redirecting
	// the read. O_NONBLOCK also prevents a concurrent replacement by a FIFO from
	// blocking startup before the regular-file check can run.
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("session: open stream key: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("session: stat stream key: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("session: stream key changed while opening")
	}
	if opened.Mode().Perm()&0o077 != 0 || opened.Mode().Perm()&0o400 == 0 {
		return nil, errors.New("session: stream key must be owner-readable only (chmod 0600)")
	}
	key, err := io.ReadAll(io.LimitReader(file, stream.KeySize+1))
	if err != nil {
		return nil, fmt.Errorf("session: read stream key: %w", err)
	}
	if len(key) != stream.KeySize {
		return nil, fmt.Errorf("session: stream key must contain exactly %d bytes; remove it only to generate a new key and invalidate existing STRM links", stream.KeySize)
	}
	return stream.NewSigner(key)
}

// StreamURL signs one existing payload file without opening its content. The
// address uses the immutable torrent-relative path, not its category or current
// FUSE display name, so reorganizing the library does not invalidate the link.
func (s *Session) StreamURL(hash metainfo.Hash, path string) (string, error) {
	if s.streamSigner == nil {
		return "", stream.ErrDisabled
	}
	if err := stream.ValidatePath(path); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, _, err := s.streamFileLocked(hash, path); err != nil {
		return "", err
	}
	return stream.BuildURL(s.cfg.STRM.BaseURL, hash, s.streamSigner.Sign(hash, path), path)
}

// OpenStream validates a file-scoped capability before opening the same reader
// used by FUSE. Closing one HTTP request releases only its own lightweight
// handle, leaving other playback and SMB requests alive.
func (s *Session) OpenStream(location stream.Location) (stream.File, error) {
	if s.streamSigner == nil {
		return stream.File{}, stream.ErrDisabled
	}
	if !s.streamSigner.Verify(location.Hash, location.Path, location.Signature) {
		return stream.File{}, stream.ErrInvalidSignature
	}
	if err := stream.ValidatePath(location.Path); err != nil {
		return stream.File{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	t, file, err := s.streamFileLocked(location.Hash, location.Path)
	if err != nil {
		return stream.File{}, err
	}
	reader, err := t.readerFor(location.Path)
	if err != nil {
		return stream.File{}, err
	}
	return stream.File{
		Reader:     reader,
		Size:       file.Length(),
		ModifiedAt: s.states[location.Hash].CreatedAt,
	}, nil
}

// streamFileLocked resolves a registered ready payload while s.mu is held.
func (s *Session) streamFileLocked(hash metainfo.Hash, path string) (*Torrent, *torrent.File, error) {
	if err := s.ensureActiveLocked(); err != nil {
		return nil, nil, err
	}
	entry := s.states[hash]
	t := s.torrents[hash]
	if entry == nil || entry.State != StateReady || t == nil || t.Info() == nil {
		return nil, nil, filesystem.ErrNotFound
	}
	file := t.fileByPath(path)
	if file == nil {
		return nil, nil, filesystem.ErrNotFound
	}
	return t, file, nil
}
