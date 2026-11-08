package configctl_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
)

// writeExternalConfigFile replaces the configuration file behind the
// controller, the way an editor or a ConfigMap update would.
func writeExternalConfigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("change the configuration file externally: %v", err)
	}
}

// TestReloadAppliesAHotExternalChange covers the primary contract of the
// external reload: an edit on disk goes through the same load and apply path
// as a save, so the running component receives the new value and the view
// reports the successful reload.
func TestReloadAppliesAHotExternalChange(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)

	writeExternalConfigFile(t, path, "[log]\nlevel = \"debug\"\n")

	result, err := controller.CheckExternalChange()
	if err != nil {
		t.Fatalf("CheckExternalChange: %v", err)
	}
	if !result.Changed {
		t.Error("the check did not report the external change")
	}
	if strings.Join(result.Applied, ",") != "log.level" {
		t.Errorf("applied = %v, want [log.level]", result.Applied)
	}
	if len(result.RestartRequired) != 0 {
		t.Errorf("restart required = %v, want none", result.RestartRequired)
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
		t.Errorf("log.level after the reload = file %q, effective %q, source %q; want debug/debug/file",
			level.FileValue, level.EffectiveValue, level.Source)
	}
	if view.FileError != "" {
		t.Errorf("file_error = %q, want empty", view.FileError)
	}
	if view.LastReload.At == nil || !view.LastReload.OK {
		t.Errorf("last_reload = %+v, want a successful timestamped reload", view.LastReload)
	}
	if strings.Join(view.LastReload.Keys, ",") != "log.level" {
		t.Errorf("last_reload keys = %v, want [log.level]", view.LastReload.Keys)
	}
	if view.LastReload.Error != "" {
		t.Errorf("last_reload error = %q, want empty", view.LastReload.Error)
	}

	// The poll runs again two seconds later. An unchanged file is not
	// reloaded a second time.
	result, err = controller.CheckExternalChange()
	if err != nil {
		t.Fatalf("second CheckExternalChange: %v", err)
	}
	if result.Changed {
		t.Error("the second check reported a change for an unchanged file")
	}
	if len(applier.applied) != 1 {
		t.Errorf("the applier ran %d times after an unchanged poll, want 1", len(applier.applied))
	}
}

// TestReloadKeepsAColdChangeForTheNextStart covers the apply boundary: a
// restart-only key changes on disk and in the file view, but the running
// configuration keeps the old value until the next start.
func TestReloadKeepsAColdChangeForTheNextStart(t *testing.T) {
	path := writeConfigFile(t, "[cache]\ncapacity = \"1GiB\"\n")
	controller := loadController(t, path)
	// An applier for another component must not receive a cold change.
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)

	writeExternalConfigFile(t, path, "[cache]\ncapacity = \"512MiB\"\n")

	result, err := controller.CheckExternalChange()
	if err != nil {
		t.Fatalf("CheckExternalChange: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("applied = %v, want none for a cold key", result.Applied)
	}
	if strings.Join(result.RestartRequired, ",") != "cache.capacity" {
		t.Errorf("restart required = %v, want [cache.capacity]", result.RestartRequired)
	}
	if len(applier.applied) != 0 {
		t.Error("the reload applied a cold change to a component")
	}

	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	capacity := keyByPath(t, view, "cache.capacity")
	if capacity.FileValue != "512MiB" {
		t.Errorf("file value = %q, want the externally written 512MiB", capacity.FileValue)
	}
	if capacity.EffectiveValue != "1GiB" {
		t.Errorf("effective value = %q, want the running 1GiB until the restart", capacity.EffectiveValue)
	}
	if capacity.HotApply {
		t.Error("cache.capacity reports hot_apply=true, want the restart-required badge")
	}
	if view.LastReload.At == nil || !view.LastReload.OK {
		t.Errorf("last_reload = %+v, want a successful timestamped reload", view.LastReload)
	}
	if strings.Join(view.LastReload.Keys, ",") != "cache.capacity" {
		t.Errorf("last_reload keys = %v, want [cache.capacity]", view.LastReload.Keys)
	}
}

