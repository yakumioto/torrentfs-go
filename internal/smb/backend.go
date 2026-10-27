package smb

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"iter"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

const readAccess uint32 = 0x80000000 | 0x20000000 | 0x02000000 | 0x001200a9

type backend struct {
	files   *filesystem.VirtualFS
	timeout time.Duration
}

func newBackend(source filesystem.Backend, timeout time.Duration) *backend {
	return &backend{files: filesystem.NewVirtualFS(source), timeout: timeout}
}

func smbPath(raw string) (string, error) {
	name := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(name, "//") || strings.ContainsRune(name, '\x00') || (len(name) >= 2 && name[1] == ':') {
		return "", fs.ErrInvalid
	}
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return "", nil
	}
	if !fs.ValidPath(name) || name == "." {
		return "", fs.ErrInvalid
	}
	return name, nil
}

func (b *backend) Open(ctx context.Context, opts vfs.OpenOptions) (vfs.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.DesiredAccess & ^readAccess != 0 || opts.Append || opts.CreateOptions&wire.FileDeleteOnClose != 0 {
		return nil, fs.ErrPermission
	}
	if opts.Disposition != vfs.DispositionOpen && opts.Disposition != vfs.DispositionOpenIf {
		return nil, fs.ErrPermission
	}
	name, err := smbPath(opts.Path)
	if err != nil {
		return nil, err
	}
	r, entry, err := b.files.Open(name)
	if err != nil {
		return nil, backendError(err)
	}
	if (opts.CreateDir && !entry.IsDir) || (opts.CreateOptions&0x40 != 0 && entry.IsDir) {
		if closer, ok := r.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, fs.ErrInvalid
	}
	handleCtx, cancel := context.WithCancel(ctx)
	return &handle{backend: b, name: name, reader: r, info: fileInfo(entry), ctx: handleCtx, cancel: cancel}, nil
}

func (b *backend) Remove(context.Context, string) error { return fs.ErrPermission }
func (b *backend) Mkdir(context.Context, string) error  { return fs.ErrPermission }

type handle struct {
	backend  *backend
	name     string
	reader   io.ReaderAt
	info     vfs.FileInfo
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.RWMutex
	closed   bool
	once     sync.Once
	closeErr error
}

type contextualReaderAt interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

func (h *handle) Read(ctx context.Context, offset int64, p []byte) (int, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed || h.ctx.Err() != nil {
		return 0, fs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if h.info.IsDir || offset < 0 {
		return 0, fs.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	if offset >= h.info.Size {
		return 0, io.EOF
	}
	if remaining := h.info.Size - offset; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	readCtx, cancel := context.WithTimeout(ctx, h.backend.timeout)
	stop := context.AfterFunc(h.ctx, cancel)
	defer func() { stop(); cancel() }()
	var n int
	var err error
	if reader, ok := h.reader.(contextualReaderAt); ok {
		n, err = reader.ReadAtContext(readCtx, p, offset)
	} else {
		n, err = h.reader.ReadAt(p, offset)
	}
	if readCtx.Err() != nil {
		return n, readCtx.Err()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, backendError(err)
	}
	if n != len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (h *handle) Write(context.Context, int64, []byte) (int, error)  { return 0, fs.ErrPermission }
func (h *handle) SetInfo(context.Context, *vfs.SetInfoRequest) error { return fs.ErrPermission }
func (h *handle) Rename(context.Context, string, bool) error         { return fs.ErrPermission }

func (h *handle) Close(context.Context) error {
	h.once.Do(func() {
		h.cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		h.closed = true
		if closer, ok := h.reader.(io.Closer); ok {
			h.closeErr = closer.Close()
		}
	})
	return h.closeErr
}

func (h *handle) Stat(ctx context.Context) (vfs.FileInfo, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed || h.ctx.Err() != nil {
		return vfs.FileInfo{}, fs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return vfs.FileInfo{}, err
	}
	return h.info, nil
}

func (h *handle) Enumerate(ctx context.Context, pattern string) iter.Seq2[vfs.FileInfo, error] {
	return func(yield func(vfs.FileInfo, error) bool) {
		info, err := h.Stat(ctx)
		if err != nil {
			yield(vfs.FileInfo{}, err)
			return
		}
		if !info.IsDir {
			yield(vfs.FileInfo{}, fs.ErrInvalid)
			return
		}
		if pattern == "" || pattern == "*.*" {
			pattern = "*"
		}
		if strings.ContainsAny(pattern, "/\\\x00") {
			yield(vfs.FileInfo{}, fs.ErrInvalid)
			return
		}
		if _, err := path.Match(pattern, ""); err != nil {
			yield(vfs.FileInfo{}, fs.ErrInvalid)
			return
		}
		entries, err := h.backend.files.ReadDir(h.name)
		if err != nil {
			yield(vfs.FileInfo{}, backendError(err))
			return
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				yield(vfs.FileInfo{}, err)
				return
			}
			if h.ctx.Err() != nil {
				yield(vfs.FileInfo{}, fs.ErrClosed)
				return
			}
			matched, _ := path.Match(strings.ToLower(pattern), strings.ToLower(entry.Name))
			if matched && !yield(fileInfo(entry), nil) {
				return
			}
		}
	}
}

func fileInfo(entry filesystem.VirtualEntry) vfs.FileInfo {
	modified := entry.ModifiedAt
	if modified.IsZero() {
		modified = entry.CreatedAt
	}
	return vfs.FileInfo{Name: entry.Name, Size: entry.Size, IsDir: entry.IsDir, Attributes: 1,
		CreationTime: entry.CreatedAt, LastAccess: modified, LastWrite: modified, ChangeTime: modified}
}

func backendError(err error) error {
	switch {
	case errors.Is(err, filesystem.ErrNotFound):
		return fs.ErrNotExist
	case errors.Is(err, filesystem.ErrClosed):
		return fs.ErrClosed
	case errors.Is(err, filesystem.ErrPermission), errors.Is(err, filesystem.ErrReadOnly):
		return fs.ErrPermission
	case errors.Is(err, filesystem.ErrInvalidName), errors.Is(err, filesystem.ErrNotDir), errors.Is(err, filesystem.ErrIsDir):
		return fs.ErrInvalid
	default:
		return err
	}
}

var (
	_ vfs.Backend   = (*backend)(nil)
	_ vfs.Remover   = (*backend)(nil)
	_ vfs.Mkdirer   = (*backend)(nil)
	_ vfs.Handle    = (*handle)(nil)
	_ vfs.SetInfoer = (*handle)(nil)
	_ vfs.Renamer   = (*handle)(nil)
)
