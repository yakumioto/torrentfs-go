package filesystem

import (
	"context"
	"errors"
	iofs "io/fs"
	"os"
	"syscall"
)

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

func errnoFor(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}

	switch {
	case errors.Is(err, ErrExists), errors.Is(err, os.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, ErrNotFound), errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, ErrPermission), errors.Is(err, os.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, ErrInvalidName), errors.Is(err, iofs.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, ErrCrossDir):
		return syscall.EXDEV
	case errors.Is(err, ErrClosed):
		return syscall.EBADF
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return syscall.EINTR
	default:
		return syscall.EIO
	}
}
