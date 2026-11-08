package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
)

const configPath = "/api/v1/config"

// configResponse mirrors the wire contract of the configuration view. The
// fields are written out here instead of reusing configctl.View, so a change
// to the JSON tags fails this test instead of moving with it.
type configResponse struct {
	ConfigFile     string `json:"config_file"`
	Baseline       string `json:"baseline"`
	ReadOnlyReason string `json:"read_only_reason"`
	Keys           []struct {
		Path           string   `json:"path"`
		Section        string   `json:"section"`
		Kind           string   `json:"kind"`
		Values         []string `json:"values"`
		Default        string   `json:"default"`
		FileValue      string   `json:"file_value"`
		EffectiveValue string   `json:"effective_value"`
		Source         string   `json:"source"`
		Environment    string   `json:"environment"`
		Overridable    bool     `json:"overridable"`
		Secret         bool     `json:"secret"`
		Component      string   `json:"component"`
		HotApply       bool     `json:"hot_apply"`
		Documentation  string   `json:"documentation"`
		ReadOnlyReason string   `json:"read_only_reason"`
	} `json:"keys"`
	LastSave   configStatusResponse `json:"last_save"`
	LastReload configStatusResponse `json:"last_reload"`
}

// configStatusResponse mirrors the placeholder status of a save or reload.
type configStatusResponse struct {
	At    *string  `json:"at"`
	OK    bool     `json:"ok"`
	Error string   `json:"error"`
	Keys  []string `json:"keys"`
}

func decodeConfigResponse(t *testing.T, rec *httptest.ResponseRecorder) configResponse {
	t.Helper()
	var response configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode configuration response: %v; body %s", err, rec.Body.String())
	}
	return response
}

func configKey(t *testing.T, response configResponse, path string) int {
	t.Helper()
	for index, key := range response.Keys {
		if key.Path == path {
			return index
		}
	}
	t.Fatalf("the configuration response has no key %q", path)
	return -1
}

// configTestServer builds a server with a real configuration controller. The
// controller reads a temporary configuration file, so the test drives the
// same path the process does.
func configTestServer(t *testing.T, controller api.ConfigViewer, tune func(*config.Config)) *api.Server {
	t.Helper()
	cfg := config.Default()
	if tune != nil {
		tune(&cfg)
	}
	options := []api.Option{}
	if controller != nil {
		options = append(options, api.WithConfigViewer(controller))
	}
	srv, err := api.New(cfg, &fakeBackend{}, options...)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return srv
}

// writeAPIConfigFile writes a configuration file into a private temporary
// directory.
func writeAPIConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write configuration file: %v", err)
	}
	return path
}

