// Package vfs projects torrent session data into a protocol-independent,
// read-only virtual file tree.
package vfs

import (
	"errors"
	"io"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type FileView struct {
	Path string
	Size int64
}

type SubtitleView struct {
	Path       string
	VideoPath  string
	Size       int64
	ModifiedAt time.Time
}

type SubtitleStat struct {
	Size       int64
	ModifiedAt time.Time
}

// FileListID identifies a permanently immutable payload list. A new list must
// have a new identity; nil opts out of sharing and uses copy semantics.
type FileListID struct{ _ byte }

type TorrentContents struct {
	Files      []FileView
	FilesID    *FileListID
	Subtitles  []SubtitleView
	CreatedAt  time.Time
	SingleFile bool
}

type TorrentView struct {
	Name       string
	Hash       metainfo.Hash
	Category   string
	CreatedAt  time.Time
	Files      []FileView
	FilesID    *FileListID
	Subtitles  []SubtitleView
	SingleFile bool
}

type Backend interface {
	Torrents() []TorrentView
	OpenFile(metainfo.Hash, string) (io.ReaderAt, error)
}

type CategoryBackend interface {
	Categories() []string
}

type TorrentContentsBackend interface {
	TorrentContents(metainfo.Hash) (TorrentContents, error)
}

type FileStat struct {
	Size      int64
	CreatedAt time.Time
}

type FileSnapshot struct {
	Reader    io.ReaderAt
	Size      int64
	CreatedAt time.Time
}

type FileStatBackend interface {
	FileStat(metainfo.Hash, string) (FileStat, error)
}

type FileSnapshotBackend interface {
	OpenFileSnapshot(metainfo.Hash, string) (FileSnapshot, error)
}

// SubtitleSnapshot binds metadata to the same descriptor as the readable bytes.
type SubtitleSnapshot struct {
	Reader     io.ReaderAt
	Size       int64
	ModifiedAt time.Time
}

type SubtitleBackend interface {
	OpenSubtitle(metainfo.Hash, string) (SubtitleSnapshot, error)
}

type SubtitleStatBackend interface {
	SubtitleStat(metainfo.Hash, string) (SubtitleStat, error)
}

var (
	ErrExists      = errors.New("filesystem: entry exists")
	ErrNotFound    = errors.New("filesystem: entry not found")
	ErrIsDir       = errors.New("filesystem: entry is a directory")
	ErrNotDir      = errors.New("filesystem: entry is not a directory")
	ErrNotEmpty    = errors.New("filesystem: directory not empty")
	ErrReadOnly    = errors.New("filesystem: read-only")
	ErrPermission  = errors.New("filesystem: permission denied")
	ErrInvalidName = errors.New("filesystem: invalid name")
	ErrCrossDir    = errors.New("filesystem: cross-directory operation")
	ErrClosed      = errors.New("filesystem: closed")
)
