package lab

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

func getenvFunc(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadUploadSettingsDisabled(t *testing.T) {
	settings, enabled, err := LoadUploadSettings(getenvFunc(nil))
	if err != nil {
		t.Fatalf("LoadUploadSettings: %v", err)
	}
	if enabled {
		t.Fatal("feature is enabled without any environment variable")
	}
	if settings != (UploadSettings{}) {
		t.Fatalf("settings = %+v, want the zero value", settings)
	}
}

func TestLoadUploadSettingsFullConfiguration(t *testing.T) {
	settings, enabled, err := LoadUploadSettings(getenvFunc(map[string]string{
		EnvUploadRate:   "2MiB/s",
		EnvUploadJitter: "25%",
		EnvUploadCap:    "3TiB",
		EnvUploadSeed:   "42",
	}))
	if err != nil {
		t.Fatalf("LoadUploadSettings: %v", err)
	}
	if !enabled {
		t.Fatal("feature is disabled although a rate is configured")
	}
	if want := float64(2 << 20); settings.RateBytesPerSecond != want {
		t.Fatalf("rate = %v, want %v", settings.RateBytesPerSecond, want)
	}
	if settings.JitterFraction != 0.25 {
		t.Fatalf("jitter = %v, want 0.25", settings.JitterFraction)
	}
	if want := int64(3 << 40); settings.CapBytes != want {
		t.Fatalf("cap = %d, want %d", settings.CapBytes, want)
	}
	if settings.Seed != 42 {
		t.Fatalf("seed = %d, want 42", settings.Seed)
	}
}

func TestLoadUploadSettingsDefaults(t *testing.T) {
	settings, enabled, err := LoadUploadSettings(getenvFunc(map[string]string{
		EnvUploadRate: "1MiB",
	}))
	if err != nil {
		t.Fatalf("LoadUploadSettings: %v", err)
	}
	if !enabled {
		t.Fatal("feature is disabled although a rate is configured")
	}
	if want := float64(1 << 20); settings.RateBytesPerSecond != want {
		t.Fatalf("rate = %v, want %v", settings.RateBytesPerSecond, want)
	}
	if settings.JitterFraction != 0.20 {
		t.Fatalf("default jitter = %v, want 0.20", settings.JitterFraction)
	}
	if settings.CapBytes != 0 {
		t.Fatalf("default cap = %d, want uncapped", settings.CapBytes)
	}
	if settings.Seed == 0 {
		t.Fatal("default seed is zero; expected a wall-clock seed")
	}
}

func TestLoadUploadSettingsRejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]map[string]string{
		"jitter without rate": {EnvUploadJitter: "10"},
		"cap without rate":    {EnvUploadCap: "1TiB"},
		"seed without rate":   {EnvUploadSeed: "7"},
		"rate without unit":   {EnvUploadRate: "2"},
		"rate zero":           {EnvUploadRate: "0MiB"},
		"rate not a number":   {EnvUploadRate: "fast/s"},
		"jitter above 100":    {EnvUploadRate: "1MiB", EnvUploadJitter: "101"},
		"jitter negative":     {EnvUploadRate: "1MiB", EnvUploadJitter: "-1"},
		"jitter not a number": {EnvUploadRate: "1MiB", EnvUploadJitter: "lots"},
		"cap without unit":    {EnvUploadRate: "1MiB", EnvUploadCap: "1024"},
		"cap bad unit":        {EnvUploadRate: "1MiB", EnvUploadCap: "1XB"},
		"seed not an integer": {EnvUploadRate: "1MiB", EnvUploadSeed: "later"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := LoadUploadSettings(getenvFunc(env)); err == nil {
				t.Fatalf("LoadUploadSettings(%v) = nil error, want rejection", env)
			}
		})
	}
}

func TestUploadReporterGrowsAndCaps(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewUploadReporter(UploadSettings{
		RateBytesPerSecond: 1 << 20,
		JitterFraction:     0,
		CapBytes:           3 << 20,
		Seed:               1,
	})
	reporter.now = func() time.Time { return clock }

	if got := reporter.report("hash"); got != 0 {
		t.Fatalf("first announce = %d, want 0", got)
	}
	clock = clock.Add(time.Second)
	if got := reporter.report("hash"); got != 1<<20 {
		t.Fatalf("after 1s = %d, want %d", got, 1<<20)
	}
	clock = clock.Add(2 * time.Second)
	if got := reporter.report("hash"); got != 3<<20 {
		t.Fatalf("after 3s = %d, want the %d cap", got, 3<<20)
	}
	clock = clock.Add(10 * time.Second)
	if got := reporter.report("hash"); got != 3<<20 {
		t.Fatalf("after cap = %d, want the cap value", got)
	}
}

func TestUploadReporterJitterStaysWithinBounds(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewUploadReporter(UploadSettings{
		RateBytesPerSecond: 10_000,
		JitterFraction:     0.25,
		Seed:               9,
	})
	reporter.now = func() time.Time { return clock }

	if got := reporter.report("hash"); got != 0 {
		t.Fatalf("first announce = %d, want 0", got)
	}
	previous := int64(0)
	for i := 0; i < 500; i++ {
		clock = clock.Add(10 * time.Millisecond) // 100 nominal bytes per announce.
		value := reporter.report("hash")
		increment := value - previous
		if increment < 74 || increment > 126 {
			t.Fatalf("announce %d increment = %d, want within [74, 126]", i, increment)
		}
		if value < previous {
			t.Fatalf("announce %d decreased from %d to %d", i, previous, value)
		}
		previous = value
	}
}

