package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticHandlerServesShellAndAssets(t *testing.T) {
	handler := newStaticHandler(fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>shell</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('ok')")},
	})

	tests := []struct {
		name        string
		method      string
		path        string
		wantStatus  int
		wantBody    string
		contentType string
		cache       string
	}{
		{name: "root", method: http.MethodGet, path: "/", wantStatus: http.StatusOK, wantBody: "shell", contentType: "text/html; charset=utf-8", cache: "no-cache"},
		{name: "deep link", method: http.MethodGet, path: "/torrents/abc", wantStatus: http.StatusOK, wantBody: "shell", contentType: "text/html; charset=utf-8", cache: "no-cache"},
		{name: "asset", method: http.MethodGet, path: "/assets/app.js", wantStatus: http.StatusOK, wantBody: "console.log", contentType: "text/javascript; charset=utf-8", cache: "public, max-age=31536000, immutable"},
		{name: "missing asset", method: http.MethodGet, path: "/assets/missing.js", wantStatus: http.StatusNotFound},
		{name: "asset directory", method: http.MethodGet, path: "/assets/", wantStatus: http.StatusNotFound},
		{name: "head", method: http.MethodHead, path: "/", wantStatus: http.StatusOK, wantBody: "", cache: "no-cache"},
		{name: "post", method: http.MethodPost, path: "/", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", rec.Body.String(), tt.wantBody)
			}
			if tt.contentType != "" && rec.Header().Get("Content-Type") != tt.contentType {
				t.Fatalf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tt.contentType)
			}
			if tt.cache != "" && rec.Header().Get("Cache-Control") != tt.cache {
				t.Fatalf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tt.cache)
			}
		})
	}
}
