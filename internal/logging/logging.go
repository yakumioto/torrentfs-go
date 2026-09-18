// Package logging builds the process logger from configuration.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

var (
	urlUserinfoPattern = regexp.MustCompile(`(?i)(socks5h?|https?)://[^/\s@]+@`)
	bearerPattern      = regexp.MustCompile(`(?i)\bBearer\s+\S+`)
	credentialPattern  = regexp.MustCompile(`(?i)\b(password|passwd|token|access_token|authorization)=([^&\s]+)`)
)

// New builds a logger writing to w and returns the level controller used by its
// handler. An empty level or format uses the configured defaults.
func New(cfg config.Log, w io.Writer) (*slog.Logger, *slog.LevelVar, error) {
	if w == nil {
		w = os.Stderr
	}

	level, err := config.ParseLogLevel(cfg.Level)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: invalid level %q: %w", cfg.Level, err)
	}

	format := strings.ToLower(strings.TrimSpace(cfg.Format))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" {
		return nil, nil, fmt.Errorf("logging: invalid format %q", cfg.Format)
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(level)
	opts := &slog.HandlerOptions{
		AddSource:   cfg.AddSource,
		Level:       levelVar,
		ReplaceAttr: redactAttr,
	}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler), levelVar, nil
}

// Discard returns a logger that emits no records.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func redactAttr(_ []string, attr slog.Attr) slog.Attr {
	key := strings.ToLower(attr.Key)
	if strings.Contains(key, "password") ||
		strings.Contains(key, "passwd") ||
		strings.Contains(key, "socks5_url") ||
		strings.Contains(key, "authorization") ||
		key == "token" || strings.HasSuffix(key, ".token") {
		return slog.String(attr.Key, "[REDACTED]")
	}

	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.String(attr.Key, redactString(value.String()))
	case slog.KindAny:
		anyValue := value.Any()
		switch typed := anyValue.(type) {
		case *url.URL:
			if typed == nil {
				return attr
			}
			return slog.String(attr.Key, redactString(redactURL(typed)))
		case url.URL:
			return slog.String(attr.Key, redactString(redactURL(&typed)))
		case error:
			return slog.String(attr.Key, redactString(typed.Error()))
		}
	}
	return attr
}

func redactURL(value *url.URL) string {
	copy := *value
	copy.User = nil
	return copy.String()
}

func redactString(value string) string {
	value = urlUserinfoPattern.ReplaceAllString(value, "$1://[REDACTED]@")
	value = bearerPattern.ReplaceAllString(value, "Bearer [REDACTED]")
	return credentialPattern.ReplaceAllString(value, "$1=[REDACTED]")
}
