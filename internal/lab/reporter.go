package lab

import (
	"encoding/hex"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reporter rewrites the "uploaded" parameter of HTTP(S) tracker announce
// requests according to the configured experiment mode, and records every
// announce it sees as ground truth.
//
// State is kept per info hash, so one torrent reports the same value to every
// tracker in its announce list:
//
//   - the first announce reports zero for the injection modes, matching the
//     session counter of a "started" event; baseline reports the real counter;
//   - inject adds rate*elapsed, deviated by a random factor in
//     [1-jitter, 1+jitter] and never negative, so the reported value is
//     monotonic and independent of real traffic;
//   - amplify adds multiplier*realDelta, where realDelta is the client's real
//     payload bytes sent since the previous announce; no real transfer means no
//     growth, which models a coupled cheater;
//   - the optional cap freezes the reported value once the ceiling is reached.
//
// The client's real counters are never modified; only the value written into
// the announce request changes.
type Reporter struct {
	settings Settings
	recorder *Recorder
	// now is time.Now in production and a controllable clock in tests.
	now func() time.Time
	rng *rand.Rand

	mu     sync.Mutex
	byHash map[string]*schedule
}

type schedule struct {
	lastAnnounce     time.Time
	lastRealUploaded int64
	reported         int64
}

// NewReporter builds a mode-aware reporter. The caller validates settings with
// LoadSettings first. recorder may be nil.
func NewReporter(settings Settings, recorder *Recorder) *Reporter {
	return &Reporter{
		settings: settings,
		recorder: recorder,
		now:      time.Now,
		rng:      rand.New(rand.NewSource(settings.Seed)),
		byHash:   make(map[string]*schedule),
	}
}

// Director is the hook installed through
// session.WithAnnounceRequestDirector. It rewrites only the "uploaded" query
// parameter and leaves every other parameter, including its original encoding,
// untouched, because private trackers may require an exact passkey query.
// Baseline mode leaves the request unchanged but still records ground truth.
func (r *Reporter) Director(request *http.Request) error {
	if r == nil || request == nil || request.URL == nil {
		return nil
	}
	rawQuery := request.URL.RawQuery
	valueStart, valueEnd, ok := lastQueryParam(rawQuery, "uploaded")
	if !ok {
		// Not an announce request, or a client variant without the parameter:
		// leave it alone rather than guessing.
		return nil
	}
	infoHash, ok := rawQueryValue(rawQuery, "info_hash")
	if !ok {
		return nil
	}
	realUploaded := parseIntParam(rawQuery, "uploaded")
	realDownloaded := parseIntParam(rawQuery, "downloaded")
	left := parseIntParam(rawQuery, "left")
	event := stringParam(rawQuery, "event")

	reported, realDelta, reportedDelta := r.report(infoHash, realUploaded)
	if r.settings.Mode != ModeBaseline {
		request.URL.RawQuery = rawQuery[:valueStart] + strconv.FormatInt(reported, 10) + rawQuery[valueEnd:]
	}
	r.recorder.WriteAnnounce(AnnounceRecord{
		Time:             r.now(),
		InfoHash:         infoHashHex(infoHash),
		Tracker:          request.URL.Host,
		Event:            event,
		Mode:             r.settings.Mode,
		RealUploaded:     realUploaded,
		RealDownloaded:   realDownloaded,
		Left:             left,
		RealDelta:        realDelta,
		ReportedUploaded: reported,
		ReportedDelta:    reportedDelta,
	})
	return nil
}

// report advances one torrent's schedule and returns the reported cumulative
// upload plus the real and reported deltas observed for this announce.
func (r *Reporter) report(infoHash string, realUploaded int64) (reported, realDelta, reportedDelta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	current := r.byHash[infoHash]
	if current == nil {
		current = &schedule{lastAnnounce: now, lastRealUploaded: realUploaded}
		if r.settings.Mode == ModeBaseline {
			current.reported = realUploaded
		}
		r.byHash[infoHash] = current
		return current.reported, 0, 0
	}

	realDelta = realUploaded - current.lastRealUploaded
	current.lastRealUploaded = realUploaded
	elapsed := now.Sub(current.lastAnnounce).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	current.lastAnnounce = now

	var increment float64
	switch r.settings.Mode {
	case ModeInject:
		increment = r.settings.RateBytesPerSecond * elapsed
	case ModeAmplify:
		if realDelta > 0 {
			increment = float64(realDelta) * r.settings.Multiplier
		}
	}
	if increment > 0 {
		factor := 1 + r.settings.JitterFraction*(2*r.rng.Float64()-1)
		increment *= factor
	}
	if increment < 0 {
		increment = 0
	}

	previous := current.reported
	current.reported += int64(math.Round(increment))
	if capBytes := r.settings.CapBytes; capBytes > 0 && current.reported > capBytes {
		current.reported = capBytes
	}
	return current.reported, realDelta, current.reported - previous
}

// infoHashHex renders the decoded 20-byte info hash as hex for the log.
func infoHashHex(infoHash string) string {
	return hex.EncodeToString([]byte(infoHash))
}

// parseIntParam decodes an integer query parameter, returning zero when it is
// absent or malformed.
func parseIntParam(rawQuery, key string) int64 {
	value, ok := rawQueryValue(rawQuery, key)
	if !ok {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// stringParam decodes a query parameter, returning an empty string when it is
// absent or malformed.
func stringParam(rawQuery, key string) string {
	value, ok := rawQueryValue(rawQuery, key)
	if !ok {
		return ""
	}
	return value
}

// lastQueryParam returns the byte range of the last value for key in a raw
// query string. Announce parameters are appended after any query already
// present in the tracker URL, so the last occurrence is the client's.
func lastQueryParam(rawQuery, key string) (valueStart, valueEnd int, ok bool) {
	for offset := 0; offset <= len(rawQuery); {
		partEnd := len(rawQuery)
		if next := strings.IndexByte(rawQuery[offset:], '&'); next >= 0 {
			partEnd = offset + next
		}
		part := rawQuery[offset:partEnd]
		if equals := strings.IndexByte(part, '='); equals >= 0 && part[:equals] == key {
			valueStart = offset + equals + 1
			valueEnd = partEnd
			ok = true
		}
		if partEnd == len(rawQuery) {
			break
		}
		offset = partEnd + 1
	}
	return valueStart, valueEnd, ok
}

// rawQueryValue decodes the last value for key, or reports that it is absent.
func rawQueryValue(rawQuery, key string) (string, bool) {
	valueStart, valueEnd, ok := lastQueryParam(rawQuery, key)
	if !ok {
		return "", false
	}
	value, err := url.QueryUnescape(rawQuery[valueStart:valueEnd])
	if err != nil {
		return "", false
	}
	return value, true
}
