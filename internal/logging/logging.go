// Package logging builds the process logger from configuration.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

var (
	urlUserinfoPattern = regexp.MustCompile(`(?i)(socks5h?|https?)://[^/\s@]+@`)
	bearerPattern      = regexp.MustCompile(`(?i)\bBearer\s+\S+`)
	credentialPattern  = regexp.MustCompile(`(?i)\b(password|passwd|token|access_token|authorization|credential|passkey|authkey)=([^&\s]+)`)
)

// sensitiveQueryKeys are query parameters that identify an account rather than
// the resource being fetched. Private trackers put the account credential in
// exactly this position, so a URL is only safe to log once they are stripped.
var sensitiveQueryKeys = map[string]bool{
	"password":      true,
	"passwd":        true,
	"token":         true,
	"access_token":  true,
	"authorization": true,
	"credential":    true,
	"passkey":       true,
	"authkey":       true,
}

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
	return slog.New(demotingHandler{Handler: handler}), levelVar, nil
}

// repeatingUpstreamLevels maps an upstream message prefix to the level its
// records are rewritten to. The DHT library reports a family with no usable
// starting nodes once per routing-table refresh — once a minute, forever —
// while the session reports that same state once per transition with an
// explicit reason. Repeating it at error level is what buried the tracker
// problem in the MIO-43 logs, so the repeat is demoted and the bounded session
// warning carries it; the original record is still visible at debug level.
var repeatingUpstreamLevels = []struct {
	prefix string
	level  slog.Level
}{
	{prefix: "error bootstrapping during bucket refresh", level: slog.LevelDebug},
}

// demotingHandler rewrites the level of known repeating upstream records.
type demotingHandler struct {
	slog.Handler
}

func (h demotingHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, demote := range repeatingUpstreamLevels {
		if strings.HasPrefix(record.Message, demote.prefix) && record.Level > demote.level {
			record.Level = demote.level
			// The wrapped handler does not re-check its own threshold, so a
			// demoted record has to be dropped here when the new level is below
			// it. Otherwise "demoted" would still print at the old severity's
			// frequency.
			if !h.Handler.Enabled(ctx, record.Level) {
				return nil
			}
			break
		}
	}
	return h.Handler.Handle(ctx, record)
}

func (h demotingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return demotingHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h demotingHandler) WithGroup(name string) slog.Handler {
	return demotingHandler{Handler: h.Handler.WithGroup(name)}
}

// Discard returns a logger that emits no records.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func redactAttr(_ []string, attr slog.Attr) slog.Attr {
	if sensitiveKey(attr.Key) {
		return slog.String(attr.Key, "[REDACTED]")
	}

	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.String(attr.Key, redactString(value.String()))
	case slog.KindAny:
		switch typed := value.Any().(type) {
		case *url.URL:
			if typed == nil {
				return attr
			}
			return slog.String(attr.Key, redactString(redactURL(typed)))
		case url.URL:
			return slog.String(attr.Key, redactString(redactURL(&typed)))
		case *http.Request:
			if typed == nil {
				return attr
			}
			return slog.String(attr.Key, redactRequest(typed))
		case http.Request:
			return slog.String(attr.Key, redactRequest(&typed))
		case error:
			return slog.String(attr.Key, redactString(typed.Error()))
		}
	}
	return attr
}

// sensitiveKey reports whether an attribute or query-parameter name carries a
// secret by convention.
func sensitiveKey(key string) bool {
	key = strings.ToLower(key)
	if strings.Contains(key, "password") ||
		strings.Contains(key, "passwd") ||
		strings.Contains(key, "socks5_url") ||
		strings.Contains(key, "authorization") ||
		strings.Contains(key, "credential") ||
		strings.Contains(key, "passkey") ||
		strings.Contains(key, "authkey") {
		return true
	}
	return key == "token" || strings.HasSuffix(key, ".token")
}

// redactRequest renders a request as "METHOD url" with the URL's userinfo and
// account query parameters removed. A whole-request value would otherwise be
// formatted by the handler with its raw URL intact, which is how a tracker
// credential reaches the logs.
func redactRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	urlText := ""
	if r.URL != nil {
		urlText = redactString(redactURL(r.URL))
	}
	return r.Method + " " + urlText
}

func redactURL(value *url.URL) string {
	copy := *value
	copy.User = nil
	query := copy.Query()
	changed := false
	for key := range query {
		if sensitiveKey(key) {
			query.Set(key, "[REDACTED]")
			changed = true
		}
	}
	if changed {
		copy.RawQuery = query.Encode()
	}
	return copy.String()
}

func redactString(value string) string {
	value = urlUserinfoPattern.ReplaceAllString(value, "$1://[REDACTED]@")
	value = bearerPattern.ReplaceAllString(value, "Bearer [REDACTED]")
	return credentialPattern.ReplaceAllString(value, "$1=[REDACTED]")
}
