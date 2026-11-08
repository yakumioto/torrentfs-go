package configctl_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
)

// recordingApplier is a component applier that records the configurations it
// was asked to apply. It is the seam the save path uses to drive a component.
type recordingApplier struct {
	keys    []string
	applied []config.Config
	err     error
}

func (a *recordingApplier) Keys() []string { return a.keys }

func (a *recordingApplier) Apply(cfg config.Config) error {
	a.applied = append(a.applied, cfg)
	return a.err
}

// loadController loads the configuration and builds a controller for it.
func loadController(t *testing.T, path string) *configctl.Controller {
	t.Helper()
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	return configctl.New(path, cfg, sources)
}

// registerApplier registers an applier and fails the test when the
// registration is refused.
func registerApplier(t *testing.T, controller *configctl.Controller, applier configctl.Applier) {
	t.Helper()
	if err := controller.RegisterApplier(applier); err != nil {
		t.Fatalf("RegisterApplier: %v", err)
	}
}

// baselineOf returns the current baseline digest of a configuration file.
func baselineOf(t *testing.T, controller *configctl.Controller) string {
	t.Helper()
	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	return view.Baseline
}

// saveError returns the SaveError of a failed save.
func saveError(t *testing.T, err error) *configctl.SaveError {
	t.Helper()
	var saveErr *configctl.SaveError
	if !errors.As(err, &saveErr) {
		t.Fatalf("save error %v is not a SaveError", err)
	}
	return saveErr
}

// TestSaveAppliesAHotKeyAndWritesTheFile covers the smallest save path: the
// page patches a hot-applied key with a fresh baseline, the append-only write
// back keeps the rest of the file, and the running component receives the new
// value.
func TestSaveAppliesAHotKeyAndWritesTheFile(t *testing.T) {
	body := `username = "alice"
password = "s3cr3t"

[cache]
capacity = "1GiB"

[log]
level = "info"
format = "text"
`
	path := writeConfigFile(t, body)
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)

	result, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "debug"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if result.Persisted != true {
		t.Error("the save did not report the configuration as persisted")
	}
	if strings.Join(result.Applied, ",") != "log.level" {
		t.Errorf("applied = %v, want [log.level]", result.Applied)
	}
	if len(result.RestartRequired) != 0 {
		t.Errorf("restart required = %v, want none", result.RestartRequired)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	written := string(content)
	for _, want := range []string{
		`username = "alice"`,
		`password = "s3cr3t"`,
		`capacity = "1GiB"`,
		`level = "debug"`,
		`format = "text"`,
	} {
		if !strings.Contains(written, want) {
			t.Errorf("the written configuration does not hold %q:\n%s", want, written)
		}
	}
	if strings.Contains(written, `level = "info"`) {
		t.Errorf("the written configuration still holds the old level:\n%s", written)
	}

	backup, err := os.ReadFile(config.WriteBackBackupPath(path))
	if err != nil {
		t.Fatalf("read the configuration backup: %v", err)
	}
	if !strings.Contains(string(backup), `level = "info"`) {
		t.Errorf("the backup does not hold the previous configuration:\n%s", backup)
	}

	if len(applier.applied) != 1 {
		t.Fatalf("the applier received %d configurations, want 1", len(applier.applied))
	}
	if applier.applied[0].Log.Level != "debug" {
		t.Errorf("the applier received log.level %q, want debug", applier.applied[0].Log.Level)
	}

	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	level := keyByPath(t, view, "log.level")
	if level.FileValue != "debug" || level.EffectiveValue != "debug" || level.Source != "file" {
		t.Errorf("log.level after the save = file %q, effective %q, source %q; want debug/debug/file",
			level.FileValue, level.EffectiveValue, level.Source)
	}
	if view.LastSave.At == nil || !view.LastSave.OK {
		t.Errorf("last_save = %+v, want a successful timestamped save", view.LastSave)
	}
	if strings.Join(view.LastSave.Keys, ",") != "log.level" {
		t.Errorf("last_save keys = %v, want [log.level]", view.LastSave.Keys)
	}
	if view.LastSave.Error != "" {
		t.Errorf("last_save error = %q, want empty", view.LastSave.Error)
	}
}

