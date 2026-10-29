package smb

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"math"
	"path"
	"strings"
	"sync"
	"time"

	wire "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	smbunicode "github.com/jfjallid/go-smb/smb/unicode"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

const (
	fileOpen             = 1
	fileOpenIf           = 3
	fileDirectoryFile    = 0x00000001
	fileNonDirectoryFile = 0x00000040
	fileDeleteOnClose    = 0x00001000
	readAccess           = 0x001200a9 // READ_CONTROL | SYNCHRONIZE | READ_DATA/EA/ATTRIBUTES | EXECUTE/TRAVERSE
	writeAccess          = 0x500d0156 // GENERIC_ALL/WRITE | DELETE | WRITE_DAC/OWNER | file writes
)

type filesystem struct {
	tree        *vfs.Tree
	readTimeout time.Duration
	namesMu     sync.Mutex
	indexes     map[uint64]*nameIndex
}

type handle struct {
	entry    vfs.Entry
	path     string
	file     *vfs.Handle
	owner    *filesystem
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	reads    sync.WaitGroup
	once     sync.Once
	closeErr error
}

func (h *handle) Stat() (server.FileInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return server.FileInfo{}, fs.ErrClosed
	}
	return fileInfo(h.entry), nil
}

func (h *handle) Path() string { return h.path }
func (h *handle) IsDir() bool  { return h.entry.IsDir }

func (h *handle) close() error {
	h.once.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.cancel()
		h.mu.Unlock()
		h.reads.Wait()
		if h.file != nil {
			h.closeErr = h.file.Close()
		}
	})
	return h.closeErr
}

func (f *filesystem) Create(ctx context.Context, _ *server.Session, req server.CreateRequest) (server.CreateResult, uint32, error) {
	if err := ctx.Err(); err != nil {
		return server.CreateResult{}, statusFor(err), nil
	}
	if req.DesiredAccess&writeAccess != 0 || req.CreateOptions&fileDeleteOnClose != 0 ||
		(req.CreateDisposition != fileOpen && req.CreateDisposition != fileOpenIf) {
		return server.CreateResult{}, wire.StatusAccessDenied, nil
	}
	entry, smbPath, err := f.resolve(req.Path)
	if err != nil {
		return server.CreateResult{}, statusFor(err), nil
	}
	if req.CreateOptions&fileDirectoryFile != 0 && !entry.IsDir {
		return server.CreateResult{}, statusNotADirectory, nil
	}
	if req.CreateOptions&fileNonDirectoryFile != 0 && entry.IsDir {
		return server.CreateResult{}, statusFileIsADirectory, nil
	}
	var opened *vfs.Handle
	if !entry.IsDir {
		opened, err = f.tree.Open(entry.Path)
		if err != nil {
			return server.CreateResult{}, statusFor(err), nil
		}
		// An opened subtitle or generated STRM binds metadata to these bytes,
		// not the potentially older directory listing used to resolve its name.
		name := entry.Name
		entry = opened.Metadata()
		entry.Name = name
	}
	hctx, cancel := context.WithCancel(context.Background())
	h := &handle{entry: entry, path: smbPath, file: opened, owner: f, ctx: hctx, cancel: cancel}
	return server.CreateResult{Handle: h, CreateAction: 1, Info: fileInfo(entry)}, wire.StatusOk, nil
}

func (f *filesystem) Close(_ context.Context, value server.Handle) error {
	h, ok := value.(*handle)
	if !ok || h.owner != f {
		return fs.ErrInvalid
	}
	return h.close()
}

