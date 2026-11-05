package lab

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReporterInjectGrowsAndCaps(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewReporter(Settings{
		Mode:               ModeInject,
		RateBytesPerSecond: 1 << 20,
		JitterFraction:     0,
		CapBytes:           3 << 20,
		Seed:               1,
	}, nil)
	reporter.now = func() time.Time { return clock }

	if reported, _, _ := reporter.report("hash", 0); reported != 0 {
		t.Fatalf("first announce = %d, want 0", reported)
	}
	clock = clock.Add(time.Second)
	if reported, _, _ := reporter.report("hash", 0); reported != 1<<20 {
		t.Fatalf("after 1s = %d, want %d", reported, 1<<20)
	}
	clock = clock.Add(2 * time.Second)
	if reported, _, _ := reporter.report("hash", 0); reported != 3<<20 {
		t.Fatalf("after 3s = %d, want the %d cap", reported, 3<<20)
	}
	clock = clock.Add(10 * time.Second)
	if reported, _, _ := reporter.report("hash", 0); reported != 3<<20 {
		t.Fatalf("after cap = %d, want the cap value", reported)
	}
}

func TestReporterInjectJitterStaysWithinBounds(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewReporter(Settings{
		Mode:               ModeInject,
		RateBytesPerSecond: 10_000,
		JitterFraction:     0.25,
		Seed:               9,
	}, nil)
	reporter.now = func() time.Time { return clock }

	if reported, _, _ := reporter.report("hash", 0); reported != 0 {
		t.Fatalf("first announce = %d, want 0", reported)
	}
	previous := int64(0)
	for i := 0; i < 500; i++ {
		clock = clock.Add(10 * time.Millisecond) // 100 nominal bytes per announce.
		reported, _, _ := reporter.report("hash", 0)
		increment := reported - previous
		if increment < 74 || increment > 126 {
			t.Fatalf("announce %d increment = %d, want within [74, 126]", i, increment)
		}
		if reported < previous {
			t.Fatalf("announce %d decreased from %d to %d", i, previous, reported)
		}
		previous = reported
	}
}

func TestReporterAmplifyFollowsRealTransfer(t *testing.T) {
	reporter := NewReporter(Settings{
		Mode:           ModeAmplify,
		Multiplier:     5,
		JitterFraction: 0,
		Seed:           1,
	}, nil)

	if reported, _, _ := reporter.report("hash", 0); reported != 0 {
		t.Fatalf("first announce = %d, want 0", reported)
	}
	reported, realDelta, reportedDelta := reporter.report("hash", 1000)
	if realDelta != 1000 || reportedDelta != 5000 || reported != 5000 {
		t.Fatalf("after 1000 real bytes: realDelta=%d reportedDelta=%d reported=%d", realDelta, reportedDelta, reported)
	}
	// No real transfer since the previous announce: the reported value must not
	// grow, unlike inject mode.
	reported, realDelta, reportedDelta = reporter.report("hash", 1000)
	if realDelta != 0 || reportedDelta != 0 || reported != 5000 {
		t.Fatalf("after idle interval: realDelta=%d reportedDelta=%d reported=%d", realDelta, reportedDelta, reported)
	}
	reported, realDelta, reportedDelta = reporter.report("hash", 4000)
	if realDelta != 3000 || reportedDelta != 15000 || reported != 20000 {
		t.Fatalf("after 3000 more real bytes: realDelta=%d reportedDelta=%d reported=%d", realDelta, reportedDelta, reported)
	}
}

func TestReporterAmplifyCapIsNeverExceeded(t *testing.T) {
	reporter := NewReporter(Settings{
		Mode:           ModeAmplify,
		Multiplier:     5,
		JitterFraction: 0.2,
		CapBytes:       8_000,
		Seed:           11,
	}, nil)

	if reported, _, _ := reporter.report("hash", 0); reported != 0 {
		t.Fatalf("first announce = %d, want 0", reported)
	}
	for i, real := range []int64{1000, 2000, 3000, 4000} {
		reported, _, _ := reporter.report("hash", real)
		if reported > 8_000 {
			t.Fatalf("announce %d reported %d, exceeds the 8000 cap", i, reported)
		}
	}
	if reported, _, _ := reporter.report("hash", 5000); reported != 8_000 {
		t.Fatalf("after cap = %d, want 8000", reported)
	}
}

func TestReporterKeepsTorrentsIndependent(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewReporter(Settings{Mode: ModeInject, RateBytesPerSecond: 1 << 20, Seed: 1}, nil)
	reporter.now = func() time.Time { return clock }

	if reported, _, _ := reporter.report("first", 0); reported != 0 {
		t.Fatalf("first torrent first announce = %d, want 0", reported)
	}
	clock = clock.Add(time.Second)
	if reported, _, _ := reporter.report("first", 0); reported != 1<<20 {
		t.Fatalf("first torrent after 1s = %d, want %d", reported, 1<<20)
	}
	if reported, _, _ := reporter.report("second", 0); reported != 0 {
		t.Fatalf("second torrent starts at %d, want 0", reported)
	}
}