// TestSaveRejectsAStaleBaseline covers the conflict contract: a file another
// writer changed after the page was rendered is not overwritten.
func TestSaveRejectsAStaleBaseline(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)
	baseline := baselineOf(t, controller)

	external := "backend = \"none\"\n\n[log]\nlevel = \"warn\"\n"
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatalf("change the configuration file externally: %v", err)
	}

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baseline,
		Patch:    map[string]string{"log.level": "debug"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveConflict {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveConflict)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if string(content) != external {
		t.Errorf("the conflicting save changed the file:\n%s", content)
	}
	if len(applier.applied) != 0 {
		t.Error("the conflicting save applied a configuration")
	}
	view, viewErr := controller.View()
	if viewErr != nil {
		t.Fatalf("configuration view: %v", viewErr)
	}
	if view.LastSave.OK || view.LastSave.At == nil {
		t.Errorf("last_save = %+v, want a failed timestamped save", view.LastSave)
	}
}

// TestSaveValidationFailureLeavesEverythingUntouched covers the contract that
// validation runs before the rename: a rejected value changes neither the file
// nor the running configuration.
func TestSaveValidationFailureLeavesEverythingUntouched(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)

	_, err = controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "verbose"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveInvalid {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveInvalid)
	}
	if len(saveErr.FieldErrors) == 0 {
		t.Fatal("the rejected save reported no field errors")
	}
	if saveErr.FieldErrors[0].Path != "log.level" {
		t.Errorf("the field error names %q, want log.level", saveErr.FieldErrors[0].Path)
	}
	if !strings.Contains(saveErr.FieldErrors[0].Message, "must be one of debug, info, warn, error") {
		t.Errorf("the field error uses different wording than a start: %q", saveErr.FieldErrors[0].Message)
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if string(after) != string(before) {
		t.Errorf("the rejected save changed the file:\n%s", after)
	}
	if len(applier.applied) != 0 {
		t.Error("the rejected save applied a configuration")
	}
	view, viewErr := controller.View()
	if viewErr != nil {
		t.Fatalf("configuration view: %v", viewErr)
	}
	if got := keyByPath(t, view, "log.level").EffectiveValue; got != "info" {
		t.Errorf("the running log.level = %q, want the unchanged value info", got)
	}
	if view.LastSave.OK || view.LastSave.At == nil {
		t.Errorf("last_save = %+v, want a failed timestamped save", view.LastSave)
	}
}

// TestSaveReportsEveryRejectedKey covers the contract that one save reports
// every problem, so the page can show all of them at once.
func TestSaveReportsEveryRejectedKey(t *testing.T) {
	path := writeConfigFile(t, "[cache]\ncapacity = \"1GiB\"\n\n[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch: map[string]string{
			"cache.capacity": "0B",
			"log.level":      "verbose",
		},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveInvalid {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveInvalid)
	}
	reported := map[string]bool{}
	for _, field := range saveErr.FieldErrors {
		reported[field.Path] = true
	}
	for _, want := range []string{"cache.capacity", "log.level"} {
		if !reported[want] {
			t.Errorf("the save did not report %q: %+v", want, saveErr.FieldErrors)
		}
	}
}

// TestSaveReportsAnUnparsableValue covers the formatting errors of a patch: a
// value the configuration cannot read is reported against its key and nothing
// is written.
func TestSaveReportsAnUnparsableValue(t *testing.T) {
	path := writeConfigFile(t, "[cache]\ncapacity = \"1GiB\"\n")
	controller := loadController(t, path)

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"cache.capacity": "huge"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveInvalid {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveInvalid)
	}
	if len(saveErr.FieldErrors) != 1 || saveErr.FieldErrors[0].Path != "cache.capacity" {
		t.Fatalf("field errors = %+v, want one cache.capacity problem", saveErr.FieldErrors)
	}
}

