package config

import (
	"testing"
)

// TestSourcesReportWhereEveryValueCameFrom covers the source record: it
// names the input that won for every configuration key.
func TestSourcesReportWhereEveryValueCameFrom(t *testing.T) {
	path := writeConfigFile(t, "backend = \"none\"\n[cache]\ncapacity = \"1GiB\"\n")
	_, sources, err := loadWithSources(path, lookupEnvironment(map[string]string{
		"TORRENTFS_LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !sources.FromFile("cache.capacity") {
		t.Fatalf("cache.capacity source = %q, want the configuration file", sources.SourceOf("cache.capacity"))
	}
	if !sources.FromEnvironment("log.level") {
		t.Fatalf("log.level source = %q, want the environment", sources.SourceOf("log.level"))
	}
	if sources.FromFile("log.level") || sources.FromEnvironment("cache.capacity") {
		t.Fatal("a configuration key reported the wrong source")
	}
	if got := sources.SourceOf("backend"); got != SourceFile {
		t.Fatalf("backend source = %q, want %q", got, SourceFile)
	}
}

// TestSourcesReportTheDefaultForAnUntouchedKey covers the untouched keys: a
// key no input set kept its built-in default.
func TestSourcesReportTheDefaultForAnUntouchedKey(t *testing.T) {
	_, sources, err := loadWithSources("", lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := sources.SourceOf("cache.capacity"); got != SourceDefault {
		t.Fatalf("cache.capacity source = %q, want %q", got, SourceDefault)
	}
	if sources.FromFile("cache.capacity") || sources.FromEnvironment("cache.capacity") {
		t.Fatal("an untouched key reported a value source")
	}
}

// TestSourcesReportTheEnvironmentOverTheFile covers the precedence in the
// source record: the environment wins over the configuration file.
func TestSourcesReportTheEnvironmentOverTheFile(t *testing.T) {
	path := writeConfigFile(t, "[cache]\ncapacity = \"1GiB\"\n")
	_, sources, err := loadWithSources(path, lookupEnvironment(map[string]string{
		"TORRENTFS_CACHE_CAPACITY": "2GiB",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !sources.FromEnvironment("cache.capacity") {
		t.Fatalf("cache.capacity source = %q, want the environment", sources.SourceOf("cache.capacity"))
	}
}

// TestSourcesReportThePasswordOrigin covers the credential warning: the
// record tells whether the password came from the configuration file.
func TestSourcesReportThePasswordOrigin(t *testing.T) {
	fromFile, sources, err := loadWithSources(
		writeConfigFile(t, "password = \"a-password\"\n"), lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fromFile.Password != "a-password" {
		t.Fatalf("password = %q, want the configured password", fromFile.Password)
	}
	if !sources.FromFile("password") {
		t.Fatalf("password source = %q, want the configuration file", sources.SourceOf("password"))
	}

	fromEnvironment, sources, err := loadWithSources("", lookupEnvironment(map[string]string{
		"TORRENTFS_PASSWORD": "an-environment-password",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fromEnvironment.Password != "an-environment-password" {
		t.Fatalf("password = %q, want the environment password", fromEnvironment.Password)
	}
	if !sources.FromEnvironment("password") {
		t.Fatalf("password source = %q, want the environment", sources.SourceOf("password"))
	}
	if sources.FromFile("password") {
		t.Fatal("an environment password reported the configuration file as its source")
	}
}

// TestSourcesCoverEverySchemaKey covers the completeness of the record: every
// configuration key has a source, so a caller never has to guess.
func TestSourcesCoverEverySchemaKey(t *testing.T) {
	_, sources, err := loadWithSources("", lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, key := range Schema() {
		if _, ok := sources[key.Path]; !ok {
			t.Fatalf("the configuration key %q has no source", key.Path)
		}
	}
}
