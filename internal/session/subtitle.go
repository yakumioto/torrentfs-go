package session

import (
	"context"
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

func ensureManagedParents(root, relDir string) error {
	if err := ensureManagedDirectory(root, 0o700); err != nil {
		return err
	}
	current := root
	if relDir == "" || relDir == "." {
		return nil
	}
	for _, component := range strings.Split(relDir, "/") {
		current = filepath.Join(current, component)
		if err := ensureManagedDirectory(current, 0o700); err != nil {
			return err
		}
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

func videoExtension(name string) (string, bool) {
	ext := strings.ToLower(path.Ext(name))
	_, ok := videoExtensions[ext]
	return ext, ok
}

func basenameStem(name, ext string) string {
	return strings.TrimSuffix(path.Base(name), ext)
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
		videoExt, ok := videoExtension(file.DisplayPath())
		if !ok || validateSubtitleRelativePath(file.DisplayPath()) != nil {
			continue
		}
		base := path.Base(file.DisplayPath())
		candidates = append(candidates, videoCandidate{
			path:       file.DisplayPath(),
			stem:       basenameStem(base, videoExt),
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
		videoExt, _ := videoExtension(base)
		expectedBasename := basenameStem(base, videoExt)
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
	videoBase := path.Base(videoPath)
	videoExt, _ := videoExtension(videoBase)
	expected := basenameStem(videoBase, videoExt)
	if basenameStem(fileName, ext) != expected {
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
	root := s.subtitleDir(hash)
	dir := filepath.Dir(filepath.Join(root, filepath.FromSlash(relPath)))
	if err := ensureManagedParents(root, path.Dir(relPath)); err != nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeStorageUnavailable, ErrSubtitleStorageUnavailable)
	}
	if info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(relPath))); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeStorageUnavailable, ErrSubtitleStorageUnavailable)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeStorageUnavailable, ErrSubtitleStorageUnavailable)
	}

	// os.CreateTemp names the staging file itself (0600), so the destination
	// directory never receives a client-influenced name.
	file, err := os.CreateTemp(dir, ".torrentfs-subtitle-*.tmp")
	if err != nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeStorageUnavailable, ErrSubtitleStorageUnavailable)
	}
	tmp := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}
	limited := io.LimitReader(&contextReader{ctx: ctx, reader: src}, maxBytes+1)
	written, copyErr := io.Copy(file, limited)
	if copyErr != nil {
		cleanup()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, ErrSubtitleWriteFailed)
	}
	if written > maxBytes {
		cleanup()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeUploadTooLarge, ErrSubtitleUploadTooLarge)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeStorageFull, ErrSubtitleStorageFull)
	}
	if err := file.Chmod(0o444); err != nil {
		cleanup()
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, ErrSubtitleWriteFailed)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, ErrSubtitleWriteFailed)
	}
	destination := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.Rename(tmp, destination); err != nil {
		_ = os.Remove(tmp)
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, ErrSubtitleWriteFailed)
	}
	if err := syncDirectory(dir); err != nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, ErrSubtitleWriteFailed)
	}
	stat, err := os.Stat(destination)
	if err != nil {
		return SubtitleUploadResponse{}, newSubtitleError(SubtitleCodeWriteFailed, ErrSubtitleWriteFailed)
	}
	updatedAt := stat.ModTime().UTC()
	record := managedSubtitle{VideoPath: videoPath, Path: relPath, Format: strings.TrimPrefix(ext, "."), Size: stat.Size(), UpdatedAt: updatedAt}
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
			var target SubtitleTarget
			var videoPath string
			for candidatePath, candidate := range mapping {
				if subtitleTargetPath(candidatePath, path.Base(relPath)) == relPath {
					if videoPath != "" {
						return fmt.Errorf("session: subtitle %s has ambiguous video", relPath)
					}
					videoPath = candidatePath
					target = candidate
				}
			}
			if videoPath == "" || !target.Uploadable {
				return fmt.Errorf("session: subtitle %s has no unique target", relPath)
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