// TestSaveReportsTheExperimentCrossFieldRules covers the contract that the
// save path applies the same cross-field rules as a start.
func TestSaveReportsTheExperimentCrossFieldRules(t *testing.T) {
	path := writeConfigFile(t, "[experiments.upload]\nmode = \"baseline\"\n")
	controller := loadController(t, path)

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"experiments.upload.mode": "inject"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveInvalid {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveInvalid)
	}
	if len(saveErr.FieldErrors) != 1 || saveErr.FieldErrors[0].Path != "experiments.upload" {
		t.Fatalf("field errors = %+v, want one experiments.upload problem", saveErr.FieldErrors)
	}
	if !strings.Contains(saveErr.FieldErrors[0].Message, "experiments.upload.rate") {
		t.Errorf("the experiment problem does not name the rate: %q", saveErr.FieldErrors[0].Message)
	}
}

// TestSaveRejectsAnUnknownKey covers a malformed patch: a path the schema does
// not know is rejected instead of being written.
func TestSaveRejectsAnUnknownKey(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.verbosity": "debug"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveInvalid {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveInvalid)
	}
	if len(saveErr.FieldErrors) != 1 || saveErr.FieldErrors[0].Path != "log.verbosity" {
		t.Fatalf("field errors = %+v, want one log.verbosity problem", saveErr.FieldErrors)
	}
}

// TestSaveRejectsEnvironmentOverriddenKeys covers the source constraint: the
// environment is fixed for the life of the process, so a file change could
// never take effect.
func TestSaveRejectsEnvironmentOverriddenKeys(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")
	controller := loadController(t, path)

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "warn"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveReadOnly {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveReadOnly)
	}
	if len(saveErr.FieldErrors) != 1 || saveErr.FieldErrors[0].Path != "log.level" {
		t.Fatalf("field errors = %+v, want one log.level problem", saveErr.FieldErrors)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if strings.Contains(string(content), "warn") {
		t.Errorf("the read-only save changed the file:\n%s", content)
	}
}

// TestSaveRejectsSecretKeys covers the security boundary: the page never
// receives a secret, so a patch cannot set it.
func TestSaveRejectsSecretKeys(t *testing.T) {
	path := writeConfigFile(t, "username = \"alice\"\npassword = \"s3cr3t\"\n")
	controller := loadController(t, path)

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"password": "a-new-password"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveReadOnly {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveReadOnly)
	}
	if len(saveErr.FieldErrors) != 1 || saveErr.FieldErrors[0].Path != "password" {
		t.Fatalf("field errors = %+v, want one password problem", saveErr.FieldErrors)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if !strings.Contains(string(content), `password = "s3cr3t"`) {
		t.Errorf("the rejected save changed the password:\n%s", content)
	}
	if strings.Contains(saveErr.Error(), "a-new-password") || strings.Contains(saveErr.Error(), "s3cr3t") {
		t.Errorf("the save error disclosed a password: %v", saveErr)
	}
}

// TestSaveReportsAnUnwritableFile covers the read-only deployment: the file
// exists, but the process cannot replace it.
func TestSaveReportsAnUnwritableFile(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod the configuration file: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "debug"},
	})
	if saveErr := saveError(t, err); saveErr.Code != configctl.SaveReadOnly {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveReadOnly)
	}
}

// TestSaveWithoutAConfigurationFile covers a deployment that started without
// -config: there is no file to write a change into.
func TestSaveWithoutAConfigurationFile(t *testing.T) {
	controller := configctl.New("", config.Default(), config.Sources{})
	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Patch: map[string]string{"log.level": "debug"},
	})
	if saveErr := saveError(t, err); saveErr.Code != configctl.SaveNotConfigured {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveNotConfigured)
	}
}

