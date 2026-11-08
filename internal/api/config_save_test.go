package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
)

// saveResponse mirrors the success contract of the configuration save. The
// fields are written out here instead of reusing configctl.SaveResult, so a
// change to the JSON tags fails this test instead of moving with it.
type saveResponse struct {
	Applied         []string `json:"applied"`
	RestartRequired []string `json:"restart_required"`
	Persisted       bool     `json:"persisted"`
}

// saveErrorResponse mirrors the failure contract of the configuration save.
type saveErrorResponse struct {
	Error      string `json:"error"`
	Code       string `json:"code"`
	FieldError []struct {
		Path    string `json:"path"`
		Message string `json:"message"`
	} `json:"field_errors"`
	Applied         []string `json:"applied"`
	RestartRequired []string `json:"restart_required"`
	Persisted       bool     `json:"persisted"`
}

func decodeSaveResponse(t *testing.T, rec *httptest.ResponseRecorder) saveResponse {
	t.Helper()
	var response saveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode save response: %v; body %s", err, rec.Body.String())
	}
	return response
}

func decodeSaveError(t *testing.T, rec *httptest.ResponseRecorder) saveErrorResponse {
	t.Helper()
	var response saveErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode save error: %v; body %s", err, rec.Body.String())
	}
	return response
}

// saveConfigRequest builds the PUT request of the configuration save.
func saveConfigRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPut, configPath, strings.NewReader(body))
}

// saveJSON renders a save request body.
func saveJSON(t *testing.T, baseline string, patch map[string]string) string {
	t.Helper()
	document, err := json.Marshal(map[string]any{"baseline": baseline, "patch": patch})
	if err != nil {
		t.Fatalf("encode save request: %v", err)
	}
	return string(document)
}

// apiConfigApplier is the component applier the API tests register, so a save
// reaches a running component through the stream contract.
type apiConfigApplier struct {
	keys    []string
	applied []config.Config
}

func (a *apiConfigApplier) Keys() []string { return a.keys }

func (a *apiConfigApplier) Apply(cfg config.Config) error {
	a.applied = append(a.applied, cfg)
	return nil
}

// configSaveController builds a controller for a temporary configuration file
// and registers the log level applier.
func configSaveController(t *testing.T, path string) (*configctl.Controller, *apiConfigApplier) {
	t.Helper()
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	controller := configctl.New(path, cfg, sources)
	applier := &apiConfigApplier{keys: []string{"log.level"}}
	if err := controller.RegisterApplier(applier); err != nil {
		t.Fatalf("RegisterApplier: %v", err)
	}
	return controller, applier
}

// configViewOf fetches the current view of a server.
func configViewOf(t *testing.T, srv *api.Server) configResponse {
	t.Helper()
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, configPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config status = %d; body %s", rec.Code, rec.Body.String())
	}
	return decodeConfigResponse(t, rec)
}

// configSaveTestServer builds a server with a real controller as both the view
// and the save surface.
func configSaveTestServer(t *testing.T, controller *configctl.Controller, tune func(*config.Config)) *api.Server {
	t.Helper()
	return configTestServerWithSaver(t, controller, controller, tune)
}

// configTestServerWithSaver builds a server from an explicit viewer and saver.
func configTestServerWithSaver(t *testing.T, viewer api.ConfigViewer, saver api.ConfigSaver, tune func(*config.Config)) *api.Server {
	t.Helper()
	cfg := config.Default()
	if tune != nil {
		tune(&cfg)
	}
	options := []api.Option{}
	if viewer != nil {
		options = append(options, api.WithConfigViewer(viewer))
	}
	if saver != nil {
		options = append(options, api.WithConfigSaver(saver))
	}
	srv, err := api.New(cfg, &fakeBackend{}, options...)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return srv
}

