// Package handoff hands a torrent off to an external downloader. The external
// client takes over downloading and seeding; the handoff client only delivers
// the torrent's metainfo and reports what the target did with it.
package handoff

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	// ErrUnreachable means the target could not be reached or did not answer
	// within the deadline. Nothing is known about whether it accepted the
	// torrent.
	ErrUnreachable = errors.New("handoff target unreachable")
	// ErrAuthFailed means the target answered but rejected the configured
	// credentials.
	ErrAuthFailed = errors.New("handoff target rejected the credentials")
	// ErrRejected means the target answered and refused the request, either
	// with an error result or with an unusable response.
	ErrRejected = errors.New("handoff target rejected the request")
)

const (
	// DefaultTimeout bounds one handoff call, including the session-id retry.
	DefaultTimeout = 10 * time.Second
	// sessionHeader carries the CSRF token Transmission hands out with its
	// first 409 response.
	sessionHeader = "X-Transmission-Session-Id"
	// maxBodyBytes caps how much of a response is read; every response of the
	// protocol is small, so a larger one is a misdirected endpoint.
	maxBodyBytes = 1 << 20
	// maxAttempts is the 409 handshake plus one retry: the second 409 means
	// the endpoint is not the Transmission RPC it claimed to be.
	maxAttempts = 2
)

// Config is one handoff target. An empty URL means the feature is off.
type Config struct {
	URL      string
	Username string
	Password string
}

// Configured reports whether the target has an endpoint.
func (c Config) Configured() bool { return strings.TrimSpace(c.URL) != "" }

// Target is a Transmission RPC endpoint. It keeps the CSRF session id across
// calls, because Transmission rotates it on restart and expects the client to
// adopt the new one from the 409 response.
type Target struct {
	config Config
	client *http.Client

	mu        sync.Mutex
	sessionID string
}

// New returns a target for cfg. Every call is bounded by its context; the
// client timeout is the backstop for a caller that passes a context without a
// deadline, so a stuck target cannot hang the caller forever.
func New(cfg Config) *Target {
	return &Target{
		config: Config{
			URL:      strings.TrimSpace(cfg.URL),
			Username: cfg.Username,
			Password: cfg.Password,
		},
		client: &http.Client{Timeout: DefaultTimeout},
	}
}

// rpcRequest is the request envelope of the Transmission RPC. The method and
// argument names are kebab-case because Transmission 3.00 speaks the original
// protocol; 4.x still accepts it.
type rpcRequest struct {
	Method    string `json:"method"`
	Arguments any    `json:"arguments,omitempty"`
}

// rpcResponse is the response envelope. Result is "success" or a human
// readable failure, and Arguments is decoded by each caller.
type rpcResponse struct {
	Result    string          `json:"result"`
	Arguments json.RawMessage `json:"arguments"`
}

// Add hands one torrent's metainfo to the target. A torrent the target already
// knows is not an error: the caller's intent (the target serves this torrent)
// is already satisfied, so it reports success.
func (t *Target) Add(ctx context.Context, metainfo []byte) error {
	if len(metainfo) == 0 {
		return fmt.Errorf("%w: empty metainfo", ErrRejected)
	}
	arguments := map[string]string{
		"metainfo": base64.StdEncoding.EncodeToString(metainfo),
	}
	raw, err := t.call(ctx, "torrent-add", arguments)
	if err != nil {
		return err
	}
	var added struct {
		TorrentAdded     json.RawMessage `json:"torrent-added"`
		TorrentDuplicate json.RawMessage `json:"torrent-duplicate"`
	}
	if err := json.Unmarshal(raw, &added); err != nil {
		return fmt.Errorf("%w: decode torrent-add response: %v", ErrRejected, err)
	}
	if len(added.TorrentAdded) == 0 && len(added.TorrentDuplicate) == 0 {
		return fmt.Errorf("%w: torrent-add returned neither an added nor a duplicate torrent", ErrRejected)
	}
	return nil
}

// Version asks the target for its version, which is how the configuration page
// checks the endpoint and the credentials. Transmission reports it as
// "3.00" or "4.0.6".
func (t *Target) Version(ctx context.Context) (string, error) {
	raw, err := t.call(ctx, "session-get", struct{}{})
	if err != nil {
		return "", err
	}
	var session struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return "", fmt.Errorf("%w: decode session-get response: %v", ErrRejected, err)
	}
	if session.Version == "" {
		return "", fmt.Errorf("%w: session-get returned no version", ErrRejected)
	}
	return session.Version, nil
}

// call performs one RPC, adopting a new CSRF session id when the target asks
// for one. Every non-success outcome is mapped onto one of the package errors
// so the caller can report a stable code.
func (t *Target) call(ctx context.Context, method string, arguments any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{Method: method, Arguments: arguments})
	if err != nil {
		return nil, fmt.Errorf("%w: encode %s request: %v", ErrRejected, method, err)
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		response, sessionID, retry, err := t.send(ctx, body)
		if err != nil {
			return nil, err
		}
		if retry {
			t.adoptSessionID(sessionID)
			continue
		}
		var out rpcResponse
		if err := json.Unmarshal(response, &out); err != nil {
			return nil, fmt.Errorf("%w: decode %s response: %v", ErrRejected, method, err)
		}
		if out.Result != "success" {
			return nil, fmt.Errorf("%w: %s", ErrRejected, out.Result)
		}
		return out.Arguments, nil
	}
	return nil, fmt.Errorf("%w: the session id was rejected twice", ErrRejected)
}

// send performs one HTTP round trip. A 409 teaches the session id and asks the
// caller to try again; every other status is mapped here.
func (t *Target) send(ctx context.Context, body []byte) (response []byte, sessionID string, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.config.URL, bytes.NewReader(body))
	if err != nil {
		return nil, "", false, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if id := t.currentSessionID(); id != "" {
		req.Header.Set(sessionHeader, id)
	}
	if t.config.Username != "" || t.config.Password != "" {
		req.SetBasicAuth(t.config.Username, t.config.Password)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	switch {
	case resp.StatusCode == http.StatusConflict:
		// The CSRF handshake: no session id yet, or the target restarted.
		if id := resp.Header.Get(sessionHeader); id != "" {
			return nil, id, true, nil
		}
		return nil, "", false, fmt.Errorf("%w: %s without a session id", ErrUnreachable, resp.Status)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, "", false, fmt.Errorf("%w: %s", ErrAuthFailed, resp.Status)
	case resp.StatusCode >= http.StatusInternalServerError:
		return nil, "", false, fmt.Errorf("%w: %s", ErrUnreachable, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, "", false, fmt.Errorf("%w: %s", ErrRejected, resp.Status)
	}
	if readErr != nil {
		return nil, "", false, fmt.Errorf("%w: read response: %v", ErrUnreachable, readErr)
	}
	return data, "", false, nil
}

func (t *Target) currentSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *Target) adoptSessionID(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionID = id
}
