package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestReadmeHotApplyKeysMatchTheSchema covers the contract that the hand-written
// hot-apply list in README.md names exactly the configuration keys the schema
// marks hot-applied. The list explains an effect the page promises, so a key
// added to or removed from the schema must change the list in the same commit.
func TestReadmeHotApplyKeysMatchTheSchema(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	list := regexp.MustCompile("可热应用的是 ([0-9]+) 个配置项：([^。\n]+)").FindStringSubmatch(string(content))
	if list == nil {
		t.Fatal("README.md no longer carries the hot-apply key list")
	}
	count, err := strconv.Atoi(list[1])
	if err != nil {
		t.Fatalf("parse the hot-apply count %q: %v", list[1], err)
	}

	documented := map[string]bool{}
	for _, match := range regexp.MustCompile("`([a-z0-9_.]+)`").FindAllStringSubmatch(list[2], -1) {
		documented[match[1]] = true
	}

	hot := map[string]bool{}
	for _, key := range config.Schema() {
		if key.HotApply {
			hot[key.Path] = true
		}
	}
	if len(documented) != count {
		t.Errorf("README.md names %d hot-apply keys but announces %d", len(documented), count)
	}
	if len(hot) != count {
		t.Errorf("README.md announces %d hot-apply keys but the schema marks %d", count, len(hot))
	}
	for path := range hot {
		if !documented[path] {
			t.Errorf("the schema marks %q hot-applied but README.md does not name it", path)
		}
	}
	for path := range documented {
		if !hot[path] {
			t.Errorf("README.md names %q hot-applied but the schema does not mark it", path)
		}
	}
}
