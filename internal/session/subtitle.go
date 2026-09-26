package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/sys/unix"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

const (
	SubtitleCodeNameMismatch       = "subtitle_name_mismatch"
	SubtitleCodeFormatUnsupported  = "subtitle_format_unsupported"
	SubtitleCodeVideoNotFound      = "subtitle_video_not_found"
	SubtitleCodeNameConflict       = "subtitle_name_conflict"
	SubtitleCodePayloadConflict    = "subtitle_payload_conflict"
	SubtitleCodeStorageUnavailable = "subtitle_storage_unavailable"
	SubtitleCodeStorageFull        = "subtitle_storage_full"
	SubtitleCodeWriteFailed        = "subtitle_write_failed"
	SubtitleCodeTorrentDeleting    = "torrent_deleting"
	SubtitleCodeCleanupFailed      = "subtitle_cleanup_failed"
	SubtitleCodeUploadTooLarge     = "subtitle_upload_too_large"
	// SubtitleCodeNamespaceConflict is reported when an add is refused because it
	// would break an existing managed subtitle's mount path.
	SubtitleCodeNamespaceConflict = "subtitle_namespace_conflict"
)

var (
	ErrSubtitleNameMismatch       = errors.New("subtitle name does not match video")
	ErrSubtitleFormatUnsupported  = errors.New("subtitle format is unsupported")
	ErrSubtitleVideoNotFound      = errors.New("subtitle video was not found")
	ErrSubtitleNameConflict       = errors.New("subtitle name is ambiguous")
	ErrSubtitlePayloadConflict    = errors.New("subtitle path belongs to torrent payload")
	ErrSubtitleStorageUnavailable = errors.New("subtitle storage is unavailable")
	ErrSubtitleStorageFull        = errors.New("subtitle storage is full")
	ErrSubtitleWriteFailed        = errors.New("subtitle write failed")
	ErrSubtitleCleanupFailed      = errors.New("subtitle cleanup failed")
	ErrSubtitleUploadTooLarge     = errors.New("subtitle upload is too large")
	// ErrSubtitleNamespaceConflict means adding a torrent would change or shadow
	// an existing managed subtitle's mount path.
	ErrSubtitleNamespaceConflict = errors.New("subtitle namespace conflict")
)

var subtitleCodeMessages = map[string]string{
	SubtitleCodeNameMismatch:       "字幕文件名必须与目标视频的名称严格对应。",
	SubtitleCodeFormatUnsupported:  "仅支持小写 .srt、.ass 或 .vtt 字幕文件。",
	SubtitleCodeVideoNotFound:      "目标视频不存在或尚未准备好。",
	SubtitleCodeNameConflict:       "目标视频的字幕名称存在歧义，无法安全上传。",
	SubtitleCodePayloadConflict:    "目标路径属于种子自带文件，不能覆盖。",
	SubtitleCodeStorageUnavailable: "字幕存储当前不可用。",
	SubtitleCodeStorageFull:        "字幕存储空间不足。",
	SubtitleCodeWriteFailed:        "字幕文件写入失败。",
	SubtitleCodeTorrentDeleting:    "任务正在删除，暂时不能上传字幕。",
	SubtitleCodeCleanupFailed:      "字幕文件清理失败。",
	SubtitleCodeUploadTooLarge:     "字幕文件超过后台服务的大小限制。",
}

// SubtitleTarget describes one server-derived video target.
type SubtitleTarget struct {
	VideoPath        string
	MountPath        string
	ExpectedBasename string
	Uploadable       bool
	Reason           string
}

// Subtitle describes one managed subtitle sidecar.
type Subtitle struct {
	TorrentID string
	VideoPath string
	Path      string
	MountPath string
	Format    string
	Size      int64
	UpdatedAt time.Time
}

