package configwrite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configcheck"
	"github.com/yakumioto/torrentfs-go/internal/configwrite"
)

const document = `[log]
level = "info"

[cache]
capacity = "2GiB"
`

// TestPrepareAppliesThePatchAndValidatesTheDocument covers the shared pipeline:
// the file layer is loaded through the start loader, the patch is rendered into
// a sparse document, and the document is validated with the process rules.
func TestPrepareAppliesThePatchAndValidatesTheDocument(t *testing.T) {
	file, err := configwrite.Load([]byte(document), "torrentfs.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prepared, err := configwrite.Prepare(file, "torrentfs.toml", map[string]string{"log.level": "debug"}, false)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Effective.Log.Level != "debug" {
		t.Errorf("effective log.level = %q, want debug", prepared.Effective.Log.Level)
	}
	if !prepared.EffectiveSources.FromFile("log.level") {
		t.Errorf("the source of log.level = %q, want file", prepared.EffectiveSources.SourceOf("log.level"))
	}
	if len(prepared.Changed) != 1 || prepared.Changed[0] != "log.level" {
		t.Errorf("changed = %v, want [log.level]", prepared.Changed)
	}
	if !strings.Contains(string(prepared.Document), `level = "debug"`) {
		t.Errorf("the rendered document does not carry the patch:\n%s", prepared.Document)
	}
}

// TestPrepareOnlyMissingKeepsTheFileValue covers the migration rule: a value
// the configuration file carries wins over a retired sidecar.
func TestPrepareOnlyMissingKeepsTheFileValue(t *testing.T) {
	file, err := configwrite.Load([]byte(document), "torrentfs.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prepared, err := configwrite.Prepare(file, "torrentfs.toml", map[string]string{"log.level": "debug"}, true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Effective.Log.Level != "info" {
		t.Errorf("effective log.level = %q, want the file value info", prepared.Effective.Log.Level)
	}
	if len(prepared.Changed) != 0 {
		t.Errorf("changed = %v, want no change", prepared.Changed)
	}
}

// TestPrepareReportsEveryRejectedKey covers the one-save-one-report contract:
// a malformed value does not hide the validation of the other keys.
func TestPrepareReportsEveryRejectedKey(t *testing.T) {
	file, err := configwrite.Load([]byte(document), "torrentfs.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = configwrite.Prepare(file, "torrentfs.toml", map[string]string{
		"log.level":      "debug",
		"cache.capacity": "not-a-quantity",
	}, false)
	if err == nil {
		t.Fatal("Prepare accepted a malformed value")
	}
	fields := configcheck.FieldErrors(err)
	if len(fields) == 0 || fields[0].Path != "cache.capacity" {
		t.Errorf("field errors = %+v, want cache.capacity", fields)
	}
}

// TestPublishReportsAnUnpublishedWrite covers the shared storage boundary: a
// writer that reports no publication is a failure even without an error.
func TestPublishReportsAnUnpublishedWrite(t *testing.T) {
	published, err := configwrite.Publish("torrentfs.toml", []byte("x"), func(string, []byte) (bool, error) {
		return false, nil
	})
	if published || err == nil {
		t.Fatalf("Publish = (%v, %v), want unpublished with an error", published, err)
	}
	if !strings.Contains(err.Error(), "did not publish") {
		t.Errorf("Publish error = %v, want it to name the missing publication", err)
	}
}

// TestPublishPropagatesTheWriterError covers the failure the controller turns
// into persistence_unavailable.
func TestPublishPropagatesTheWriterError(t *testing.T) {
	cause := errors.New("injected storage failure")
	published, err := configwrite.Publish("torrentfs.toml", []byte("x"), func(string, []byte) (bool, error) {
		return false, cause
	})
	if published || !errors.Is(err, cause) {
		t.Fatalf("Publish = (%v, %v), want the writer error", published, err)
	}
}

// TestLoadReturnsTheFileLayerWithoutTheEnvironment covers the layer the
// write-back edits: the environment never reaches a configuration file.
func TestLoadReturnsTheFileLayerWithoutTheEnvironment(t *testing.T) {
	file, err := configwrite.Load([]byte(document), "torrentfs.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if file.Config.Log.Level != "info" {
		t.Errorf("file log.level = %q, want info", file.Config.Log.Level)
	}
	if file.Sources.SourceOf("log.level") != config.SourceFile {
		t.Errorf("the source of log.level = %q, want file", file.Sources.SourceOf("log.level"))
	}
	if file.Sources.SourceOf("backend") != config.SourceDefault {
		t.Errorf("the source of an unset key = %q, want default", file.Sources.SourceOf("backend"))
	}
}
