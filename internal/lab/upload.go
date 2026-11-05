// Package lab holds experimental research helpers. It is developed and kept off
// the main branch; nothing here is enabled unless the operator explicitly sets
// the documented environment variables.
//
// SyntheticUpload exists to generate HTTP(S) tracker announce traffic with
// controlled, plausible upload statistics, so that anomaly detection running on
// a tracker you operate can be exercised. It is a test harness, not a
// production feature, and it must only be pointed at a tracker whose operator
// authorized the experiment. It reports synthetic numbers only; the client's
// real payload counters are ignored.
package lab

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const (
	// EnvUploadRate enables the synthetic uploader and sets its average rate.
	// The value is a byte quantity, optionally suffixed with "/s" for clarity:
	// "2MiB/s" and "2MiB" both mean two mebibytes per second.
	EnvUploadRate = "TORRENTFS_LAB_FAKE_UPLOAD_RATE"
	// EnvUploadJitter is the per-announce deviation from the average rate, in
	// percent (0-100). The default is 20 percent.
	EnvUploadJitter = "TORRENTFS_LAB_FAKE_UPLOAD_JITTER"
	// EnvUploadCap is the per-torrent ceiling on the synthetic cumulative
	// upload. Unset or "0" means no ceiling.
	EnvUploadCap = "TORRENTFS_LAB_FAKE_UPLOAD_CAP"
	// EnvUploadSeed fixes the random jitter sequence for reproducible runs.
	// Unset seeds from the wall clock.
	EnvUploadSeed = "TORRENTFS_LAB_FAKE_UPLOAD_SEED"
)

const defaultJitterPercent = 20

// UploadSettings is the validated synthetic upload schedule.
type UploadSettings struct {
	// RateBytesPerSecond is the average growth of the reported cumulative
	// upload, in bytes per second.
	RateBytesPerSecond float64
	// JitterFraction is the maximum deviation from the average rate for a
	// single announce, in the range [0, 1].
	JitterFraction float64
	// CapBytes is the per-torrent ceiling on the reported cumulative upload.
	// Zero means uncapped.
	CapBytes int64
	// Seed seeds the jitter sequence.
	Seed int64
}

// LoadUploadSettings reads the lab-only environment variables through getenv.
// enabled is false with a nil error when the feature is not configured; a
// partially configured setup (any knob set without a rate) is an error so a
// typo cannot silently fall back to normal announce reporting.
func LoadUploadSettings(getenv func(string) string) (settings UploadSettings, enabled bool, err error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	rawRate := strings.TrimSpace(getenv(EnvUploadRate))
	if rawRate == "" {
		for _, key := range []string{EnvUploadJitter, EnvUploadCap, EnvUploadSeed} {
			if strings.TrimSpace(getenv(key)) != "" {
				return UploadSettings{}, false, fmt.Errorf("%s is set but %s is empty", key, EnvUploadRate)
			}
		}
		return UploadSettings{}, false, nil
	}

	rate, err := parseByteRate(rawRate)
	if err != nil {
		return UploadSettings{}, false, err
	}
	jitterFraction := float64(defaultJitterPercent) / 100
	if raw := strings.TrimSpace(getenv(EnvUploadJitter)); raw != "" {
		jitterFraction, err = parseJitterPercent(raw)
		if err != nil {
			return UploadSettings{}, false, err
		}
	}
	var capBytes int64
	if raw := strings.TrimSpace(getenv(EnvUploadCap)); raw != "" {
		capBytes, err = config.ParseByteQuantity(raw)
		if err != nil {
			return UploadSettings{}, false, fmt.Errorf("%s: %w", EnvUploadCap, err)
		}
	}
	seed := time.Now().UnixNano()
	if raw := strings.TrimSpace(getenv(EnvUploadSeed)); raw != "" {
		seed, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return UploadSettings{}, false, fmt.Errorf("%s: must be a signed integer", EnvUploadSeed)
		}
	}
	return UploadSettings{
		RateBytesPerSecond: rate,
		JitterFraction:     jitterFraction,
		CapBytes:           capBytes,
		Seed:               seed,
	}, true, nil
}

func parseByteRate(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "/s"))
	bytes, err := config.ParseByteQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", EnvUploadRate, err)
	}
	if bytes <= 0 {
		return 0, fmt.Errorf("%s: must be greater than zero", EnvUploadRate)
	}
	return float64(bytes), nil
}

func parseJitterPercent(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	percent, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, fmt.Errorf("%s: must be a number between 0 and 100", EnvUploadJitter)
	}
	if percent < 0 || percent > 100 {
		return 0, fmt.Errorf("%s: must be between 0 and 100 percent", EnvUploadJitter)
	}
	return percent / 100, nil
}

// UploadReporter rewrites the "uploaded" parameter of HTTP tracker announce
// requests to a synthetic cumulative value. Growth is wall-clock based and per
// info hash, so a single torrent reports the same value to every tracker in its
// announce list:
//
//   - the first announce for an info hash reports zero, matching what a client
//     reports on the "started" event;
//   - every later announce adds rate*(elapsed since the previous announce),
//     deviated by a random factor in [1-jitter, 1+jitter] and never negative,
//     which keeps the reported value monotonic;
//   - an optional cap freezes the value once the ceiling is reached.
//
// The reporter is safe for concurrent use. It ignores the client's real upload
// counter and never reports a number larger than the configured cap.
type UploadReporter struct {
	settings UploadSettings
	// now is time.Now in production and a controllable clock in tests.
	now func() time.Time
	rng *rand.Rand

	mu     sync.Mutex
	byHash map[string]*uploadSchedule
}

type uploadSchedule struct {
	lastAnnounce time.Time
	reported     int64
}

// NewUploadReporter builds a reporter for the given settings. The caller is
// expected to validate the settings with LoadUploadSettings first.
func NewUploadReporter(settings UploadSettings) *UploadReporter {
	return &UploadReporter{
		settings: settings,
		now:      time.Now,
		rng:      rand.New(rand.NewSource(settings.Seed)),
		byHash:   make(map[string]*uploadSchedule),
	}
}

// Director is the hook installed through
// session.WithAnnounceRequestDirector. It rewrites only the "uploaded" query
// parameter and leaves every other parameter, including its original encoding,
// untouched, because private trackers may require an exact passkey query.
func (r *UploadReporter) Director(request *http.Request) error {
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
	reported := r.report(infoHash)
	request.URL.RawQuery = rawQuery[:valueStart] + strconv.FormatInt(reported, 10) + rawQuery[valueEnd:]
	return nil
}

// report advances one torrent's synthetic schedule and returns the new
// cumulative value.
func (r *UploadReporter) report(infoHash string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	schedule := r.byHash[infoHash]
	if schedule == nil {
		// The first announce is the client's "started" event: a real client
		// reports the upload of this session, which is zero.
		r.byHash[infoHash] = &uploadSchedule{lastAnnounce: now}
		return 0
	}

	elapsed := now.Sub(schedule.lastAnnounce).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	schedule.lastAnnounce = now

	factor := 1 + r.settings.JitterFraction*(2*r.rng.Float64()-1)
	increment := r.settings.RateBytesPerSecond * elapsed * factor
	if increment < 0 {
		increment = 0
	}
	schedule.reported += int64(math.Round(increment))
	if capBytes := r.settings.CapBytes; capBytes > 0 && schedule.reported > capBytes {
		schedule.reported = capBytes
	}
	return schedule.reported
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