// SubtitleUploadResponse is returned after a subtitle is created or replaced.
type SubtitleUploadResponse struct {
	TorrentID string    `json:"torrent_id"`
	VideoPath string    `json:"video_path"`
	Path      string    `json:"path"`
	MountPath string    `json:"mount_path"`
	Format    string    `json:"format"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
	Replaced  bool      `json:"replaced"`
}

type managedSubtitle struct {
	VideoPath string
	Path      string
	Format    string
	Size      int64
	UpdatedAt time.Time
}

type subtitleError struct {
	code  string
	cause error
}

func (e *subtitleError) Error() string {
	if message := subtitleCodeMessages[e.code]; message != "" {
		return message
	}
	return e.code
}

func (e *subtitleError) Unwrap() error     { return e.cause }
func (e *subtitleError) ErrorCode() string { return e.code }

func newSubtitleError(code string, cause error) error {
	return &subtitleError{code: code, cause: cause}
}

// SubtitleErrorCode returns the stable API code carried by err, if any.
func SubtitleErrorCode(err error) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}

var videoExtensions = map[string]struct{}{
	".3gp": {}, ".avi": {}, ".flv": {}, ".m2ts": {}, ".m4v": {},
	".mkv": {}, ".mov": {}, ".mp4": {}, ".mpeg": {}, ".mpg": {},
	".mts": {}, ".ts": {}, ".webm": {}, ".wmv": {},
}

var subtitleExtensions = map[string]struct{}{".srt": {}, ".ass": {}, ".vtt": {}}

// Upload staging stages, named so a fault can be attributed to one of them.
// subtitleStageParents probes the window between the store checks and the
// staging create; the next four run before the publish rename; and
// subtitleStageCommit is the post-commit durability step, which cannot fail the
// upload.
const (
	subtitleStageParents = "parents"
	subtitleStageCreate  = "create"
	subtitleStageWrite   = "write"
	subtitleStageSync    = "sync"
	subtitleStageRename  = "rename"
	subtitleStageCommit  = "commit"
)

// subtitleStagingName names one staging file. The name is generated here rather
// than derived from client input, and it is the prefix the startup scan cleans
// up if a crash leaves one behind.
func subtitleStagingName() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".torrentfs-subtitle-" + hex.EncodeToString(raw[:]) + ".tmp", nil
}

// subtitleIOFault, when set, forces the named upload stage to fail. It is a
// test-only seam: production never sets it.
var subtitleIOFault func(stage string) error

func subtitleStageFault(stage string) error {
	if subtitleIOFault == nil {
		return nil
	}
	return subtitleIOFault(stage)
}

// subtitleIOError keeps the failing stage's cause while mapping it onto a
// stable code, so a full disk is reported as one and a broken store as another,
// and callers can still tell ENOSPC from EACCES with errors.Is.
func subtitleIOError(stage string, err error) error {
	switch {
	case isStorageFull(err):
		return newSubtitleError(SubtitleCodeStorageFull, fmt.Errorf("subtitle %s: %w", stage, err))
	case isStorageUnavailable(err):
		return newSubtitleError(SubtitleCodeStorageUnavailable, fmt.Errorf("subtitle %s: %w", stage, err))
	default:
		return newSubtitleError(SubtitleCodeWriteFailed, fmt.Errorf("subtitle %s: %w", stage, err))
	}
}

// subtitleCommitSteps runs the work that follows a successful publish rename:
// making the new directory entry durable and verifying the file that is now
// visible. It is the post-commit half of an upload, so its error is logged by
// the caller rather than reported, and the returned error joins every failing
// step so the log explains all of them. Both paths stay inside the store.
func subtitleCommitSteps(store *os.Root, relDir, relDestination string, size int64) error {
	var errs []error
	if err := subtitleStageFault(subtitleStageCommit); err != nil {
		errs = append(errs, fmt.Errorf("directory sync: %w", err))
	} else if err := syncRootDirectory(store, relDir); err != nil {
		errs = append(errs, fmt.Errorf("directory sync: %w", err))
	}
	if info, err := store.Lstat(relDestination); err != nil {
		errs = append(errs, fmt.Errorf("verify published file: %w", err))
	} else if !info.Mode().IsRegular() {
		errs = append(errs, fmt.Errorf("published file %q is not a regular file", relDestination))
	} else if info.Size() != size {
		errs = append(errs, fmt.Errorf("published file %q is %d bytes, wrote %d", relDestination, info.Size(), size))
	}
	return errors.Join(errs...)
}

// syncRootDirectory fsyncs a directory inside a Root by opening it through the
// same handle, so the sync cannot be redirected by a path swap either.
func syncRootDirectory(store *os.Root, relDir string) error {
	dir, err := store.Open(relDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// subtitleStoreError covers the pre-write store checks: a full disk is still a
// full disk, while any other failure means the managed store is unusable.
func subtitleStoreError(err error) error {
	if isStorageFull(err) {
		return newSubtitleError(SubtitleCodeStorageFull, fmt.Errorf("subtitle store: %w", err))
	}
	return newSubtitleError(SubtitleCodeStorageUnavailable, fmt.Errorf("subtitle store: %w", err))
}

// isStorageFull reports whether err means the store ran out of space or quota.
func isStorageFull(err error) bool {
	return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT)
}

// isStorageUnavailable reports whether err means the store cannot be written at
// all, which is an operator-fixable permission or filesystem condition rather
// than a transient write failure.
func isStorageUnavailable(err error) bool {
	return errors.Is(err, unix.EROFS) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

// subtitleCleanupHook, when set, forces the subtitle cleanup stage of a
// deletion to fail. It is a test-only seam: production never sets it.
var subtitleCleanupHook func(metainfo.Hash) error

func subtitleDirName(hash metainfo.Hash) string { return hash.HexString() }

func (s *Session) subtitleDir(hash metainfo.Hash) string {
	return filepath.Join(s.subtitleRoot, subtitleDirName(hash))
}

func ensureManagedDirectory(dir string, mode os.FileMode) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, mode); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path %q must be a directory", dir)
	}
	return nil
}

func validateSubtitleRelativePath(raw string) error {
	if raw == "" || path.IsAbs(raw) || strings.ContainsAny(raw, "\\\x00") || path.Clean(raw) != raw {
		return fmt.Errorf("invalid relative path %q", raw)
	}
	for _, component := range strings.Split(raw, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("invalid relative path %q", raw)
		}
	}
	return nil
}

func subtitleExtension(name string) (string, bool) {
	ext := path.Ext(name)
	if ext == "" || ext != strings.ToLower(ext) {
		return "", false
	}
	_, ok := subtitleExtensions[ext]
	return ext, ok
}

// videoExtension reports whether name carries a supported video extension. The
// comparison is case-insensitive, and the returned extension is the file's own
// spelling, so callers must not use it to cut bytes off the name.
func videoExtension(name string) (string, bool) {
	ext := path.Ext(name)
	_, ok := videoExtensions[strings.ToLower(ext)]
	return ext, ok
}

// basenameStem is the basename with its final extension removed, keeping the
// original case: the extension is cut by its actual length, never by a
// lowercased copy, so "Movie.MKV" has stem "Movie" and not "Movie.MKV".
func basenameStem(name string) string {
	base := path.Base(name)
	return strings.TrimSuffix(base, path.Ext(base))
}

func subtitleTargetPath(videoPath, subtitleName string) string {
	dir := path.Dir(videoPath)
	if dir == "." {
		return subtitleName
	}
	return dir + "/" + subtitleName
}

func mountPath(rootName, relPath string, single bool) string {
	if single {
		return relPath
	}
	return rootName + "/" + relPath
}

type videoCandidate struct {
	path       string
	stem       string
	directory  string
	rootName   string
	single     bool
	uploadable bool
}

func (s *Session) subtitleTargetMapLocked(hash metainfo.Hash, st *Torrent) (map[string]SubtitleTarget, error) {
	if st == nil || st.Info() == nil {
		return map[string]SubtitleTarget{}, nil
	}
	allViews := s.filesystemViewsLocked()
	var current filesystem.TorrentView
	found := false
	for _, view := range allViews {
		if view.Hash == hash {
			current = view
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("session: torrent %s is not ready", hash)
	}
	rootName, _ := filesystem.RootNameFor(current, allViews)
	candidates := make([]videoCandidate, 0)
	for _, file := range st.tor.Files() {
		if _, ok := videoExtension(file.DisplayPath()); !ok || validateSubtitleRelativePath(file.DisplayPath()) != nil {
			continue
		}
		base := path.Base(file.DisplayPath())
		candidates = append(candidates, videoCandidate{
			path:       file.DisplayPath(),
			stem:       basenameStem(base),
			directory:  path.Dir(file.DisplayPath()),
			rootName:   rootName,
			single:     current.SingleFile,
			uploadable: true,
		})
	}
	result := make(map[string]SubtitleTarget, len(candidates))
	rootNames := make(map[string]struct{}, len(allViews))
	for _, view := range allViews {
		if name, ok := filesystem.RootNameFor(view, allViews); ok {
			rootNames[name] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		matching := 0
		for _, other := range candidates {
			if other.directory == candidate.directory && other.stem == candidate.stem {
				matching++
			}
		}
		nameConflict := matching > 1
		if candidate.single && candidate.rootName != candidate.path {
			nameConflict = true
		}
		base := path.Base(candidate.path)
		expectedBasename := basenameStem(base)
		defaultPath := subtitleTargetPath(candidate.path, expectedBasename+".srt")
		if candidate.single {
			if _, exists := rootNames[path.Base(defaultPath)]; exists && path.Base(defaultPath) != candidate.rootName {
				nameConflict = true
			}
		}
		reason := ""
		if nameConflict {
			reason = SubtitleCodeNameConflict
		}
		result[candidate.path] = SubtitleTarget{
			VideoPath:        candidate.path,
			MountPath:        mountPath(candidate.rootName, defaultPath, candidate.single),
			ExpectedBasename: expectedBasename,
			Uploadable:       !nameConflict,
			Reason:           reason,
		}
	}
	return result, nil
}

func (s *Session) subtitleTargetsLocked(hash metainfo.Hash, st *Torrent) ([]SubtitleTarget, error) {
	mapping, err := s.subtitleTargetMapLocked(hash, st)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(mapping))
	for videoPath := range mapping {
		paths = append(paths, videoPath)
	}
	sort.Strings(paths)
	out := make([]SubtitleTarget, 0, len(paths))
	for _, videoPath := range paths {
		out = append(out, mapping[videoPath])
	}
	return out, nil
}

func (s *Session) subtitleViewsLocked(hash metainfo.Hash) []filesystem.SubtitleView {
	records := s.subtitles[hash]
	if len(records) == 0 {
		return nil
	}
	paths := make([]string, 0, len(records))
	for relPath := range records {
		paths = append(paths, relPath)
	}
	sort.Strings(paths)
	out := make([]filesystem.SubtitleView, 0, len(paths))
	for _, relPath := range paths {
		record := records[relPath]
		out = append(out, filesystem.SubtitleView{
			Path:       record.Path,
			VideoPath:  record.VideoPath,
			Size:       record.Size,
			ModifiedAt: record.UpdatedAt,
		})
	}
	return out
}

func (s *Session) subtitlesLocked(hash metainfo.Hash, rootName string, single bool) []Subtitle {
	records := s.subtitles[hash]
	if len(records) == 0 {
		return []Subtitle{}
	}
	paths := make([]string, 0, len(records))
	for relPath := range records {
		paths = append(paths, relPath)
	}
	sort.Strings(paths)
	out := make([]Subtitle, 0, len(paths))
	for _, relPath := range paths {
		record := records[relPath]
		out = append(out, Subtitle{
			TorrentID: hash.HexString(),
			VideoPath: record.VideoPath,
			Path:      record.Path,
			MountPath: mountPath(rootName, record.Path, single),
			Format:    record.Format,
			Size:      record.Size,
			UpdatedAt: record.UpdatedAt,
		})
	}
	return out
}

func (s *Session) UploadSubtitle(ctx context.Context, id, videoPath, fileName string, src io.Reader, maxBytes int64) (SubtitleUploadResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hash, err := parseInfoHash(id)
	if err != nil {
		return SubtitleUploadResponse{}, ErrUnknownTorrent
	}
	if src == nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, ErrSubtitleWriteFailed)
	}
	if err := validateSubtitleRelativePath(videoPath); err != nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeVideoNotFound, ErrSubtitleVideoNotFound)
	}
	if err := validateSubtitleFileName(fileName); err != nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeFormatUnsupported, ErrSubtitleFormatUnsupported)
	}
	ext, ok := subtitleExtension(fileName)
	if !ok {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeFormatUnsupported, ErrSubtitleFormatUnsupported)
	}
	// The mount-root layout must not change between validating this target and
	// publishing the subtitle, or the returned mount path and the video it
	// belongs to could disagree with what the mount actually shows. Holding the
	// namespace lock across the staging I/O is what keeps a concurrent add's
	// guard from deciding on a layout this upload is about to change.
	s.rootNamespaceMu.Lock()
	defer s.rootNamespaceMu.Unlock()
	unlock := s.lockHash(hash)
	defer unlock()

	s.mu.RLock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, err
	}
	entry := s.states[hash]
	st := s.torrents[hash]
	if entry == nil {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, ErrUnknownTorrent
	}
	if entry.State == StateDeleting || entry.State == StateDeleteFailed {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeTorrentDeleting, ErrDeleting)
	}
	if entry.State != StateReady || st == nil || st.Info() == nil {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeVideoNotFound, ErrSubtitleVideoNotFound)
	}
	targets, err := s.subtitleTargetMapLocked(hash, st)
	if err != nil {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeVideoNotFound, err)
	}
	target, ok := targets[videoPath]
	if !ok {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeVideoNotFound, ErrSubtitleVideoNotFound)
	}
	if !target.Uploadable {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeNameConflict, ErrSubtitleNameConflict)
	}
	expected := basenameStem(videoPath)
	if basenameStem(fileName) != expected {
		s.mu.RUnlock()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeNameMismatch, ErrSubtitleNameMismatch)
	}
	relPath := subtitleTargetPath(videoPath, fileName)
	for _, file := range st.tor.Files() {
		if file.DisplayPath() == relPath {
			s.mu.RUnlock()
			return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodePayloadConflict, ErrSubtitlePayloadConflict)
		}
	}
	current := filesystem.TorrentView{Hash: hash, Name: st.Name(), SingleFile: !st.Info().IsDir()}
	allViews := s.filesystemViewsLocked()
	for _, view := range allViews {
		if view.Hash == hash {
			current = view
			break
		}
	}
	rootName, _ := filesystem.RootNameFor(current, allViews)
	_, replacing := s.subtitles[hash][relPath]
	s.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, err)
	}
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	// Every path below is resolved relative to the managed store's own directory
	// handle, so a host process that swaps a path component for a symlink between
	// the checks and the write cannot redirect the staging file or the published
	// subtitle outside the store.
	store, err := os.OpenRoot(s.subtitleRoot)
	if err != nil {
		return SubtitleUploadResponse{}, subtitleStoreError(err)
	}
	defer func() { _ = store.Close() }()
	relDir := filepath.Join(subtitleDirName(hash), filepath.FromSlash(path.Dir(relPath)))
	relDestination := filepath.Join(subtitleDirName(hash), filepath.FromSlash(relPath))
	if err := store.MkdirAll(relDir, 0o700); err != nil {
		return SubtitleUploadResponse{}, subtitleStoreError(err)
	}
	if info, statErr := store.Lstat(relDestination); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return SubtitleUploadResponse{}, subtitleStoreError(fmt.Errorf("subtitle path %q is not a regular file", relPath))
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return SubtitleUploadResponse{}, subtitleStoreError(statErr)
	}

	// The stage probe runs after the store checks and before the staging file
	// exists, which is the window a host-side symlink swap would use.
	if err := subtitleStageFault(subtitleStageParents); err != nil {
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageParents, err)
	}
	stagingName, err := subtitleStagingName()
	if err != nil {
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageCreate, err)
	}
	if err := subtitleStageFault(subtitleStageCreate); err != nil {
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageCreate, err)
	}
	relStaging := filepath.Join(relDir, stagingName)
	file, err := store.OpenFile(relStaging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageCreate, err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = store.Remove(relStaging)
	}
	if err := subtitleStageFault(subtitleStageWrite); err != nil {
		cleanup()
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageWrite, err)
	}
	limited := io.LimitReader(&contextReader{ctx: ctx, reader: src}, maxBytes+1)
	written, copyErr := io.Copy(file, limited)
	if copyErr != nil {
		cleanup()
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageWrite, copyErr)
	}
	if written > maxBytes {
		cleanup()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeUploadTooLarge, ErrSubtitleUploadTooLarge)
	}
	if err := subtitleStageFault(subtitleStageSync); err != nil {
		cleanup()
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageSync, err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageSync, err)
	}
	if err := file.Chmod(0o444); err != nil {
		cleanup()
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageSync, err)
	}
	// The published file keeps the staging file's inode, so its size and mtime
	// are read before the rename that makes it visible. Reading them afterwards
	// would add a failure point past the commit point below.
	staged, err := file.Stat()
	if err != nil {
		cleanup()
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageSync, err)
	}
	if err := file.Close(); err != nil {
		_ = store.Remove(relStaging)
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageSync, err)
	}
	if err := subtitleStageFault(subtitleStageRename); err != nil {
		_ = store.Remove(relStaging)
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageRename, err)
	}
	if err := store.Rename(relStaging, relDestination); err != nil {
		_ = store.Remove(relStaging)
		return SubtitleUploadResponse{}, subtitleIOError(subtitleStageRename, err)
	}
	// The rename is the commit point: the destination is the new content from
	// here on, so the durability and verification steps below cannot roll it
	// back. Their failure is reported as a warning instead of a failed upload,
	// because a caller told the write failed would retry a file that is already
	// live, and the index would disagree with the mount about what exists.
	if err := subtitleCommitSteps(store, relDir, relDestination, written); err != nil {
		s.logger.Warn("subtitle durability step failed after publish", "hash", hash.HexString(), "path", relPath, "err", err)
	}
	updatedAt := staged.ModTime().UTC()
	record := managedSubtitle{VideoPath: videoPath, Path: relPath, Format: strings.TrimPrefix(ext, "."), Size: written, UpdatedAt: updatedAt}
	s.mu.Lock()
	if s.subtitles[hash] == nil {
		s.subtitles[hash] = make(map[string]managedSubtitle)
	}
	s.subtitles[hash][relPath] = record
	s.mu.Unlock()
	return SubtitleUploadResponse{
		TorrentID: hash.HexString(),
		VideoPath: videoPath,
		Path:      relPath,
		MountPath: mountPath(rootName, relPath, current.SingleFile),
		Format:    record.Format,
		Size:      record.Size,
		UpdatedAt: record.UpdatedAt,
		Replaced:  replacing,
	}, nil
}

func validateSubtitleFileName(name string) error {
	if name == "" || path.Base(name) != name || strings.ContainsAny(name, "\\\x00") {
		return ErrSubtitleFormatUnsupported
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}

// subtitleNamespaceConflictLocked reports why exposing one more torrent at the
// mount root would break an existing managed subtitle's correspondence. Root
// names are assigned by hash order, so a new single-file torrent can rename an
// older video out from under its subtitle, and a new root name can shadow a
// subtitle entry outright. Both cases are refused before the torrent is
// published, instead of silently renaming or hiding either side.
//
// Callers must hold s.mu (the subtitle index is read) and rootNamespaceMu (a
// subtitle upload in flight must not be invisible to this decision).
func (s *Session) subtitleNamespaceConflictLocked(candidate metainfo.Hash, name string, single bool) error {
	views := s.filesystemViewsLocked()
	existing := make([]filesystem.TorrentView, 0, len(views)+1)
	protected := false
	for _, view := range views {
		if view.Hash == candidate {
			continue
		}
		existing = append(existing, view)
		if view.SingleFile && len(s.subtitles[view.Hash]) > 0 {
			protected = true
		}
	}
	// Without an existing single-file subtitle there is nothing to protect, and
	// the projected layout is not worth computing.
	if !protected {
		return nil
	}
	projected := append(existing, filesystem.TorrentView{Hash: candidate, Name: name, SingleFile: single})
	return s.subtitleLayoutConflictLocked(projected)
}

// subtitleLayoutConflictLocked validates one whole mount-root layout: every
// single-file torrent with managed subtitles must keep its own root name, and
// no root entry may shadow a root-level subtitle path.
func (s *Session) subtitleLayoutConflictLocked(views []filesystem.TorrentView) error {
	rootNames := make(map[string]struct{}, len(views))
	for _, view := range views {
		if rootName, ok := filesystem.RootNameFor(view, views); ok {
			rootNames[rootName] = struct{}{}
		}
	}
	for _, view := range views {
		if !view.SingleFile || len(s.subtitles[view.Hash]) == 0 {
			continue
		}
		rootName, ok := filesystem.RootNameFor(view, views)
		if !ok || rootName != view.Name {
			return fmt.Errorf("%w: torrent %s would be renamed at the mount root", ErrSubtitleNamespaceConflict, view.Hash)
		}
		for relPath := range s.subtitles[view.Hash] {
			if _, shadowed := rootNames[relPath]; !strings.Contains(relPath, "/") && shadowed {
				return fmt.Errorf("%w: root name %q is already a managed subtitle", ErrSubtitleNamespaceConflict, relPath)
			}
		}
	}
	return nil
}

// subtitleVideoFor resolves the single video a stored subtitle belongs to. The
// sidecar must sit in that video's directory and carry exactly that video's
// expected basename; a sidecar that matches no video, or more than one, is
// rejected instead of being adopted, so a host-planted file is never exposed
// and a legal subtitle is never claimed by its directory neighbour.
func subtitleVideoFor(mapping map[string]SubtitleTarget, relPath string) (string, error) {
	stem := basenameStem(relPath)
	if stem == "" {
		return "", fmt.Errorf("session: subtitle %s has no basename", relPath)
	}
	dir := path.Dir(relPath)
	videoPath := ""
	for candidatePath, candidate := range mapping {
		if path.Dir(candidatePath) != dir || candidate.ExpectedBasename != stem {
			continue
		}
		if videoPath != "" {
			return "", fmt.Errorf("session: subtitle %s matches more than one video", relPath)
		}
		videoPath = candidatePath
	}
	if videoPath == "" {
		return "", fmt.Errorf("session: subtitle %s matches no video basename", relPath)
	}
	return videoPath, nil
}

func (s *Session) loadManagedSubtitles() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.subtitleRoot)
	if err != nil {
		return fmt.Errorf("session: scan subtitle dir: %w", err)
	}
	for _, entry := range entries {
		hash, err := parseInfoHash(entry.Name())
		if err != nil || entry.Name() != hash.HexString() {
			return fmt.Errorf("session: subtitle directory %q is not canonical", entry.Name())
		}
		info, err := os.Lstat(filepath.Join(s.subtitleRoot, entry.Name()))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("session: subtitle directory %s is not a directory", hash)
		}
		registry := s.states[hash]
		if registry == nil || registry.State != StateReady || s.torrents[hash] == nil || s.torrents[hash].Info() == nil {
			continue
		}
		files := make([]string, 0)
		if err := scanManagedSubtitleFiles(filepath.Join(s.subtitleRoot, entry.Name()), "", &files); err != nil {
			return err
		}
		mapping, err := s.subtitleTargetMapLocked(hash, s.torrents[hash])
		if err != nil {
			return err
		}
		records := make(map[string]managedSubtitle, len(files))
		for _, relPath := range files {
			ext, ok := subtitleExtension(path.Base(relPath))
			if !ok {
				return fmt.Errorf("session: subtitle %s has unsupported format", relPath)
			}
			videoPath, err := subtitleVideoFor(mapping, relPath)
			if err != nil {
				return err
			}
			target := mapping[videoPath]
			if !target.Uploadable {
				// The video's own target is unusable because another torrent or a
				// same-stem sibling is competing for the name, which is the same
				// information the namespace guard rejects: report it as one so a
				// restart failure is classifiable.
				return fmt.Errorf("%w: subtitle %s has no uploadable target", ErrSubtitleNamespaceConflict, relPath)
			}
			info, err := os.Stat(filepath.Join(s.subtitleDir(hash), filepath.FromSlash(relPath)))
			if err != nil {
				return err
			}
			records[relPath] = managedSubtitle{VideoPath: videoPath, Path: relPath, Format: strings.TrimPrefix(ext, "."), Size: info.Size(), UpdatedAt: info.ModTime().UTC()}
		}
		if len(records) > 0 {
			s.subtitles[hash] = records
		}
	}
	// No separate layout re-check runs here: a restored torrent that would rename
	// a subtitled video, or whose root name shadows a subtitle path, makes the
	// subtitle resolve to an unusable target below, so the same refusal already
	// covers the persisted state an older build or a hand-placed metainfo can
	// leave behind.
	return nil
}

func scanManagedSubtitleFiles(root, rel string, out *[]string) error {
	dir := root
	if rel != "" {
		dir = filepath.Join(root, filepath.FromSlash(rel))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("session: scan subtitle path %q: %w", rel, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		relPath := name
		if rel != "" {
			relPath = rel + "/" + name
		}
		full := filepath.Join(dir, name)
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (info.Mode()&os.ModeType != 0 && !info.IsDir()) {
			return fmt.Errorf("session: subtitle path %q is not a regular file", relPath)
		}
		if info.IsDir() {
			if err := scanManagedSubtitleFiles(root, relPath, out); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(name, ".torrentfs-subtitle-") && strings.HasSuffix(name, ".tmp") {
			if err := os.Remove(full); err != nil {
				return err
			}
			continue
		}
		if err := validateSubtitleRelativePath(relPath); err != nil {
			return err
		}
		*out = append(*out, relPath)
	}
	return nil
}

func validateManagedSubtitleTree(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		full := filepath.Join(root, entry.Name())
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("subtitle path %q is a symlink", full)
		}
		if info.IsDir() {
			if err := validateManagedSubtitleTree(full); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("subtitle path %q is not regular", full)
		}
	}
	return nil
}

func (s *Session) removeManagedSubtitlesForDelete(hash metainfo.Hash) error {
	if subtitleCleanupHook != nil {
		if err := subtitleCleanupHook(hash); err != nil {
			return newSubtitleError(SubtitleCodeCleanupFailed, ErrSubtitleCleanupFailed)
		}
	}
	root := s.subtitleDir(hash)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return newSubtitleError(SubtitleCodeCleanupFailed, ErrSubtitleCleanupFailed)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return newSubtitleError(SubtitleCodeCleanupFailed, ErrSubtitleCleanupFailed)
	}
	if err := validateManagedSubtitleTree(root); err != nil {
		return newSubtitleError(SubtitleCodeCleanupFailed, ErrSubtitleCleanupFailed)
	}
	if err := os.RemoveAll(root); err != nil {
		return newSubtitleError(SubtitleCodeCleanupFailed, ErrSubtitleCleanupFailed)
	}
	if err := syncDirectory(s.subtitleRoot); err != nil {
		return newSubtitleError(SubtitleCodeCleanupFailed, ErrSubtitleCleanupFailed)
	}
	s.mu.Lock()
	delete(s.subtitles, hash)
	s.mu.Unlock()
	return nil
}
