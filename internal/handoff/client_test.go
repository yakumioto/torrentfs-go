package handoff

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSessionID = "session-from-target"

// fakeTransmission answers one scripted call per handler invocation and
// records the requests it saw.
type fakeTransmission struct {
	t        *testing.T
	server   *httptest.Server
	requests []rpcRequest
	body     []byte
	// handler produces the response body and status for a request.
	handler func(request rpcRequest) (status int, body string)
}

func newFakeTransmission(t *testing.T, handler func(rpcRequest) (int, string)) *fakeTransmission {
	t.Helper()
	fake := &fakeTransmission{t: t, handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		var request rpcRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fake.body = raw
		fake.requests = append(fake.requests, request)
		if r.Header.Get(sessionHeader) == "" {
			w.Header().Set(sessionHeader, testSessionID)
			w.WriteHeader(http.StatusConflict)
			return
		}
		status, body := fake.handler(request)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	fake.server = server
	return fake
}

func (f *fakeTransmission) url() string { return f.server.URL }

func TestAddAdoptsTheSessionIDAndSendsBase64Metainfo(t *testing.T) {
	fake := newFakeTransmission(t, func(rpcRequest) (int, string) {
		return http.StatusOK, `{"result":"success","arguments":{"torrent-added":{"hashString":"abc","id":1,"name":"x"}}}`
	})
	target := New(Config{URL: fake.url()})
	metainfo := []byte("d4:infod4:name1:xe")
	if err := target.Add(context.Background(), metainfo); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d, want the 409 handshake and the retry", len(fake.requests))
	}
	if got := fake.requests[1].Method; got != "torrent-add" {
		t.Errorf("method = %q, want torrent-add", got)
	}
	// The second request must carry the adopted session id; the fake only
	// answers with success once it sees one, so reaching this point proves it.
	var arguments struct {
		Metainfo string `json:"metainfo"`
	}
	var envelope struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(fake.body, &envelope); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if err := json.Unmarshal(envelope.Arguments, &arguments); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}
	if arguments.Metainfo != base64.StdEncoding.EncodeToString(metainfo) {
		t.Errorf("metainfo = %q, want the base64 of the delivered bytes", arguments.Metainfo)
	}
}

func TestAddTreatsADuplicateAsSuccess(t *testing.T) {
	fake := newFakeTransmission(t, func(rpcRequest) (int, string) {
		return http.StatusOK, `{"result":"success","arguments":{"torrent-duplicate":{"hashString":"abc","id":2,"name":"x"}}}`
	})
	if err := New(Config{URL: fake.url()}).Add(context.Background(), []byte("x")); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestAddWithoutAddedOrDuplicateIsRejected(t *testing.T) {
	fake := newFakeTransmission(t, func(rpcRequest) (int, string) {
		return http.StatusOK, `{"result":"success","arguments":{}}`
	})
	err := New(Config{URL: fake.url()}).Add(context.Background(), []byte("x"))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

func TestCallRejectsAnErrorResult(t *testing.T) {
	fake := newFakeTransmission(t, func(rpcRequest) (int, string) {
		return http.StatusOK, `{"result":"invalid argument","arguments":{}}`
	})
	err := New(Config{URL: fake.url()}).Add(context.Background(), []byte("x"))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if !strings.Contains(err.Error(), "invalid argument") {
		t.Errorf("err = %v, want the target's reason", err)
	}
}

func TestCallMapsAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "admin" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set(sessionHeader, testSessionID)
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(server.Close)

	_, err := New(Config{URL: server.URL, Username: "admin", Password: "wrong"}).Version(context.Background())
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestCallMapsAnUnreachableTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	_, err := New(Config{URL: url}).Version(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestVersionReadsTheSessionField(t *testing.T) {
	fake := newFakeTransmission(t, func(request rpcRequest) (int, string) {
		if request.Method != "session-get" {
			t.Errorf("method = %q, want session-get", request.Method)
		}
		return http.StatusOK, `{"result":"success","arguments":{"version":"3.00"}}`
	})
	version, err := New(Config{URL: fake.url()}).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != "3.00" {
		t.Errorf("version = %q, want 3.00", version)
	}
}

func TestSecondConflictIsRejectedInsteadOfLooping(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set(sessionHeader, testSessionID+"-rotating")
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(server.Close)

	_, err := New(Config{URL: server.URL}).Version(context.Background())
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want two attempts", calls)
	}
}