// TestSaveReportsPersistenceUnavailable covers a write that could not replace
// the file: the file and the running configuration stay as they were.
func TestSaveReportsPersistenceUnavailable(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)
	configctl.SetWriteBackForTest(controller, func(string, []byte) (bool, error) {
		return false, fmt.Errorf("%w: injected storage failure", config.ErrWriteBackUnavailable)
	})

	_, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "debug"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SavePersistenceUnavailable {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SavePersistenceUnavailable)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if strings.Contains(string(content), "debug") {
		t.Errorf("the failed write-back changed the file:\n%s", content)
	}
	if len(applier.applied) != 0 {
		t.Error("the failed write-back applied a configuration")
	}
}

// TestSaveReportsDurabilityUnconfirmed covers the contract inherited from the
// upload-rate save: once the rename committed the values are in force even
// when the directory sync failed, so the save reports them as applied and not
// as a plain failure.
func TestSaveReportsDurabilityUnconfirmed(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)
	configctl.SetWriteBackForTest(controller, func(path string, document []byte) (bool, error) {
		published, err := config.WriteBackFile(path, document)
		if err != nil {
			return published, err
		}
		return true, fmt.Errorf("%w: injected directory sync failure", config.ErrWriteBackDurabilityUnconfirmed)
	})

	result, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "debug"},
	})
	saveErr := saveError(t, err)
	if saveErr.Code != configctl.SaveDurabilityUnconfirmed {
		t.Fatalf("code = %q, want %q", saveErr.Code, configctl.SaveDurabilityUnconfirmed)
	}
	if result.Persisted {
		t.Error("the unconfirmed save reported the file as persisted")
	}
	if strings.Join(result.Applied, ",") != "log.level" {
		t.Errorf("applied = %v, want [log.level]: the value is in force", result.Applied)
	}
	if strings.Join(saveErr.Applied, ",") != "log.level" {
		t.Errorf("error applied = %v, want [log.level]", saveErr.Applied)
	}
	if len(applier.applied) != 1 {
		t.Error("the committed save did not reach the applier")
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if !strings.Contains(string(content), `level = "debug"`) {
		t.Errorf("the committed save did not write the file:\n%s", content)
	}
	view, viewErr := controller.View()
	if viewErr != nil {
		t.Fatalf("configuration view: %v", viewErr)
	}
	if got := keyByPath(t, view, "log.level").EffectiveValue; got != "debug" {
		t.Errorf("the running log.level = %q, want the applied value debug", got)
	}
	if view.LastSave.OK || view.LastSave.At == nil {
		t.Errorf("last_save = %+v, want a failed timestamped save", view.LastSave)
	}
}

// TestSaveKeepsRestartRequiredValues covers the apply boundary: a key with no
// applier is written but the running configuration keeps the old value until
// the next start.
func TestSaveKeepsRestartRequiredValues(t *testing.T) {
	path := writeConfigFile(t, "[cache]\ncapacity = \"1GiB\"\n")
	controller := loadController(t, path)

	result, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"cache.capacity": "512MiB"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("applied = %v, want none", result.Applied)
	}
	if strings.Join(result.RestartRequired, ",") != "cache.capacity" {
		t.Errorf("restart required = %v, want [cache.capacity]", result.RestartRequired)
	}
	if !result.Persisted {
		t.Error("the save did not report the file as persisted")
	}
	view, viewErr := controller.View()
	if viewErr != nil {
		t.Fatalf("configuration view: %v", viewErr)
	}
	capacity := keyByPath(t, view, "cache.capacity")
	if capacity.FileValue != "512MiB" {
		t.Errorf("file value = %q, want the written 512MiB", capacity.FileValue)
	}
	if capacity.EffectiveValue != "1GiB" {
		t.Errorf("effective value = %q, want the running 1GiB until the restart", capacity.EffectiveValue)
	}
}

// TestSaveMovesAFailedHotApplyToRestartRequired covers an applier that cannot
// apply: the file is already written, so the key waits for a restart instead
// of failing the save.
func TestSaveMovesAFailedHotApplyToRestartRequired(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}, err: errors.New("injected apply failure")}
	registerApplier(t, controller, applier)

	result, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "debug"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(result.Applied) != 0 || strings.Join(result.RestartRequired, ",") != "log.level" {
		t.Errorf("applied = %v, restart required = %v; want none/[log.level]",
			result.Applied, result.RestartRequired)
	}
	view, viewErr := controller.View()
	if viewErr != nil {
		t.Fatalf("configuration view: %v", viewErr)
	}
	if got := keyByPath(t, view, "log.level").EffectiveValue; got != "info" {
		t.Errorf("the running log.level = %q, want the unchanged value info", got)
	}
}