// TestReloadKeepsTheRunningConfigurationOnInvalidContent covers the failure
// contract: a broken external edit changes neither the running configuration
// nor the file values the page shows, the view degrades to the last known good
// values with the file error instead of failing, and the failure is recorded
// once per file version.
func TestReloadKeepsTheRunningConfigurationOnInvalidContent(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)

	writeExternalConfigFile(t, path, "[log]\nlevel = \"verbose\"\n")

	if _, err := controller.CheckExternalChange(); err == nil {
		t.Fatal("CheckExternalChange accepted an invalid configuration file")
	}
	if len(applier.applied) != 0 {
		t.Error("the invalid reload applied a configuration to a component")
	}

	view, err := controller.View()
	if err != nil {
		t.Fatalf("the view failed for an invalid configuration file: %v", err)
	}
	level := keyByPath(t, view, "log.level")
	if level.FileValue != "info" {
		t.Errorf("file value = %q, want the last known good info", level.FileValue)
	}
	if level.EffectiveValue != "info" {
		t.Errorf("effective value = %q, want the unchanged info", level.EffectiveValue)
	}
	if view.FileError == "" {
		t.Error("the view reports no file error for an invalid configuration file")
	}
	if strings.Contains(view.FileError, "verbose") {
		t.Errorf("file_error repeats the rejected value: %q", view.FileError)
	}
	if view.LastReload.At == nil || view.LastReload.OK {
		t.Errorf("last_reload = %+v, want a failed timestamped reload", view.LastReload)
	}
	if view.LastReload.Error == "" {
		t.Error("last_reload carries no failure reason")
	}
	if !strings.Contains(view.LastReload.Error, "must be one of debug, info, warn, error") {
		t.Errorf("last_reload reason uses different wording than a start: %q", view.LastReload.Error)
	}

	// A second poll on the same invalid content is not a second failure the
	// page or the log reports as a new event.
	if _, err := controller.CheckExternalChange(); err == nil {
		t.Fatal("the repeated check reported success for an invalid file")
	}
	view, err = controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	if view.LastReload.Error == "" {
		t.Error("the repeated invalid check cleared the recorded failure")
	}
}

// TestReloadKeepsEnvironmentOverrides covers the source constraint: the
// environment is fixed for the process, so an external file edit of an
// overridden key changes the file view but never the running value.
func TestReloadKeepsEnvironmentOverrides(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")
	controller := loadController(t, path)
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)

	writeExternalConfigFile(t, path, "[log]\nlevel = \"warn\"\n")

	if _, err := controller.CheckExternalChange(); err != nil {
		t.Fatalf("CheckExternalChange: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Error("the reload applied a file value the environment overrides")
	}
	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	level := keyByPath(t, view, "log.level")
	if level.FileValue != "warn" {
		t.Errorf("file value = %q, want the externally written warn", level.FileValue)
	}
	if level.EffectiveValue != "debug" || level.Source != "environment" {
		t.Errorf("log.level = effective %q, source %q; want the environment value debug/environment",
			level.EffectiveValue, level.Source)
	}
}

// TestReloadReportsAMissingFile covers a configuration file that disappears:
// the running configuration stays, the view reports the read error, and the
// polls that find the file still missing do not repeat the failure.
func TestReloadReportsAMissingFile(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the configuration file: %v", err)
	}
	if _, err := controller.CheckExternalChange(); err == nil {
		t.Fatal("CheckExternalChange accepted a missing configuration file")
	}
	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	if view.LastReload.At == nil || view.LastReload.OK {
		t.Errorf("last_reload = %+v, want a failed timestamped reload", view.LastReload)
	}
	if view.LastReload.Error == "" || view.FileError == "" {
		t.Errorf("missing file error = reload %q, file %q; want both recorded", view.LastReload.Error, view.FileError)
	}
	if got := keyByPath(t, view, "log.level").EffectiveValue; got != "info" {
		t.Errorf("running log.level = %q, want the unchanged info", got)
	}
}