func TestReporterBaselineLeavesRequestUnchanged(t *testing.T) {
	reporter := NewReporter(Settings{Mode: ModeBaseline}, nil)
	const original = "passkey=SECRET&info_hash=%AA%BB&uploaded=123&downloaded=456&left=789&event=started"
	request := &http.Request{URL: &url.URL{
		Scheme:   "http",
		Host:     "tracker.local",
		Path:     "/announce",
		RawQuery: original,
	}}
	for i := 0; i < 3; i++ {
		if err := reporter.Director(request); err != nil {
			t.Fatalf("Director: %v", err)
		}
		if request.URL.RawQuery != original {
			t.Fatalf("baseline announce %d query = %q, want the original %q", i, request.URL.RawQuery, original)
		}
	}
}

func TestReporterInjectsOnlyUploaded(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewReporter(Settings{Mode: ModeInject, RateBytesPerSecond: 1 << 20, Seed: 1}, nil)
	reporter.now = func() time.Time { return clock }

	request := &http.Request{URL: &url.URL{
		Scheme:   "http",
		Host:     "tracker.local",
		Path:     "/announce",
		RawQuery: "passkey=SECRET&info_hash=%AA%BB&uploaded=0&downloaded=123&left=456&event=started",
	}}
	if err := reporter.Director(request); err != nil {
		t.Fatalf("Director: %v", err)
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

func TestReporterReplacesTheLastUploadedParameter(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewReporter(Settings{Mode: ModeInject, RateBytesPerSecond: 1 << 20, Seed: 1}, nil)
	reporter.now = func() time.Time { return clock }

	// The client appends announce parameters after a query already present in
	// the tracker URL; only the appended one may change.
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

func TestReporterIgnoresRequestsWithoutAnnounceParameters(t *testing.T) {
	reporter := NewReporter(Settings{Mode: ModeInject, RateBytesPerSecond: 1 << 20, Seed: 1}, nil)

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

func TestReporterRecordsGroundTruth(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "announces.jsonl")
	recorder, err := OpenRecorder(logPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("OpenRecorder: %v", err)
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reporter := NewReporter(Settings{Mode: ModeInject, RateBytesPerSecond: 1 << 20, Seed: 1}, recorder)
	reporter.now = func() time.Time { return clock }

	const infoHash = "\xaa\xbb"
	query := "passkey=SECRET&peer_id=PEERIDENTITY&info_hash=%AA%BB&uploaded=0&downloaded=7&left=11&event=started"
	request := &http.Request{URL: &url.URL{Scheme: "http", Host: "tracker.local", Path: "/announce", RawQuery: query}}
	if err := reporter.Director(request); err != nil {
		t.Fatalf("Director: %v", err)
	}
	// The second announce carries the real counter, which the reporter uses for
	// its ground-truth record and replaces in inject mode.
	clock = clock.Add(time.Second)
	request.URL.RawQuery = "passkey=SECRET&peer_id=PEERIDENTITY&info_hash=%AA%BB&uploaded=4096&downloaded=7&left=11"
	if err := reporter.Director(request); err != nil {
		t.Fatalf("Director: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records := readAnnounceRecords(t, logPath)
	if len(records) != 2 {
		t.Fatalf("got %d announce records, want 2", len(records))
	}
	first := records[0]
	if first.Type != recordTypeAnnounce || first.Mode != ModeInject {
		t.Fatalf("first record = %+v", first)
	}
	if first.InfoHash != hex.EncodeToString([]byte(infoHash)) {
		t.Fatalf("info hash = %q, want %q", first.InfoHash, hex.EncodeToString([]byte(infoHash)))
	}
	if first.Tracker != "tracker.local" {
		t.Fatalf("tracker = %q, want tracker.local", first.Tracker)
	}
	if first.RealUploaded != 0 || first.ReportedUploaded != 0 {
		t.Fatalf("first announce real=%d reported=%d, want 0/0", first.RealUploaded, first.ReportedUploaded)
	}
	if first.Event != "started" {
		t.Fatalf("first event = %q, want started", first.Event)
	}
	second := records[1]
	if second.RealUploaded != 4096 {
		t.Fatalf("second real uploaded = %d, want 4096", second.RealUploaded)
	}
	if second.RealDelta != 4096 || second.ReportedDelta != 1<<20 || second.ReportedUploaded != 1<<20 {
		t.Fatalf("second record = %+v, want real delta 4096 and reported 1048576", second)
	}
	if second.RealDownloaded != 7 || second.Left != 11 {
		t.Fatalf("second real downloaded/left = %d/%d, want 7/11", second.RealDownloaded, second.Left)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, secret := range []string{"SECRET", "PEERIDENTITY"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("announce log leaks %q", secret)
		}
	}
}

func TestReporterConcurrentUse(t *testing.T) {
	reporter := NewReporter(Settings{
		Mode:               ModeInject,
		RateBytesPerSecond: 1 << 20,
		JitterFraction:     0.5,
		CapBytes:           1 << 30,
		Seed:               3,
	}, nil)

	var wait sync.WaitGroup
	for torrent := 0; torrent < 8; torrent++ {
		hash := fmt.Sprintf("hash-%d", torrent)
		wait.Add(1)
		go func() {
			defer wait.Done()
			for announce := 0; announce < 200; announce++ {
				reporter.report(hash, 0)
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

func readAnnounceRecords(t *testing.T, path string) []AnnounceRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer func() { _ = file.Close() }()
	var records []AnnounceRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record AnnounceRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode record %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read log: %v", err)
	}
	return records
}