// TestSaveAuditNamesOnlyKeyPaths covers the audit contract: a save records the
// changed configuration keys and never a value, in particular never a secret.
func TestSaveAuditNamesOnlyKeyPaths(t *testing.T) {
	path := writeConfigFile(t, "username = \"alice\"\npassword = \"s3cr3t\"\n\n[log]\nlevel = \"info\"\n")
	var audit bytes.Buffer
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	controller := configctl.New(path, cfg, sources,
		configctl.WithLogger(slog.New(slog.NewTextHandler(&audit, nil))))
	registerApplier(t, controller, &recordingApplier{keys: []string{"log.level"}})

	if _, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "debug"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	record := audit.String()
	if !strings.Contains(record, "configuration saved") {
		t.Fatalf("the save was not audited: %s", record)
	}
	if !strings.Contains(record, "log.level") {
		t.Errorf("the audit does not name the changed key: %s", record)
	}
	if strings.Contains(record, "s3cr3t") {
		t.Errorf("the audit disclosed the password: %s", record)
	}
	if strings.Contains(record, "a-new-password") {
		t.Errorf("the audit disclosed a password: %s", record)
	}
}

// TestConcurrentSavesApplyOnlyOneEdit covers the concurrency contract: a save
// holds the controller while it validates, writes and applies, so concurrent
// saves from the same baseline cannot interleave. Exactly one wins and the
// others report a conflict without touching the file.
func TestConcurrentSavesApplyOnlyOneEdit(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	baseline := baselineOf(t, controller)

	const saves = 8
	type outcome struct {
		result configctl.SaveResult
		err    error
	}
	outcomes := make(chan outcome, saves)
	var wg sync.WaitGroup
	for index := 0; index < saves; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := controller.Save(context.Background(), configctl.SaveRequest{
				Baseline: baseline,
				Patch:    map[string]string{"log.level": "debug"},
			})
			outcomes <- outcome{result: result, err: err}
		}()
		go func() {
			// A view may run while a save replaces the file; it must not
			// observe a torn state or race with the writer.
			if _, err := controller.View(); err != nil {
				t.Errorf("configuration view: %v", err)
			}
		}()
	}
	wg.Wait()
	close(outcomes)

	succeeded, conflicted := 0, 0
	for outcome := range outcomes {
		if outcome.err == nil {
			succeeded++
			continue
		}
		saveErr := saveError(t, outcome.err)
		if saveErr.Code != configctl.SaveConflict {
			t.Errorf("concurrent save failed with %q, want conflict", saveErr.Code)
		}
		conflicted++
	}
	if succeeded != 1 || conflicted != saves-1 {
		t.Errorf("saves succeeded=%d conflicted=%d, want 1/%d", succeeded, conflicted, saves-1)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	if strings.Count(string(content), `level = "debug"`) != 1 {
		t.Errorf("the configuration file does not hold exactly one applied edit:\n%s", content)
	}
	view, viewErr := controller.View()
	if viewErr != nil {
		t.Fatalf("configuration view: %v", viewErr)
	}
	// The last attempt may be one of the conflicting saves, so only the
	// recorded timestamp and the running value are asserted here; the success
	// itself is covered by the file content above.
	if view.LastSave.At == nil {
		t.Errorf("last_save = %+v, want the most recent attempt recorded", view.LastSave)
	}
	level := keyByPath(t, view, "log.level")
	if level.FileValue != "debug" {
		t.Errorf("file log.level = %q, want the written debug", level.FileValue)
	}
	if level.EffectiveValue != "info" {
		t.Errorf("running log.level = %q, want the unchanged info: no applier is registered", level.EffectiveValue)
	}
}
