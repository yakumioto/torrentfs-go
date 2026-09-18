package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

func TestNewFiltersDebugAndSupportsDynamicLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, level, err := logging.New(config.Log{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Debug("hidden")
	logger.Info("visible")
	output := buf.String()
	if strings.Contains(output, "hidden") {
		t.Fatalf("debug output at info level = %q", output)
	}
	if !strings.Contains(output, "level=INFO") || !strings.Contains(output, "msg=visible") {
		t.Fatalf("info output = %q", output)
	}

	level.Set(slog.LevelDebug)
	logger.Debug("visible-debug")
	if !strings.Contains(buf.String(), "msg=visible-debug") {
		t.Fatalf("debug output after enabling level = %q", buf.String())
	}

	level.Set(slog.LevelInfo)
	logger.Debug("hidden-again")
	if strings.Contains(buf.String(), "hidden-again") {
		t.Fatalf("debug output after restoring info level = %q", buf.String())
	}
}

func TestNewJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "warn", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hidden")
	logger.Warn("visible", "count", 1)

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &record); err != nil {
		t.Fatalf("decode JSON log: %v; output=%q", err, buf.String())
	}
	if record["level"] != "WARN" || record["msg"] != "visible" {
		t.Fatalf("record = %v, want WARN/visible", record)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	for _, cfg := range []config.Log{
		{Level: "verbose", Format: "text"},
		{Level: "info", Format: "console"},
	} {
		if _, _, err := logging.New(cfg, &bytes.Buffer{}); err == nil {
			t.Fatalf("New(%+v) succeeded", cfg)
		}
	}
}

func TestNewRedactsCredentialsAndTokens(t *testing.T) {
	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "debug", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Error("request failed",
		"proxy.socks5_url", "socks5h://user:proxy-password@proxy.example:1080",
		"authorization", "Bearer bearer-secret",
		"err", errors.New("request to https://user:another-password@example failed with token=error-secret"),
	)
	output := buf.String()
	for _, secret := range []string{"proxy-password", "another-password", "bearer-secret", "error-secret"} {
		if strings.Contains(output, secret) {
			t.Fatalf("log output contains %q: %q", secret, output)
		}
	}
}
