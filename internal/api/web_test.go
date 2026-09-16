package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticAndAPIPathsHaveSeparateBoundaries(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)

	root := do(t, srv, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK || root.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("root response = %d %q, want public HTML", root.Code, root.Header().Get("Content-Type"))
	}
	deepLink := do(t, srv, httptest.NewRequest(http.MethodGet, "/torrents/pending", nil))
	if deepLink.Code != http.StatusOK || deepLink.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("deep link response = %d %q, want public HTML", deepLink.Code, deepLink.Header().Get("Content-Type"))
	}
	missingAsset := do(t, srv, httptest.NewRequest(http.MethodGet, "/assets/not-built.js", nil))
	if missingAsset.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", missingAsset.Code)
	}

	unknown := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/not-a-route", nil))
	if unknown.Code != http.StatusUnauthorized || unknown.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unknown API without token = %d/%q, want 401/Bearer", unknown.Code, unknown.Header().Get("WWW-Authenticate"))
	}
	token := loginForTest(t, srv)
	authenticatedUnknown := httptest.NewRequest(http.MethodGet, "/api/v1/not-a-route", nil)
	authenticatedUnknown.Header.Set("Authorization", "Bearer "+token)
	rec := do(t, srv, authenticatedUnknown)
	if rec.Code != http.StatusNotFound || strings.Contains(strings.ToLower(rec.Header().Get("Content-Type")), "text/html") || strings.Contains(strings.ToLower(rec.Body.String()), "<html") {
		t.Fatalf("unknown API with token = %d/%q, want non-HTML 404", rec.Code, rec.Header().Get("Content-Type"))
	}

	staticPost := do(t, srv, httptest.NewRequest(http.MethodPost, "/", nil))
	if staticPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST root status = %d, want 405", staticPost.Code)
	}

}
