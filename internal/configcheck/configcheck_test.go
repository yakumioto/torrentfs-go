package configcheck_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configcheck"
)

// writeConfigFile writes a configuration file for the check tests.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write configuration file: %v", err)
	}
	return path
}

// TestLoadAppliesTheValueAndProcessRulesTogether covers the contract that a
// start reports every problem in one run: the value rules, the "one output"
// rule, and the cross-field rules of the upload statistics experiment.
func TestLoadAppliesTheValueAndProcessRulesTogether(t *testing.T) {
	path := writeConfigFile(t, `backend = "none"

[http]
listen_addr = ""

[cache]
capacity = "0B"
`)
	_, _, err := configcheck.Load(path)
	if err == nil {
		t.Fatal("Load accepted a configuration that cannot start")
	}
	for _, want := range []string{"cache.capacity", "at least one output must be enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report does not name %q: %v", want, err)
		}
	}
}

// TestLoadAppliesTheExperimentCrossFieldRules covers the contract that the
// rules relating one experiment setting to another are part of the start path,
// not only of the configuration command.
func TestLoadAppliesTheExperimentCrossFieldRules(t *testing.T) {
	path := writeConfigFile(t, `backend = "none"

[experiments.upload]
mode = "inject"
`)
	_, _, err := configcheck.Load(path)
	if err == nil {
		t.Fatal("Load accepted an inject experiment without a rate")
	}
	if !strings.Contains(err.Error(), "experiments.upload.rate") {
		t.Errorf("the report does not name experiments.upload.rate: %v", err)
	}
}

// TestLoadKeepsOneProblemOneDiagnosis covers the contract that a rejected
// experiment value is not restated by the cross-field rules.
func TestLoadKeepsOneProblemOneDiagnosis(t *testing.T) {
	path := writeConfigFile(t, `[experiments.upload]
mode = "turbo"
`)
	_, _, err := configcheck.Load(path)
	if err == nil {
		t.Fatal("Load accepted an unknown experiment mode")
	}
	if strings.Contains(err.Error(), "experiments.upload experiment") {
		t.Errorf("the report restates the value problem as an experiment rule: %v", err)
	}
}

// TestFieldErrorsNameEveryRejectedKey covers the per-key contract of the save
// path: the page shows each problem next to the field that caused it.
func TestFieldErrorsNameEveryRejectedKey(t *testing.T) {
	path := writeConfigFile(t, `[cache]
capacity = "0B"

[log]
level = "nonsense"
`)
	_, _, err := configcheck.Load(path)
	if err == nil {
		t.Fatal("Load accepted an invalid configuration")
	}
	fields := configcheck.FieldErrors(err)
	byPath := map[string]string{}
	for _, field := range fields {
		byPath[field.Path] = field.Message
	}
	for _, want := range []string{"cache.capacity", "log.level"} {
		message, ok := byPath[want]
		if !ok {
			t.Fatalf("FieldErrors did not name %q: %+v", want, fields)
		}
		if message == "" {
			t.Errorf("the error of %q has no message", want)
		}
	}
}

// TestFieldErrorsAttributesTheExperimentRulesToTheSection covers the contract
// that a cross-field experiment problem has a stable place on the page: the
// rule relates several keys, so it belongs to the experiment section.
func TestFieldErrorsAttributesTheExperimentRulesToTheSection(t *testing.T) {
	path := writeConfigFile(t, `[experiments.upload]
mode = "baseline"
rate = "1MiB/s"
`)
	_, _, err := configcheck.Load(path)
	if err == nil {
		t.Fatal("Load accepted a baseline experiment with a rate")
	}
	fields := configcheck.FieldErrors(err)
	if len(fields) != 1 {
		t.Fatalf("FieldErrors = %+v, want one experiment problem", fields)
	}
	if fields[0].Path != "experiments.upload" {
		t.Errorf("the experiment problem names %q, want experiments.upload", fields[0].Path)
	}
	if !strings.Contains(fields[0].Message, "experiments.upload.rate") {
		t.Errorf("the experiment problem does not name the setting: %q", fields[0].Message)
	}
}

// TestFieldErrorsKeepsAGeneralProblem covers a failure that belongs to no
// single key, such as a removed key: it is still reported, with an empty path.
func TestFieldErrorsKeepsAGeneralProblem(t *testing.T) {
	path := writeConfigFile(t, "[lab]\nupload_mode = \"baseline\"\n")
	_, _, err := configcheck.Load(path)
	if err == nil {
		t.Fatal("Load accepted a removed configuration section")
	}
	fields := configcheck.FieldErrors(err)
	if len(fields) != 1 || fields[0].Path != "" {
		t.Fatalf("FieldErrors = %+v, want one problem without a key", fields)
	}
	if !strings.Contains(fields[0].Message, "lab") {
		t.Errorf("the general problem does not name the removed section: %q", fields[0].Message)
	}
}

// TestLoadContentMatchesThePathLoader covers the save path: the rendered
// document is validated through the same rules and the same environment as a
// start.
func TestLoadContentMatchesThePathLoader(t *testing.T) {
	body := "backend = \"none\"\n[log]\nlevel = \"warn\"\n"
	path := writeConfigFile(t, body)
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")

	fromPath, pathSources, err := configcheck.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fromContent, contentSources, err := configcheck.LoadContent([]byte(body), path)
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	if !reflect.DeepEqual(fromContent, fromPath) {
		t.Errorf("content configuration = %+v, want the path configuration %+v", fromContent, fromPath)
	}
	if len(contentSources) != len(pathSources) {
		t.Errorf("content sources = %v, want the path sources %v", contentSources, pathSources)
	}
	for key, source := range pathSources {
		if contentSources[key] != source {
			t.Errorf("source of %q = %q, want %q", key, contentSources[key], source)
		}
	}
}

// TestLoadReportsTheStartupWording covers the contract that the page and a
// start use the same rules and wording: the message of a rejected value is the
// one the configuration loader produces.
func TestLoadReportsTheStartupWording(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"nonsense\"\n")
	_, _, checkErr := configcheck.Load(path)
	_, loadErr := config.Load(path)
	if checkErr == nil || loadErr == nil {
		t.Fatal("an invalid log level was accepted")
	}
	if !strings.Contains(loadErr.Error(), "log.level") {
		t.Fatalf("the loader message does not name log.level: %v", loadErr)
	}
	if !strings.Contains(checkErr.Error(), "config: log.level: must be one of debug, info, warn, error") {
		t.Errorf("the check message differs from the startup wording: %v", checkErr)
	}
}
