package session_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// Log phases label captured reader logs so teardown cancellations can never be
// counted as playback failures. Tests move the phase with an atomic store, and
// every captured event records the phase that was current when it was handled.
const (
	logPhaseSetup int32 = iota
	logPhasePlayback
	logPhaseClosing
	logPhasePostClose
)

// readerCancellationMessages are the anacrolix reader messages that a
// cancellation storm produces. They are logged at Error level only after a
// read fails.
var readerCancellationMessages = []string{
	"initial read failed",
	"read failed after reader reset",
	"read failed after completion resync",
}

type capturedLogEvent struct {
	phase   int32
	message string
	err     error
}

// phaseLogState is shared by every handler derived from one base handler, so
// WithAttrs/WithGroup copies all report into the same event list.
type phaseLogState struct {
	phase  atomic.Int32
	mu     sync.Mutex
	events []capturedLogEvent
}

// phaseLogHandler is a concurrency-safe slog.Handler capturing messages, the
// "err" attribute, and the phase each record was handled in.
type phaseLogHandler struct {
	state *phaseLogState
	attrs []slog.Attr
}

func newPhaseLogHandler() (*phaseLogHandler, *phaseLogState) {
	state := &phaseLogState{}
	return &phaseLogHandler{state: state}, state
}

func (h *phaseLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *phaseLogHandler) Handle(_ context.Context, record slog.Record) error {
	var errValue error
	for _, attr := range allAttrs(h.attrs, record) {
		if attr.Key != "err" {
			continue
		}
		if err, ok := attr.Value.Any().(error); ok {
			errValue = err
		}
	}
	h.state.mu.Lock()
	h.state.events = append(h.state.events, capturedLogEvent{
		phase:   h.state.phase.Load(),
		message: record.Message,
		err:     errValue,
	})
	h.state.mu.Unlock()
	return nil
}

func (h *phaseLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &phaseLogHandler{state: h.state, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *phaseLogHandler) WithGroup(string) slog.Handler { return h }

func allAttrs(base []slog.Attr, record slog.Record) []slog.Attr {
	out := append([]slog.Attr(nil), base...)
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Value.Kind() == slog.KindGroup {
			out = append(out, attr.Value.Group()...)
			return true
		}
		out = append(out, attr)
		return true
	})
	return out
}

// cancellationCount reports how many reader cancellation logs were captured in
// phase.
// messages returns every captured log message, so a test can assert that a
// diagnostic surfaced instead of being swallowed.
func (s *phaseLogState) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, event := range s.events {
		out = append(out, event.message)
	}
	return out
}

// messagesContaining reports whether any captured message contains substr.
func (s *phaseLogState) messagesContaining(substr string) bool {
	for _, message := range s.messages() {
		if strings.Contains(message, substr) {
			return true
		}
	}
	return false
}

func (s *phaseLogState) cancellationCount(phase int32) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, event := range s.events {
		if event.phase != phase || !errors.Is(event.err, context.Canceled) {
			continue
		}
		for _, message := range readerCancellationMessages {
			if event.message == message {
				count++
				break
			}
		}
	}
	return count
}

// recordingBackend wraps a real Backend and records every reader handle it
// hands out, so a test can prove two FUSE opens share one session reader. It
// returns the session's reader unchanged, so the reader still carries the
// contextual read path.
type recordingBackend struct {
	inner filesystem.Backend

	mu     sync.Mutex
	opened []io.ReaderAt
}

func (b *recordingBackend) Torrents() []filesystem.TorrentView { return b.inner.Torrents() }

func (b *recordingBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	reader, err := b.inner.OpenFile(hash, path)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.opened = append(b.opened, reader)
	b.mu.Unlock()
	return reader, nil
}

func (b *recordingBackend) openedReaders() []io.ReaderAt {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]io.ReaderAt(nil), b.opened...)
}

// externalProbeWait bounds every external probe/handshake assertion.
const externalProbeWait = 15 * time.Second

func waitProbeEvent(t *testing.T, events <-chan session.ReadProbeEvent, kind string, offset int64) session.ReadProbeEvent {
	t.Helper()
	deadline := time.After(externalProbeWait)
	for {
		select {
		case event := <-events:
			if event.Kind == kind && event.Offset == offset {
				return event
			}
		case <-deadline:
			t.Fatalf("no %q probe event for offset %d", kind, offset)
			return session.ReadProbeEvent{}
		}
	}
}

func probeDoneClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func waitProbeDoneClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(externalProbeWait):
		t.Fatalf("%s was never cancelled", what)
	}
}

type readOutcome struct {
	data []byte
	err  error
}

// readAtAsync starts one mounted-file read and delivers its outcome on a
// buffered channel, so an assertion failure cannot strand the goroutine.
func readAtAsync(f *os.File, off int64, length int) <-chan readOutcome {
	done := make(chan readOutcome, 1)
	go func() {
		buf := make([]byte, length)
		n, err := f.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			done <- readOutcome{err: err}
			return
		}
		done <- readOutcome{data: append([]byte(nil), buf[:n]...)}
	}()
	return done
}

func waitReadOutcome(t *testing.T, done <-chan readOutcome, what string) readOutcome {
	t.Helper()
	return waitReadOutcomeWithin(t, done, what, externalProbeWait)
}

func waitReadOutcomeWithin(t *testing.T, done <-chan readOutcome, what string, timeout time.Duration) readOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(timeout):
		t.Fatalf("%s did not return", what)
		return readOutcome{}
	}
}

func requireNoReadOutcome(t *testing.T, done <-chan readOutcome, what string) {
	t.Helper()
	select {
	case outcome := <-done:
		t.Fatalf("%s returned before it should have: %d bytes, %v", what, len(outcome.data), outcome.err)
	case <-time.After(100 * time.Millisecond):
	}
}

// waitOpenedReaders waits until the recording backend has handed out want
// readers, so identity assertions never race the FUSE open path.
func waitOpenedReaders(t *testing.T, backend *recordingBackend, want int) []io.ReaderAt {
	t.Helper()
	deadline := time.After(externalProbeWait)
	for {
		readers := backend.openedReaders()
		if len(readers) >= want {
			return readers
		}
		select {
		case <-deadline:
			t.Fatalf("only %d backend readers were opened, want %d", len(readers), want)
			return readers
		case <-time.After(10 * time.Millisecond):
		}
	}
}