func (f *filesystem) Read(ctx context.Context, value server.Handle, offset int64, buf []byte) (int, uint32, error) {
	h, ok := value.(*handle)
	if !ok || h.owner != f {
		return 0, wire.StatusFileClosed, nil
	}
	if offset < 0 {
		return 0, wire.StatusInvalidParameter, nil
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, wire.StatusFileClosed, nil
	}
	if h.entry.IsDir {
		h.mu.Unlock()
		return 0, statusFileIsADirectory, nil
	}
	h.reads.Add(1)
	rctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	h.mu.Unlock()
	defer h.reads.Done()
	defer cancel()
	defer stop()
	if f.readTimeout > 0 {
		var timeoutCancel context.CancelFunc
		rctx, timeoutCancel = context.WithTimeout(rctx, f.readTimeout)
		defer timeoutCancel()
	}
	if err := rctx.Err(); err != nil {
		return 0, statusFor(err), nil
	}
	n, err := h.file.ReadAtContext(rctx, buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		// Partial data must not turn cancellation, missing pieces or storage
		// errors into a successful short read which the client treats as EOF.
		return 0, statusFor(err), nil
	}
	if n == 0 && len(buf) > 0 {
		return 0, wire.StatusEndOfFile, nil
	}
	return n, wire.StatusOk, nil
}

func (f *filesystem) Write(context.Context, server.Handle, int64, []byte) (int, uint32, error) {
	return 0, wire.StatusAccessDenied, nil
}

func (f *filesystem) Flush(context.Context, server.Handle) (uint32, error) {
	return wire.StatusOk, nil
}

func (f *filesystem) QueryFileInfo(ctx context.Context, value server.Handle, class byte) (any, uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, statusFor(err), nil
	}
	h, ok := value.(*handle)
	if !ok || h.owner != f {
		return nil, wire.StatusFileClosed, nil
	}
	info, err := h.Stat()
	if err != nil {
		return nil, statusFor(err), nil
	}
	switch class {
	case 0x08: // FileAccessInformation
		out := make([]byte, 4)
		binary.LittleEndian.PutUint32(out, readAccess)
		return out, wire.StatusOk, nil
	case 0x16: // FileStreamInformation: only the unnamed default data stream.
		if h.IsDir() {
			return []byte{}, wire.StatusOk, nil
		}
		name := smbunicode.ToUnicode("::$DATA")
		out := make([]byte, 24+len(name))
		binary.LittleEndian.PutUint32(out[4:], uint32(len(name)))
		binary.LittleEndian.PutUint64(out[8:], uint64(info.Size))
		binary.LittleEndian.PutUint64(out[16:], uint64(info.AllocationSize))
		copy(out[24:], name)
		return out, wire.StatusOk, nil
	default:
		return nil, wire.StatusNotSupported, nil
	}
}

func (f *filesystem) SetFileInfo(context.Context, server.Handle, byte, []byte) (uint32, error) {
	return wire.StatusAccessDenied, nil
}

func (f *filesystem) QueryDirectory(ctx context.Context, value server.Handle, pattern string, _ bool) ([]server.DirEntry, uint32, error) {
	h, ok := value.(*handle)
	if !ok || h.owner != f {
		return nil, wire.StatusFileClosed, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, statusFor(err), nil
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return nil, wire.StatusFileClosed, nil
	}
	if !h.entry.IsDir {
		return nil, statusNotADirectory, nil
	}
	directory, err := f.tree.OpenDir(h.entry.Path)
	if err != nil {
		return nil, statusFor(err), nil
	}
	// The protocol server owns the continuation cursor and calls us again
	// only for a new/restarted scan. Never truncate a snapshot to one PDU.
	named := f.nameIndex(directory).entries
	out := make([]server.DirEntry, 0, len(named)+2)
	current := directory.Metadata()
	parent, err := f.tree.Lookup(path.Dir(current.Path))
	if err != nil {
		return nil, statusFor(err), nil
	}
	// Windows directory enumeration includes these virtual entries even in an
	// empty share. They never reference the host's parent directory.
	for _, dot := range []struct {
		name  string
		entry vfs.Entry
	}{{".", current}, {"..", parent}} {
		if wildcardMatch(pattern, dot.name) {
			info := fileInfo(dot.entry)
			info.Name = dot.name
			out = append(out, server.DirEntry{FileInfo: info})
		}
	}
	for _, entry := range named {
		if wildcardMatch(pattern, entry.Name) {
			out = append(out, server.DirEntry{FileInfo: fileInfo(entry)})
		}
	}
	return out, wire.StatusOk, nil
}