// TestGetConfigReturnsTheReadOnlyView covers the HTTP contract of the
// configuration page: metadata, the file value, the effective value, the
// source, and the read-only reason of every key.
func TestGetConfigReturnsTheReadOnlyView(t *testing.T) {
	path := writeAPIConfigFile(t, "backend = \"none\"\npassword = \"a-password-that-must-not-leak\"\n\n[log]\nlevel = \"info\"\n")
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")
	controller, _ := configSaveController(t, path)
	srv := configTestServer(t, controller, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, configPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	response := decodeConfigResponse(t, rec)
	if response.ConfigFile != path {
		t.Errorf("config_file = %q, want %q", response.ConfigFile, path)
	}
	if response.Baseline == "" {
		t.Error("the read-only view carries no baseline digest")
	}
	if response.ReadOnlyReason != "" {
		t.Errorf("read_only_reason = %q, want empty for a writable file", response.ReadOnlyReason)
	}

	level := response.Keys[configKey(t, response, "log.level")]
	if level.Section != "log" || level.Kind != "string" || level.Default != "info" {
		t.Errorf("log.level = section %q, kind %q, default %q; want log/string/info",
			level.Section, level.Kind, level.Default)
	}
	if level.FileValue != "info" || level.EffectiveValue != "debug" || level.Source != "environment" {
		t.Errorf("log.level = file %q, effective %q, source %q; want info/debug/environment",
			level.FileValue, level.EffectiveValue, level.Source)
	}
	if level.ReadOnlyReason != configctl.ReadOnlyEnvironmentOverride {
		t.Errorf("log.level read_only_reason = %q, want %q", level.ReadOnlyReason, configctl.ReadOnlyEnvironmentOverride)
	}
	if level.Environment != "TORRENTFS_LOG_LEVEL" || !level.Overridable {
		t.Errorf("log.level environment = %q, overridable = %t; want TORRENTFS_LOG_LEVEL/true",
			level.Environment, level.Overridable)
	}
	if !level.HotApply || level.Component != "logging" {
		t.Errorf("log.level hot_apply = %t, component = %q; want true/logging", level.HotApply, level.Component)
	}
	if level.Documentation == "" {
		t.Error("log.level carries no documentation")
	}

	backend := response.Keys[configKey(t, response, "backend")]
	if backend.Kind != "enum" || len(backend.Values) != 3 || backend.FileValue != "none" || backend.Source != "file" {
		t.Errorf("backend = kind %q, values %v, file %q, source %q; want enum/[none fuse smb]/none/file",
			backend.Kind, backend.Values, backend.FileValue, backend.Source)
	}

	password := response.Keys[configKey(t, response, "password")]
	if !password.Secret || password.ReadOnlyReason != configctl.ReadOnlySecret {
		t.Errorf("password = secret %t, reason %q; want true/%q",
			password.Secret, password.ReadOnlyReason, configctl.ReadOnlySecret)
	}
	if password.FileValue != "" || password.EffectiveValue != "" {
		t.Errorf("password values leaked: file %q, effective %q", password.FileValue, password.EffectiveValue)
	}
	if strings.Contains(rec.Body.String(), "a-password-that-must-not-leak") {
		t.Fatal("the configuration response disclosed the password")
	}

	for name, status := range map[string]configStatusResponse{
		"last_save":   response.LastSave,
		"last_reload": response.LastReload,
	} {
		if status.At != nil || status.OK || status.Error != "" {
			t.Errorf("%s = %+v, want the zero status", name, status)
		}
		if status.Keys == nil || len(status.Keys) != 0 {
			t.Errorf("%s keys = %v, want an empty list", name, status.Keys)
		}
	}
}

// TestGetConfigWithoutConfigurationFileIsReadOnly covers the deployment that
// starts without -config.
func TestGetConfigWithoutConfigurationFileIsReadOnly(t *testing.T) {
	srv := configTestServer(t, configctl.New("", config.Default(), config.Sources{}), nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, configPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	response := decodeConfigResponse(t, rec)
	if response.ConfigFile != "" {
		t.Errorf("config_file = %q, want empty", response.ConfigFile)
	}
	if response.ReadOnlyReason != configctl.ReadOnlyNotConfigured {
		t.Errorf("read_only_reason = %q, want %q", response.ReadOnlyReason, configctl.ReadOnlyNotConfigured)
	}
	if len(response.Keys) != len(config.Schema()) {
		t.Errorf("key count = %d, want %d", len(response.Keys), len(config.Schema()))
	}
}

// TestGetConfigReportsAnUnwritableFile covers the read-only deployment: the
// file exists, but the process cannot replace it.
func TestGetConfigReportsAnUnwritableFile(t *testing.T) {
	path := writeAPIConfigFile(t, "[log]\nlevel = \"info\"\n")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod configuration file: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	srv := configTestServer(t, configctl.New(path, cfg, sources), nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, configPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeConfigResponse(t, rec).ReadOnlyReason; got != configctl.ReadOnlyFileNotWritable {
		t.Errorf("read_only_reason = %q, want %q", got, configctl.ReadOnlyFileNotWritable)
	}
}

// TestConfigRouteIsBehindTheAuthenticationMiddleware covers the boundary of
// the configuration page: with authentication enabled the view requires a
// login like every other management route.
func TestConfigRouteIsBehindTheAuthenticationMiddleware(t *testing.T) {
	path := writeAPIConfigFile(t, "[log]\nlevel = \"info\"\n")
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	srv := configTestServer(t, configctl.New(path, cfg, sources), dynamicAuth)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, configPath, nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unauthenticated GET = %d/%q, want 401/Bearer",
			rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if strings.Contains(rec.Body.String(), "config_file") {
		t.Fatal("the unauthenticated response disclosed the configuration view")
	}

	token := loginForTest(t, srv)
	req := httptest.NewRequest(http.MethodGet, configPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = do(t, srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}

// TestGetConfigWithoutAControllerFailsLoudly covers a server that was built
// without the configuration controller: the route exists, but it reports the
// missing wiring instead of an empty page.
func TestGetConfigWithoutAControllerFailsLoudly(t *testing.T) {
	srv := configTestServer(t, nil, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, configPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
}