func TestUploadReporterKeepsTorrentsIndependent(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewUploadReporter(UploadSettings{RateBytesPerSecond: 1 << 20, Seed: 1})
	reporter.now = func() time.Time { return clock }

	if got := reporter.report("first"); got != 0 {
		t.Fatalf("first torrent first announce = %d, want 0", got)
	}
	clock = clock.Add(time.Second)
	if got := reporter.report("first"); got != 1<<20 {
		t.Fatalf("first torrent after 1s = %d, want %d", got, 1<<20)
	}
	if got := reporter.report("second"); got != 0 {
		t.Fatalf("second torrent starts at %d, want 0", got)
	}
}

func TestDirectorRewritesOnlyUploaded(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewUploadReporter(UploadSettings{RateBytesPerSecond: 1 << 20, Seed: 1})
	reporter.now = func() time.Time { return clock }

	const original = "passkey=SECRET&info_hash=%AA%BB&uploaded=0&downloaded=123&left=456&event=started"
	request := &http.Request{URL: &url.URL{
		Scheme:   "http",
		Host:     "tracker.local",
		Path:     "/announce",
		RawQuery: original,
	}}

	if err := reporter.Director(request); err != nil {
		t.Fatalf("Director: %v", err)
	}
	if request.URL.RawQuery != original {
		t.Fatalf("first announce query = %q, want the original %q", request.URL.RawQuery, original)
	}

	clock = clock.Add(time.Second)
	if err := reporter.Director(request); err != nil {
		t.Fatalf("Director: %v", err)
	}
	want := "passkey=SECRET&info_hash=%AA%BB&uploaded=1048576&downloaded=123&left=456&event=started"
	if request.URL.RawQuery != want {
		t.Fatalf("announce query = %q, want %q", request.URL.RawQuery, want)
	}
}

func TestDirectorReplacesTheLastUploadedParameter(t *testing.T) {
	// The client appends announce parameters after a query already present in
	// the tracker URL; only the appended one may change.
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewUploadReporter(UploadSettings{RateBytesPerSecond: 1 << 20, Seed: 1})
	reporter.now = func() time.Time { return clock }

	request := &http.Request{URL: &url.URL{
		Scheme:   "https",
		Host:     "tracker.local",
		Path:     "/announce",
		RawQuery: "uploaded=1&info_hash=%AA&uploaded=0&downloaded=2",
	}}
	if err := reporter.Director(request); err != nil {
		t.Fatalf("Director: %v", err)
	}
	clock = clock.Add(time.Second)
	if err := reporter.Director(request); err != nil {
		t.Fatalf("Director: %v", err)
	}
	want := "uploaded=1&info_hash=%AA&uploaded=1048576&downloaded=2"
	if request.URL.RawQuery != want {
		t.Fatalf("query = %q, want %q", request.URL.RawQuery, want)
	}
}

func TestDirectorIgnoresRequestsWithoutAnnounceParameters(t *testing.T) {
	reporter := NewUploadReporter(UploadSettings{RateBytesPerSecond: 1 << 20, Seed: 1})

	cases := []*http.Request{
		nil,
		{URL: nil},
		{URL: &url.URL{RawQuery: "info_hash=%AA%BB&compact=1"}}, // scrape
		{URL: &url.URL{RawQuery: "uploaded=10&downloaded=20"}},  // no info hash
		{URL: &url.URL{RawQuery: "uploaded=10&info_hash=%ZZ"}},  // undecodable hash
	}
	for i, request := range cases {
		before := ""
		if request != nil && request.URL != nil {
			before = request.URL.RawQuery
		}
		if err := reporter.Director(request); err != nil {
			t.Fatalf("case %d: Director: %v", i, err)
		}
		if request != nil && request.URL != nil && request.URL.RawQuery != before {
			t.Fatalf("case %d: query changed from %q to %q", i, before, request.URL.RawQuery)
		}
	}
}

func TestUploadReporterConcurrentUse(t *testing.T) {
	reporter := NewUploadReporter(UploadSettings{
		RateBytesPerSecond: 1 << 20,
		JitterFraction:     0.5,
		CapBytes:           1 << 30,
		Seed:               3,
	})

	var wait sync.WaitGroup
	for torrent := 0; torrent < 8; torrent++ {
		hash := fmt.Sprintf("hash-%d", torrent)
		wait.Add(1)
		go func() {
			defer wait.Done()
			for announce := 0; announce < 200; announce++ {
				reporter.report(hash)
			}
		}()
	}
	wait.Wait()

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	for hash, schedule := range reporter.byHash {
		if schedule.reported < 0 || schedule.reported > reporter.settings.CapBytes {
			t.Fatalf("%s reported %d, want within [0, %d]", hash, schedule.reported, reporter.settings.CapBytes)
		}
	}
}

func TestUploadReporterCapIsNeverExceeded(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const capBytes = int64(5 << 20)
	reporter := NewUploadReporter(UploadSettings{
		RateBytesPerSecond: 1 << 20,
		JitterFraction:     0.2,
		CapBytes:           capBytes,
		Seed:               11,
	})
	reporter.now = func() time.Time { return clock }

	for i := 0; i < 100; i++ {
		clock = clock.Add(time.Second)
		value := reporter.report("hash")
		if value < 0 || value > capBytes {
			t.Fatalf("announce %d = %d, want within [0, %d]", i, value, capBytes)
		}
	}
	clock = clock.Add(time.Second)
	if got := reporter.report("hash"); got != capBytes {
		t.Fatalf("after cap = %d, want %d", got, capBytes)
	}
}