// TestReloadLogsOncePerFileVersion covers the audit contract of the poll: a
// failed edit is one recorded failure and one log record, not one per poll,
// and a later valid edit is the next event.
func TestReloadLogsOncePerFileVersion(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	var audit bytes.Buffer
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	controller := configctl.New(path, cfg, sources,
		configctl.WithLogger(slog.New(slog.NewTextHandler(&audit, nil))))

	writeExternalConfigFile(t, path, "[log]\nlevel = \"verbose\"\n")
	_, _ = controller.CheckExternalChange()
	_, _ = controller.CheckExternalChange()

	if got := strings.Count(audit.String(), "configuration reload failed"); got != 1 {
		t.Errorf("the invalid file was logged %d times, want once:\n%s", got, audit.String())
	}
	if strings.Contains(audit.String(), "configuration reloaded") {
		t.Errorf("the invalid file was logged as a successful reload:\n%s", audit.String())
	}

	writeExternalConfigFile(t, path, "[log]\nlevel = \"debug\"\n")
	if _, err := controller.CheckExternalChange(); err != nil {
		t.Fatalf("CheckExternalChange: %v", err)
	}
	if got := strings.Count(audit.String(), "configuration reloaded"); got != 1 {
		t.Errorf("the recovered file was logged %d times, want once:\n%s", got, audit.String())
	}
	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	if view.FileError != "" {
		t.Errorf("file_error = %q after a valid edit, want empty", view.FileError)
	}
	if !view.LastReload.OK {
		t.Errorf("last_reload = %+v, want the recovered valid edit", view.LastReload)
	}
	if !strings.Contains(audit.String(), "log.level") {
		t.Errorf("the reload audit does not name the changed key:\n%s", audit.String())
	}
	if strings.Contains(audit.String(), "verbose") {
		t.Errorf("the reload audit repeats the rejected value:\n%s", audit.String())
	}
}

// TestSaveDoesNotReloadItsOwnWrite covers self-write suppression: the save
// adopts the bytes it wrote as the baseline, so the poll after the save finds
// no external change, applies nothing again and logs no second event.
func TestSaveDoesNotReloadItsOwnWrite(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	var audit bytes.Buffer
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	controller := configctl.New(path, cfg, sources,
		configctl.WithLogger(slog.New(slog.NewTextHandler(&audit, nil))))
	applier := &recordingApplier{keys: []string{"log.level"}}
	registerApplier(t, controller, applier)

	if _, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: baselineOf(t, controller),
		Patch:    map[string]string{"log.level": "debug"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	result, err := controller.CheckExternalChange()
	if err != nil {
		t.Fatalf("CheckExternalChange after the save: %v", err)
	}
	if result.Changed {
		t.Error("the poll reported the save's own write as an external change")
	}
	if len(applier.applied) != 1 {
		t.Errorf("the applier ran %d times, want only the save's one", len(applier.applied))
	}
	if strings.Contains(audit.String(), "configuration reloaded") {
		t.Errorf("the poll after the save logged a reload:\n%s", audit.String())
	}

	view, err := controller.View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	if view.LastReload.At != nil {
		t.Errorf("last_reload = %+v, want untouched by the save's own write", view.LastReload)
	}
}

// signalApplier is a goroutine-safe applier: Watch runs on another goroutine,
// so the test observes the apply through a channel instead of a slice.
type signalApplier struct {
	keys    []string
	applied chan config.Config
}

func (a *signalApplier) Keys() []string { return a.keys }

func (a *signalApplier) Apply(cfg config.Config) error {
	a.applied <- cfg
	return nil
}

// TestWatchReloadsOnAnInjectedTick covers the poll loop: Watch checks the file
// on every tick of the injected ticker, applies the external change, and
// stops when its context is cancelled. Driving the ticker from the test keeps
// the poll deterministic instead of waiting for the real two-second interval.
func TestWatchReloadsOnAnInjectedTick(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	controller := loadController(t, path)
	applier := &signalApplier{keys: []string{"log.level"}, applied: make(chan config.Config, 1)}
	registerApplier(t, controller, applier)

	ticks := make(chan time.Time)
	configctl.SetReloadTickerForTest(controller, func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() {}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.Watch(ctx)
	}()

	writeExternalConfigFile(t, path, "[log]\nlevel = \"debug\"\n")
	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not poll")
	}
	select {
	case cfg := <-applier.applied:
		if cfg.Log.Level != "debug" {
			t.Errorf("the watcher applied log.level %q, want debug", cfg.Log.Level)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not apply the external change")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}
}

// TestWatchWithoutAConfigurationFileReturnsImmediately covers the deployment
// that started without a configuration file: there is nothing to poll.
func TestWatchWithoutAConfigurationFileReturnsImmediately(t *testing.T) {
	controller := configctl.New("", config.Default(), config.Sources{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.Watch(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch blocked without a configuration file")
	}
}