// TestPutConfigSavesAndHotApplies covers the save contract end to end: a fresh
// baseline and a sparse patch replace the file, reach the applier, and the
// next view reports the new baseline and the save status.
func TestPutConfigSavesAndHotApplies(t *testing.T) {
	path := writeAPIConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller, applier := configSaveController(t, path)
	srv := configSaveTestServer(t, controller, nil)
	before := configViewOf(t, srv)

	rec := do(t, srv, saveConfigRequest(saveJSON(t, before.Baseline, map[string]string{"log.level": "debug"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	response := decodeSaveResponse(t, rec)
	if strings.Join(response.Applied, ",") != "log.level" || len(response.RestartRequired) != 0 || !response.Persisted {
		t.Fatalf("response = %+v, want applied [log.level], no restart, persisted", response)
	}
	if len(applier.applied) != 1 || applier.applied[0].Log.Level != "debug" {
		t.Errorf("the applier received %+v, want the new debug level", applier.applied)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	if !strings.Contains(string(content), `level = "debug"`) {
		t.Errorf("the saved file does not hold the new level:\n%s", content)
	}

	after := configViewOf(t, srv)
	if after.Baseline == before.Baseline {
		t.Error("the baseline did not change after the save")
	}
	if !after.LastSave.OK || after.LastSave.At == nil {
		t.Errorf("last_save = %+v, want a successful timestamped save", after.LastSave)
	}
	level := after.Keys[configKey(t, after, "log.level")]
	if level.FileValue != "debug" || level.EffectiveValue != "debug" || level.Source != "file" {
		t.Errorf("log.level after the save = file %q, effective %q, source %q; want debug/debug/file",
			level.FileValue, level.EffectiveValue, level.Source)
	}
}

// TestPutConfigReportsAStaleBaseline covers the conflict contract over HTTP.
func TestPutConfigReportsAStaleBaseline(t *testing.T) {
	path := writeAPIConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller, _ := configSaveController(t, path)
	srv := configSaveTestServer(t, controller, nil)
	before := configViewOf(t, srv)

	external := "[log]\nlevel = \"warn\"\n"
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatalf("change the configuration file externally: %v", err)
	}
	rec := do(t, srv, saveConfigRequest(saveJSON(t, before.Baseline, map[string]string{"log.level": "debug"})))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if code := decodeSaveError(t, rec).Code; code != configctl.SaveConflict {
		t.Errorf("code = %q, want %q", code, configctl.SaveConflict)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	if string(content) != external {
		t.Errorf("the conflicting save changed the file:\n%s", content)
	}
}

// TestPutConfigReportsPerKeyValidationErrors covers the per-field contract: a
// rejected value is reported with its key and the startup wording.
func TestPutConfigReportsPerKeyValidationErrors(t *testing.T) {
	path := writeAPIConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller, _ := configSaveController(t, path)
	srv := configSaveTestServer(t, controller, nil)
	before := configViewOf(t, srv)

	rec := do(t, srv, saveConfigRequest(saveJSON(t, before.Baseline, map[string]string{"log.level": "verbose"})))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	response := decodeSaveError(t, rec)
	if response.Code != configctl.SaveInvalid {
		t.Errorf("code = %q, want %q", response.Code, configctl.SaveInvalid)
	}
	if len(response.FieldError) != 1 || response.FieldError[0].Path != "log.level" {
		t.Fatalf("field errors = %+v, want one log.level problem", response.FieldError)
	}
	if !strings.Contains(response.FieldError[0].Message, "must be one of debug, info, warn, error") {
		t.Errorf("the field error uses different wording than a start: %q", response.FieldError[0].Message)
	}
	if response.Error == "" {
		t.Error("the response carries no display message")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	if !strings.Contains(string(content), `level = "info"`) {
		t.Errorf("the rejected save changed the file:\n%s", content)
	}
}

// TestPutConfigReportsReadOnlyKeys covers the source constraint over HTTP: an
// environment-overridden key is read-only and nothing is written.
func TestPutConfigReportsReadOnlyKeys(t *testing.T) {
	path := writeAPIConfigFile(t, "[log]\nlevel = \"info\"\n")
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")
	controller, _ := configSaveController(t, path)
	srv := configSaveTestServer(t, controller, nil)
	before := configViewOf(t, srv)

	rec := do(t, srv, saveConfigRequest(saveJSON(t, before.Baseline, map[string]string{"log.level": "warn"})))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	response := decodeSaveError(t, rec)
	if response.Code != configctl.SaveReadOnly {
		t.Errorf("code = %q, want %q", response.Code, configctl.SaveReadOnly)
	}
	if len(response.FieldError) != 1 || response.FieldError[0].Path != "log.level" {
		t.Fatalf("field errors = %+v, want one log.level problem", response.FieldError)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	if !strings.Contains(string(content), `level = "info"`) {
		t.Errorf("the read-only save changed the file:\n%s", content)
	}
}

// TestPutConfigWithoutAConfigurationFile covers a deployment that started
// without -config.
func TestPutConfigWithoutAConfigurationFile(t *testing.T) {
	controller := configctl.New("", config.Default(), config.Sources{})
	srv := configSaveTestServer(t, controller, nil)

	rec := do(t, srv, saveConfigRequest(saveJSON(t, "", map[string]string{"log.level": "debug"})))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if code := decodeSaveError(t, rec).Code; code != configctl.SaveNotConfigured {
		t.Errorf("code = %q, want %q", code, configctl.SaveNotConfigured)
	}
}

// fakeConfigSaver is a save surface whose outcome the test controls.
type fakeConfigSaver struct {
	request configctl.SaveRequest
	result  configctl.SaveResult
	err     error
}

func (f *fakeConfigSaver) Save(_ context.Context, request configctl.SaveRequest) (configctl.SaveResult, error) {
	f.request = request
	return f.result, f.err
}

// TestPutConfigReportsDurabilityUnconfirmed covers the contract inherited
// from the upload-rate save: the new values are in force, so the response
// lists them as applied and must not read like a plain failure.
func TestPutConfigReportsDurabilityUnconfirmed(t *testing.T) {
	saver := &fakeConfigSaver{
		result: configctl.SaveResult{Applied: []string{"log.level"}, Persisted: false},
		err: &configctl.SaveError{
			Code:      configctl.SaveDurabilityUnconfirmed,
			Err:       errors.New("injected directory sync failure"),
			Applied:   []string{"log.level"},
			Persisted: false,
		},
	}
	srv := configTestServerWithSaver(t, nil, saver, nil)

	rec := do(t, srv, saveConfigRequest(saveJSON(t, "baseline", map[string]string{"log.level": "debug"})))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	response := decodeSaveError(t, rec)
	if response.Code != configctl.SaveDurabilityUnconfirmed {
		t.Errorf("code = %q, want %q", response.Code, configctl.SaveDurabilityUnconfirmed)
	}
	if strings.Join(response.Applied, ",") != "log.level" {
		t.Errorf("applied = %v, want [log.level]: the value is already in force", response.Applied)
	}
	if response.Persisted {
		t.Error("the unconfirmed save reported the file as persisted")
	}
	if saver.request.Baseline != "baseline" || saver.request.Patch["log.level"] != "debug" {
		t.Errorf("the saver received %+v, want the decoded request", saver.request)
	}
}

// TestPutConfigReportsPersistenceUnavailable covers a write that could not
// replace the file.
func TestPutConfigReportsPersistenceUnavailable(t *testing.T) {
	saver := &fakeConfigSaver{
		err: &configctl.SaveError{
			Code: configctl.SavePersistenceUnavailable,
			Err:  errors.New("injected storage failure"),
		},
	}
	srv := configTestServerWithSaver(t, nil, saver, nil)

	rec := do(t, srv, saveConfigRequest(saveJSON(t, "baseline", map[string]string{"log.level": "debug"})))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if code := decodeSaveError(t, rec).Code; code != configctl.SavePersistenceUnavailable {
		t.Errorf("code = %q, want %q", code, configctl.SavePersistenceUnavailable)
	}
}

// TestPutConfigRejectsMalformedRequests covers a body that is not a save
// request.
func TestPutConfigRejectsMalformedRequests(t *testing.T) {
	saver := &fakeConfigSaver{result: configctl.SaveResult{Applied: []string{}, RestartRequired: []string{}, Persisted: true}}
	srv := configTestServerWithSaver(t, nil, saver, nil)

	rec := do(t, srv, saveConfigRequest(`{"baseline":`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if code := decodeSaveError(t, rec).Code; code != configctl.SaveInvalid {
		t.Errorf("code = %q, want %q", code, configctl.SaveInvalid)
	}
}

// TestPutConfigWithoutASaverFailsLoudly covers a server built without the save
// surface: the route exists, but it reports the missing wiring.
func TestPutConfigWithoutASaverFailsLoudly(t *testing.T) {
	srv := configTestServerWithSaver(t, nil, nil, nil)

	rec := do(t, srv, saveConfigRequest(saveJSON(t, "baseline", map[string]string{"log.level": "debug"})))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
}

// TestConfigSaveRouteIsBehindTheAuthenticationMiddleware covers the boundary:
// with authentication enabled, saving requires a login like every other
// management route.
func TestConfigSaveRouteIsBehindTheAuthenticationMiddleware(t *testing.T) {
	path := writeAPIConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller, _ := configSaveController(t, path)
	srv := configSaveTestServer(t, controller, dynamicAuth)
	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}

	rec := do(t, srv, saveConfigRequest(saveJSON(t, view.Baseline, map[string]string{"log.level": "debug"})))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unauthenticated PUT = %d/%q, want 401/Bearer", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	token := loginForTest(t, srv)
	req := saveConfigRequest(saveJSON(t, view.Baseline, map[string]string{"log.level": "debug"}))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = do(t, srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated PUT = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}