func (f *filesystem) QueryFSInfo(context.Context, byte) (any, uint32, error) {
	return nil, wire.StatusNotSupported, nil
}

func (f *filesystem) QuerySecurity(context.Context, server.Handle, uint32) ([]byte, uint32, error) {
	return nil, wire.StatusNotSupported, nil
}

func (f *filesystem) Ioctl(context.Context, server.Handle, uint32, []byte, uint32) ([]byte, uint32, error) {
	return nil, wire.StatusNotSupported, nil
}

func fileInfo(entry vfs.Entry) server.FileInfo {
	created := entry.CreatedAt
	if created.IsZero() {
		created = time.Unix(0, 0).UTC()
	}
	modified := entry.ModifiedAt
	if modified.IsZero() {
		modified = created
	}
	attributes := uint32(0x00000001) // FILE_ATTRIBUTE_READONLY
	if entry.IsDir {
		attributes |= 0x00000010 // FILE_ATTRIBUTE_DIRECTORY
	}
	allocation := entry.Size
	if allocation >= 0 && allocation <= math.MaxInt64-4095 {
		allocation = (allocation + 4095) / 4096 * 4096
	}
	return server.FileInfo{
		Name: entry.Name, Size: entry.Size, AllocationSize: allocation, Attributes: attributes,
		CreationTime: created, LastAccessTime: modified, LastWriteTime: modified,
		ChangeTime: modified, FileID: entry.Identity,
	}
}

const (
	statusNotADirectory     = 0xc0000103
	statusFileIsADirectory  = 0xc00000ba
	statusIOTimeout         = 0xc00000b5
	statusInvalidObjectName = 0xc0000033
	statusIOError           = 0xc0000185
)

func statusFor(err error) uint32 {
	switch {
	case err == nil:
		return wire.StatusOk
	case errors.Is(err, context.Canceled):
		return wire.StatusCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return statusIOTimeout
	case errors.Is(err, fs.ErrClosed), errors.Is(err, vfs.ErrClosed):
		return wire.StatusFileClosed
	case errors.Is(err, vfs.ErrIsDir):
		return statusFileIsADirectory
	case errors.Is(err, vfs.ErrNotDir):
		return statusNotADirectory
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, vfs.ErrNotFound):
		return wire.StatusObjectNameNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, vfs.ErrPermission), errors.Is(err, vfs.ErrReadOnly):
		return wire.StatusAccessDenied
	case errors.Is(err, fs.ErrInvalid), errors.Is(err, vfs.ErrInvalidName):
		return statusInvalidObjectName
	case errors.Is(err, io.EOF):
		return wire.StatusEndOfFile
	default:
		return statusIOError
	}
}

func (f *filesystem) resolve(raw string) (vfs.Entry, string, error) {
	parts, err := pathParts(raw)
	if err != nil {
		return vfs.Entry{}, "", err
	}
	entry, err := f.tree.Lookup("/")
	if err != nil {
		return vfs.Entry{}, "", err
	}
	var names []string
	for _, part := range parts {
		if !entry.IsDir {
			return vfs.Entry{}, "", fs.ErrNotExist
		}
		directory, readErr := f.tree.OpenDir(entry.Path)
		if readErr != nil {
			return vfs.Entry{}, "", readErr
		}
		child, found := f.nameIndex(directory).byName[foldName(part)]
		if !found {
			return vfs.Entry{}, "", fs.ErrNotExist
		}
		entry = child
		names = append(names, child.Name)
	}
	return entry, strings.Join(names, `\`), nil
}
